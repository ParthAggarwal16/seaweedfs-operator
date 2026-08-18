package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/metrics"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
)

const bucketClusterRefIndex = ".spec.clusterRef.name"

type ObjectStorageBucketReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clients  ClientFactory
}

func (r *ObjectStorageBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	started := time.Now()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		metrics.ReconcileTotal.WithLabelValues("ObjectStorageBucket", outcome).Inc()
		metrics.ReconcileDuration.WithLabelValues("ObjectStorageBucket").Observe(time.Since(started).Seconds())
	}()

	var bucket objectstoragev1alpha1.ObjectStorageBucket
	if err := r.Get(ctx, req.NamespacedName, &bucket); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get bucket: %w", err)
	}

	cluster, clients, ready, result, err := r.resolveStorage(ctx, &bucket)
	if err != nil || !ready {
		return result, err
	}

	if !bucket.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &bucket, clients)
	}

	if !controllerutil.ContainsFinalizer(&bucket, objectstoragev1alpha1.BucketFinalizer) {
		patch := client.MergeFrom(bucket.DeepCopy())
		controllerutil.AddFinalizer(&bucket, objectstoragev1alpha1.BucketFinalizer)
		if err := r.Patch(ctx, &bucket, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add bucket finalizer: %w", err)
		}
	}

	name := bucket.EffectiveBucketName()
	created, err := clients.S3.EnsureBucket(ctx, name, bucket.Spec.ObjectLockEnabled)
	if err != nil {
		metrics.StorageAPIErrors.WithLabelValues("s3", "create-bucket").Inc()
		r.Recorder.Eventf(&bucket, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonBucketCreateFailed,
			"Failed to create bucket %q: %v", name, err)
		r.setFailed(ctx, &bucket, objectstoragev1alpha1.ReasonBucketCreateFailed, err.Error())
		return ctrl.Result{RequeueAfter: errorRequeue}, err
	}

	if created {
		logger.Info("created S3 bucket", "bucket", name)
		r.Recorder.Eventf(&bucket, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonBucketCreated,
			"Created bucket %q on cluster %q", name, cluster.Name)
	}

	if q := bucket.Spec.Quota; q != nil {
		if err := clients.Filer.SetBucketQuota(ctx, resources.BucketsRoot, name, q.SizeGiB*1024*1024*1024, q.Enforce); err != nil {
			metrics.StorageAPIErrors.WithLabelValues("filer", "set-quota").Inc()
			r.Recorder.Eventf(&bucket, corev1.EventTypeWarning, "QuotaNotApplied",
				"Bucket %q created but quota could not be applied: %v", name, err)
		}
	}

	if bucket.Spec.ConnectionSecretName != "" {
		if err := r.ensureConnectionSecret(ctx, &bucket, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.setReady(ctx, &bucket, cluster, created); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: steadyStateRequeue}, nil
}

func (r *ObjectStorageBucketReconciler) resolveStorage(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
) (*objectstoragev1alpha1.ObjectStorageCluster, *StorageClients, bool, ctrl.Result, error) {
	cluster, err := resolveClusterForChild(ctx, r.Client, bucket.Namespace, bucket.Spec.ClusterRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if !bucket.DeletionTimestamp.IsZero() {
				return nil, nil, false, ctrl.Result{}, r.releaseFinalizer(ctx, bucket)
			}
			msg := fmt.Sprintf("ObjectStorageCluster %q not found in namespace %q",
				bucket.Spec.ClusterRef.Name, bucket.Namespace)
			r.setFailed(ctx, bucket, objectstoragev1alpha1.ReasonClusterNotFound, msg)
			return nil, nil, false, ctrl.Result{RequeueAfter: errorRequeue}, nil
		}
		return nil, nil, false, ctrl.Result{}, fmt.Errorf("get cluster for bucket: %w", err)
	}

	if !clusterS3Ready(cluster) {
		if !bucket.DeletionTimestamp.IsZero() {
			r.Recorder.Event(bucket, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonClusterNotReady,
				"Cluster S3 endpoint is not ready; releasing finalizer without deleting bucket contents")
			return nil, nil, false, ctrl.Result{}, r.releaseFinalizer(ctx, bucket)
		}
		r.setPending(ctx, bucket, objectstoragev1alpha1.ReasonClusterNotReady,
			fmt.Sprintf("Waiting for cluster %q S3 endpoint to become ready", cluster.Name))
		return nil, nil, false, ctrl.Result{RequeueAfter: progressingRequeue}, nil
	}

	creds, err := ensureAdminCredentials(ctx, r.Client, r.Scheme, cluster)
	if err != nil {
		return nil, nil, false, ctrl.Result{RequeueAfter: errorRequeue}, err
	}
	clients, err := r.Clients.For(ctx, cluster, creds)
	if err != nil {
		return nil, nil, false, ctrl.Result{RequeueAfter: errorRequeue}, err
	}
	return cluster, clients, true, ctrl.Result{}, nil
}

func (r *ObjectStorageBucketReconciler) reconcileDelete(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
	clients *StorageClients,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(bucket, objectstoragev1alpha1.BucketFinalizer) {
		return ctrl.Result{}, nil
	}

	name := bucket.EffectiveBucketName()
	if bucket.Spec.DeletionPolicy == objectstoragev1alpha1.DeletionPolicyDelete {
		logger.Info("deleting S3 bucket and its contents", "bucket", name)
		if err := clients.S3.DeleteBucket(ctx, name); err != nil {
			metrics.StorageAPIErrors.WithLabelValues("s3", "delete-bucket").Inc()
			r.Recorder.Eventf(bucket, corev1.EventTypeWarning, "BucketDeleteFailed",
				"Failed to delete bucket %q: %v", name, err)
			return ctrl.Result{RequeueAfter: errorRequeue}, err
		}
		r.Recorder.Eventf(bucket, corev1.EventTypeNormal, "BucketDeleted",
			"Deleted bucket %q and all of its objects", name)
	} else {
		r.Recorder.Eventf(bucket, corev1.EventTypeNormal, "BucketRetained",
			"Retaining bucket %q in SeaweedFS; spec.deletionPolicy is Retain", name)
	}

	return ctrl.Result{}, r.releaseFinalizer(ctx, bucket)
}

func (r *ObjectStorageBucketReconciler) releaseFinalizer(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
) error {
	if !controllerutil.ContainsFinalizer(bucket, objectstoragev1alpha1.BucketFinalizer) {
		return nil
	}
	patch := client.MergeFrom(bucket.DeepCopy())
	controllerutil.RemoveFinalizer(bucket, objectstoragev1alpha1.BucketFinalizer)
	if err := r.Patch(ctx, bucket, patch); err != nil {
		return fmt.Errorf("remove bucket finalizer: %w", err)
	}
	return nil
}

func (r *ObjectStorageBucketReconciler) ensureConnectionSecret(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) error {
	desired := resources.BucketConnectionSecret(bucket, resources.S3Endpoint(cluster))
	if err := controllerutil.SetControllerReference(bucket, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on bucket connection secret: %w", err)
	}

	var existing corev1.Secret
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create bucket connection secret: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get bucket connection secret: %w", err)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.StringData = desired.StringData
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch bucket connection secret: %w", err)
	}
	return nil
}

func (r *ObjectStorageBucketReconciler) setReady(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	created bool,
) error {
	updated := bucket.DeepCopy()
	updated.Status.ObservedGeneration = bucket.Generation
	updated.Status.Phase = objectstoragev1alpha1.BucketPhaseReady
	updated.Status.BucketName = bucket.EffectiveBucketName()
	updated.Status.Endpoint = resources.S3Endpoint(cluster)
	if bucket.Spec.ConnectionSecretName != "" {
		updated.Status.ConnectionSecretRef = bucket.Spec.ConnectionSecretName
	}
	if updated.Status.CreationTime == nil {
		now := metav1.Now()
		updated.Status.CreationTime = &now
	}
	reason := objectstoragev1alpha1.ReasonBucketExists
	message := fmt.Sprintf("Bucket %q is present on cluster %q", updated.Status.BucketName, cluster.Name)
	if created {
		reason = objectstoragev1alpha1.ReasonBucketCreated
		message = fmt.Sprintf("Created bucket %q on cluster %q", updated.Status.BucketName, cluster.Name)
	}
	meta.SetStatusCondition(&updated.Status.Conditions,
		conditionTrue(objectstoragev1alpha1.ConditionReady, reason, message, bucket.Generation))

	metrics.BucketsManaged.WithLabelValues(bucket.Namespace, cluster.Name, string(objectstoragev1alpha1.BucketPhaseReady)).
		Set(1)
	return r.writeStatus(ctx, bucket, updated)
}

func (r *ObjectStorageBucketReconciler) setPending(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
	reason, message string,
) {
	updated := bucket.DeepCopy()
	updated.Status.ObservedGeneration = bucket.Generation
	updated.Status.Phase = objectstoragev1alpha1.BucketPhasePending
	meta.SetStatusCondition(&updated.Status.Conditions,
		conditionFalse(objectstoragev1alpha1.ConditionReady, reason, message, bucket.Generation))
	if err := r.writeStatus(ctx, bucket, updated); err != nil {
		log.FromContext(ctx).Error(err, "failed to write pending bucket status")
	}
}

func (r *ObjectStorageBucketReconciler) setFailed(
	ctx context.Context,
	bucket *objectstoragev1alpha1.ObjectStorageBucket,
	reason, message string,
) {
	updated := bucket.DeepCopy()
	updated.Status.ObservedGeneration = bucket.Generation
	updated.Status.Phase = objectstoragev1alpha1.BucketPhaseFailed
	meta.SetStatusCondition(&updated.Status.Conditions,
		conditionFalse(objectstoragev1alpha1.ConditionReady, reason, message, bucket.Generation))
	if err := r.writeStatus(ctx, bucket, updated); err != nil {
		log.FromContext(ctx).Error(err, "failed to write failed bucket status")
	}
}

func (r *ObjectStorageBucketReconciler) writeStatus(
	ctx context.Context,
	current, updated *objectstoragev1alpha1.ObjectStorageBucket,
) error {
	a, b := current.Status.DeepCopy(), updated.Status.DeepCopy()
	normalizeConditions(a.Conditions)
	normalizeConditions(b.Conditions)
	if equalJSON(a, b) {
		return nil
	}
	if err := r.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("update bucket status: %w", err)
	}
	current.Status = updated.Status
	return nil
}

func (r *ObjectStorageBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clients == nil {
		r.Clients = DefaultClientFactory{}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&objectstoragev1alpha1.ObjectStorageBucket{}, bucketClusterRefIndex,
		func(obj client.Object) []string {
			b, ok := obj.(*objectstoragev1alpha1.ObjectStorageBucket)
			if !ok {
				return nil
			}
			return []string{b.Spec.ClusterRef.Name}
		}); err != nil {
		return fmt.Errorf("index buckets by cluster ref: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&objectstoragev1alpha1.ObjectStorageBucket{}).
		Owns(&corev1.Secret{}).
		Watches(
			&objectstoragev1alpha1.ObjectStorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.bucketsForCluster),
			builder.WithPredicates(clusterReadinessChanged{}),
		).
		Named("objectstoragebucket").
		Complete(r)
}

func (r *ObjectStorageBucketReconciler) bucketsForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*objectstoragev1alpha1.ObjectStorageCluster)
	if !ok {
		return nil
	}
	var list objectstoragev1alpha1.ObjectStorageBucketList
	if err := r.List(ctx, &list,
		client.InNamespace(cluster.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(bucketClusterRefIndex, cluster.Name)},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list buckets for cluster", "cluster", cluster.Name)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name},
		})
	}
	return reqs
}
