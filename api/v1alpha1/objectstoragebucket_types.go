package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type BucketPhase string

const (
	BucketPhasePending  BucketPhase = "Pending"
	BucketPhaseReady    BucketPhase = "Ready"
	BucketPhaseFailed   BucketPhase = "Failed"
	BucketPhaseDeleting BucketPhase = "Deleting"
)

type BucketQuota struct {
	SizeGiB int64 `json:"sizeGiB"`
	Enforce bool  `json:"enforce,omitempty"`
}

type ObjectStorageBucketSpec struct {
	ClusterRef     ClusterReference `json:"clusterRef"`
	BucketName     string           `json:"bucketName,omitempty"`
	DeletionPolicy DeletionPolicy   `json:"deletionPolicy,omitempty"`

	Quota                *BucketQuota `json:"quota,omitempty"`
	ObjectLockEnabled    bool         `json:"objectLockEnabled,omitempty"`
	ConnectionSecretName string       `json:"connectionSecretName,omitempty"`
}

type ObjectStorageBucketStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase BucketPhase `json:"phase,omitempty"`

	Conditions   []metav1.Condition `json:"conditions,omitempty"`
	BucketName   string             `json:"bucketName,omitempty"`
	Endpoint     string             `json:"endpoint,omitempty"`
	CreationTime *metav1.Time       `json:"creationTime,omitempty"`

	ConnectionSecretRef string `json:"connectionSecretRef,omitempty"`
}

type ObjectStorageBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageBucketSpec   `json:"spec,omitempty"`
	Status ObjectStorageBucketStatus `json:"status,omitempty"`
}

func (b *ObjectStorageBucket) EffectiveBucketName() string {
	if b.Spec.BucketName != "" {
		return b.Spec.BucketName
	}
	return b.Name
}

type ObjectStorageBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStorageBucket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ObjectStorageBucket{}, &ObjectStorageBucketList{})
}
