package resources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

func testCluster(mutators ...func(*objectstoragev1alpha1.ObjectStorageCluster)) *objectstoragev1alpha1.ObjectStorageCluster {
	c := &objectstoragev1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: "team-a"},
		Spec: objectstoragev1alpha1.ObjectStorageClusterSpec{
			Version:            "3.80",
			ImagePullPolicy:    "IfNotPresent",
			DefaultReplication: "001",
			Metrics:            true,
			Master: objectstoragev1alpha1.MasterSpec{
				Replicas:          3,
				VolumeSizeLimitMB: 30000,
				GarbageThreshold:  "0.3",
			},
			Volume: objectstoragev1alpha1.VolumeSpec{
				Replicas: 2,
				Index:    "leveldb",
				Storage: objectstoragev1alpha1.StorageSpec{
					Size: resource.MustParse("50Gi"),
				},
			},
			Filer: objectstoragev1alpha1.FilerSpec{Replicas: 1, MaxMB: 16},
			S3:    objectstoragev1alpha1.S3Spec{Enabled: true, Replicas: 2},
		},
	}
	for _, m := range mutators {
		m(c)
	}
	return c
}

func TestNamesAreStableAndDistinct(t *testing.T) {
	c := testCluster()

	if got := Name(c, ComponentMaster); got != "store-master" {
		t.Errorf("Name(master) = %q, want store-master", got)
	}
	if HeadlessServiceName(c, ComponentMaster) == ClientServiceName(c, ComponentMaster) {
		t.Error("headless and client service names collide")
	}
	if got := PodFQDN(c, ComponentMaster, 1); got != "store-master-1.store-master.team-a.svc.cluster.local" {
		t.Errorf("PodFQDN = %q", got)
	}
}

func TestMasterPeersListsEveryReplica(t *testing.T) {
	c := testCluster()
	peers := strings.Split(MasterPeers(c), ",")
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers, got %d: %v", len(peers), peers)
	}
	for i, p := range peers {
		want := fmt.Sprintf("store-master-%d.store-master.team-a.svc.cluster.local:9333", i)
		if p != want {
			t.Errorf("peer %d = %q, want %q", i, p, want)
		}
	}
}

func TestImageResolution(t *testing.T) {
	if got := Image(testCluster()); got != "chrislusf/seaweedfs:3.80" {
		t.Errorf("composed image = %q", got)
	}
	explicit := testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Image = "registry.internal/seaweedfs:custom"
	})
	if got := Image(explicit); got != "registry.internal/seaweedfs:custom" {
		t.Errorf("explicit image = %q", got)
	}
}

func TestMasterArgsOmitPeersForSingleReplica(t *testing.T) {
	single := testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Master.Replicas = 1
	})
	args := strings.Join(MasterArgs(single), " ")
	if strings.Contains(args, "-peers=") {
		t.Errorf("single-replica master should not get a peers flag: %s", args)
	}

	quorum := strings.Join(MasterArgs(testCluster()), " ")
	if !strings.Contains(quorum, "-peers=") {
		t.Errorf("multi-replica master needs a peers flag: %s", quorum)
	}
	if !strings.Contains(quorum, "-defaultReplication=001") {
		t.Errorf("replication string not propagated: %s", quorum)
	}
}

func TestMasterHeadlessServicePublishesNotReadyAddresses(t *testing.T) {
	svc := MasterHeadlessService(testCluster())
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Error("governing service must be headless")
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("master headless service must publish not-ready addresses so Raft can bootstrap")
	}
}

func TestSelectorLabelsExcludeVersion(t *testing.T) {
	c := testCluster()
	selector := SelectorLabels(c, ComponentVolume)
	if _, ok := selector[LabelVersion]; ok {
		t.Error("selector labels must not include the version")
	}
	full := Labels(c, ComponentVolume)
	if full[LabelVersion] != "3.80" {
		t.Errorf("informational labels should carry the version, got %q", full[LabelVersion])
	}
	for k, v := range selector {
		if full[k] != v {
			t.Errorf("full labels must be a superset of the selector; %s mismatched", k)
		}
	}
}

func TestVolumeStatefulSetStorageAndTopology(t *testing.T) {
	sts := VolumeStatefulSet(testCluster())
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected one claim template, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	size := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "50Gi" {
		t.Errorf("claim size = %s, want 50Gi", size.String())
	}
	policy := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if policy == nil || policy.WhenScaled != "Retain" || policy.WhenDeleted != "Retain" {
		t.Errorf("volume PVCs must be retained on scale-down and delete, got %+v", policy)
	}
}

func TestVolumeTopologyFromNodeLabelsAddsInitContainer(t *testing.T) {
	c := testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Volume.TopologyFromNodeLabels = &objectstoragev1alpha1.TopologyFromNodeLabels{
			DataCenterLabel: "topology.kubernetes.io/region",
			RackLabel:       "topology.kubernetes.io/zone",
		}
	})
	sts := VolumeStatefulSet(c)
	if len(sts.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected a topology init container, got %d", len(sts.Spec.Template.Spec.InitContainers))
	}
	main := sts.Spec.Template.Spec.Containers[0]
	joined := strings.Join(main.Args, " ")
	if !strings.Contains(joined, "topology.env") {
		t.Errorf("main container should source the resolved topology: %s", joined)
	}
	if !strings.Contains(joined, "$(SEAWEED_DATACENTER)") {
		t.Errorf("dataCenter flag should reference the resolved env var: %s", joined)
	}
}

func TestVolumeStaticTopologyFlags(t *testing.T) {
	c := testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Volume.DataCenter = "dc1"
		c.Spec.Volume.Rack = "rack3"
	})
	args := strings.Join(VolumeArgs(c), " ")
	if !strings.Contains(args, "-dataCenter=dc1") || !strings.Contains(args, "-rack=rack3") {
		t.Errorf("static topology not applied: %s", args)
	}
}

func TestPodDisruptionBudgetOnlyForQuorum(t *testing.T) {
	if pdb := MasterPodDisruptionBudget(testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Spec.Master.Replicas = 1
	})); pdb != nil {
		t.Error("single-replica master must not get a PodDisruptionBudget")
	}

	pdb := MasterPodDisruptionBudget(testCluster())
	if pdb == nil {
		t.Fatal("three-replica master should get a PodDisruptionBudget")
	}
	if got := pdb.Spec.MinAvailable.IntValue(); got != 2 {
		t.Errorf("minAvailable = %d, want the Raft quorum of 2", got)
	}
}

func TestS3DeploymentDoesNotPassConfigFlag(t *testing.T) {
	args := strings.Join(S3Args(testCluster()), " ")
	if strings.Contains(args, "-config") {
		t.Errorf("S3 gateway must load IAM config from the filer, not a flag: %s", args)
	}
	if !strings.Contains(args, "-filer=store-filer-client.team-a.svc.cluster.local:8888") {
		t.Errorf("S3 gateway filer address wrong: %s", args)
	}
}

func TestS3DeploymentIsSurgeCapable(t *testing.T) {
	deploy := S3Deployment(testCluster())
	ru := deploy.Spec.Strategy.RollingUpdate
	if ru == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("stateless gateway should roll with zero unavailable, got %+v", ru)
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *deploy.Spec.Replicas)
	}
}

func TestFilerConfigChangeRollsPods(t *testing.T) {
	a := FilerStatefulSet(testCluster())
	b := FilerStatefulSet(testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) {
		c.Name = "other"
	}))
	const key = "objectstorage.openeverest.io/filer-config-hash"
	if a.Spec.Template.Annotations[key] == b.Spec.Template.Annotations[key] {
		t.Error("filer config hash should differ when the rendered filer.toml differs")
	}
	again := FilerStatefulSet(testCluster())
	if a.Spec.Template.Annotations[key] != again.Spec.Template.Annotations[key] {
		t.Error("filer config hash is not deterministic")
	}
}

func TestBuildersAreDeterministic(t *testing.T) {
	cases := map[string]func() any{
		"master": func() any { return MasterStatefulSet(testCluster()) },
		"volume": func() any { return VolumeStatefulSet(testCluster()) },
		"filer":  func() any { return FilerStatefulSet(testCluster()) },
		"s3":     func() any { return S3Deployment(testCluster()) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			first, err := json.Marshal(build())
			if err != nil {
				t.Fatal(err)
			}
			second, err := json.Marshal(build())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Error("builder output is not deterministic")
			}
		})
	}
}

func TestAdminSecretCarriesAWSAliases(t *testing.T) {
	secret := AdminSecret(testCluster(), "AKIAFAKE", "supersecret")
	for _, key := range []string{"accessKeyID", "secretAccessKey", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL"} {
		if secret.StringData[key] == "" {
			t.Errorf("admin secret missing key %q", key)
		}
	}
	if secret.StringData["AWS_ENDPOINT_URL"] != S3Endpoint(testCluster()) {
		t.Error("endpoint alias does not match the cluster S3 endpoint")
	}
}

func TestMetricsDisabledDropsScrapeAnnotations(t *testing.T) {
	c := testCluster(func(c *objectstoragev1alpha1.ObjectStorageCluster) { c.Spec.Metrics = false })
	sts := MasterStatefulSet(c)
	if _, ok := sts.Spec.Template.Annotations["prometheus.io/scrape"]; ok {
		t.Error("scrape annotations should be absent when metrics are disabled")
	}
	if strings.Contains(strings.Join(MasterArgs(c), " "), "-metricsPort") {
		t.Error("metricsPort flag should be absent when metrics are disabled")
	}
}
