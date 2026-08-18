package seaweed_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
	"github.com/openeverest/seaweedfs-operator/internal/testutil"
)

func TestParseEmptyIAMConfig(t *testing.T) {
	cfg, err := seaweed.ParseIAMConfig(nil)
	if err != nil {
		t.Fatalf("empty input: %v", err)
	}
	if len(cfg.Identities) != 0 {
		t.Errorf("expected no identities, got %d", len(cfg.Identities))
	}

	if _, err := seaweed.ParseIAMConfig([]byte("   \n")); err != nil {
		t.Errorf("whitespace-only input: %v", err)
	}
	if _, err := seaweed.ParseIAMConfig([]byte("{ not json")); err == nil {
		t.Error("malformed JSON should be an error")
	}
}

func TestIAMUpsertIsIdempotent(t *testing.T) {
	cfg := &seaweed.IAMConfig{}
	id := seaweed.Identity{
		Name:        "team-a-app",
		Credentials: []seaweed.Credential{{AccessKey: "AK", SecretKey: "SK"}},
		Actions:     []string{"Read:data", "Write:data"},
	}

	if !cfg.Upsert(id) {
		t.Fatal("first upsert should report a change")
	}
	if cfg.Upsert(id) {
		t.Error("re-applying an identical identity should report no change")
	}

	reordered := id
	reordered.Actions = []string{"Write:data", "Read:data"}
	if cfg.Upsert(reordered) {
		t.Error("reordered actions should not count as a change")
	}

	changed := id
	changed.Actions = []string{"Read:data"}
	if !cfg.Upsert(changed) {
		t.Error("narrowing permissions should report a change")
	}
	if len(cfg.Identities) != 1 {
		t.Errorf("upsert should replace, not append: got %d identities", len(cfg.Identities))
	}
}

func TestIAMMarshalIsStable(t *testing.T) {
	build := func() *seaweed.IAMConfig {
		cfg := &seaweed.IAMConfig{}
		cfg.Upsert(seaweed.Identity{Name: "zeta", Actions: []string{"Read"}})
		cfg.Upsert(seaweed.Identity{Name: "alpha", Actions: []string{"Write", "Read"}})
		return cfg
	}
	first, err := build().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := build().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("marshal output is not stable across runs")
	}
	if !strings.Contains(string(first), `"alpha"`) ||
		strings.Index(string(first), `"alpha"`) > strings.Index(string(first), `"zeta"`) {
		t.Errorf("identities should be sorted by name:\n%s", first)
	}
}

func TestIAMRemove(t *testing.T) {
	cfg := &seaweed.IAMConfig{}
	cfg.Upsert(seaweed.Identity{Name: "gone", Actions: []string{"Read"}})
	cfg.Upsert(seaweed.Identity{Name: "stays", Actions: []string{"Read"}})

	if !cfg.Remove("gone") {
		t.Error("removing an existing identity should report a change")
	}
	if cfg.Remove("gone") {
		t.Error("removing a missing identity should report no change")
	}
	if _, ok := cfg.Get("stays"); !ok {
		t.Error("Remove deleted the wrong identity")
	}
}

func TestBuildAction(t *testing.T) {
	cases := []struct {
		verb, bucket, prefix, want string
	}{
		{"Admin", "", "", "Admin"},
		{"Read", "*", "", "Read"},
		{"Read", "photos", "", "Read:photos"},
		{"Write", "photos", "raw/", "Write:photos/raw/"},
		{"Write", "photos", "/raw", "Write:photos/raw"},
	}
	for _, tc := range cases {
		if got := seaweed.BuildAction(tc.verb, tc.bucket, tc.prefix); got != tc.want {
			t.Errorf("BuildAction(%q,%q,%q) = %q, want %q", tc.verb, tc.bucket, tc.prefix, got, tc.want)
		}
	}
}

func TestFilerReadWriteDeleteRoundTrip(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()

	ctx := context.Background()
	client := seaweed.NewFilerClient(fake.FilerURL())

	if _, err := client.ReadFile(ctx, "/etc/iam/identity.json"); !errors.Is(err, seaweed.ErrNotFound) {
		t.Fatalf("missing path should yield ErrNotFound, got %v", err)
	}

	payload := []byte(`{"identities":[]}`)
	if err := client.WriteFile(ctx, "/etc/iam/identity.json", payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := client.ReadFile(ctx, "/etc/iam/identity.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("round trip mismatch: %q", got)
	}

	if err := client.DeletePath(ctx, "/etc/iam/identity.json", false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := client.DeletePath(ctx, "/etc/iam/identity.json", false); err != nil {
		t.Fatalf("repeat delete should be a no-op, got %v", err)
	}
}

func TestIAMStoreMutateSkipsWriteWhenUnchanged(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()

	ctx := context.Background()
	store := seaweed.NewIAMStore(seaweed.NewFilerClient(fake.FilerURL()), "/etc/iam/identity.json")

	changed, err := store.Mutate(ctx, func(cfg *seaweed.IAMConfig) (bool, error) {
		return cfg.Upsert(seaweed.Identity{Name: "admin", Actions: seaweed.AdminActions}), nil
	})
	if err != nil || !changed {
		t.Fatalf("first mutate: changed=%v err=%v", changed, err)
	}
	if _, ok := fake.File("/etc/iam/identity.json"); !ok {
		t.Fatal("identity.json was not written to the filer")
	}

	changed, err = store.Mutate(ctx, func(cfg *seaweed.IAMConfig) (bool, error) {
		return cfg.Upsert(seaweed.Identity{Name: "admin", Actions: seaweed.AdminActions}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("re-applying the same identity should not write to the filer")
	}
}

func TestIAMStoreSurfacesFilerFailures(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()
	fake.FailFilerWrites = true

	store := seaweed.NewIAMStore(seaweed.NewFilerClient(fake.FilerURL()), "/etc/iam/identity.json")
	_, err := store.Mutate(context.Background(), func(cfg *seaweed.IAMConfig) (bool, error) {
		return cfg.Upsert(seaweed.Identity{Name: "admin", Actions: seaweed.AdminActions}), nil
	})
	if err == nil {
		t.Fatal("a failing filer write must surface as an error, not be swallowed")
	}
}

func TestS3EnsureBucketIsIdempotent(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()

	ctx := context.Background()
	client := seaweed.NewS3Client(seaweed.S3Options{
		Endpoint:        fake.S3URL(),
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
	})

	created, err := client.EnsureBucket(ctx, "photos", false)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !created {
		t.Error("first ensure should create the bucket")
	}

	created, err = client.EnsureBucket(ctx, "photos", false)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if created {
		t.Error("second ensure should be a no-op")
	}
	if !fake.HasBucket("photos") {
		t.Error("bucket missing from the fake backend")
	}
}

func TestS3DeleteBucketEmptiesFirst(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()

	ctx := context.Background()
	client := seaweed.NewS3Client(seaweed.S3Options{
		Endpoint:        fake.S3URL(),
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
	})
	if _, err := client.EnsureBucket(ctx, "photos", false); err != nil {
		t.Fatal(err)
	}
	fake.PutObject("photos", "a.jpg", []byte("x"))
	fake.PutObject("photos", "b.jpg", []byte("y"))

	if err := client.DeleteBucket(ctx, "photos"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if fake.HasBucket("photos") {
		t.Error("bucket still present after delete")
	}
}

func TestS3DeleteMissingBucketSucceeds(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()

	client := seaweed.NewS3Client(seaweed.S3Options{
		Endpoint:        fake.S3URL(),
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
	})
	if err := client.DeleteBucket(context.Background(), "never-existed"); err != nil {
		t.Fatalf("deleting a missing bucket should succeed, got %v", err)
	}
}

func TestGeneratedCredentialsHaveExpectedShape(t *testing.T) {
	access, err := seaweed.GenerateAccessKeyID()
	if err != nil {
		t.Fatal(err)
	}
	if len(access) != 20 || access != strings.ToUpper(access) {
		t.Errorf("access key %q should be 20 uppercase characters", access)
	}
	secret, err := seaweed.GenerateSecretAccessKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 40 {
		t.Errorf("secret key %q should be 40 characters", secret)
	}

	other, _ := seaweed.GenerateAccessKeyID()
	if access == other {
		t.Error("generated access keys must not repeat")
	}
}

func TestMasterTopologyFlattening(t *testing.T) {
	fake := testutil.NewFakeSeaweed()
	defer fake.Close()
	fake.Topology = testutil.TopologyReport{
		DataCenters: 2, Racks: 2, VolumeServers: 3, Volumes: 4, Max: 10, Free: 6,
	}

	top, err := seaweed.NewMasterClient(fake.MasterURL()).Topology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if top.DataCenters != 2 {
		t.Errorf("dataCenters = %d, want 2", top.DataCenters)
	}
	if top.Racks != 4 {
		t.Errorf("racks = %d, want 4 (2 per data center)", top.Racks)
	}
	if top.VolumeServers != 12 {
		t.Errorf("volumeServers = %d, want 12", top.VolumeServers)
	}
	if top.FreeVolumes != 6 {
		t.Errorf("freeVolumes = %d, want 6", top.FreeVolumes)
	}
}

func TestIdentityJSONMatchesSeaweedFSSchema(t *testing.T) {
	cfg := &seaweed.IAMConfig{}
	cfg.Upsert(seaweed.Identity{
		Name:        "app",
		Credentials: []seaweed.Credential{{AccessKey: "AK", SecretKey: "SK"}},
		Actions:     []string{"Read:bucket"},
	})
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string][]map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	identities, ok := raw["identities"]
	if !ok || len(identities) != 1 {
		t.Fatalf("expected one identity under the \"identities\" key, got %v", raw)
	}
	creds, ok := identities[0]["credentials"].([]any)
	if !ok || len(creds) != 1 {
		t.Fatalf("expected one credential, got %v", identities[0])
	}
	cred := creds[0].(map[string]any)
	if cred["accessKey"] != "AK" || cred["secretKey"] != "SK" {
		t.Errorf("credential field names changed: %v", cred)
	}
}
