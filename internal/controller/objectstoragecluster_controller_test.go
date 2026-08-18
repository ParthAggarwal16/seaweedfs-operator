package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
)

func TestClusterCreatesAllComponents(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.mustReconcileCluster("store")

	cluster := h.getCluster("store")

	for _, comp := range []resources.Component{resources.ComponentMaster, resources.ComponentVolume, resources.ComponentFiler} {
		var sts appsv1.StatefulSet
		key := client.ObjectKey{Namespace: h.namespace, Name: resources.Name(cluster, comp)}
		if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
			t.Fatalf("expected StatefulSet for %s: %v", comp, err)
		}
		if len(sts.OwnerReferences) != 1 || sts.OwnerReferences[0].Kind != "ObjectStorageCluster" {
			t.Errorf("%s StatefulSet is not owned by the cluster: %+v", comp, sts.OwnerReferences)
		}
		if !*sts.OwnerReferences[0].Controller {
			t.Errorf("%s owner reference should be a controller reference", comp)
		}
	}

	var deploy appsv1.Deployment
	if err := k8sClient.Get(h.ctx, client.ObjectKey{
		Namespace: h.namespace, Name: resources.Name(cluster, resources.ComponentS3),
	}, &deploy); err != nil {
		t.Fatalf("expected S3 Deployment: %v", err)
	}

	var services corev1.ServiceList
	if err := k8sClient.List(h.ctx, &services, client.InNamespace(h.namespace)); err != nil {
		t.Fatal(err)
	}
	if len(services.Items) != 7 {
		names := make([]string, 0, len(services.Items))
		for _, s := range services.Items {
			names = append(names, s.Name)
		}
		t.Errorf("expected 7 services, got %d: %v", len(services.Items), names)
	}

	var cm corev1.ConfigMap
	if err := k8sClient.Get(h.ctx, client.ObjectKey{
		Namespace: h.namespace, Name: resources.FilerConfigMapName(cluster),
	}, &cm); err != nil {
		t.Fatalf("expected filer ConfigMap: %v", err)
	}
}

func TestClusterFinalizerAddedBeforeProvisioning(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.mustReconcileCluster("store")

	cluster := h.getCluster("store")
	found := false
	for _, f := range cluster.Finalizers {
		if f == objectstoragev1alpha1.ClusterFinalizer {
			found = true
		}
	}
	if !found {
		t.Errorf("cluster finalizer missing: %v", cluster.Finalizers)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")

	h.mustReconcileCluster("store")
	var first appsv1.StatefulSet
	key := client.ObjectKey{Namespace: h.namespace, Name: "store-volume"}
	if err := k8sClient.Get(h.ctx, key, &first); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		h.mustReconcileCluster("store")
	}

	var after appsv1.StatefulSet
	if err := k8sClient.Get(h.ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != after.ResourceVersion {
		t.Errorf("repeated reconciles rewrote the StatefulSet: %s -> %s",
			first.ResourceVersion, after.ResourceVersion)
	}
}

func TestEvenMasterReplicasRejected(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store", func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Master.Replicas = 2
	})

	res, err := h.reconcileCluster("store")
	if err != nil {
		t.Fatalf("an invalid spec should be reported, not retried as an error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("invalid spec should not be requeued in a hot loop, got %v", res.RequeueAfter)
	}

	cluster := h.getCluster("store")
	if conditionStatus(cluster.Status.Conditions, objectstoragev1alpha1.ConditionDegraded) != metav1.ConditionTrue {
		t.Errorf("expected Degraded=True, conditions: %+v", cluster.Status.Conditions)
	}
	if got := conditionReason(cluster.Status.Conditions, objectstoragev1alpha1.ConditionAvailable); got != objectstoragev1alpha1.ReasonInvalidSpec {
		t.Errorf("Available reason = %q, want %q", got, objectstoragev1alpha1.ReasonInvalidSpec)
	}
	if !h.hasEvent(objectstoragev1alpha1.ReasonInvalidSpec) {
		t.Error("expected a Warning event describing the invalid spec")
	}

	var sts appsv1.StatefulSet
	err = k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-master"}, &sts)
	if !apierrors.IsNotFound(err) {
		t.Error("no workloads should be created for an invalid spec")
	}
}

func TestAdminSecretIsCreatedAndAdopted(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.mustReconcileCluster("store")

	var secret corev1.Secret
	key := client.ObjectKey{Namespace: h.namespace, Name: "store-s3-admin"}
	if err := k8sClient.Get(h.ctx, key, &secret); err != nil {
		t.Fatalf("admin secret not created: %v", err)
	}
	original := string(secret.Data["accessKeyID"])
	if original == "" {
		t.Fatal("admin secret has no access key")
	}

	h.mustReconcileCluster("store")
	if err := k8sClient.Get(h.ctx, key, &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["accessKeyID"]) != original {
		t.Error("admin credentials were regenerated on a subsequent reconcile")
	}
}

func TestClusterBecomesAvailableAndSeedsIdentity(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	cluster := h.bringUpCluster("store")

	if conditionStatus(cluster.Status.Conditions, objectstoragev1alpha1.ConditionAvailable) != metav1.ConditionTrue {
		t.Errorf("expected Available=True, got %+v", cluster.Status.Conditions)
	}
	if cluster.Status.Phase != objectstoragev1alpha1.ClusterPhaseRunning {
		t.Errorf("phase = %q, want Running", cluster.Status.Phase)
	}
	if cluster.Status.CurrentVersion != "3.80" {
		t.Errorf("currentVersion = %q, want 3.80", cluster.Status.CurrentVersion)
	}
	if cluster.Status.S3Endpoint == "" {
		t.Error("S3 endpoint not published on status")
	}

	raw, ok := h.fake.File(resources.IAMConfigPath)
	if !ok {
		t.Fatal("identity.json was never written to the filer")
	}
	cfg, err := seaweed.ParseIAMConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Get("seaweedfs-operator-admin"); !ok {
		t.Errorf("admin identity missing from IAM config: %s", raw)
	}
}

func TestTopologyIsPublishedOnStatus(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	if cluster.Status.Topology == nil {
		t.Fatal("topology not published")
	}
	if cluster.Status.Topology.VolumeServers == 0 {
		t.Error("topology reports no volume servers")
	}
	if cluster.Status.Topology.FreeVolumes == 0 {
		t.Error("topology reports no free volume slots")
	}
}

func TestS3UnreachableSurfacesOnStatus(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.mustReconcileCluster("store")
	h.markWorkloadsReady("store")

	h.fake.FailS3 = true
	h.mustReconcileCluster("store")

	cluster := h.getCluster("store")
	if conditionStatus(cluster.Status.Conditions, objectstoragev1alpha1.ConditionS3Ready) != metav1.ConditionFalse {
		t.Errorf("expected S3Ready=False when the endpoint is down, got %+v", cluster.Status.Conditions)
	}
	if conditionStatus(cluster.Status.Conditions, objectstoragev1alpha1.ConditionAvailable) != metav1.ConditionTrue {
		t.Error("an S3 outage should not clear the Available condition")
	}
}

func TestScalingVolumeReplicas(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.Volume.Replicas = 3
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	var sts appsv1.StatefulSet
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-volume"}, &sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 3 {
		t.Errorf("volume replicas = %d, want 3", *sts.Spec.Replicas)
	}

	updated := h.getCluster("store")
	if updated.Status.ProvisionedCapacity != "30Gi" {
		t.Errorf("provisionedCapacity = %q, want 30Gi (3 x 10Gi)", updated.Status.ProvisionedCapacity)
	}
}

func TestStorageExpansionRecreatesStatefulSet(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.Volume.Storage.Size = resource.MustParse("40Gi")
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: h.namespace, Name: "store-volume"}
	if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
		t.Fatalf("get volume statefulset: %v", err)
	}
	if sts.DeletionTimestamp.IsZero() {
		t.Fatal("expected the volume StatefulSet to be deleted for the expansion")
	}
	if !h.hasEvent(objectstoragev1alpha1.ReasonCapacityExpanding) {
		t.Error("expected a CapacityExpanding event")
	}

	h.mustReconcileCluster("store")
	if h.getCluster("store").Status.Phase != objectstoragev1alpha1.ClusterPhaseScaling {
		t.Errorf("phase during expansion = %q, want Scaling", h.getCluster("store").Status.Phase)
	}

	sts.Finalizers = nil
	if err := k8sClient.Update(h.ctx, &sts); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")
	if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
		t.Fatalf("StatefulSet not recreated: %v", err)
	}
	size := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "40Gi" {
		t.Errorf("recreated claim template size = %s, want 40Gi", size.String())
	}
}

func TestStorageShrinkIsRejected(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store", func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Volume.Storage.Size = resource.MustParse("20Gi")
	})
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.Volume.Storage.Size = resource.MustParse("5Gi")
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	var sts appsv1.StatefulSet
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-volume"}, &sts); err != nil {
		t.Fatalf("StatefulSet should be left alone on a rejected shrink: %v", err)
	}
	size := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "20Gi" {
		t.Errorf("claim size = %s, want the original 20Gi", size.String())
	}
	if !h.hasEvent(objectstoragev1alpha1.ReasonExpansionUnsupportd) {
		t.Error("expected a warning event about the rejected shrink")
	}

	updated := h.getCluster("store")
	if conditionStatus(updated.Status.Conditions, objectstoragev1alpha1.ConditionDegraded) != metav1.ConditionTrue {
		t.Error("a rejected shrink should mark the cluster Degraded so it is visible")
	}
}

func TestOrderedUpgradeHoldsBackLaterTiers(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.Version = "3.81"
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	get := func(name string) string {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, &sts); err != nil {
			t.Fatal(err)
		}
		return sts.Spec.Template.Spec.Containers[0].Image
	}

	if got := get("store-master"); got != "chrislusf/seaweedfs:3.81" {
		t.Errorf("master image = %q, want the new version", got)
	}
	if got := get("store-volume"); got != "chrislusf/seaweedfs:3.80" {
		t.Errorf("volume image = %q, want to still be held at the old version", got)
	}

	updated := h.getCluster("store")
	if updated.Status.CurrentVersion != "3.80" {
		t.Errorf("currentVersion advanced to %q before the rollout finished", updated.Status.CurrentVersion)
	}
	if updated.Status.Phase != objectstoragev1alpha1.ClusterPhaseUpgrading {
		t.Errorf("phase = %q, want Upgrading", updated.Status.Phase)
	}

	h.markWorkloadsReady("store")
	h.mustReconcileCluster("store")
	if got := get("store-volume"); got != "chrislusf/seaweedfs:3.81" {
		t.Errorf("volume image = %q, want the new version once masters are done", got)
	}
}

func TestPausedUpgradeHoldsEverything(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.Version = "3.81"
	cluster.Spec.Upgrade.Paused = true
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	var sts appsv1.StatefulSet
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-master"}, &sts); err != nil {
		t.Fatal(err)
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "chrislusf/seaweedfs:3.80" {
		t.Errorf("master image = %q, a paused upgrade must not roll anything", got)
	}
	if got := conditionReason(h.getCluster("store").Status.Conditions, objectstoragev1alpha1.ConditionProgressing); got != objectstoragev1alpha1.ReasonUpgradePaused {
		t.Errorf("Progressing reason = %q, want %q", got, objectstoragev1alpha1.ReasonUpgradePaused)
	}
}

func TestDriftIsCorrected(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	var sts appsv1.StatefulSet
	key := client.ObjectKey{Namespace: h.namespace, Name: "store-volume"}
	if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
		t.Fatal(err)
	}
	three := int32(3)
	sts.Spec.Replicas = &three
	sts.Spec.Template.Spec.Containers[0].Image = "someone/else:latest"
	if err := k8sClient.Update(h.ctx, &sts); err != nil {
		t.Fatal(err)
	}

	h.mustReconcileCluster("store")

	if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 1 {
		t.Errorf("replica drift not corrected: got %d, want 1", *sts.Spec.Replicas)
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "chrislusf/seaweedfs:3.80" {
		t.Errorf("image drift not corrected: got %q", got)
	}
}

func TestDeletedServiceIsRecreated(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: h.namespace, Name: "store-s3-client"}}
	if err := k8sClient.Delete(h.ctx, svc); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-s3-client"}, svc); err != nil {
		t.Fatalf("deleted Service was not recreated: %v", err)
	}
}

func TestDisablingS3RemovesTheGateway(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	cluster := h.getCluster("store")
	cluster.Spec.S3.Enabled = false
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	h.mustReconcileCluster("store")

	var deploy appsv1.Deployment
	err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-s3"}, &deploy)
	if !apierrors.IsNotFound(err) {
		t.Error("disabling S3 should remove the gateway Deployment, not leave it serving")
	}
	if got := conditionStatus(h.getCluster("store").Status.Conditions, objectstoragev1alpha1.ConditionS3Ready); got != metav1.ConditionFalse {
		t.Errorf("S3Ready = %v, want False when disabled", got)
	}
}

func TestClusterDeletionBlockedByDependents(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	bucket := &objectstoragev1alpha1.ObjectStorageBucket{
		ObjectMeta: metav1.ObjectMeta{Name: "photos", Namespace: h.namespace},
		Spec: objectstoragev1alpha1.ObjectStorageBucketSpec{
			ClusterRef: objectstoragev1alpha1.ClusterReference{Name: "store"},
		},
	}
	if err := k8sClient.Create(h.ctx, bucket); err != nil {
		t.Fatal(err)
	}

	cluster := h.getCluster("store")
	if err := k8sClient.Delete(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	res, err := h.reconcileCluster("store")
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("deletion blocked by dependents should requeue")
	}

	after := h.getCluster("store")
	if after.Status.Phase != objectstoragev1alpha1.ClusterPhaseDeleting {
		t.Errorf("phase = %q, want Deleting", after.Status.Phase)
	}

	if err := k8sClient.Delete(h.ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if _, err := h.reconcileCluster("store"); err != nil {
			t.Fatal(err)
		}
		var c objectstoragev1alpha1.ObjectStorageCluster
		if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store"}, &c); apierrors.IsNotFound(err) {
			return
		}
		var deploy appsv1.Deployment
		if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-s3"}, &deploy); err == nil {
			deploy.Status.Replicas = 0
			deploy.Status.ReadyReplicas = 0
			_ = k8sClient.Status().Update(h.ctx, &deploy)
		}
	}
	t.Error("cluster finalizer was never released")
}

func TestUserSuppliedFilerConfigMapIsNotOverwritten(t *testing.T) {
	h := newHarness(t)
	custom := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-filer-config", Namespace: h.namespace},
		Data:       map[string]string{"filer.toml": "[postgres]\nenabled = true\n"},
	}
	if err := k8sClient.Create(h.ctx, custom); err != nil {
		t.Fatal(err)
	}

	h.newCluster("store", func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Filer.ConfigMapName = "my-filer-config"
	})
	h.mustReconcileCluster("store")

	var cm corev1.ConfigMap
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "my-filer-config"}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["filer.toml"] != "[postgres]\nenabled = true\n" {
		t.Errorf("operator overwrote a user-supplied ConfigMap: %q", cm.Data["filer.toml"])
	}

	err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-filer-config"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Error("operator generated a ConfigMap despite spec.filer.configMapName being set")
	}

	var sts appsv1.StatefulSet
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "store-filer"}, &sts); err != nil {
		t.Fatal(err)
	}
	mounted := ""
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			mounted = v.ConfigMap.Name
		}
	}
	if mounted != "my-filer-config" {
		t.Errorf("filer mounts %q, want my-filer-config", mounted)
	}
}

func TestCRDDefaultsAreApplied(t *testing.T) {
	h := newHarness(t)
	minimal := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "objectstorage.openeverest.io/v1alpha1",
		"kind":       "ObjectStorageCluster",
		"metadata": map[string]any{
			"name":      "tiny",
			"namespace": h.namespace,
		},
		"spec": map[string]any{},
	}}
	if err := k8sClient.Create(h.ctx, minimal); err != nil {
		t.Fatalf("a spec with no fields set should be accepted: %v", err)
	}

	created := h.getCluster("tiny")
	if created.Spec.Version == "" {
		t.Error("spec.version default not applied")
	}
	if created.Spec.Master.Replicas != 1 {
		t.Errorf("master replicas default = %d, want 1", created.Spec.Master.Replicas)
	}
	if created.Spec.DefaultReplication != "000" {
		t.Errorf("defaultReplication = %q, want 000", created.Spec.DefaultReplication)
	}
	if !created.Spec.S3.Enabled {
		t.Error("S3 should default to enabled")
	}
	if created.Spec.Volume.Storage.Size.String() != "10Gi" {
		t.Errorf("volume storage default = %s, want 10Gi", created.Spec.Volume.Storage.Size.String())
	}
}

func TestInvalidReplicationStringRejectedByAPIServer(t *testing.T) {
	h := newHarness(t)
	bad := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "objectstorage.openeverest.io/v1alpha1",
		"kind":       "ObjectStorageCluster",
		"metadata": map[string]any{
			"name":      "bad",
			"namespace": h.namespace,
		},
		"spec": map[string]any{
			"version":            "3.80",
			"defaultReplication": "banana",
		},
	}}
	if err := k8sClient.Create(h.ctx, bad); err == nil {
		t.Error("the API server should reject a malformed replication string")
	}
}
