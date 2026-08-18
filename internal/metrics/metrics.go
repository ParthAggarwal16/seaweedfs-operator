package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "seaweedfs_operator"

var (
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_total",
		Help:      "Total reconciles by kind and outcome.",
	}, []string{"kind", "outcome"})

	ReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "reconcile_duration_seconds",
		Help:      "Reconcile duration by kind.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
	}, []string{"kind"})

	ClusterPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cluster_phase",
		Help:      "1 for the cluster's current phase, 0 otherwise.",
	}, []string{"namespace", "cluster", "phase"})

	ComponentReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "component_replicas",
		Help:      "Replica counts per SeaweedFS component and state.",
	}, []string{"namespace", "cluster", "component", "state"})

	ClusterCapacityBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cluster_provisioned_capacity_bytes",
		Help:      "Sum of the volume server PVC requests.",
	}, []string{"namespace", "cluster"})

	ClusterFreeVolumeSlots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "cluster_free_volume_slots",
		Help:      "Free volume slots reported by the SeaweedFS master.",
	}, []string{"namespace", "cluster"})

	BucketsManaged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "buckets_managed",
		Help:      "Managed ObjectStorageBucket objects by phase.",
	}, []string{"namespace", "cluster", "phase"})

	UsersManaged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "users_managed",
		Help:      "Managed ObjectStorageUser objects by phase.",
	}, []string{"namespace", "cluster", "phase"})

	StorageAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "storage_api_errors_total",
		Help:      "Errors talking to the SeaweedFS control plane, by surface and operation.",
	}, []string{"surface", "operation"})

	UpgradesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "upgrades_total",
		Help:      "Completed SeaweedFS version rollouts.",
	}, []string{"namespace", "cluster"})
)

var AllPhases = []string{"Pending", "Creating", "Running", "Scaling", "Upgrading", "Degraded", "Deleting"}

func SetClusterPhase(namespaceName, cluster, phase string) {
	for _, p := range AllPhases {
		v := 0.0
		if p == phase {
			v = 1.0
		}
		ClusterPhase.WithLabelValues(namespaceName, cluster, p).Set(v)
	}
}

func ForgetCluster(namespaceName, cluster string) {
	for _, p := range AllPhases {
		ClusterPhase.DeleteLabelValues(namespaceName, cluster, p)
	}
	ComponentReplicas.DeletePartialMatch(prometheus.Labels{"namespace": namespaceName, "cluster": cluster})
	BucketsManaged.DeletePartialMatch(prometheus.Labels{"namespace": namespaceName, "cluster": cluster})
	UsersManaged.DeletePartialMatch(prometheus.Labels{"namespace": namespaceName, "cluster": cluster})
	ClusterCapacityBytes.DeleteLabelValues(namespaceName, cluster)
	ClusterFreeVolumeSlots.DeleteLabelValues(namespaceName, cluster)
}

func init() {
	metrics.Registry.MustRegister(
		ReconcileTotal,
		ReconcileDuration,
		ClusterPhase,
		ComponentReplicas,
		ClusterCapacityBytes,
		ClusterFreeVolumeSlots,
		BucketsManaged,
		UsersManaged,
		StorageAPIErrors,
		UpgradesTotal,
	)
}
