package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
)

const userClusterRefIndex = ".spec.clusterRef.name"

type ObjectStorageUserReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clients  ClientFactory
}

func (r *ObjectStorageUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	started := time.Now()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		metrics.ReconcileTotal.WithLabelValues("ObjectStorageUser", outcome).Inc()
		metrics.ReconcileDuration.WithLabelValues("ObjectStorageUser").Observe(time.Since(started).Seconds())
	}()

	var user objectstoragev1alpha1.ObjectStorageUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get user: %w", err)
	}

	cluster, clients, ready, res, err := r.resolveStorage(ctx, &user)
	if err != nil || !ready {
		return res, err
	}

	if !user.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &user, clients)
	}

	if !controllerutil.ContainsFinalizer(&user, objectstoragev1alpha1.UserFinalizer) {
		patch := client.MergeFrom(user.DeepCopy())
		controllerutil.AddFinalizer(&user, objectstoragev1alpha1.UserFinalizer)
		if err := r.Patch(ctx, &user, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add user finalizer: %w", err)
		}
	}

	creds, err := r.ensureUserSecret(ctx, &user, cluster)
	if err != nil {
		r.setFailed(ctx, &user, objectstoragev1alpha1.ReasonIdentityFailed, err.Error())
		return ctrl.Result{RequeueAfter: errorRequeue}, err
	}

	actions, grantedBuckets, err := r.buildActions(ctx, &user)
	if err != nil {
		r.setFailed(ctx, &user, objectstoragev1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{RequeueAfter: errorRequeue}, nil
	}

	identityName := user.EffectiveIdentityName()
	changed, err := clients.IAM.Mutate(ctx, func(cfg *seaweed.IAMConfig) (bool, error) {
		return cfg.Upsert(seaweed.Identity{
			Name:        identityName,
			Credentials: []seaweed.Credential{{AccessKey: creds.AccessKeyID, SecretKey: creds.SecretAccessKey}},
			Actions:     actions,
		}), nil
	})
	if err != nil {
		metrics.StorageAPIErrors.WithLabelValues("filer", "upsert-identity").Inc()
		r.Recorder.Eventf(&user, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonIdentityFailed,
			"Failed to configure S3 identity %q: %v", identityName, err)
		r.setFailed(ctx, &user, objectstoragev1alpha1.ReasonIdentityFailed, err.Error())
		return ctrl.Result{RequeueAfter: errorRequeue}, err
	}
	if changed {
		log.FromContext(ctx).Info("configured S3 identity", "identity", identityName, "actions", actions)
		r.Recorder.Eventf(&user, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonIdentityConfigured,
			"Configured S3 identity %q with %d permissions", identityName, len(actions))
	}

	if err := r.setReady(ctx, &user, cluster, creds.AccessKeyID, grantedBuckets); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: steadyStateRequeue}, nil
}

func (r *ObjectStorageUserReconciler) resolveStorage(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
) (*objectstoragev1alpha1.ObjectStorageCluster, *StorageClients, bool, ctrl.Result, error) {
	cluster, err := resolveClusterForChild(ctx, r.Client, user.Namespace, user.Spec.ClusterRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if !user.DeletionTimestamp.IsZero() {
				return nil, nil, false, ctrl.Result{}, r.releaseFinalizer(ctx, user)
			}
			msg := fmt.Sprintf("ObjectStorageCluster %q not found in namespace %q",
				user.Spec.ClusterRef.Name, user.Namespace)
			r.setFailed(ctx, user, objectstoragev1alpha1.ReasonClusterNotFound, msg)
			return nil, nil, false, ctrl.Result{RequeueAfter: errorRequeue}, nil
		}
		return nil, nil, false, ctrl.Result{}, fmt.Errorf("get cluster for user: %w", err)
	}

	if !clusterS3Ready(cluster) {
		if !user.DeletionTimestamp.IsZero() {
			r.Recorder.Event(user, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonClusterNotReady,
				"Cluster S3 endpoint is not ready; releasing finalizer without revoking the identity")
			return nil, nil, false, ctrl.Result{}, r.releaseFinalizer(ctx, user)
		}
		r.setPending(ctx, user, objectstoragev1alpha1.ReasonClusterNotReady,
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

func (r *ObjectStorageUserReconciler) buildActions(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
) ([]string, []string, error) {
	actions := make([]string, 0, 8)
	bucketSet := map[string]struct{}{}

	for _, a := range user.Spec.ClusterActions {
		actions = append(actions, string(a))
	}

	for i, grant := range user.Spec.BucketGrants {
		bucketName := grant.BucketName
		if grant.BucketRef != nil {
			var bucket objectstoragev1alpha1.ObjectStorageBucket
			key := types.NamespacedName{Namespace: user.Namespace, Name: grant.BucketRef.Name}
			if err := r.Get(ctx, key, &bucket); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, nil, fmt.Errorf(
						"spec.bucketGrants[%d].bucketRef references ObjectStorageBucket %q, which does not exist in namespace %q",
						i, grant.BucketRef.Name, user.Namespace)
				}
				return nil, nil, fmt.Errorf("resolve bucketRef %q: %w", grant.BucketRef.Name, err)
			}
			if bucket.Spec.ClusterRef.Name != user.Spec.ClusterRef.Name {
				return nil, nil, fmt.Errorf(
					"spec.bucketGrants[%d] references bucket %q on cluster %q, but this user belongs to cluster %q",
					i, grant.BucketRef.Name, bucket.Spec.ClusterRef.Name, user.Spec.ClusterRef.Name)
			}
			bucketName = bucket.EffectiveBucketName()
		}
		if bucketName == "" {
			return nil, nil, fmt.Errorf("spec.bucketGrants[%d] has neither bucketName nor bucketRef", i)
		}
		for _, action := range grant.Actions {
			actions = append(actions, seaweed.BuildAction(string(action), bucketName, grant.Prefix))
		}
		if bucketName != "*" {
			bucketSet[bucketName] = struct{}{}
		}
	}

	buckets := make([]string, 0, len(bucketSet))
	for b := range bucketSet {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)
	return actions, buckets, nil
}

func (r *ObjectStorageUserReconciler) ensureUserSecret(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) (AdminCredentials, error) {
	name := user.EffectiveSecretName()
	key := types.NamespacedName{Namespace: user.Namespace, Name: name}

	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if err == nil {
		access := string(existing.Data[adminAccessKeyField])
		secret := string(existing.Data[adminSecretKeyField])
		if access != "" && secret != "" {
			desired := resources.UserSecret(user, resources.S3Endpoint(cluster), access, secret)
			if !equalJSON(existing.Labels, desired.Labels) || string(existing.Data["endpoint"]) != resources.S3Endpoint(cluster) {
				patch := client.MergeFrom(existing.DeepCopy())
				existing.Labels = desired.Labels
				existing.StringData = desired.StringData
				if err := r.Patch(ctx, &existing, patch); err != nil {
					return AdminCredentials{}, fmt.Errorf("refresh user secret: %w", err)
				}
			}
			return AdminCredentials{AccessKeyID: access, SecretAccessKey: secret}, nil
		}
		return AdminCredentials{}, fmt.Errorf(
			"secret %s/%s exists but is missing %q or %q; delete it to have the operator issue fresh credentials",
			user.Namespace, name, adminAccessKeyField, adminSecretKeyField)
	}
	if !apierrors.IsNotFound(err) {
		return AdminCredentials{}, fmt.Errorf("get user secret: %w", err)
	}

	accessKey, err := seaweed.GenerateAccessKeyID()
	if err != nil {
		return AdminCredentials{}, err
	}
	secretKey, err := seaweed.GenerateSecretAccessKey()
	if err != nil {
		return AdminCredentials{}, err
	}

	desired := resources.UserSecret(user, resources.S3Endpoint(cluster), accessKey, secretKey)
	if err := controllerutil.SetControllerReference(user, desired, r.Scheme); err != nil {
		return AdminCredentials{}, fmt.Errorf("set owner on user secret: %w", err)
	}
	if err := r.Create(ctx, desired); err != nil {
		return AdminCredentials{}, fmt.Errorf("create user secret: %w", err)
	}
	r.Recorder.Eventf(user, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonCredentialsIssued,
		"Issued S3 credentials into Secret %q", name)
	return AdminCredentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}, nil
}

func (r *ObjectStorageUserReconciler) reconcileDelete(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
	clients *StorageClients,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(user, objectstoragev1alpha1.UserFinalizer) {
		return ctrl.Result{}, nil
	}

	identityName := user.EffectiveIdentityName()
	if user.Spec.DeletionPolicy != objectstoragev1alpha1.DeletionPolicyRetain {
		removed, err := clients.IAM.Mutate(ctx, func(cfg *seaweed.IAMConfig) (bool, error) {
			return cfg.Remove(identityName), nil
		})
		if err != nil {
			metrics.StorageAPIErrors.WithLabelValues("filer", "remove-identity").Inc()
			r.Recorder.Eventf(user, corev1.EventTypeWarning, "IdentityRevokeFailed",
				"Failed to revoke S3 identity %q: %v", identityName, err)
			return ctrl.Result{RequeueAfter: errorRequeue}, err
		}
		if removed {
			r.Recorder.Eventf(user, corev1.EventTypeNormal, "IdentityRevoked",
				"Revoked S3 identity %q; its credentials no longer authenticate", identityName)
		}
	} else {
		r.Recorder.Eventf(user, corev1.EventTypeWarning, "IdentityRetained",
			"Retaining S3 identity %q per spec.deletionPolicy: its credentials remain valid after this object is gone", identityName)
	}

	return ctrl.Result{}, r.releaseFinalizer(ctx, user)
}

func (r *ObjectStorageUserReconciler) releaseFinalizer(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
) error {
	if !controllerutil.ContainsFinalizer(user, objectstoragev1alpha1.UserFinalizer) {
		return nil
	}
	patch := client.MergeFrom(user.DeepCopy())
	controllerutil.RemoveFinalizer(user, objectstoragev1alpha1.UserFinalizer)
	if err := r.Patch(ctx, user, patch); err != nil {
		return fmt.Errorf("remove user finalizer: %w", err)
	}
	return nil
}

func (r *ObjectStorageUserReconciler) setReady(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	accessKeyID string,
	buckets []string,
) error {
	updated := user.DeepCopy()
	updated.Status.ObservedGeneration = user.Generation
	updated.Status.Phase = objectstoragev1alpha1.UserPhaseReady
	updated.Status.IdentityName = user.EffectiveIdentityName()
	updated.Status.SecretRef = user.EffectiveSecretName()
	updated.Status.AccessKeyID = accessKeyID
	updated.Status.Endpoint = resources.S3Endpoint(cluster)
	updated.Status.GrantedBuckets = buckets
	meta.SetStatusCondition(&updated.Status.Conditions, conditionTrue(
		objectstoragev1alpha1.ConditionReady, objectstoragev1alpha1.ReasonIdentityConfigured,
		fmt.Sprintf("Identity %q is configured on cluster %q", updated.Status.IdentityName, cluster.Name),
		user.Generation))
	metrics.UsersManaged.WithLabelValues(user.Namespace, cluster.Name, string(objectstoragev1alpha1.UserPhaseReady)).Set(1)
	return r.writeStatus(ctx, user, updated)
}

func (r *ObjectStorageUserReconciler) setPending(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
	reason, message string,
) {
	updated := user.DeepCopy()
	updated.Status.ObservedGeneration = user.Generation
	updated.Status.Phase = objectstoragev1alpha1.UserPhasePending
	meta.SetStatusCondition(&updated.Status.Conditions,
		conditionFalse(objectstoragev1alpha1.ConditionReady, reason, message, user.Generation))
	if err := r.writeStatus(ctx, user, updated); err != nil {
		log.FromContext(ctx).Error(err, "failed to write pending user status")
	}
}

func (r *ObjectStorageUserReconciler) setFailed(
	ctx context.Context,
	user *objectstoragev1alpha1.ObjectStorageUser,
	reason, message string,
) {
	updated := user.DeepCopy()
	updated.Status.ObservedGeneration = user.Generation
	updated.Status.Phase = objectstoragev1alpha1.UserPhaseFailed
	meta.SetStatusCondition(&updated.Status.Conditions,
		conditionFalse(objectstoragev1alpha1.ConditionReady, reason, message, user.Generation))
	if err := r.writeStatus(ctx, user, updated); err != nil {
		log.FromContext(ctx).Error(err, "failed to write failed user status")
	}
}

func (r *ObjectStorageUserReconciler) writeStatus(
	ctx context.Context,
	current, updated *objectstoragev1alpha1.ObjectStorageUser,
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
		return fmt.Errorf("update user status: %w", err)
	}
	current.Status = updated.Status
	return nil
}

func (r *ObjectStorageUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clients == nil {
		r.Clients = DefaultClientFactory{}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&objectstoragev1alpha1.ObjectStorageUser{}, userClusterRefIndex,
		func(obj client.Object) []string {
			u, ok := obj.(*objectstoragev1alpha1.ObjectStorageUser)
			if !ok {
				return nil
			}
			return []string{u.Spec.ClusterRef.Name}
		}); err != nil {
		return fmt.Errorf("index users by cluster ref: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&objectstoragev1alpha1.ObjectStorageUser{}).
		Owns(&corev1.Secret{}).
		Watches(
			&objectstoragev1alpha1.ObjectStorageCluster{},
			handler.EnqueueRequestsFromMapFunc(r.usersForCluster),
			builder.WithPredicates(clusterReadinessChanged{}),
		).
		Named("objectstorageuser").
		Complete(r)
}

func (r *ObjectStorageUserReconciler) usersForCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*objectstoragev1alpha1.ObjectStorageCluster)
	if !ok {
		return nil
	}
	var list objectstoragev1alpha1.ObjectStorageUserList
	if err := r.List(ctx, &list,
		client.InNamespace(cluster.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(userClusterRefIndex, cluster.Name)},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list users for cluster", "cluster", cluster.Name)
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
