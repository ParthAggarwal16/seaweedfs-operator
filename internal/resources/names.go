package resources

import (
	"fmt"
	"strings"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

type Component string

const (
	ComponentMaster Component = "master"
	ComponentVolume Component = "volume"
	ComponentFiler  Component = "filer"
	ComponentS3     Component = "s3"
)

var UpgradeOrder = []Component{ComponentMaster, ComponentVolume, ComponentFiler, ComponentS3}

const (
	MasterHTTPPort = 9333
	MasterGRPCPort = 19333
	VolumeHTTPPort = 8080
	VolumeGRPCPort = 18080
	FilerHTTPPort  = 8888
	FilerGRPCPort  = 18888
	S3Port         = 8333
	MetricsPort    = 9327
)

const DefaultImageRepository = "chrislusf/seaweedfs"

const IAMConfigPath = "/etc/iam/identity.json"

const BucketsRoot = "/buckets"

func Name(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component) string {
	return fmt.Sprintf("%s-%s", cluster.Name, c)
}

func HeadlessServiceName(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component) string {
	return Name(cluster, c)
}

func ClientServiceName(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component) string {
	return fmt.Sprintf("%s-%s-client", cluster.Name, c)
}

func FilerConfigMapName(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	return fmt.Sprintf("%s-filer-config", cluster.Name)
}

func AdminSecretName(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	if cluster.Spec.S3.AdminSecretName != "" {
		return cluster.Spec.S3.AdminSecretName
	}
	return fmt.Sprintf("%s-s3-admin", cluster.Name)
}

func PodFQDN(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component, ordinal int32) string {
	return fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
		Name(cluster, c), ordinal, HeadlessServiceName(cluster, c), cluster.Namespace)
}

func ServiceFQDN(cluster *objectstoragev1alpha1.ObjectStorageCluster, c Component) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", ClientServiceName(cluster, c), cluster.Namespace)
}

func MasterPeers(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	peers := make([]string, 0, cluster.Spec.Master.Replicas)
	for i := int32(0); i < cluster.Spec.Master.Replicas; i++ {
		peers = append(peers, fmt.Sprintf("%s:%d", PodFQDN(cluster, ComponentMaster, i), MasterHTTPPort))
	}
	return strings.Join(peers, ",")
}

func MasterEndpoint(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	return MasterPeers(cluster)
}

func FilerEndpoint(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	return fmt.Sprintf("%s:%d", ServiceFQDN(cluster, ComponentFiler), FilerHTTPPort)
}

func S3Endpoint(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	return fmt.Sprintf("http:
}

func Image(cluster *objectstoragev1alpha1.ObjectStorageCluster) string {
	if cluster.Spec.Image != "" {
		return cluster.Spec.Image
	}
	return fmt.Sprintf("%s:%s", DefaultImageRepository, cluster.Spec.Version)
}
