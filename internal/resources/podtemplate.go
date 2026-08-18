package resources

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

const DataVolumeName = "data"

const DataMountPath = "/data"

type podTemplateOptions struct {
	cluster   *objectstoragev1alpha1.ObjectStorageCluster
	component Component
	overrides objectstoragev1alpha1.PodOverrides

	args               []string
	httpPort           int32
	probePath          string
	ports              []corev1.ContainerPort
	mountData          bool
	extraVolumes       []corev1.Volume
	extraMounts        []corev1.VolumeMount
	initContainers     []corev1.Container
	annotations        map[string]string
	serviceAccountName string
}

func buildPodTemplate(o podTemplateOptions) corev1.PodTemplateSpec {
	cluster := o.cluster
	selector := SelectorLabels(cluster, o.component)
	labels := mergeLabels(Labels(cluster, o.component), o.overrides.PodLabels)

	annotations := map[string]string{}
	for k, v := range o.annotations {
		annotations[k] = v
	}
	if cluster.Spec.Metrics {
		annotations["prometheus.io/scrape"] = "true"
		annotations["prometheus.io/port"] = strconv.Itoa(MetricsPort)
		annotations["prometheus.io/path"] = "/metrics"
	}
	annotations = mergeAnnotations(annotations, o.overrides.PodAnnotations)

	env := []corev1.EnvVar{
		{
			Name:      "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		},
		{
			Name:      "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
		},
		{
			Name:      "POD_IP",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
		},
		{
			Name:      "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
		},
	}
	env = append(env, o.overrides.ExtraEnv...)

	mounts := append([]corev1.VolumeMount{}, o.extraMounts...)
	if o.mountData {
		mounts = append(mounts, corev1.VolumeMount{Name: DataVolumeName, MountPath: DataMountPath})
	}

	ports := append([]corev1.ContainerPort{}, o.ports...)
	if cluster.Spec.Metrics {
		ports = append(ports, corev1.ContainerPort{Name: "metrics", ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP})
	}

	container := corev1.Container{
		Name:            string(o.component),
		Image:           Image(cluster),
		ImagePullPolicy: corev1.PullPolicy(cluster.Spec.ImagePullPolicy),
		Command:         []string{"/usr/bin/weed"},
		Args:            append(o.args, o.overrides.ExtraArgs...),
		Ports:           ports,
		Env:             env,
		VolumeMounts:    mounts,
		Resources:       o.overrides.Resources,
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: o.probePath,
					Port: intstr.FromInt32(o.httpPort),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
			FailureThreshold:    60,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: o.probePath,
					Port: intstr.FromInt32(o.httpPort),
				},
			},
			PeriodSeconds:    10,
			TimeoutSeconds:   3,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: o.probePath,
					Port: intstr.FromInt32(o.httpPort),
				},
			},
			PeriodSeconds:    20,
			TimeoutSeconds:   5,
			FailureThreshold: 6,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}

	pullSecrets := make([]corev1.LocalObjectReference, 0, len(cluster.Spec.ImagePullSecrets))
	for _, s := range cluster.Spec.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: s.Name})
	}

	affinity := o.overrides.Affinity
	if affinity == nil {
		affinity = defaultAntiAffinity(selector)
	}

	grace := o.overrides.TerminationGracePeriodSeconds
	if grace == nil {
		grace = ptr.To(int64(60))
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            o.serviceAccountName,
			InitContainers:                o.initContainers,
			Containers:                    []corev1.Container{container},
			Volumes:                       o.extraVolumes,
			ImagePullSecrets:              pullSecrets,
			NodeSelector:                  o.overrides.NodeSelector,
			Tolerations:                   o.overrides.Tolerations,
			Affinity:                      affinity,
			TopologySpreadConstraints:     o.overrides.TopologySpreadConstraints,
			PriorityClassName:             o.overrides.PriorityClassName,
			TerminationGracePeriodSeconds: grace,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(false),
				FSGroup:      ptr.To(int64(0)),
			},
		},
	}
}

func defaultAntiAffinity(selector map[string]string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchLabels: selector},
						TopologyKey:   "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

func dataVolumeClaim(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component, storage objectstoragev1alpha1.StorageSpec) corev1.PersistentVolumeClaim {
	accessModes := storage.AccessModes
	if len(accessModes) == 0 {
		accessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   DataVolumeName,
			Labels: Labels(cluster, c),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			StorageClassName: storage.StorageClassName,
			VolumeMode:       storage.VolumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storage.Size,
				},
			},
		},
	}
}

func statefulSetSkeleton(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(cluster, c),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, c),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(replicas),
			ServiceName:         HeadlessServiceName(cluster, c),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: SelectorLabels(cluster, c)},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}

func flag(name string, value any) string {
	return fmt.Sprintf("-%s=%v", name, value)
}
