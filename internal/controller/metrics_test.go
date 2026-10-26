package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robfig/cron/v3"
	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func resetMetrics(t *testing.T) {
	t.Helper()
	reset := func() {
		targetAsleep.Reset()
		targetOriginalReplicas.Reset()
		targetAsleepSeconds.Reset()
		transitions.Reset()
		tracker = &sleepTracker{lastSeen: map[string]time.Time{}, targets: map[string]map[string]struct{}{}}
	}
	reset()
	t.Cleanup(reset)
}

func labels(target string) prometheus.Labels {
	return prometheus.Labels{"namespace": namespace, "schedule": "web-schedule", "target": target}
}

func TestReconcile_SetsMetrics_WhenTargetScaledDownOrUp(t *testing.T) {
	tests := []struct {
		name         string
		state        string
		original     int32
		deployment   int32
		wantAsleep   float64
		wantOriginal float64
		wantDir      string
	}{
		{"sleep records asleep and original replicas", "MANUAL_SLEEP", 0, 3, 1, 3, "sleep"},
		{"wake clears asleep and keeps original replicas", "MANUAL_WAKE", 3, 0, 0, 3, "wake"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetMetrics(t)

			reconcile(t, schedule(tt.state, tt.original), deployment(tt.deployment))

			if got := testutil.ToFloat64(targetAsleep.With(labels("Deployment/web"))); got != tt.wantAsleep {
				t.Errorf("asleep = %v, want %v", got, tt.wantAsleep)
			}
			if got := testutil.ToFloat64(targetOriginalReplicas.With(labels("Deployment/web"))); got != tt.wantOriginal {
				t.Errorf("original replicas = %v, want %v", got, tt.wantOriginal)
			}
			got := testutil.ToFloat64(transitions.WithLabelValues(namespace, "web-schedule", tt.wantDir, "success"))
			if got != 1 {
				t.Errorf("%s success transitions = %v, want 1", tt.wantDir, got)
			}
		})
	}
}

func TestReconcile_UsesRolloutKindInTarget_WhenRolloutOwnsTheDeployment(t *testing.T) {
	resetMetrics(t)

	reconcile(t, schedule("MANUAL_SLEEP", 0), deployment(0), rollout(2))

	if got := testutil.ToFloat64(targetAsleep.With(labels("Rollout/web"))); got != 1 {
		t.Fatalf("asleep = %v, want 1", got)
	}
}

func TestReconcile_CountsTransitionError_WhenScaleFails(t *testing.T) {
	resetMetrics(t)
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(schedule("MANUAL_SLEEP", 0), deployment(3)).
		WithStatusSubresource(&v1alpha1.OffhoursSchedule{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return errors.New("boom")
			},
		}).
		Build()
	r := &OffhoursScheduleReconciler{
		Client:     c,
		CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: "web-schedule"}}

	_, err := r.Reconcile(context.Background(), req)

	if err == nil {
		t.Fatal("expected reconcile error")
	}
	if got := testutil.ToFloat64(transitions.WithLabelValues(namespace, "web-schedule", "sleep", "error")); got != 1 {
		t.Fatalf("sleep error transitions = %v, want 1", got)
	}
}

func TestReconcile_DeletesSeries_WhenScheduleIsDeleted(t *testing.T) {
	resetMetrics(t)
	c := reconcile(t, schedule("MANUAL_SLEEP", 0), deployment(3))
	var sched v1alpha1.OffhoursSchedule
	key := types.NamespacedName{Namespace: namespace, Name: "web-schedule"}
	if err := c.Get(context.Background(), key, &sched); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &sched); err != nil {
		t.Fatal(err)
	}
	r := &OffhoursScheduleReconciler{Client: c, CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	if got := testutil.CollectAndCount(targetAsleep); got != 0 {
		t.Errorf("asleep series = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(targetOriginalReplicas); got != 0 {
		t.Errorf("original replicas series = %d, want 0", got)
	}
}

func TestReconcile_DeletesSeries_WhenTargetIsRemovedFromSchedule(t *testing.T) {
	resetMetrics(t)
	other := deployment(2)
	other.Name = "api"
	c := reconcile(t, schedule("MANUAL_SLEEP", 0), deployment(3), other)
	var sched v1alpha1.OffhoursSchedule
	key := types.NamespacedName{Namespace: namespace, Name: "web-schedule"}
	if err := c.Get(context.Background(), key, &sched); err != nil {
		t.Fatal(err)
	}
	sched.Spec.TargetRefs = []v1alpha1.TargetRef{{Name: "api"}}
	if err := c.Update(context.Background(), &sched); err != nil {
		t.Fatal(err)
	}
	r := &OffhoursScheduleReconciler{Client: c, CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	if got := testutil.CollectAndCount(targetAsleep); got != 1 {
		t.Fatalf("asleep series = %d, want 1 (api only)", got)
	}
	if got := testutil.ToFloat64(targetAsleep.With(labels("Deployment/api"))); got != 1 {
		t.Errorf("api asleep = %v, want 1", got)
	}
}

func TestObserve_AccumulatesAsleepSeconds_AcrossObservationsIncludingWake(t *testing.T) {
	resetMetrics(t)
	start := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)
	target := "Deployment/web"

	tracker.observe(namespace, "web-schedule", target, true, 3, start)
	tracker.observe(namespace, "web-schedule", target, true, 3, start.Add(90*time.Second))
	tracker.observe(namespace, "web-schedule", target, false, 3, start.Add(10*time.Hour))
	tracker.observe(namespace, "web-schedule", target, false, 3, start.Add(11*time.Hour))

	want := (10 * time.Hour).Seconds()
	if got := testutil.ToFloat64(targetAsleepSeconds.With(labels(target))); got != want {
		t.Fatalf("asleep seconds = %v, want %v", got, want)
	}
}
