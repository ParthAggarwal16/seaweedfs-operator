package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

type PodOverrides struct {
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	PodLabels map[string]string `json:"podLabels,omitempty"`

	PriorityClassName string `json:"priorityClassName,omitempty"`

	ExtraArgs []string `json:"extraArgs,omitempty"`

	ExtraEnv                      []corev1.EnvVar `json:"extraEnv,omitempty"`
	TerminationGracePeriodSeconds *int64          `json:"terminationGracePeriodSeconds,omitempty"`
}

type StorageSpec struct {
	Size resource.Quantity `json:"size"`

	StorageClassName *string `json:"storageClassName,omitempty"`

	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	VolumeMode *corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`
}

type ServiceSpec struct {
	Type corev1.ServiceType `json:"type,omitempty"`

	Annotations map[string]string `json:"annotations,omitempty"`

	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	NodePort int32 `json:"nodePort,omitempty"`
}

type LocalSecretReference struct {
	Name string `json:"name"`
}

type ClusterReference struct {
	Name string `json:"name"`
}

type ComponentStatus struct {
	DesiredReplicas int32  `json:"desiredReplicas"`
	ReadyReplicas   int32  `json:"readyReplicas"`
	CurrentReplicas int32  `json:"currentReplicas"`
	UpdatedReplicas int32  `json:"updatedReplicas"`
	Image           string `json:"image,omitempty"`

	Ready bool `json:"ready"`
}
