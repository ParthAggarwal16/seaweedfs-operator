package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
	"github.com/openeverest/seaweedfs-operator/internal/resources"
	"github.com/openeverest/seaweedfs-operator/internal/seaweed"
)

type StorageClients struct {
	S3     *seaweed.S3Client
	Filer  *seaweed.FilerClient
	Master *seaweed.MasterClient
	IAM    *seaweed.IAMStore
	AdminAccessKey string
	AdminSecretKey string
}

type ClientFactory interface {
	For(ctx context.Context, cluster *objectstoragev1alpha1.ObjectStorageCluster, creds AdminCredentials) (*StorageClients, error)
}

type AdminCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

type DefaultClientFactory struct{}

func (DefaultClientFactory) For(_ context.Context, cluster *objectstoragev1alpha1.ObjectStorageCluster, creds AdminCredentials) (*StorageClients, error) {
	filer := seaweed.NewFilerClient("http:
	return &StorageClients{
		S3: seaweed.NewS3Client(seaweed.S3Options{
			Endpoint:        resources.S3Endpoint(cluster),
			Region:          resources.DefaultRegion,
			AccessKeyID:     creds.AccessKeyID,
			SecretAccessKey: creds.SecretAccessKey,
		}),
		Filer: filer,
		Master: seaweed.NewMasterClient(fmt.Sprintf("http:
			resources.ServiceFQDN(cluster, resources.ComponentMaster), resources.MasterHTTPPort)),
		IAM:            seaweed.NewIAMStore(filer, resources.IAMConfigPath),
		AdminAccessKey: creds.AccessKeyID,
		AdminSecretKey: creds.SecretAccessKey,
	}, nil
}

const (
	adminAccessKeyField = "accessKeyID"
	adminSecretKeyField = "secretAccessKey"
)

func ensureAdminCredentials(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	cluster *objectstoragev1alpha1.ObjectStorageCluster,
) (AdminCredentials, error) {
	name := resources.AdminSecretName(cluster)
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		access := string(existing.Data[adminAccessKeyField])
		secret := string(existing.Data[adminSecretKeyField])
		if access != "" && secret != "" {
			return AdminCredentials{AccessKeyID: access, SecretAccessKey: secret}, nil
		}
		return AdminCredentials{}, fmt.Errorf(
			"secret %s/%s exists but is missing %q or %q; delete it to have the operator regenerate credentials",
			cluster.Namespace, name, adminAccessKeyField, adminSecretKeyField)
	case !apierrors.IsNotFound(err):
		return AdminCredentials{}, fmt.Errorf("get admin secret: %w", err)
	}

	accessKey, err := seaweed.GenerateAccessKeyID()
	if err != nil {
		return AdminCredentials{}, err
	}
	secretKey, err := seaweed.GenerateSecretAccessKey()
	if err != nil {
		return AdminCredentials{}, err
	}

	secret := resources.AdminSecret(cluster, accessKey, secretKey)
	if err := controllerutil.SetControllerReference(cluster, secret, scheme); err != nil {
		return AdminCredentials{}, fmt.Errorf("set owner on admin secret: %w", err)
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			var raced corev1.Secret
			if getErr := c.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, &raced); getErr == nil {
				return AdminCredentials{
					AccessKeyID:     string(raced.Data[adminAccessKeyField]),
					SecretAccessKey: string(raced.Data[adminSecretKeyField]),
				}, nil
			}
		}
		return AdminCredentials{}, fmt.Errorf("create admin secret: %w", err)
	}

	return AdminCredentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}, nil
}

func seedAdminIdentity(ctx context.Context, clients *StorageClients) (bool, error) {
	return clients.IAM.Mutate(ctx, func(cfg *seaweed.IAMConfig) (bool, error) {
		return cfg.Upsert(seaweed.Identity{
			Name: "seaweedfs-operator-admin",
			Credentials: []seaweed.Credential{{
				AccessKey: clients.AdminAccessKey,
				SecretKey: clients.AdminSecretKey,
			}},
			Actions: seaweed.AdminActions,
		}), nil
	})
}

func resolveClusterForChild(
	ctx context.Context,
	c client.Client,
	namespace, clusterName string,
) (*objectstoragev1alpha1.ObjectStorageCluster, error) {
	var cluster objectstoragev1alpha1.ObjectStorageCluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: clusterName}, &cluster); err != nil {
		return nil, err
	}
	return &cluster, nil
}

func clusterS3Ready(cluster *objectstoragev1alpha1.ObjectStorageCluster) bool {
	for _, cond := range cluster.Status.Conditions {
		if cond.Type == objectstoragev1alpha1.ConditionS3Ready {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
