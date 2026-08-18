package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type UserPhase string

const (
	UserPhasePending  UserPhase = "Pending"
	UserPhaseReady    UserPhase = "Ready"
	UserPhaseFailed   UserPhase = "Failed"
	UserPhaseDeleting UserPhase = "Deleting"
)

type S3Action string

const (
	S3ActionRead     S3Action = "Read"
	S3ActionWrite    S3Action = "Write"
	S3ActionList     S3Action = "List"
	S3ActionTagging  S3Action = "Tagging"
	S3ActionAdmin    S3Action = "Admin"
	S3ActionReadAcp  S3Action = "ReadAcp"
	S3ActionWriteAcp S3Action = "WriteAcp"
)

type BucketGrant struct {
	BucketName string                `json:"bucketName"`
	BucketRef  *LocalSecretReference `json:"bucketRef,omitempty"`

	Actions []S3Action `json:"actions"`

	Prefix string `json:"prefix,omitempty"`
}

type ObjectStorageUserSpec struct {
	ClusterRef   ClusterReference `json:"clusterRef"`
	IdentityName string           `json:"identityName,omitempty"`
	BucketGrants []BucketGrant    `json:"bucketGrants,omitempty"`

	ClusterActions []S3Action `json:"clusterActions,omitempty"`

	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

type ObjectStorageUserStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              UserPhase          `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	IdentityName string `json:"identityName,omitempty"`

	SecretRef string `json:"secretRef,omitempty"`

	AccessKeyID string `json:"accessKeyID,omitempty"`

	Endpoint string `json:"endpoint,omitempty"`

	GrantedBuckets []string `json:"grantedBuckets,omitempty"`
}

type ObjectStorageUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStorageUserSpec   `json:"spec,omitempty"`
	Status ObjectStorageUserStatus `json:"status,omitempty"`
}

func (u *ObjectStorageUser) EffectiveIdentityName() string {
	if u.Spec.IdentityName != "" {
		return u.Spec.IdentityName
	}
	return u.Namespace + "-" + u.Name
}

func (u *ObjectStorageUser) EffectiveSecretName() string {
	if u.Spec.SecretName != "" {
		return u.Spec.SecretName
	}
	return u.Name + "-s3-credentials"
}

type ObjectStorageUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStorageUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ObjectStorageUser{}, &ObjectStorageUserList{})
}
