package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/metrics"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
)

type clusterState struct {
	master resourceStatus
	volume resourceStatus
	filer  resourceStatus
	s3     resourceStatus

	adminSecretName  string
	topology         *seaweed.Topology
	s3Authenticated  bool
	s3Err            error
	reconcileErr     error
	invalidSpec      error
	expandingStorage bool
	storageWarnings  []string
	upgradeHeldBack  []string
	deleting         bool

	deleteBlockedReason string

	provisionedCapacity resource.Quantity
	rolloutInProgress   bool
	currentVersion      string
}

type resourceStatus struct {
	found   bool
	desired int32
	ready   int32
	current int32
	updated int32
	image   string
	generation         int64
	observedGeneration int64
}

func (s resourceStatus) toAPI() objectstoragev1alpha1.ComponentStatus {
	return objectstoragev1alpha1.ComponentStatus{
		DesiredReplicas: s.desired,
		ReadyReplicas:   s.ready,
		CurrentReplicas: s.current,
		UpdatedReplicas: s.updated,
		Image:           s.image,
		Ready:           s.isReady(),
	}
}

func (s resourceStatus) isReady() bool {
	return s.found && s.desired > 0 && s.observedGeneration >= s.generation &&
		s.ready == s.desired && s.updated == s.desired
}

func (s resourceStatus) stale() bool {
	return s.found && s.observedGeneration < s.generation
}

func (st *clusterState) filerReady() bool { return st.filer.isReady() }
func (st *clusterState) s3Ready() bool    { return st.s3.isReady() }

func (st *clusterState) available() bool {
	return st.master.ready > 0 && st.volume.ready > 0 && st.filer.ready > 0
}

func (st *clusterState) progressing() bool {
	return st.rolloutInProgress || st.expandingStorage || len(st.upgradeHeldBack) > 0 ||
		!st.master.isReady() || !st.volume.isReady() || !st.filer.isReady()
}

func (r *ObjectStorageClusterReconciler) collectComponentStatus(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	state *clusterState,
) error {
	var err error
	if state.master, err = r.statefulSetStatus(ctx, cluster, resources.ComponentMaster); err != nil {
		return err
	}
	if state.volume, err = r.statefulSetStatus(ctx, cluster, resources.ComponentVolume); err != nil {
		return err
	}
	if state.filer, err = r.statefulSetStatus(ctx, cluster, resources.ComponentFiler); err != nil {
		return err
	}
	if cluster.Spec.S3.Enabled {
		if state.s3, err = r.deploymentStatus(ctx, cluster, resources.ComponentS3); err != nil {
			return err
		}
	}

	perReplica := cluster.Spec.Volume.Storage.Size.DeepCopy()
	total := resource.NewQuantity(perReplica.Value()*int64(cluster.Spec.Volume.Replicas), perReplica.Format)
	state.provisionedCapacity = *total

	state.rolloutInProgress = state.master.stale() || state.volume.stale() ||
		state.filer.stale() || (cluster.Spec.S3.Enabled && state.s3.stale()) ||
		state.master.updated != state.master.current ||
		state.volume.updated != state.volume.current ||
		state.filer.updated != state.filer.current ||
		(cluster.Spec.S3.Enabled && state.s3.updated != state.s3.current)

	desiredImage := resources.Image(cluster)
	allAtDesired := state.master.image == desiredImage &&
		state.volume.image == desiredImage &&
		state.filer.image == desiredImage &&
		(!cluster.Spec.S3.Enabled || state.s3.image == desiredImage)
	if allAtDesired && !state.rolloutInProgress {
		if cluster.Status.CurrentVersion != "" && cluster.Status.CurrentVersion != cluster.Spec.Version {
			metrics.UpgradesTotal.WithLabelValues(cluster.Namespace, cluster.Name).Inc()
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "UpgradeCompleted",
				"All components now running SeaweedFS %s", cluster.Spec.Version)
		}
		state.currentVersion = cluster.Spec.Version
	} else {
		state.currentVersion = cluster.Status.CurrentVersion
	}

	return nil
}

func (r *ObjectStorageClusterReconciler) statefulSetStatus(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
) (resourceStatus, error) {
	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: resources.Name(cluster, component)}
	if err := r.Get(ctx, key, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return resourceStatus{}, nil
		}
		return resourceStatus{}, fmt.Errorf("get %s statefulset status: %w", component, err)
	}
	desired := int32(0)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	st := resourceStatus{
		found:              true,
		desired:            desired,
		ready:              sts.Status.ReadyReplicas,
		current:            sts.Status.Replicas,
		updated:            sts.Status.UpdatedReplicas,
		image:              containerImage(sts.Spec.Template.Spec.Containers),
		generation:         sts.Generation,
		observedGeneration: sts.Status.ObservedGeneration,
	}
	r.publishComponentMetrics(cluster, component, st)
	return st, nil
}

func (r *ObjectStorageClusterReconciler) deploymentStatus(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
) (resourceStatus, error) {
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: resources.Name(cluster, component)}
	if err := r.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return resourceStatus{}, nil
		}
		return resourceStatus{}, fmt.Errorf("get %s deployment status: %w", component, err)
	}
	desired := int32(0)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	st := resourceStatus{
		found:              true,
		desired:            desired,
		ready:              deploy.Status.ReadyReplicas,
		current:            deploy.Status.Replicas,
		updated:            deploy.Status.UpdatedReplicas,
		image:              containerImage(deploy.Spec.Template.Spec.Containers),
		generation:         deploy.Generation,
		observedGeneration: deploy.Status.ObservedGeneration,
	}
	r.publishComponentMetrics(cluster, component, st)
	return st, nil
}

func (r *ObjectStorageClusterReconciler) publishComponentMetrics(
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
	st resourceStatus,
) {
	labels := []string{cluster.Namespace, cluster.Name, string(component)}
	metrics.ComponentReplicas.WithLabelValues(append(labels, "desired")...).Set(float64(st.desired))
	metrics.ComponentReplicas.WithLabelValues(append(labels, "ready")...).Set(float64(st.ready))
	metrics.ComponentReplicas.WithLabelValues(append(labels, "updated")...).Set(float64(st.updated))
}

func (r *ObjectStorageClusterReconciler) updateStatus(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	state *clusterState,
) error {
	updated := cluster.DeepCopy()
	status := &updated.Status

	status.ObservedGeneration = cluster.Generation
	status.Master = state.master.toAPI()
	status.Volume = state.volume.toAPI()
	status.Filer = state.filer.toAPI()
	status.S3 = state.s3.toAPI()
	status.MasterEndpoint = fmt.Sprintf("http:
	status.FilerEndpoint = "http:
	if cluster.Spec.S3.Enabled {
		status.S3Endpoint = resources.S3Endpoint(cluster)
	} else {
		status.S3Endpoint = ""
	}
	if state.adminSecretName != "" {
		status.AdminSecretName = state.adminSecretName
	}
	status.CurrentVersion = state.currentVersion
	if !state.provisionedCapacity.IsZero() {
		status.ProvisionedCapacity = state.provisionedCapacity.String()
		metrics.ClusterCapacityBytes.WithLabelValues(cluster.Namespace, cluster.Name).
			Set(float64(state.provisionedCapacity.Value()))
	}
	if state.topology != nil {
		now := metav1.Now()
		status.Topology = &objectstoragev1alpha1.TopologyStatus{
			DataCenters:   state.topology.DataCenters,
			Racks:         state.topology.Racks,
			VolumeServers: state.topology.VolumeServers,
			ActiveVolumes: state.topology.ActiveVolumes,
			MaxVolumes:    state.topology.MaxVolumes,
			FreeVolumes:   state.topology.FreeVolumes,
			LastProbeTime: &now,
		}
	}

	applyClusterConditions(updated, state)
	status.Phase = derivePhase(updated, state)
	metrics.SetClusterPhase(cluster.Namespace, cluster.Name, string(status.Phase))

	if equalStatus(&cluster.Status, status) {
		return nil
	}
	if err := r.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("update cluster status: %w", err)
	}
	cluster.Status = *status
	return nil
}

func applyClusterConditions(cluster *objectstoragev1alpha1.ObjectStorageCluster, state *clusterState) {
	gen := cluster.Generation

	if state.invalidSpec != nil {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionAvailable, objectstoragev1alpha1.ReasonInvalidSpec,
			state.invalidSpec.Error(), gen))
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonInvalidSpec,
			"Reconciliation is halted until the spec is corrected", gen))
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionDegraded, objectstoragev1alpha1.ReasonInvalidSpec,
			state.invalidSpec.Error(), gen))
		return
	}

	if state.deleting {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionAvailable, objectstoragev1alpha1.ReasonDeleting,
			"Cluster is being deleted", gen))
		reason := state.deleteBlockedReason
		if reason == "" {
			reason = "Tearing down cluster resources"
		}
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonDeleting, reason, gen))
		return
	}

	if state.available() {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionAvailable, objectstoragev1alpha1.ReasonAllComponentsReady,
			"Master, volume and filer tiers each have at least one ready replica", gen))
	} else {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionAvailable, waitingReason(state),
			waitingMessage(state), gen))
	}

	switch {
	case len(state.upgradeHeldBack) > 0 && cluster.Spec.Upgrade.Paused:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonUpgradePaused,
			fmt.Sprintf("Upgrade paused; %s still on the previous version", strings.Join(state.upgradeHeldBack, ", ")), gen))
	case len(state.upgradeHeldBack) > 0:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonUpgradeInProgress,
			fmt.Sprintf("Ordered upgrade in progress; waiting to roll %s", strings.Join(state.upgradeHeldBack, ", ")), gen))
	case state.expandingStorage:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonCapacityExpanding,
			"Expanding volume server storage", gen))
	case state.rolloutInProgress:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonRolloutInProgress,
			"Pods are being replaced to match the current spec", gen))
	case state.progressing():
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonScalingInProgress,
			waitingMessage(state), gen))
	default:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionProgressing, objectstoragev1alpha1.ReasonAllComponentsReady,
			"Cluster matches the desired state", gen))
	}

	degradedMessages := append([]string{}, state.storageWarnings...)
	if state.reconcileErr != nil {
		degradedMessages = append(degradedMessages, state.reconcileErr.Error())
	}
	if state.available() && (!state.master.isReady() || !state.volume.isReady() || !state.filer.isReady()) &&
		!state.rolloutInProgress {
		degradedMessages = append(degradedMessages,
			fmt.Sprintf("running below requested replicas (master %d/%d, volume %d/%d, filer %d/%d)",
				state.master.ready, state.master.desired,
				state.volume.ready, state.volume.desired,
				state.filer.ready, state.filer.desired))
	}
	if len(degradedMessages) > 0 {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionDegraded, objectstoragev1alpha1.ReasonComponentsNotReady,
			strings.Join(degradedMessages, "; "), gen))
	} else {
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionDegraded, objectstoragev1alpha1.ReasonAllComponentsReady,
			"No degradation detected", gen))
	}

	switch {
	case !cluster.Spec.S3.Enabled:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionS3Ready, "S3Disabled",
			"spec.s3.enabled is false", gen))
	case state.s3Authenticated:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionTrue(
			objectstoragev1alpha1.ConditionS3Ready, objectstoragev1alpha1.ReasonS3Authenticated,
			"S3 endpoint is reachable and the operator admin credentials are accepted", gen))
	case state.s3Err != nil:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionS3Ready, objectstoragev1alpha1.ReasonS3Unreachable,
			state.s3Err.Error(), gen))
	default:
		meta.SetStatusCondition(&cluster.Status.Conditions, conditionFalse(
			objectstoragev1alpha1.ConditionS3Ready, objectstoragev1alpha1.ReasonWaitingForS3,
			"Waiting for the S3 gateway and filer to become ready", gen))
	}
}

func waitingReason(state *clusterState) string {
	switch {
	case state.master.ready == 0:
		return objectstoragev1alpha1.ReasonWaitingForMasters
	case state.volume.ready == 0:
		return objectstoragev1alpha1.ReasonWaitingForVolumes
	case state.filer.ready == 0:
		return objectstoragev1alpha1.ReasonWaitingForFiler
	default:
		return objectstoragev1alpha1.ReasonComponentsNotReady
	}
}

func waitingMessage(state *clusterState) string {
	parts := []string{}
	if !state.master.isReady() {
		parts = append(parts, fmt.Sprintf("master %d/%d ready", state.master.ready, state.master.desired))
	}
	if !state.volume.isReady() {
		parts = append(parts, fmt.Sprintf("volume %d/%d ready", state.volume.ready, state.volume.desired))
	}
	if !state.filer.isReady() {
		parts = append(parts, fmt.Sprintf("filer %d/%d ready", state.filer.ready, state.filer.desired))
	}
	if !state.s3.isReady() && state.s3.found {
		parts = append(parts, fmt.Sprintf("s3 %d/%d ready", state.s3.ready, state.s3.desired))
	}
	if len(parts) == 0 {
		return "All components ready"
	}
	return strings.Join(parts, ", ")
}

func derivePhase(cluster *objectstoragev1alpha1.ObjectStorageCluster, state *clusterState) objectstoragev1alpha1.ClusterPhase {
	switch {
	case state.deleting:
		return objectstoragev1alpha1.ClusterPhaseDeleting
	case state.invalidSpec != nil:
		return objectstoragev1alpha1.ClusterPhaseDegraded
	case !state.master.found && !state.volume.found:
		return objectstoragev1alpha1.ClusterPhasePending
	case !state.available() && !state.rolloutInProgress:
		return objectstoragev1alpha1.ClusterPhaseCreating
	case len(state.upgradeHeldBack) > 0 || (state.rolloutInProgress && state.currentVersion != cluster.Spec.Version):
		return objectstoragev1alpha1.ClusterPhaseUpgrading
	case state.expandingStorage || state.rolloutInProgress || state.progressing():
		return objectstoragev1alpha1.ClusterPhaseScaling
	case state.reconcileErr != nil || len(state.storageWarnings) > 0:
		return objectstoragev1alpha1.ClusterPhaseDegraded
	default:
		return objectstoragev1alpha1.ClusterPhaseRunning
	}
}

func equalStatus(a, b *objectstoragev1alpha1.ObjectStorageClusterStatus) bool {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	normalizeConditions(ac.Conditions)
	normalizeConditions(bc.Conditions)
	if ac.Topology != nil {
		ac.Topology.LastProbeTime = nil
	}
	if bc.Topology != nil {
		bc.Topology.LastProbeTime = nil
	}
	return equalJSON(ac, bc)
}

func normalizeConditions(conds []metav1.Condition) {
	for i := range conds {
		conds[i].LastTransitionTime = metav1.Time{}
	}
}
