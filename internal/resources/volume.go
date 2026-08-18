package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

const topologyInitContainerName = "topology"

const (
	topologyVolumeName = "topology"
	topologyMountPath  = "/etc/seaweedfs/topology"
)

func VolumeArgs(cluster *objectstoragev1alpha1.ObjectStorageCluster) []string {
	v := cluster.Spec.Volume
	args := []string{
		"volume",
		flag("ip", fmt.Sprintf("$(POD_NAME).%s.%s.svc.cluster.local", HeadlessServiceName(cluster, ComponentVolume), cluster.Namespace)),
		flag("ip.bind", "0.0.0.0"),
		flag("port", VolumeHTTPPort),
		flag("dir", DataMountPath),
		flag("mserver", MasterEndpoint(cluster)),
		flag("index", v.Index),
		flag("max", v.MaxVolumeCounts),
	}
	if v.CompactionMBps > 0 {
		args = append(args, flag("compactionMBps", v.CompactionMBps))
	}
	if v.TopologyFromNodeLabels == nil {
		if v.DataCenter != "" {
			args = append(args, flag("dataCenter", v.DataCenter))
		}
		if v.Rack != "" {
			args = append(args, flag("rack", v.Rack))
		}
	} else {
		args = append(args,
			flag("dataCenter", "$(SEAWEED_DATACENTER)"),
			flag("rack", "$(SEAWEED_RACK)"),
		)
	}
	if cluster.Spec.Metrics {
		args = append(args, flag("metricsPort", MetricsPort))
	}
	return args
}

func VolumeStatefulSet(cluster *objectstoragev1alpha1.ObjectStorageCluster) *appsv1.StatefulSet {
	sts := statefulSetSkeleton(cluster, ComponentVolume, cluster.Spec.Volume.Replicas)

	opts := podTemplateOptions{
		cluster:   cluster,
		component: ComponentVolume,
		overrides: cluster.Spec.Volume.PodOverrides,
		args:      VolumeArgs(cluster),
		httpPort:  VolumeHTTPPort,
		probePath: "/status",
		ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: VolumeHTTPPort, Protocol: corev1.ProtocolTCP},
			{Name: "grpc", ContainerPort: VolumeGRPCPort, Protocol: corev1.ProtocolTCP},
		},
		mountData: true,
	}

	if t := cluster.Spec.Volume.TopologyFromNodeLabels; t != nil {
		opts.initContainers = []corev1.Container{volumeTopologyInitContainer(cluster, *t)}
		opts.extraVolumes = append(opts.extraVolumes, corev1.Volume{
			Name:         topologyVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		opts.extraMounts = append(opts.extraMounts, corev1.VolumeMount{
			Name:      topologyVolumeName,
			MountPath: topologyMountPath,
		})
	}

	sts.Spec.Template = buildPodTemplate(opts)

	if cluster.Spec.Volume.TopologyFromNodeLabels != nil {
		c := &sts.Spec.Template.Spec.Containers[0]
		joined := shellJoin(append([]string{"/usr/bin/weed"}, c.Args...))
		c.Command = []string{"/bin/sh", "-c"}
		c.Args = []string{fmt.Sprintf(". %s/topology.env && exec %s", topologyMountPath, joined)}
	}

	sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
		dataVolumeClaim(cluster, ComponentVolume, cluster.Spec.Volume.Storage),
	}
	return sts
}

func volumeTopologyInitContainer(cluster *objectstoragev1alpha1.ObjectStorageCluster, t objectstoragev1alpha1.TopologyFromNodeLabels) corev1.Container {
	dcLabel := t.DataCenterLabel
	if dcLabel == "" {
		dcLabel = "topology.kubernetes.io/region"
	}
	rackLabel := t.RackLabel
	if rackLabel == "" {
		rackLabel = "topology.kubernetes.io/zone"
	}

	script := fmt.Sprintf(`set -eu
API=https:
TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
CA=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
NODE_JSON=$(wget -q -O - --ca-certificate=$CA --header="Authorization: Bearer $TOKEN" "$API/api/v1/nodes/$NODE_NAME" || echo '{}')
extract() {
}
DC=$(extract %q)
RACK=$(extract %q)
[ -z "$DC" ] && DC=default
[ -z "$RACK" ] && RACK=default
echo "SEAWEED_DATACENTER=$DC" > %s/topology.env
echo "SEAWEED_RACK=$RACK" >> %s/topology.env
echo "resolved topology dataCenter=$DC rack=$RACK"
`, dcLabel, rackLabel, topologyMountPath, topologyMountPath)

	return corev1.Container{
		Name:            topologyInitContainerName,
		Image:           Image(cluster),
		ImagePullPolicy: corev1.PullPolicy(cluster.Spec.ImagePullPolicy),
		Command:         []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{{
			Name:      "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
		}},
		VolumeMounts: []corev1.VolumeMount{{Name: topologyVolumeName, MountPath: topologyMountPath}},
	}
}

func VolumeHeadlessService(cluster *objectstoragev1alpha1.ObjectStorageCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadlessServiceName(cluster, ComponentVolume),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentVolume),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 SelectorLabels(cluster, ComponentVolume),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: VolumeHTTPPort, TargetPort: intstr.FromInt32(VolumeHTTPPort)},
				{Name: "grpc", Port: VolumeGRPCPort, TargetPort: intstr.FromInt32(VolumeGRPCPort)},
			},
		},
	}
}

func VolumeClientService(cluster *objectstoragev1alpha1.ObjectStorageCluster) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClientServiceName(cluster, ComponentVolume),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentVolume),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SelectorLabels(cluster, ComponentVolume),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: VolumeHTTPPort, TargetPort: intstr.FromInt32(VolumeHTTPPort)},
				{Name: "grpc", Port: VolumeGRPCPort, TargetPort: intstr.FromInt32(VolumeGRPCPort)},
			},
		},
	}
}

func shellJoin(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += shellQuote(a)
	}
	return out
}

func shellQuote(s string) string {
	needsQuote := s == ""
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	return `"` + s + `"`
}
