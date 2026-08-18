package resources

import (
	"crypto/sha256"
	"encoding/hex"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

func S3Args(cluster *objectstoragev1alpha1.ObjectStorageCluster) []string {
	s := cluster.Spec.S3
	args := []string{
		"s3",
		flag("port", S3Port),
		flag("filer", FilerEndpoint(cluster)),
	}
	if s.DomainName != "" {
		args = append(args, flag("domainName", s.DomainName))
	}
	if s.AllowEmptyFolder {
		args = append(args, flag("allowEmptyFolder", "true"))
	} else {
		args = append(args, flag("allowEmptyFolder", "false"))
	}
	if cluster.Spec.Metrics {
		args = append(args, flag("metricsPort", MetricsPort))
	}
	return args
}

func S3Deployment(cluster *objectstoragev1alpha1.ObjectStorageCluster) *appsv1.Deployment {
	template := buildPodTemplate(podTemplateOptions{
		cluster:   cluster,
		component: ComponentS3,
		overrides: cluster.Spec.S3.PodOverrides,
		args:      S3Args(cluster),
		httpPort:  S3Port,
		probePath: "/status",
		ports: []corev1.ContainerPort{
			{Name: "s3", ContainerPort: S3Port, Protocol: corev1.ProtocolTCP},
		},
	})

	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      Name(cluster, ComponentS3),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentS3),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(cluster.Spec.S3.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: SelectorLabels(cluster, ComponentS3)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Template: template,
		},
	}
}

func S3Service(cluster *objectstoragev1alpha1.ObjectStorageCluster) *corev1.Service {
	svcSpec := cluster.Spec.S3.Service
	serviceType := svcSpec.Type
	if serviceType == "" {
		serviceType = corev1.ServiceTypeClusterIP
	}

	port := corev1.ServicePort{
		Name:       "s3",
		Port:       S3Port,
		TargetPort: intstr.FromInt32(S3Port),
		Protocol:   corev1.ProtocolTCP,
	}
	if serviceType == corev1.ServiceTypeNodePort && svcSpec.NodePort != 0 {
		port.NodePort = svcSpec.NodePort
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ClientServiceName(cluster, ComponentS3),
			Namespace:   cluster.Namespace,
			Labels:      Labels(cluster, ComponentS3),
			Annotations: svcSpec.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                     serviceType,
			Selector:                 SelectorLabels(cluster, ComponentS3),
			Ports:                    []corev1.ServicePort{port},
			LoadBalancerSourceRanges: svcSpec.LoadBalancerSourceRanges,
		},
	}
}

func AdminSecret(cluster *objectstoragev1alpha1.ObjectStorageCluster, accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AdminSecretName(cluster),
			Namespace: cluster.Namespace,
			Labels:    Labels(cluster, ComponentS3),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"accessKeyID":           accessKey,
			"secretAccessKey":       secretKey,
			"endpoint":              S3Endpoint(cluster),
			"AWS_ACCESS_KEY_ID":     accessKey,
			"AWS_SECRET_ACCESS_KEY": secretKey,
			"AWS_ENDPOINT_URL":      S3Endpoint(cluster),
			"AWS_REGION":            DefaultRegion,
		},
	}
}

const DefaultRegion = "us-east-1"

func UserSecret(user *objectstoragev1alpha1.ObjectStorageUser, endpoint, accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      user.EffectiveSecretName(),
			Namespace: user.Namespace,
			Labels: map[string]string{
				LabelName:      AppName,
				LabelManagedBy: ManagedByValue,
				LabelInstance:  user.Spec.ClusterRef.Name,
				LabelComponent: "s3-user",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"accessKeyID":           accessKey,
			"secretAccessKey":       secretKey,
			"endpoint":              endpoint,
			"region":                DefaultRegion,
			"AWS_ACCESS_KEY_ID":     accessKey,
			"AWS_SECRET_ACCESS_KEY": secretKey,
			"AWS_ENDPOINT_URL":      endpoint,
			"AWS_REGION":            DefaultRegion,
		},
	}
}

func BucketConnectionSecret(bucket *objectstoragev1alpha1.ObjectStorageBucket, endpoint string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bucket.Spec.ConnectionSecretName,
			Namespace: bucket.Namespace,
			Labels: map[string]string{
				LabelName:      AppName,
				LabelManagedBy: ManagedByValue,
				LabelInstance:  bucket.Spec.ClusterRef.Name,
				LabelComponent: "s3-bucket",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"bucketName":       bucket.EffectiveBucketName(),
			"endpoint":         endpoint,
			"region":           DefaultRegion,
			"S3_BUCKET":        bucket.EffectiveBucketName(),
			"AWS_ENDPOINT_URL": endpoint,
			"AWS_REGION":       DefaultRegion,
		},
	}
}

func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
