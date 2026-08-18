package v1alpha1

const (
	ConditionAvailable = "Available"

	ConditionProgressing = "Progressing"

	ConditionDegraded = "Degraded"

	ConditionS3Ready = "S3Ready"

	ConditionReady = "Ready"
)

const (
	ReasonReconciling         = "Reconciling"
	ReasonReconcileError      = "ReconcileError"
	ReasonAllComponentsReady  = "AllComponentsReady"
	ReasonComponentsNotReady  = "ComponentsNotReady"
	ReasonRolloutInProgress   = "RolloutInProgress"
	ReasonScalingInProgress   = "ScalingInProgress"
	ReasonUpgradeInProgress   = "UpgradeInProgress"
	ReasonUpgradePaused       = "UpgradePaused"
	ReasonWaitingForMasters   = "WaitingForMasters"
	ReasonWaitingForVolumes   = "WaitingForVolumeServers"
	ReasonWaitingForFiler     = "WaitingForFiler"
	ReasonWaitingForS3        = "WaitingForS3Gateway"
	ReasonS3Unreachable       = "S3EndpointUnreachable"
	ReasonS3Authenticated     = "S3Authenticated"
	ReasonClusterNotFound     = "ClusterNotFound"
	ReasonClusterNotReady     = "ClusterNotReady"
	ReasonBucketCreated       = "BucketCreated"
	ReasonBucketExists        = "BucketAlreadyExists"
	ReasonBucketCreateFailed  = "BucketCreateFailed"
	ReasonIdentityConfigured  = "IdentityConfigured"
	ReasonIdentityFailed      = "IdentityConfigurationFailed"
	ReasonCredentialsIssued   = "CredentialsIssued"
	ReasonDeleting            = "Deleting"
	ReasonInvalidSpec         = "InvalidSpec"
	ReasonNoFreeVolumeSlots   = "NoFreeVolumeSlots"
	ReasonCapacityExpanding   = "CapacityExpanding"
	ReasonExpansionUnsupportd = "VolumeExpansionUnsupported"
)

const (
	ClusterFinalizer = "objectstorage.openeverest.io/cluster-cleanup"

	BucketFinalizer = "objectstorage.openeverest.io/bucket-cleanup"

	UserFinalizer = "objectstorage.openeverest.io/user-cleanup"
)
