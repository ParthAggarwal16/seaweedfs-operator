package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
)

func (h *testHarness) newBucket(name string, mutators ...func(*objectstoragev1alpha1.ObjectStorageBucket)) *objectstoragev1alpha1.ObjectStorageBucket {
	h.t.Helper()
	b := &objectstoragev1alpha1.ObjectStorageBucket{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: objectstoragev1alpha1.ObjectStorageBucketSpec{
			ClusterRef: objectstoragev1alpha1.ClusterReference{Name: "store"},
		},
	}
	for _, m := range mutators {
		m(b)
	}
	if err := k8sClient.Create(h.ctx, b); err != nil {
		h.t.Fatalf("create bucket: %v", err)
	}
	return b
}

func (h *testHarness) getBucket(name string) *objectstoragev1alpha1.ObjectStorageBucket {
	h.t.Helper()
	var b objectstoragev1alpha1.ObjectStorageBucket
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, &b); err != nil {
		h.t.Fatalf("get bucket %s: %v", name, err)
	}
	return &b
}

func (h *testHarness) newUser(name string, mutators ...func(*objectstoragev1alpha1.ObjectStorageUser)) *objectstoragev1alpha1.ObjectStorageUser {
	h.t.Helper()
	u := &objectstoragev1alpha1.ObjectStorageUser{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: objectstoragev1alpha1.ObjectStorageUserSpec{
			ClusterRef: objectstoragev1alpha1.ClusterReference{Name: "store"},
		},
	}
	for _, m := range mutators {
		m(u)
	}
	if err := k8sClient.Create(h.ctx, u); err != nil {
		h.t.Fatalf("create user: %v", err)
	}
	return u
}

func (h *testHarness) getUser(name string) *objectstoragev1alpha1.ObjectStorageUser {
	h.t.Helper()
	var u objectstoragev1alpha1.ObjectStorageUser
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: name}, &u); err != nil {
		h.t.Fatalf("get user %s: %v", name, err)
	}
	return &u
}

func (h *testHarness) iamConfig() *seaweed.IAMConfig {
	h.t.Helper()
	raw, ok := h.fake.File(resources.IAMConfigPath)
	if !ok {
		h.t.Fatal("no identity.json in the filer")
	}
	cfg, err := seaweed.ParseIAMConfig(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func TestBucketWaitsForClusterReadiness(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.mustReconcileCluster("store")
	h.newBucket("photos")

	res, err := h.reconcileBucket("photos")
	if err != nil {
		t.Fatalf("waiting on a cluster is not an error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("should requeue while waiting for the cluster")
	}

	bucket := h.getBucket("photos")
	if bucket.Status.Phase != objectstoragev1alpha1.BucketPhasePending {
		t.Errorf("phase = %q, want Pending", bucket.Status.Phase)
	}
	if got := conditionReason(bucket.Status.Conditions, objectstoragev1alpha1.ConditionReady); got != objectstoragev1alpha1.ReasonClusterNotReady {
		t.Errorf("Ready reason = %q, want %q", got, objectstoragev1alpha1.ReasonClusterNotReady)
	}
	if len(h.fake.Buckets()) != 0 {
		t.Error("no bucket should be created before the cluster is ready")
	}
}

func TestBucketLifecycle(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos")

	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}
	if !h.fake.HasBucket("photos") {
		t.Fatalf("bucket not created; existing buckets: %v", h.fake.Buckets())
	}

	bucket := h.getBucket("photos")
	if bucket.Status.Phase != objectstoragev1alpha1.BucketPhaseReady {
		t.Errorf("phase = %q, want Ready", bucket.Status.Phase)
	}
	if bucket.Status.BucketName != "photos" {
		t.Errorf("status.bucketName = %q", bucket.Status.BucketName)
	}
	if bucket.Status.Endpoint == "" {
		t.Error("status.endpoint is empty")
	}
	if bucket.Status.CreationTime == nil {
		t.Error("status.creationTime not set")
	}

	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(h.fake.Buckets()) != 1 {
		t.Errorf("expected exactly one bucket, got %v", h.fake.Buckets())
	}
}

func TestBucketNameOverride(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos", func(b *objectstoragev1alpha1.ObjectStorageBucket) {
		b.Spec.BucketName = "team-a-photos-prod"
	})

	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}
	if !h.fake.HasBucket("team-a-photos-prod") {
		t.Errorf("spec.bucketName was ignored; buckets: %v", h.fake.Buckets())
	}
}

func TestBucketRetainPolicyKeepsData(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos")

	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}
	h.fake.PutObject("photos", "vacation.jpg", []byte("data"))

	bucket := h.getBucket("photos")
	if err := k8sClient.Delete(h.ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	if !h.fake.HasBucket("photos") {
		t.Error("Retain policy must leave the bucket in place")
	}
	if h.fake.ObjectCount("photos") != 1 {
		t.Error("Retain policy must leave the objects in place")
	}
	var gone objectstoragev1alpha1.ObjectStorageBucket
	err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "photos"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Error("the Kubernetes object should be gone once the finalizer is released")
	}
}

func TestBucketDeletePolicyRemovesData(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("scratch", func(b *objectstoragev1alpha1.ObjectStorageBucket) {
		b.Spec.DeletionPolicy = objectstoragev1alpha1.DeletionPolicyDelete
	})

	if _, err := h.reconcileBucket("scratch"); err != nil {
		t.Fatal(err)
	}
	h.fake.PutObject("scratch", "tmp.bin", []byte("data"))

	bucket := h.getBucket("scratch")
	if err := k8sClient.Delete(h.ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileBucket("scratch"); err != nil {
		t.Fatal(err)
	}
	if h.fake.HasBucket("scratch") {
		t.Error("Delete policy should remove the bucket")
	}
}

func TestBucketConnectionSecret(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos", func(b *objectstoragev1alpha1.ObjectStorageBucket) {
		b.Spec.ConnectionSecretName = "photos-connection"
	})

	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	var secret corev1.Secret
	if err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "photos-connection"}, &secret); err != nil {
		t.Fatalf("connection secret not created: %v", err)
	}
	if string(secret.Data["bucketName"]) != "photos" {
		t.Errorf("bucketName = %q", secret.Data["bucketName"])
	}
	for _, forbidden := range []string{"secretAccessKey", "AWS_SECRET_ACCESS_KEY", "accessKeyID"} {
		if _, ok := secret.Data[forbidden]; ok {
			t.Errorf("bucket connection secret must not contain %q", forbidden)
		}
	}
}

func TestBucketWithMissingClusterReportsFailure(t *testing.T) {
	h := newHarness(t)
	h.newBucket("orphan", func(b *objectstoragev1alpha1.ObjectStorageBucket) {
		b.Spec.ClusterRef.Name = "does-not-exist"
	})

	if _, err := h.reconcileBucket("orphan"); err != nil {
		t.Fatalf("a dangling reference should be reported, not returned as an error: %v", err)
	}
	bucket := h.getBucket("orphan")
	if bucket.Status.Phase != objectstoragev1alpha1.BucketPhaseFailed {
		t.Errorf("phase = %q, want Failed", bucket.Status.Phase)
	}
	if got := conditionReason(bucket.Status.Conditions, objectstoragev1alpha1.ConditionReady); got != objectstoragev1alpha1.ReasonClusterNotFound {
		t.Errorf("reason = %q, want %q", got, objectstoragev1alpha1.ReasonClusterNotFound)
	}
}

func TestBucketDeletionWithMissingClusterReleasesFinalizer(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos")
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	cluster := h.getCluster("store")
	cluster.Finalizers = nil
	if err := k8sClient.Update(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Delete(h.ctx, cluster); err != nil {
		t.Fatal(err)
	}

	bucket := h.getBucket("photos")
	if err := k8sClient.Delete(h.ctx, bucket); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	var gone objectstoragev1alpha1.ObjectStorageBucket
	err := k8sClient.Get(h.ctx, client.ObjectKey{Namespace: h.namespace, Name: "photos"}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Error("bucket finalizer was not released when its cluster disappeared")
	}
}

func TestUserIssuesCredentialsAndIdentity(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos")
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	h.newUser("uploader", func(u *objectstoragev1alpha1.ObjectStorageUser) {
		u.Spec.BucketGrants = []objectstoragev1alpha1.BucketGrant{{
			BucketName: "photos",
			Actions: []objectstoragev1alpha1.S3Action{
				objectstoragev1alpha1.S3ActionRead,
				objectstoragev1alpha1.S3ActionWrite,
				objectstoragev1alpha1.S3ActionList,
			},
		}}
	})
	if _, err := h.reconcileUser("uploader"); err != nil {
		t.Fatal(err)
	}

	user := h.getUser("uploader")
	if user.Status.Phase != objectstoragev1alpha1.UserPhaseReady {
		t.Errorf("phase = %q, want Ready", user.Status.Phase)
	}
	if user.Status.AccessKeyID == "" {
		t.Error("access key ID not surfaced on status")
	}

	var secret corev1.Secret
	if err := k8sClient.Get(h.ctx, client.ObjectKey{
		Namespace: h.namespace, Name: "uploader-s3-credentials",
	}, &secret); err != nil {
		t.Fatalf("credentials secret not created: %v", err)
	}
	if len(secret.Data["secretAccessKey"]) == 0 {
		t.Error("secret access key missing from the Secret")
	}
	if string(secret.Data["secretAccessKey"]) == user.Status.AccessKeyID {
		t.Error("status appears to leak the secret access key")
	}

	cfg := h.iamConfig()
	identity, ok := cfg.Get(h.namespace + "-uploader")
	if !ok {
		t.Fatalf("identity not found in IAM config; identities: %+v", cfg.Identities)
	}
	want := map[string]bool{"Read:photos": true, "Write:photos": true, "List:photos": true}
	if len(identity.Actions) != len(want) {
		t.Errorf("actions = %v, want %v", identity.Actions, want)
	}
	for _, a := range identity.Actions {
		if !want[a] {
			t.Errorf("unexpected action %q", a)
		}
	}
	if len(identity.Credentials) != 1 || identity.Credentials[0].AccessKey != string(secret.Data["accessKeyID"]) {
		t.Error("IAM credential does not match the issued Secret")
	}
}

func TestUserCredentialsAreNotRotatedOnReconcile(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("app")

	if _, err := h.reconcileUser("app"); err != nil {
		t.Fatal(err)
	}
	first := h.getUser("app").Status.AccessKeyID

	for i := 0; i < 3; i++ {
		if _, err := h.reconcileUser("app"); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.getUser("app").Status.AccessKeyID; got != first {
		t.Errorf("credentials rotated on reconcile: %q -> %q", first, got)
	}
}

func TestUserBucketRefResolvesRenamedBucket(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newBucket("photos", func(b *objectstoragev1alpha1.ObjectStorageBucket) {
		b.Spec.BucketName = "team-a-photos"
	})
	if _, err := h.reconcileBucket("photos"); err != nil {
		t.Fatal(err)
	}

	h.newUser("reader", func(u *objectstoragev1alpha1.ObjectStorageUser) {
		u.Spec.BucketGrants = []objectstoragev1alpha1.BucketGrant{{
			BucketName: "ignored-when-ref-is-set",
			BucketRef:  &objectstoragev1alpha1.LocalSecretReference{Name: "photos"},
			Actions:    []objectstoragev1alpha1.S3Action{objectstoragev1alpha1.S3ActionRead},
		}}
	})
	if _, err := h.reconcileUser("reader"); err != nil {
		t.Fatal(err)
	}

	identity, ok := h.iamConfig().Get(h.namespace + "-reader")
	if !ok {
		t.Fatal("identity missing")
	}
	if len(identity.Actions) != 1 || identity.Actions[0] != "Read:team-a-photos" {
		t.Errorf("actions = %v, want [Read:team-a-photos]", identity.Actions)
	}
}

func TestUserGrantWithPrefix(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("scoped", func(u *objectstoragev1alpha1.ObjectStorageUser) {
		u.Spec.BucketGrants = []objectstoragev1alpha1.BucketGrant{{
			BucketName: "photos",
			Prefix:     "incoming/",
			Actions:    []objectstoragev1alpha1.S3Action{objectstoragev1alpha1.S3ActionWrite},
		}}
	})
	if _, err := h.reconcileUser("scoped"); err != nil {
		t.Fatal(err)
	}

	identity, _ := h.iamConfig().Get(h.namespace + "-scoped")
	if len(identity.Actions) != 1 || identity.Actions[0] != "Write:photos/incoming/" {
		t.Errorf("actions = %v, want [Write:photos/incoming/]", identity.Actions)
	}
}

func TestUserWithDanglingBucketRefFails(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("broken", func(u *objectstoragev1alpha1.ObjectStorageUser) {
		u.Spec.BucketGrants = []objectstoragev1alpha1.BucketGrant{{
			BucketName: "x",
			BucketRef:  &objectstoragev1alpha1.LocalSecretReference{Name: "nope"},
			Actions:    []objectstoragev1alpha1.S3Action{objectstoragev1alpha1.S3ActionRead},
		}}
	})

	if _, err := h.reconcileUser("broken"); err != nil {
		t.Fatalf("a bad reference is a spec problem, not a transport error: %v", err)
	}
	user := h.getUser("broken")
	if user.Status.Phase != objectstoragev1alpha1.UserPhaseFailed {
		t.Errorf("phase = %q, want Failed", user.Status.Phase)
	}
	if cfg := h.iamConfig(); len(cfg.Identities) != 1 {
		t.Errorf("only the operator admin identity should exist, got %+v", cfg.Identities)
	}
}

func TestUserDeletionRevokesIdentity(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("temp")
	if _, err := h.reconcileUser("temp"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.iamConfig().Get(h.namespace + "-temp"); !ok {
		t.Fatal("identity was never created")
	}

	user := h.getUser("temp")
	if err := k8sClient.Delete(h.ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileUser("temp"); err != nil {
		t.Fatal(err)
	}

	if _, ok := h.iamConfig().Get(h.namespace + "-temp"); ok {
		t.Error("identity still present after deletion")
	}
}

func TestUserRetainPolicyKeepsIdentity(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("kept", func(u *objectstoragev1alpha1.ObjectStorageUser) {
		u.Spec.DeletionPolicy = objectstoragev1alpha1.DeletionPolicyRetain
	})
	if _, err := h.reconcileUser("kept"); err != nil {
		t.Fatal(err)
	}

	user := h.getUser("kept")
	if err := k8sClient.Delete(h.ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcileUser("kept"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.iamConfig().Get(h.namespace + "-kept"); !ok {
		t.Error("Retain policy should leave the identity in place")
	}
}

func TestIdentityNamesAreNamespaced(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("app")
	if _, err := h.reconcileUser("app"); err != nil {
		t.Fatal(err)
	}

	user := h.getUser("app")
	if user.Status.IdentityName != h.namespace+"-app" {
		t.Errorf("identityName = %q, want it namespaced", user.Status.IdentityName)
	}
}

func TestUserFilerFailureIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")
	h.newUser("app")

	h.fake.FailFilerWrites = true
	if _, err := h.reconcileUser("app"); err == nil {
		t.Fatal("a filer write failure must be returned so controller-runtime backs off and retries")
	}
	if h.getUser("app").Status.Phase != objectstoragev1alpha1.UserPhaseFailed {
		t.Error("failure should be visible on status")
	}

	h.fake.FailFilerWrites = false
	if _, err := h.reconcileUser("app"); err != nil {
		t.Fatalf("reconcile should succeed once the filer recovers: %v", err)
	}
	if h.getUser("app").Status.Phase != objectstoragev1alpha1.UserPhaseReady {
		t.Error("user did not recover after the filer came back")
	}
}

func TestMultipleUsersCoexistInOneIAMConfig(t *testing.T) {
	h := newHarness(t)
	h.newCluster("store")
	h.bringUpCluster("store")

	for _, name := range []string{"alice", "bob", "carol"} {
		h.newUser(name)
		if _, err := h.reconcileUser(name); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	cfg := h.iamConfig()
	if len(cfg.Identities) != 4 {
		names := make([]string, 0, len(cfg.Identities))
		for _, id := range cfg.Identities {
			names = append(names, id.Name)
		}
		t.Errorf("expected 4 identities, got %d: %v", len(cfg.Identities), names)
	}
}
