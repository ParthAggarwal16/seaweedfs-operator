package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

func defaultMasterStorage() objectstoragev1alpha1.StorageSpec {
	return objectstoragev1alpha1.StorageSpec{
		Size:        resource.MustParse("1Gi"),
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
}

func MasterStorage(cluster *objectstoragev1alpha1.ObjectStorageCluster) objectstoragev1alpha1.StorageSpec {
	if cluster.Spec.Master.Storage != nil {
		return *cluster.Spec.Master.Storage
	}
	return defaultMasterStorage()
}

func MasterArgs(cluster *objectstoragev1alpha1.ObjectStorageCluster) []string {
	m := cluster.Spec.Master
	args := []string{
		"master",
		flag("ip", fmt.Sprintf("$(POD_NAME).%s.%s.svc.cluster.local", HeadlessServiceName(cluster, ComponentMaster), cluster.Namespace)),
		flag("ip.bind", "0.0.0.0"),
		flag("port", MasterHTTPPort),
		flag("mdir", DataMountPath),
		flag("defaultReplication", cluster.Spec.DefaultReplication),
		flag("volumeSizeLimitMB", m.VolumeSizeLimitMB),
		flag("garbageThreshold", m.GarbageThreshold),
	}
	if m.Replicas > 1 {
		args = append(args, flag("peers", MasterPeers(cluster)))
	}
	if m.VolumePreallocate {
		args = append(args, "-volumePreallocate")
	}
	if cluster.Spec.Metrics {
		args = append(args, flag("metricsPort", MetricsPort))
	}
	return args
}

func MasterStatefulSet(cluster *objectstoragev1alpha1.ObjectStorageCluster) *appsv1.StatefulSet {
	sts := statefulSetSkeleton(cluster, ComponentMaster, cluster.Spec.Master.Replicas)

	sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement

	sts.Spec.Template = buildPodTemplate(podTemplateOptions{
		cluster:   cluster,
		component: ComponentMaster,
		overrides: cluster.Spec.Master.PodOverrides,
		args:      MasterArgs(cluster),
		httpPort:  MasterHTTPPort,
		probePath: "/cluster/healthz",
		ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: MasterHTTPPort, Protocol: corev1.ProtocolTCP},
			{Name: "grpc", ContainerPort: MasterGRPCPort, Protocol: corev1.ProtocolTCP},
		},
		mountData: true,
	})

	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
		dataVolumeClaim(cluster, ComponentMaster, MasterStorage(cluster)),
	}
	return sts
}

func MasterHeadlessService(cluster *objectstoragev1alpha1.ObjectStorageCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadlessServiceName(cluster, ComponentMaster),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentMaster),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 SelectorLabels(cluster, ComponentMaster),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: MasterHTTPPort, TargetPort: intstr.FromInt32(MasterHTTPPort)},
				{Name: "grpc", Port: MasterGRPCPort, TargetPort: intstr.FromInt32(MasterGRPCPort)},
			},
		},
	}
}

func MasterClientService(cluster *objectstoragev1alpha1.ObjectStorageCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClientServiceName(cluster, ComponentMaster),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentMaster),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SelectorLabels(cluster, ComponentMaster),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: MasterHTTPPort, TargetPort: intstr.FromInt32(MasterHTTPPort)},
				{Name: "grpc", Port: MasterGRPCPort, TargetPort: intstr.FromInt32(MasterGRPCPort)},
			},
		},
	}
}

func MasterPodDisruptionBudget(cluster *objectstoragev1alpha1.ObjectStorageCluster) *policyv1.PodDisruptionBudget {
	if cluster.Spec.Master.Replicas < 3 {
		return nil
	}
	quorum := intstr.FromInt32(cluster.Spec.Master.Replicas/2 + 1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(cluster, ComponentMaster),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentMaster),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &quorum,
			Selector:     &metav1.LabelSelector{MatchLabels: SelectorLabels(cluster, ComponentMaster)},
		},
	}
}
