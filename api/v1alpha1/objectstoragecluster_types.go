package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ClusterPhase string

const (
	ClusterPhasePending   ClusterPhase = "Pending"
	ClusterPhaseCreating  ClusterPhase = "Creating"
	ClusterPhaseRunning   ClusterPhase = "Running"
	ClusterPhaseScaling   ClusterPhase = "Scaling"
	ClusterPhaseUpgrading ClusterPhase = "Upgrading"
	ClusterPhaseDegraded  ClusterPhase = "Degraded"
	ClusterPhaseDeleting  ClusterPhase = "Deleting"
)

type MasterSpec struct {
	Replicas          int32        `json:"replicas"`
	Storage           *StorageSpec `json:"storage,omitempty"`
	VolumeSizeLimitMB int32        `json:"volumeSizeLimitMB,omitempty"`
	VolumePreallocate bool         `json:"volumePreallocate,omitempty"`

	GarbageThreshold string `json:"garbageThreshold,omitempty"`

	PodOverrides `json:",inline"`
}

type VolumeSpec struct {
	Replicas int32       `json:"replicas"`
	Storage  StorageSpec `json:"storage"`

	MaxVolumeCounts int32 `json:"maxVolumeCounts,omitempty"`

	CompactionMBps int32  `json:"compactionMBps,omitempty"`
	DataCenter     string `json:"dataCenter,omitempty"`
	Rack           string `json:"rack,omitempty"`

	TopologyFromNodeLabels *TopologyFromNodeLabels `json:"topologyFromNodeLabels,omitempty"`

	Index        string `json:"index,omitempty"`
	PodOverrides `json:",inline"`
}

type TopologyFromNodeLabels struct {
	DataCenterLabel string `json:"dataCenterLabel,omitempty"`
	RackLabel       string `json:"rackLabel,omitempty"`
}

type FilerSpec struct {
	Replicas      int32        `json:"replicas"`
	Storage       *StorageSpec `json:"storage,omitempty"`
	ConfigMapName string       `json:"configMapName,omitempty"`

	MaxMB int32 `json:"maxMB,omitempty"`

	PodOverrides `json:",inline"`
}

type S3Spec struct {
	Enabled          bool        `json:"enabled"`
	Replicas         int32       `json:"replicas"`
	Service          ServiceSpec `json:"service,omitempty"`
	DomainName       string      `json:"domainName,omitempty"`
	AllowEmptyFolder bool        `json:"allowEmptyFolder,omitempty"`

	AdminSecretName string `json:"adminSecretName,omitempty"`
	PodOverrides    `json:",inline"`
}

type UpgradePolicy struct {
	Strategy string `json:"strategy,omitempty"`

	Paused bool `json:"paused,omitempty"`
}

type ObjectStorageClusterSpec struct {
	Image           string `json:"image,omitempty"`
	Version         string `json:"version"`
	ImagePullPolicy string `json:"imagePullPolicy,omitempty"`

	ImagePullSecrets   []LocalSecretReference `json:"imagePullSecrets,omitempty"`
	DefaultReplication string                 `json:"defaultReplication,omitempty"`

	Master MasterSpec `json:"master"`
	Volume VolumeSpec `json:"volume"`
	Filer  FilerSpec  `json:"filer"`

	S3 S3Spec `json:"s3,omitempty"`

	Upgrade UpgradePolicy `json:"upgrade,omitempty"`

	Metrics bool `json:"metrics,omitempty"`
}

type ObjectStorageClusterStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase      ClusterPhase       `json:"phase,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	Master     ComponentStatus    `json:"master,omitempty"`
	Volume     ComponentStatus    `json:"volume,omitempty"`
	Filer      ComponentStatus    `json:"filer,omitempty"`

	S3 ComponentStatus `json:"s3,omitempty"`

	MasterEndpoint string `json:"masterEndpoint,omitempty"`
	FilerEndpoint  string `json:"filerEndpoint,omitempty"`
	S3Endpoint     string `json:"s3Endpoint,omitempty"`

	AdminSecretName string `json:"adminSecretName,omitempty"`
	CurrentVersion  string `json:"currentVersion,omitempty"`

	ProvisionedCapacity string `json:"provisionedCapacity,omitempty"`

	Topology *TopologyStatus `json:"topology,omitempty"`
}

type TopologyStatus struct {
	DataCenters   int32        `json:"dataCenters"`
	Racks         int32        `json:"racks"`
	VolumeServers int32        `json:"volumeServers"`
	ActiveVolumes int32        `json:"activeVolumes"`
	MaxVolumes    int32        `json:"maxVolumes"`
	FreeVolumes   int32        `json:"freeVolumes"`
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

type ObjectStorageCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageClusterSpec   `json:"spec,omitempty"`
	Status ObjectStorageClusterStatus `json:"status,omitempty"`
}

type ObjectStorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStorageCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ObjectStorageCluster{}, &ObjectStorageClusterList{})
}
