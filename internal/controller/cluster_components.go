package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
)

func (r *ObjectStorageClusterReconciler) reconcileComponents(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	state *clusterState,
) error {
	upgradeGateOpen := true

	for _, component := range resources.UpgradeOrder {
		if component == resources.ComponentS3 && !cluster.Spec.S3.Enabled {
			if err := r.pruneS3(ctx, cluster); err != nil {
				return err
			}
			continue
		}

		image, gated, err := r.resolveComponentImage(ctx, cluster, component, upgradeGateOpen)
		if err != nil {
			return err
		}
		if gated {
			state.upgradeHeldBack = append(state.upgradeHeldBack, string(component))
		}

		scoped := cluster.DeepCopy()
		scoped.Spec.Image = image

		if err := r.reconcileComponent(ctx, cluster, scoped, component, state); err != nil {
			return err
		}

		atDesired, err := r.componentAtDesiredImage(ctx, cluster, component)
		if err != nil {
			return err
		}
		if !atDesired {
			upgradeGateOpen = false
		}
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) reconcileComponent(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	scoped *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
	state *clusterState,
) error {
	switch component {
	case resources.ComponentMaster:
		if err := r.applyService(ctx, owner, resources.MasterHeadlessService(scoped)); err != nil {
			return err
		}
		if err := r.applyService(ctx, owner, resources.MasterClientService(scoped)); err != nil {
			return err
		}
		if err := r.applyPodDisruptionBudget(ctx, owner, resources.MasterPodDisruptionBudget(scoped),
			resources.Name(scoped, resources.ComponentMaster)); err != nil {
			return err
		}
		return r.applyStatefulSet(ctx, owner, resources.MasterStatefulSet(scoped), state)

	case resources.ComponentVolume:
		if err := r.applyService(ctx, owner, resources.VolumeHeadlessService(scoped)); err != nil {
			return err
		}
		if err := r.applyService(ctx, owner, resources.VolumeClientService(scoped)); err != nil {
			return err
		}
		return r.applyStatefulSet(ctx, owner, resources.VolumeStatefulSet(scoped), state)

	case resources.ComponentFiler:
		if scoped.Spec.Filer.ConfigMapName == "" {
			if err := r.applyConfigMap(ctx, owner, resources.FilerConfigMap(scoped)); err != nil {
				return err
			}
		}
		if err := r.applyService(ctx, owner, resources.FilerHeadlessService(scoped)); err != nil {
			return err
		}
		if err := r.applyService(ctx, owner, resources.FilerClientService(scoped)); err != nil {
			return err
		}
		return r.applyStatefulSet(ctx, owner, resources.FilerStatefulSet(scoped), state)

	case resources.ComponentS3:
		if err := r.applyService(ctx, owner, resources.S3Service(scoped)); err != nil {
			return err
		}
		return r.applyDeployment(ctx, owner, resources.S3Deployment(scoped))
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) pruneS3(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) error {
	name := resources.Name(cluster, resources.ComponentS3)
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}}
	if err := r.Delete(ctx, deploy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete disabled S3 deployment: %w", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      resources.ClientServiceName(cluster, resources.ComponentS3),
		Namespace: cluster.Namespace,
	}}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete disabled S3 service: %w", err)
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) resolveComponentImage(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
	gateOpen bool,
) (image string, gated bool, err error) {
	desired := resources.Image(cluster)

	current, exists, err := r.currentComponentImage(ctx, cluster, component)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return desired, false, nil
	}
	if current == desired {
		return desired, false, nil
	}
	if cluster.Spec.Upgrade.Paused {
		return current, true, nil
	}
	if cluster.Spec.Upgrade.Strategy == "Simultaneous" {
		return desired, false, nil
	}
	if !gateOpen {
		return current, true, nil
	}
	return desired, false, nil
}

func (r *ObjectStorageClusterReconciler) currentComponentImage(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
) (string, bool, error) {
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: resources.Name(cluster, component)}
	if component == resources.ComponentS3 {
		var deploy appsv1.Deployment
		if err := r.Get(ctx, key, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("get %s deployment: %w", component, err)
		}
		return containerImage(deploy.Spec.Template.Spec.Containers), true, nil
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get %s statefulset: %w", component, err)
	}
	return containerImage(sts.Spec.Template.Spec.Containers), true, nil
}

func (r *ObjectStorageClusterReconciler) componentAtDesiredImage(
	ctx context.Context,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
	component resources.Component,
) (bool, error) {
	desired := resources.Image(cluster)
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: resources.Name(cluster, component)}

	if component == resources.ComponentS3 {
		var deploy appsv1.Deployment
		if err := r.Get(ctx, key, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if containerImage(deploy.Spec.Template.Spec.Containers) != desired {
			return false, nil
		}
		if deploy.Status.ObservedGeneration < deploy.Generation {
			return false, nil
		}
		return deploy.Status.UpdatedReplicas == deploy.Status.Replicas &&
			deploy.Status.ReadyReplicas == deploy.Status.Replicas &&
			deploy.Status.Replicas > 0, nil
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if containerImage(sts.Spec.Template.Spec.Containers) != desired {
		return false, nil
	}
	if sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}
	return sts.Status.UpdatedReplicas == sts.Status.Replicas &&
		sts.Status.ReadyReplicas == sts.Status.Replicas &&
		sts.Status.Replicas > 0, nil
}

func containerImage(containers []corev1.Container) string {
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

func (r *ObjectStorageClusterReconciler) applyStatefulSet(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	desired *appsv1.StatefulSet,
	state *clusterState,
) error {
	logger := log.FromContext(ctx)
	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on statefulset %s: %w", desired.Name, err)
	}

	var existing appsv1.StatefulSet
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create statefulset %s: %w", desired.Name, err)
		}
		r.Recorder.Eventf(owner, corev1.EventTypeNormal, "StatefulSetCreated",
			"Created StatefulSet %s", desired.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get statefulset %s: %w", desired.Name, err)
	}

	if !existing.DeletionTimestamp.IsZero() {
		state.expandingStorage = true
		logger.V(1).Info("waiting for terminating statefulset before recreating", "name", existing.Name)
		return nil
	}

	handled, err := r.reconcileStorageCapacity(ctx, owner, &existing, desired, state)
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	existing.Spec.PersistentVolumeClaimRetentionPolicy = desired.Spec.PersistentVolumeClaimRetentionPolicy
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch statefulset %s: %w", desired.Name, err)
	}
	logger.V(1).Info("reconciled statefulset", "name", desired.Name)
	return nil
}

func (r *ObjectStorageClusterReconciler) reconcileStorageCapacity(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	existing, desired *appsv1.StatefulSet,
	state *clusterState,
) (bool, error) {
	logger := log.FromContext(ctx)
	if len(existing.Spec.VolumeClaimTemplates) == 0 || len(desired.Spec.VolumeClaimTemplates) == 0 {
		return false, nil
	}

	currentSize := existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	desiredSize := desired.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]

	switch currentSize.Cmp(desiredSize) {
	case 0:
		return false, nil
	case 1:
		msg := fmt.Sprintf(
			"Refusing to shrink %s storage from %s to %s: Kubernetes cannot shrink a PersistentVolumeClaim. Reverting to the current size.",
			existing.Name, currentSize.String(), desiredSize.String())
		logger.Info("rejecting storage shrink", "statefulset", existing.Name,
			"current", currentSize.String(), "requested", desiredSize.String())
		r.Recorder.Event(owner, corev1.EventTypeWarning, objectstoragev1alpha1.ReasonExpansionUnsupportd, msg)
		state.storageWarnings = append(state.storageWarnings, msg)
		return false, nil
	}

	state.expandingStorage = true
	r.Recorder.Eventf(owner, corev1.EventTypeNormal, objectstoragev1alpha1.ReasonCapacityExpanding,
		"Expanding %s storage from %s to %s", existing.Name, currentSize.String(), desiredSize.String())

	if err := r.expandClaims(ctx, existing, desiredSize); err != nil {
		return false, err
	}

	policy := metav1.DeletePropagationOrphan
	if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("recreate statefulset %s for expansion: %w", existing.Name, err)
	}
	logger.Info("deleted statefulset with orphaned pods to apply new volumeClaimTemplate",
		"statefulset", existing.Name, "newSize", desiredSize.String())
	return true, nil
}

func (r *ObjectStorageClusterReconciler) expandClaims(
	ctx context.Context,
	sts *appsv1.StatefulSet,
	size resource.Quantity,
) error {
	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims, client.InNamespace(sts.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err != nil {
		return fmt.Errorf("list claims for %s: %w", sts.Name, err)
	}

	for i := range claims.Items {
		claim := &claims.Items[i]
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(size) >= 0 {
			continue
		}
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.Resources.Requests = corev1.ResourceList{}
		}
		claim.Spec.Resources.Requests[corev1.ResourceStorage] = size
		if err := r.Patch(ctx, claim, patch); err != nil {
			return fmt.Errorf(
				"expand claim %s (does the StorageClass set allowVolumeExpansion: true?): %w",
				claim.Name, err)
		}
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) applyDeployment(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	desired *appsv1.Deployment,
) error {
	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on deployment %s: %w", desired.Name, err)
	}
	var existing appsv1.Deployment
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create deployment %s: %w", desired.Name, err)
		}
		r.Recorder.Eventf(owner, corev1.EventTypeNormal, "DeploymentCreated", "Created Deployment %s", desired.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", desired.Name, err)
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Strategy = desired.Spec.Strategy
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch deployment %s: %w", desired.Name, err)
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) applyService(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	desired *corev1.Service,
) error {
	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on service %s: %w", desired.Name, err)
	}
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create service %s: %w", desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get service %s: %w", desired.Name, err)
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Annotations = desired.Annotations
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Type = desired.Spec.Type
	existing.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
	existing.Spec.LoadBalancerSourceRanges = desired.Spec.LoadBalancerSourceRanges
	existing.Spec.Ports = mergeServicePorts(existing.Spec.Ports, desired.Spec.Ports)
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch service %s: %w", desired.Name, err)
	}
	return nil
}

func mergeServicePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	assigned := make(map[string]int32, len(existing))
	for _, p := range existing {
		if p.NodePort != 0 {
			assigned[p.Name] = p.NodePort
		}
	}
	out := make([]corev1.ServicePort, 0, len(desired))
	for _, p := range desired {
		if p.NodePort == 0 {
			if np, ok := assigned[p.Name]; ok {
				p.NodePort = np
			}
		}
		out = append(out, p)
	}
	return out
}

func (r *ObjectStorageClusterReconciler) applyConfigMap(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	desired *corev1.ConfigMap,
) error {
	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on configmap %s: %w", desired.Name, err)
	}
	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create configmap %s: %w", desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get configmap %s: %w", desired.Name, err)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch configmap %s: %w", desired.Name, err)
	}
	return nil
}

func (r *ObjectStorageClusterReconciler) applyPodDisruptionBudget(
	ctx context.Context,
	owner *objectstoragev1alpha1.ObjectStorageCluster,
	desired *policyv1.PodDisruptionBudget,
	name string,
) error {
	if desired == nil {
		stale := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.Namespace}}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale pod disruption budget %s: %w", name, err)
		}
		return nil
	}

	if err := controllerutil.SetControllerReference(owner, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner on pod disruption budget %s: %w", desired.Name, err)
	}
	var existing policyv1.PodDisruptionBudget
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create pod disruption budget %s: %w", desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get pod disruption budget %s: %w", desired.Name, err)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return fmt.Errorf("patch pod disruption budget %s: %w", desired.Name, err)
	}
	return nil
}
