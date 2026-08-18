# seaweedfs-operator

A Kubernetes operator for running SeaweedFS as a proper Kubernetes workload.

It manages SeaweedFS clusters through custom resources and is meant to handle the main components: masters, volume servers, filers, the S3 gateway, buckets, and S3 users/credentials. It's built around normal Kubernetes reconciliation rather than a pile of static manifests.


## What it does

### Cluster lifecycle

- Creates SeaweedFS masters, volume servers, filers, and S3 gateways
- Reconciles the generated Kubernetes resources
- Detects and corrects basic resource drift
- Scales SeaweedFS components
- Supports SeaweedFS version changes
- Handles cluster deletion through finalizers

### Storage

- Creates PVCs for stateful components
- Supports volume-server replica scaling
- Supports PVC expansion where the StorageClass allows it
- Maps Kubernetes topology info to SeaweedFS topology config
- Retains PVCs when clusters are removed

### Buckets and users

- Creates S3 buckets through the SeaweedFS S3 API
- Configures S3 identities
- Stores generated credentials in Kubernetes Secrets
- Publishes credential-free bucket connection info
- Supports bucket-scoped permissions and prefixes

### Kubernetes integration

- Custom resources for clusters, buckets, and users
- Status conditions
- Finalizers
- Kubernetes Events
- Prometheus metrics
- Helm installation

## Architecture

```
ObjectStorageCluster
ObjectStorageBucket
ObjectStorageUser
          |
          v
      Operator
          |
          +-------------------+
          |                   |
          v                   v
   Kubernetes API       SeaweedFS APIs
          |             +----+----+----+
          |             |    |    |
          v             v    v    v
   StatefulSets       Master Filer S3
   Deployments        API    API   API
   Services
   PVCs
   Secrets
   ConfigMaps
   PDBs
```

The controllers translate custom resources into Kubernetes resources and talk to SeaweedFS where needed.

## Requirements

| Tool    | Version |
| ------- | ------- |
| Go      | 1.24+   |
| Docker  | Recent  |
| kubectl | 1.28+   |
| kind    | 0.23+   |
| Helm    | 3.12+   |

You need a Kubernetes cluster 1.27+ for normal operation.

Storage expansion needs a StorageClass with:

```yaml
allowVolumeExpansion: true
```

## Quick start

```bash
# Create a kind cluster
make kind-up

# Build and load the operator
make kind-load

# Deploy
make deploy IMG=ghcr.io/openeverest/seaweedfs-operator:dev
```

Check the controller:

```bash
kubectl -n seaweedfs-system get pods
```

Create a namespace and a cluster:

```bash
kubectl create namespace storage
kubectl -n storage apply -f config/samples/quickstart.yaml
```

Watch it come up:

```bash
kubectl -n storage get objectstoragecluster -w
```

Check conditions:

```bash
kubectl -n storage get objectstoragecluster quickstart \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

Create a bucket and user:

```bash
kubectl -n storage apply -f config/samples/bucket-and-user.yaml
kubectl -n storage get objectstoragebucket
kubectl -n storage get objectstorageuser
```

Access the S3 endpoint (if it's a ClusterIP):

```bash
kubectl -n storage port-forward svc/quickstart-s3-client 8333:8333
```

Connection info and credentials live in the generated Secrets.

## Installing with Helm

```bash
helm install seaweedfs-operator ./charts/seaweedfs-operator \
  --namespace seaweedfs-system \
  --create-namespace
```

Common config:

| Value                            | Purpose                                  |
| -------------------------------- | ---------------------------------------- |
| `watchNamespace`                 | Restrict reconciliation to one namespace |
| `replicaCount`                   | Number of controller replicas            |
| `leaderElection.enabled`         | Enable leader election                   |
| `metrics.serviceMonitor.enabled` | Create a Prometheus ServiceMonitor       |
| `logging.mode`                   | Controller logging mode                  |


## The API

Three main namespaced resources, under `objectstorage.openeverest.io/v1alpha1`:

- `ObjectStorageCluster`
- `ObjectStorageBucket`
- `ObjectStorageUser`

### ObjectStorageCluster

```yaml
apiVersion: objectstorage.openeverest.io/v1alpha1
kind: ObjectStorageCluster
metadata:
  name: objectstore
spec:
  version: "3.80"
  defaultReplication: "010"
  master:
    replicas: 3
    storage:
      size: 5Gi
  volume:
    replicas: 3
    storage:
      size: 50Gi
  filer:
    replicas: 1
  s3:
    enabled: true
    replicas: 1
  upgrade:
    strategy: OrderedComponents
    paused: false
```

This resource controls the desired SeaweedFS topology and the Kubernetes resources needed to run it.

**Status** includes phase, conditions, current version, provisioned capacity, SeaweedFS topology info, S3 endpoint, and the admin Secret reference. The conditions are more useful than phase for figuring out if the cluster is actually ready. Expected conditions: `Available`, `Progressing`, `Degraded`, `S3Ready`.

### ObjectStorageBucket

```yaml
apiVersion: objectstorage.openeverest.io/v1alpha1
kind: ObjectStorageBucket
metadata:
  name: app-uploads
spec:
  clusterRef:
    name: objectstore
  bucketName: app-uploads
  deletionPolicy: Retain
  quota:
    sizeGiB: 100
    enforce: true
```

The bucket controller talks to the SeaweedFS S3 endpoint directly. Default deletion policy is meant to protect data from an accidental resource delete.

### ObjectStorageUser

```yaml
apiVersion: objectstorage.openeverest.io/v1alpha1
kind: ObjectStorageUser
metadata:
  name: uploader
spec:
  clusterRef:
    name: objectstore
  bucketGrants:
    - bucketRef:
        name: app-uploads
      actions:
        - Read
        - Write
        - List
```

The operator creates or adopts credentials in a Kubernetes Secret. The generated Secret is meant to include:

```
accessKeyID
secretAccessKey
endpoint
region
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_ENDPOINT_URL
AWS_REGION
```

Credentials shouldn't go directly into the custom resource itself.

## Day-two operations

**Scaling volume servers**

```bash
kubectl -n storage patch objectstoragecluster objectstore \
  --type=merge \
  -p '{"spec":{"volume":{"replicas":8}}}'
```

Note this is different from resizing existing PVCs.

**Expanding storage**

```bash
kubectl -n storage patch objectstoragecluster objectstore \
  --type=merge \
  -p '{"spec":{"volume":{"storage":{"size":"1Ti"}}}}'
```

PVC shrinking isn't supported and should get rejected. The StatefulSet handling around expansion is one of the areas worth reading closely if you're digging into the implementation.

**Upgrading SeaweedFS**

```bash
kubectl -n storage patch objectstoragecluster objectstore \
  --type=merge \
  -p '{"spec":{"version":"3.81"}}'
```

Intended order:

```
masters -> volume servers -> filers -> S3 gateway
```


**Pausing an upgrade**

```bash
kubectl -n storage patch objectstoragecluster objectstore \
  --type=merge \
  -p '{"spec":{"upgrade":{"paused":true}}}'
```

**Filer configuration**

```bash
kubectl -n storage create configmap filer-config \
  --from-file=filer.toml
```

Then reference it from the cluster resource. The operator shouldn't modify a user-supplied ConfigMap.

## Deletion

Finalizers control the deletion order:

```
Bucket/User dependents -> S3 gateway -> SeaweedFS components -> operator finalizer removed
```

PVC retention is intentional, you have to explicitly delete PVCs if you want the storage back.

## Observability

Prometheus metrics exposed by the operator, expected ones include:

```
seaweedfs_operator_reconcile_total
seaweedfs_operator_reconcile_duration_seconds
seaweedfs_operator_cluster_phase
seaweedfs_operator_component_replicas
seaweedfs_operator_cluster_provisioned_capacity_bytes
seaweedfs_operator_cluster_free_volume_slots
seaweedfs_operator_buckets_managed
seaweedfs_operator_users_managed
seaweedfs_operator_storage_api_errors_total
seaweedfs_operator_upgrades_total
```

**Events** worth knowing about: `StatefulSetCreated`, `CapacityExpanding`, `VolumeExpansionUnsupported`, `NoFreeVolumeSlots`, `UpgradeCompleted`, `CredentialsIssued`, `BucketCreated`, `BucketDeleted`, `IdentityRevoked`, `InvalidSpec`.

```bash
kubectl -n storage get events \
  --field-selector involvedObject.kind=ObjectStorageCluster
```

**Logs**

```bash
kubectl -n seaweedfs-system logs \
  deploy/seaweedfs-operator-controller-manager
```

Debug logging can be turned on through the controller's logging args.

## Testing

**Unit tests**

```bash
make test-unit
```

Covers Kubernetes resource generation, SeaweedFS config, S3 interactions, IAM config, topology parsing, and controller helper logic. 

**envtest**

```bash
make test
```

Real Kubernetes API server + etcd, but SeaweedFS itself stays out of the picture. Useful for CRD behavior, status updates, finalizers, owner references, resource reconciliation, admission/schema behavior. Doesn't replace real SeaweedFS integration testing.

**End-to-end**

```bash
make test-e2e
```

Uses kind plus a real SeaweedFS deployment. Meant to validate cluster creation, S3 readiness, bucket/user creation, auth, authorization, scaling, pod recovery, drift correction, upgrade ordering, deletion, and PVC retention.

If the e2e environment or images aren't available in your setup, treat those claims as unverified until you can actually run it.

## How it works

**Controllers**: separate reconciliation paths for `ObjectStorageCluster`, `ObjectStorageBucket`, and `ObjectStorageUser`. The cluster reconciler owns the SeaweedFS infrastructure; the bucket and user reconcilers wait for the cluster to be ready before touching the S3 API.

**Kubernetes resources**: the cluster controller generates StatefulSets, Deployments, Services, PVCs, Secrets, ConfigMaps, and PDBs. Stateful components (masters, volumes, filers) use StatefulSets because stable identity and persistent storage matter for SeaweedFS. The S3 gateway is stateless so it just uses a Deployment.

**Master topology**: SeaweedFS masters use Raft, so replica counts should be odd. Stable identities and headless Services let masters find each other.

**Volume servers**: own the actual stored data. More volume servers means more capacity and throughput. Persistent volumes let a rescheduled server pick back up where it left off.

**Filers**: provide the namespace layer. Default config is fine for simple deployments, but multi-replica filers need a proper shared metadata backend, that's an operational decision, not something the operator solves for you automatically.

**S3 gateway**: exposes SeaweedFS over the S3 protocol. Bucket controller uses this for bucket ops; user management talks to SeaweedFS IAM config. Exact behavior around IAM propagation and gateway reloads depends on the SeaweedFS version in use.

## Reconciliation and idempotency

The operator is meant to be idempotent. General loop:

```
Desired state -> read current Kubernetes state -> calculate diff
-> create/update/delete resources -> update status
```

Resource builders are designed to make object generation predictable. 

**Status and observed generation**: readiness during updates is sensitive to stale status. There's generation-aware readiness logic meant to stop an old StatefulSet status from being mistaken for proof that a new rollout finished. Matters a lot during ordered upgrades.

## Failure handling

| Failure                         | Intended behavior               |
| ------------------------------- | ------------------------------- |
| Missing dependency              | Wait or report degraded         |
| Invalid master replica count    | Reject / report degraded        |
| Storage shrink                  | Reject                          |
| Missing StorageClass            | Remain pending                  |
| S3 unavailable                  | Retry                           |
| Filer unavailable               | Retry                           |
| Invalid credentials             | Report failure                  |
| Cluster deleted with dependents | Block deletion                  |
| Storage expansion unsupported   | Report failure                  |
| Resource drift                  | Reconcile back to desired state |


## Security considerations

Credentials go in Kubernetes Secrets, not directly in custom resources. Credential-bearing resources are kept namespace-scoped where possible. Bucket connection info is separated from credentials so a workload can get endpoint info without automatically getting access keys.

The controller needs its own RBAC permissions to manage the resources it creates, review those before putting this into production. SeaweedFS admin credentials shouldn't be handed to regular workloads.

## Troubleshooting

**Cluster stuck in Creating**

```bash
kubectl -n storage describe objectstoragecluster <name>
kubectl -n storage get pods
kubectl -n storage get pvc
```

Check PVC provisioning, StorageClass, image pulls, pod events, master readiness.

**S3Ready is false**

```bash
kubectl -n storage get objectstoragecluster <name> -o yaml
kubectl -n storage get pods
kubectl -n storage logs <cluster>-filer-0
kubectl -n storage logs deploy/<cluster>-s3
```

The gateway depends on the rest of the topology being up first.

**Bucket stuck waiting for the cluster**

```bash
kubectl -n storage get objectstoragecluster <name> -o yaml
```

The bucket controller shouldn't attempt S3 operations until the cluster is ready enough.

**Storage expansion does nothing**

```bash
kubectl get storageclass
```

Confirm `allowVolumeExpansion: true`, then check the PVC:

```bash
kubectl -n storage get pvc
kubectl -n storage describe pvc <name>
```

**Masters can't form a quorum**

```bash
kubectl -n storage get pods
kubectl -n storage get svc
kubectl -n storage describe pod <master-pod>
```

Make sure the master replica count is odd and masters can resolve each other through the headless Service.

**User credentials don't work**

```bash
kubectl -n storage get secret <user>-s3-credentials
```

Then check SeaweedFS IAM config through the filer/S3 interfaces. Don't paste actual secret values while debugging.

## Project layout

```
api/v1alpha1/       CRD types, conditions, API definitions
cmd/                controller entrypoint
internal/
  controller/
    cluster reconciliation
    bucket reconciliation
    user reconciliation
  resources/         Kubernetes resource builders
  seaweed/
    filer client
    master client
    S3 client
    IAM configuration
  metrics/            Prometheus metrics
  testutil/           test infrastructure
config/
  crd/
  rbac/
  manager/
  samples/
charts/
  seaweedfs-operator/
test/
  e2e/
```

## Development

After changing API types:

```bash
make manifests generate
```

Run tests before submitting changes:

```bash
go test ./...
```

See other Make targets with:

```bash
make help
```
