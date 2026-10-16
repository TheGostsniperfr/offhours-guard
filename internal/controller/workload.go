package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var rolloutListGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "RolloutList"}

// workload is what really holds a target's pods.
type workload interface {
	kind() string
	replicas() int32
	scale(ctx context.Context, c client.Client, replicas int32) error
}

type deploymentWorkload struct{ obj *appsv1.Deployment }

func (d deploymentWorkload) kind() string { return "Deployment" }

func (d deploymentWorkload) replicas() int32 {
	if d.obj.Spec.Replicas == nil {
		return 1
	}
	return *d.obj.Spec.Replicas
}

func (d deploymentWorkload) scale(ctx context.Context, c client.Client, replicas int32) error {
	d.obj.Spec.Replicas = &replicas
	return c.Update(ctx, d.obj)
}

type rolloutWorkload struct{ obj *unstructured.Unstructured }

func (r rolloutWorkload) kind() string { return "Rollout" }

func (r rolloutWorkload) replicas() int32 {
	value, found, err := unstructured.NestedInt64(r.obj.Object, "spec", "replicas")
	if err != nil || !found {
		return 1
	}
	return int32(value)
}

func (r rolloutWorkload) scale(ctx context.Context, c client.Client, replicas int32) error {
	patch := client.MergeFrom(r.obj.DeepCopy())
	if err := unstructured.SetNestedField(r.obj.Object, int64(replicas), "spec", "replicas"); err != nil {
		return err
	}
	return c.Patch(ctx, r.obj, patch)
}

// resolveWorkload returns the Argo Rollout that took the Deployment over through
// workloadRef, else the Deployment. Under a Rollout the Deployment stays at 0
// replicas: scaling it would never put the app to sleep and would start an
// extra pod on wake.
func (r *OffhoursScheduleReconciler) resolveWorkload(ctx context.Context, namespace, name string) (workload, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	rollouts := &unstructured.UnstructuredList{}
	rollouts.SetGroupVersionKind(rolloutListGVK)
	err := reader.List(ctx, rollouts, client.InNamespace(namespace))
	if err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("list rollouts in %s: %w", namespace, err)
	}
	for i := range rollouts.Items {
		ref, _, _ := unstructured.NestedStringMap(rollouts.Items[i].Object, "spec", "workloadRef")
		if ref["kind"] == "Deployment" && ref["name"] == name {
			return rolloutWorkload{obj: &rollouts.Items[i]}, nil
		}
	}

	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &deployment); err != nil {
		return nil, err
	}
	return deploymentWorkload{obj: &deployment}, nil
}
