package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/metrics"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
)

const (
	steadyStateRequeue = 2 * time.Minute
	progressingRequeue = 10 * time.Second
	errorRequeue       = 30 * time.Second
)

type ObjectStorageClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clients  ClientFactory
}

func (r *ObjectStorageClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	started := time.Now()
	defer func() {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		metrics.ReconcileTotal.WithLabelValues("ObjectStorageCluster", outcome).Inc()
		metrics.ReconcileDuration.WithLabelValues("ObjectStorageCluster").Observe(time.Since(started).Seconds())
	}()

	var cluster objectstoragev1alpha1.ObjectStorageCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.ForgetCluster(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get cluster: %w", err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cluster)
	}

	if !controllerutil.ContainsFinalizer(&cluster, objectstoragev1alpha1.ClusterFinalizer) {
		patch := client.MergeFrom(cluster.DeepCopy())
		controllerutil.AddFinalizer(&cluster, objectstoragev1alpha1.ClusterFinalizer)
		if err := r.Patch(ctx, &cluster, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	if validationErr := validateClusterSpec(&cluster); validationErr != nil {
		r.Recorder.Event(&cluster, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonInvalidSpec, validationErr.Error())
		state := &clusterState{invalidSpec: validationErr}
		if err := r.updateStatus(ctx, &cluster, state); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	state := &clusterState{}

	creds, err := ensureAdminCredentials(ctx, r.Client, r.Scheme, &cluster)
	if err != nil {
		return r.failReconcile(ctx, &cluster, state, err)
	}
	state.adminSecretName = resources.AdminSecretName(&cluster)

	if err := r.reconcileComponents(ctx, &cluster, state); err != nil {
		return r.failReconcile(ctx, &cluster, state, err)
	}

	if err := r.collectComponentStatus(ctx, &cluster, state); err != nil {
		return r.failReconcile(ctx, &cluster, state, err)
	}

	if state.filerReady() && cluster.Spec.S3.Enabled && state.s3Ready() {
		r.reconcileStorageControlPlane(ctx, &cluster, state, creds)
	}

	if err := r.updateStatus(ctx, &cluster, state); err != nil {
		return ctrl.Result{}, err
	}

	if state.progressing() {
		return ctrl.Result{RequeueAfter: progressingRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: steadyStateRequeue}, nil
}

func (r *ObjectStorageClusterReconciler) failReconcile(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	state *clusterState,
	cause error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(cause, "cluster reconcile failed")
	r.Recorder.Event(cluster, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonReconcileError, cause.Error())
	state.reconcileErr = cause
	if statusErr := r.updateStatus(ctx, cluster, state); statusErr != nil {
		logger.Error(statusErr, "failed to record reconcile error on status")
	}
	return ctrl.Result{RequeueAfter: errorRequeue}, cause
}

func (r *ObjectStorageClusterReconciler) reconcileStorageControlPlane(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	state *clusterState,
	creds AdminCredentials,
) {
	logger := log.FromContext(ctx)

	clients, err := r.Clients.For(ctx, cluster, creds)
	if err != nil {
		state.s3Err = err
		metrics.StorageAPIErrors.WithLabelValues("s3", "client").Inc()
		return
	}

	changed, err := seedAdminIdentity(ctx, clients)
	if err != nil {
		state.s3Err = fmt.Errorf("seed admin identity: %w", err)
		metrics.StorageAPIErrors.WithLabelValues("filer", "seed-identity").Inc()
		return
	}
	if changed {
		logger.Info("seeded S3 admin identity into SeaweedFS IAM configuration")
		r.Recorder.Event(cluster, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonCredentialsIssued,
			"Wrote operator S3 admin identity to the filer IAM configuration")
	}

	if _, err := clients.S3.ListBuckets(ctx); err != nil {
		state.s3Err = fmt.Errorf("authenticate against S3 endpoint: %w", err)
		metrics.StorageAPIErrors.WithLabelValues("s3", "list-buckets").Inc()
		return
	}
	state.s3Authenticated = true

	if topology, err := clients.Master.Topology(ctx); err != nil {
		logger.V(1).Info("topology probe failed", "error", err.Error())
		metrics.StorageAPIErrors.WithLabelValues("master", "topology").Inc()
	} else {
		state.topology = topology
		metrics.ClusterFreeVolumeSlots.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(topology.FreeVolumes))
		if topology.FreeVolumes == 0 && topology.MaxVolumes > 0 {
			r.Recorder.Event(cluster, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonNoFreeVolumeSlots,
				"All volume slots are allocated; add volume server replicas or expand spec.volume.storage.size before writes start failing")
		}
	}
}

func (r *ObjectStorageClusterReconciler) reconcileDelete(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(cluster, objectstoragev1alpha1.ClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := r.countDependents(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if dependents > 0 {
		msg := fmt.Sprintf("Waiting for %d ObjectStorageBucket/ObjectStorageUser objects referencing this cluster to be deleted first", dependents)
		logger.Info("deletion blocked by dependents", "count", dependents)
		r.Recorder.Event(cluster, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonDeleting, msg)
		state := &clusterState{deleting: true, deleteBlockedReason: msg}
		if err := r.updateStatus(ctx, cluster, state); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: progressingRequeue}, nil
	}

	if cluster.Spec.S3.Enabled {
		var deploy appsv1.Deployment
		key := client.ObjectKey{Namespace: cluster.Namespace, Name: resources.Name(cluster, resources.ComponentS3)}
		if err := r.Get(ctx, key, &deploy); err == nil && deploy.Spec.Replicas != nil && *deploy.Spec.Replicas > 0 {
			patch := client.MergeFrom(deploy.DeepCopy())
			zero := int32(0)
			deploy.Spec.Replicas = &zero
			if err := r.Patch(ctx, &deploy, patch); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("quiesce S3 gateway: %w", err)
			}
			logger.Info("scaled S3 gateway to zero ahead of cluster deletion")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	metrics.ForgetCluster(cluster.Namespace, cluster.Name)
	r.Recorder.Event(cluster, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonDeleting,
		"Cluster teardown complete; owned resources will be garbage collected. PersistentVolumeClaims are retained by design.")

	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.RemoveFinalizer(cluster, objectstoragev1alpha1.ClusterFinalizer)
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *ObjectStorageClusterReconciler) countDependents(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) (int, error) {
	var buckets objectstoragev1alpha1.ObjectStorageBucketList
	if err := r.List(ctx, &buckets, client.InNamespace(cluster.Namespace)); err != nil {
		return 0, fmt.Errorf("list buckets: %w", err)
	}
	var users objectstoragev1alpha1.ObjectStorageUserList
	if err := r.List(ctx, &users, client.InNamespace(cluster.Namespace)); err != nil {
		return 0, fmt.Errorf("list users: %w", err)
	}

	count := 0
	for i := range buckets.Items {
		if buckets.Items[i].Spec.ClusterRef.Name == cluster.Name && buckets.Items[i].DeletionTimestamp.IsZero() {
			count++
		}
	}
	for i := range users.Items {
		if users.Items[i].Spec.ClusterRef.Name == cluster.Name && users.Items[i].DeletionTimestamp.IsZero() {
			count++
		}
	}
	return count, nil
}

func (r *ObjectStorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clients == nil {
		r.Clients = DefaultClientFactory{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&objectstoragev1alpha1.ObjectStorageCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("objectstoragecluster").
		Complete(r)
}

func validateClusterSpec(cluster *objectstoragev1alpha1.ObjectStorageCluster) error {
	if cluster.Spec.Master.Replicas%2 == 0 {
		return fmt.Errorf(
			"spec.master.replicas is %d: SeaweedFS masters form a Raft quorum and an even replica count cannot elect a leader; use an odd number",
			cluster.Spec.Master.Replicas)
	}
	if cluster.Spec.Volume.Storage.Size.IsZero() {
		return fmt.Errorf("spec.volume.storage.size must be greater than zero")
	}
	if cluster.Spec.S3.Enabled && cluster.Spec.Filer.Replicas < 1 {
		return fmt.Errorf("spec.s3.enabled requires at least one filer replica: the S3 gateway is a client of the filer")
	}
	return nil
}

func conditionTrue(condType, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	}
}

func conditionFalse(condType, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	}
}
