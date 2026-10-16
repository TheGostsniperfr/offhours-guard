package controller

import (
	"context"
	"testing"

	"github.com/robfig/cron/v3"
	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const namespace = "shop"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypes(v1alpha1.GroupVersion, &v1alpha1.OffhoursSchedule{}, &v1alpha1.OffhoursScheduleList{})
	metav1.AddToGroupVersion(scheme, v1alpha1.GroupVersion)
	rolloutGVK := rolloutListGVK.GroupVersion().WithKind("Rollout")
	scheme.AddKnownTypeWithName(rolloutGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(rolloutListGVK, &unstructured.UnstructuredList{})
	return scheme
}

func deployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func rollout(replicas int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata":   map[string]any{"name": "web-rollout", "namespace": namespace},
		"spec": map[string]any{
			"replicas":    replicas,
			"workloadRef": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web"},
		},
	}}
	return obj
}

func schedule(state string, original int32) *v1alpha1.OffhoursSchedule {
	return &v1alpha1.OffhoursSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "web-schedule", Namespace: namespace},
		Spec: v1alpha1.OffhoursScheduleSpec{
			TargetRefs: []v1alpha1.TargetRef{{Name: "web"}},
			// Never fires during the test: the manual state drives it.
			SleepAt: "0 3 29 2 *",
			WakeAt:  "0 4 29 2 *",
		},
		Status: v1alpha1.OffhoursScheduleStatus{
			CurrentState:   state,
			TargetStatuses: []v1alpha1.TargetStatus{{Name: "web", OriginalReplicas: original}},
		},
	}
}

func reconcile(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.OffhoursSchedule{}).
		Build()
	r := &OffhoursScheduleReconciler{
		Client:     c,
		CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: "web-schedule"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

func replicasOf(t *testing.T, c client.Client) (deploymentReplicas int32, rolloutReplicas int64) {
	t.Helper()
	var d appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "web"}, &d); err != nil {
		t.Fatal(err)
	}
	r := rollout(0)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "web-rollout"}, r)
	if err == nil {
		rolloutReplicas, _, _ = unstructured.NestedInt64(r.Object, "spec", "replicas")
	}
	return *d.Spec.Replicas, rolloutReplicas
}

func TestSleep_ScalesTheRollout_WhenItOwnsTheDeploymentThroughWorkloadRef(t *testing.T) {
	// Arrange: under a canary the Rollout runs the pods and the Deployment sits at 0.
	c := reconcile(t, schedule("MANUAL_SLEEP", 0), deployment(0), rollout(2))

	// Act
	deploymentReplicas, rolloutReplicas := replicasOf(t, c)

	// Assert
	if deploymentReplicas != 0 || rolloutReplicas != 0 {
		t.Fatalf("got deployment=%d rollout=%d, want both 0", deploymentReplicas, rolloutReplicas)
	}
}

func TestWake_RestoresTheRollout_AndLeavesTheDeploymentAtZero(t *testing.T) {
	// Arrange
	c := reconcile(t, schedule("MANUAL_WAKE", 2), deployment(0), rollout(0))

	// Act
	deploymentReplicas, rolloutReplicas := replicasOf(t, c)

	// Assert
	if deploymentReplicas != 0 || rolloutReplicas != 2 {
		t.Fatalf("got deployment=%d rollout=%d, want deployment=0 rollout=2", deploymentReplicas, rolloutReplicas)
	}
}

func TestSleep_ScalesTheDeployment_WhenNoRolloutReferencesIt(t *testing.T) {
	// Arrange
	c := reconcile(t, schedule("MANUAL_SLEEP", 0), deployment(3))

	// Act
	deploymentReplicas, _ := replicasOf(t, c)

	// Assert
	if deploymentReplicas != 0 {
		t.Fatalf("got deployment=%d, want 0", deploymentReplicas)
	}
}

func TestWake_RestoresTheDeployment_WhenNoRolloutReferencesIt(t *testing.T) {
	// Arrange
	c := reconcile(t, schedule("MANUAL_WAKE", 3), deployment(0))

	// Act
	deploymentReplicas, _ := replicasOf(t, c)

	// Assert
	if deploymentReplicas != 3 {
		t.Fatalf("got deployment=%d, want 3", deploymentReplicas)
	}
}
