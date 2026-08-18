package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	objectstoragev1alpha1 "github.com/openeverest/seaweedfs-operator/api/v1alpha1"
)

type clusterReadinessChanged struct {
	predicate.Funcs
}

func (clusterReadinessChanged) Create(event.CreateEvent) bool { return true }

func (clusterReadinessChanged) Delete(event.DeleteEvent) bool { return true }

func (clusterReadinessChanged) Generic(event.GenericEvent) bool { return false }

func (clusterReadinessChanged) Update(e event.UpdateEvent) bool {
	oldCluster, ok := e.ObjectOld.(*objectstoragev1alpha1.ObjectStorageCluster)
	if !ok {
		return false
	}
	newCluster, ok := e.ObjectNew.(*objectstoragev1alpha1.ObjectStorageCluster)
	if !ok {
		return false
	}
	return s3ReadyStatus(oldCluster) != s3ReadyStatus(newCluster)
}

func s3ReadyStatus(cluster *objectstoragev1alpha1.ObjectStorageCluster) metav1.ConditionStatus {
	for _, cond := range cluster.Status.Conditions {
		if cond.Type == objectstoragev1alpha1.ConditionS3Ready {
			return cond.Status
		}
	}
	return metav1.ConditionUnknown
}
