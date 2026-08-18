package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
	"github.com/openeverest/seaweedfs-operator/internal/testutil"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(objectstoragev1alpha1.AddToScheme(scheme))

	binDir := os.Getenv("KUBEBUILDER_ASSETS")
	if binDir == "" {
		if guess := findEnvtestAssets(); guess != "" {
			binDir = guess
			_ = os.Setenv("KUBEBUILDER_ASSETS", guess)
		}
	}
	if binDir == "" {
		fmt.Fprintln(os.Stderr,
			"skipping envtest suite: no control plane binaries found. Run `make envtest` or set KUBEBUILDER_ASSETS.")
		os.Exit(0)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build client: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

func findEnvtestAssets() string {
	root := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(candidate, "kube-apiserver")); err == nil {
			abs, err := filepath.Abs(candidate)
			if err != nil {
				return ""
			}
			return abs
		}
	}
	return ""
}

type fakeClientFactory struct {
	fake *testutil.FakeSeaweed
}

func (f fakeClientFactory) For(
	_ context.Context,
	_ *objectstoragev1alpha1.ObjectStorageCluster,
	creds AdminCredentials,
) (*StorageClients, error) {
	filer := seaweed.NewFilerClient(f.fake.FilerURL())
	return &StorageClients{
		S3: seaweed.NewS3Client(seaweed.S3Options{
			Endpoint:        f.fake.S3URL(),
			Region:          resources.DefaultRegion,
			AccessKeyID:     creds.AccessKeyID,
			SecretAccessKey: creds.SecretAccessKey,
		}),
		Filer:          filer,
		Master:         seaweed.NewMasterClient(f.fake.MasterURL()),
		IAM:            seaweed.NewIAMStore(filer, resources.IAMConfigPath),
		AdminAccessKey: creds.AccessKeyID,
		AdminSecretKey: creds.SecretAccessKey,
	}, nil
}

type testHarness struct {
	t         *testing.T
	ctx       context.Context
	namespace string
	fake      *testutil.FakeSeaweed
	cluster   *ObjectStorageClusterReconciler
	bucket    *ObjectStorageBucketReconciler
	user      *ObjectStorageUserReconciler
	recorder  *record.FakeRecorder
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "osc-test-",
	}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	fake := testutil.NewFakeSeaweed()
	t.Cleanup(fake.Close)

	recorder := record.NewFakeRecorder(256)
	factory := fakeClientFactory{fake: fake}

	return &testHarness{
		t:         t,
		ctx:       ctx,
		namespace: ns.Name,
		fake:      fake,
		recorder:  recorder,
		cluster: &ObjectStorageClusterReconciler{
			Client: k8sClient, Scheme: scheme, Recorder: recorder, Clients: factory,
		},
		bucket: &ObjectStorageBucketReconciler{
			Client: k8sClient, Scheme: scheme, Recorder: recorder, Clients: factory,
		},
		user: &ObjectStorageUserReconciler{
			Client: k8sClient, Scheme: scheme, Recorder: recorder, Clients: factory,
		},
	}
}

func (h *testHarness) reconcileCluster(name string) (ctrl.Result, error) {
	return h.cluster.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: h.namespace, Name: name},
	})
}

func (h *testHarness) reconcileBucket(name string) (ctrl.Result, error) {
	return h.bucket.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: h.namespace, Name: name},
	})
}

func (h *testHarness) reconcileUser(name string) (ctrl.Result, error) {
	return h.user.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Namespace: h.namespace, Name: name},
	})
}

func (h *testHarness) mustReconcileCluster(name string) {
	h.t.Helper()
	if _, err := h.reconcileCluster(name); err != nil {
		h.t.Fatalf("cluster reconcile: %v", err)
	}
}

func (h *testHarness) newCluster(name string, mutators ...func(*objectstoragev1alpha1.ObjectStorageCluster)) *objectstoragev1alpha1.ObjectStorageCluster {
	h.t.Helper()
	c := &objectstoragev1alpha1.ObjectStorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: objectstoragev1alpha1.ObjectStorageClusterSpec{
			Version: "3.80",
			Master:  objectstoragev1alpha1.MasterSpec{Replicas: 1},
			Volume: objectstoragev1alpha1.VolumeSpec{
				Replicas: 1,
				Storage:  objectstoragev1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
			},
			Filer: objectstoragev1alpha1.FilerSpec{Replicas: 1},
			S3:    objectstoragev1alpha1.S3Spec{Enabled: true, Replicas: 1},
		},
	}
	for _, m := range mutators {
		m(c)
	}
	if err := k8sClient.Create(h.ctx, c); err != nil {
		h.t.Fatalf("create cluster: %v", err)
	}
	return c
}

func (h *testHarness) getCluster(name string) *objectstoragev1alpha1.ObjectStorageCluster {
	h.t.Helper()
	var c objectstoragev1alpha1.ObjectStorageCluster
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, &c); err != nil {
		h.t.Fatalf("get cluster %s: %v", name, err)
	}
	return &c
}

func (h *testHarness) markWorkloadsReady(clusterName string) {
	h.t.Helper()
	cluster := h.getCluster(clusterName)

	for _, comp := range []resources.Component{resources.ComponentMaster, resources.ComponentVolume, resources.ComponentFiler} {
		var sts appsv1.StatefulSet
		key := client.ObjectKey{Namespace: h.namespace, Name: resources.Name(cluster, comp)}
		if err := k8sClient.Get(h.ctx, key, &sts); err != nil {
			h.t.Fatalf("get statefulset %s: %v", key.Name, err)
		}
		replicas := int32(1)
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		sts.Status.Replicas = replicas
		sts.Status.ReadyReplicas = replicas
		sts.Status.UpdatedReplicas = replicas
		sts.Status.AvailableReplicas = replicas
		sts.Status.CurrentReplicas = replicas
		sts.Status.ObservedGeneration = sts.Generation
		if err := k8sClient.Status().Update(h.ctx, &sts); err != nil {
			h.t.Fatalf("update statefulset status %s: %v", key.Name, err)
		}
	}

	if cluster.Spec.S3.Enabled {
		var deploy appsv1.Deployment
		key := client.ObjectKey{Namespace: h.namespace, Name: resources.Name(cluster, resources.ComponentS3)}
		if err := k8sClient.Get(h.ctx, key, &deploy); err != nil {
			h.t.Fatalf("get deployment %s: %v", key.Name, err)
		}
		replicas := int32(1)
		if deploy.Spec.Replicas != nil {
			replicas = *deploy.Spec.Replicas
		}
		deploy.Status.Replicas = replicas
		deploy.Status.ReadyReplicas = replicas
		deploy.Status.UpdatedReplicas = replicas
		deploy.Status.AvailableReplicas = replicas
		deploy.Status.ObservedGeneration = deploy.Generation
		if err := k8sClient.Status().Update(h.ctx, &deploy); err != nil {
			h.t.Fatalf("update deployment status: %v", key.Name)
		}
	}
}

func (h *testHarness) bringUpCluster(name string) *objectstoragev1alpha1.ObjectStorageCluster {
	h.t.Helper()
	h.mustReconcileCluster(name)
	h.markWorkloadsReady(name)
	h.mustReconcileCluster(name)

	cluster := h.getCluster(name)
	if !clusterS3Ready(cluster) {
		h.t.Fatalf("cluster %s did not reach S3Ready; conditions: %+v", name, cluster.Status.Conditions)
	}
	return cluster
}

func (h *testHarness) hasEvent(substr string) bool {
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-h.recorder.Events:
			if contains(e, substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func conditionStatus(conds []metav1.Condition, condType string) metav1.ConditionStatus {
	for _, c := range conds {
		if c.Type == condType {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}

func conditionReason(conds []metav1.Condition, condType string) string {
	for _, c := range conds {
		if c.Type == condType {
			return c.Reason
		}
	}
	return ""
}
