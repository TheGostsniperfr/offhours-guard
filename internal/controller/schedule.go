package controller

import (
	"context"
	"log"
	"time"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type OffhoursScheduleReconciler struct {
	client.Client
	// Uncached reads of Argo Rollouts: a cache would need a watch on a CRD
	// that clusters without Argo Rollouts do not have.
	APIReader  client.Reader
	CronParser cron.Parser
}

func (r *OffhoursScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var schedule v1alpha1.OffhoursSchedule
	if err := r.Get(ctx, req.NamespacedName, &schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tz := schedule.Spec.Timezone
	if tz == "" {
		tz = "Europe/Paris"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	currentMinute := now.Truncate(time.Minute)

	sleepCron, _ := r.CronParser.Parse(schedule.Spec.SleepAt)
	wakeCron, _ := r.CronParser.Parse(schedule.Spec.WakeAt)

	nextSleep := sleepCron.Next(now)
	nextWake := wakeCron.Next(now)

	oneMinuteAgo := currentMinute.Add(-1 * time.Minute)
	sleepTriggered := sleepCron.Next(oneMinuteAgo).Equal(currentMinute)
	wakeTriggered := wakeCron.Next(oneMinuteAgo).Equal(currentMinute)

	desiredState := schedule.Status.CurrentState

	if sleepTriggered {
		desiredState = "SLEEPING"
		log.Printf("[OG] ⏰ Sleep cron schedule triggered at %s", currentMinute.Format("15:04"))
	} else if wakeTriggered {
		desiredState = "AWAKE"
		log.Printf("[OG] ⏰ Wake cron schedule triggered at %s", currentMinute.Format("15:04"))
	} else if schedule.Status.CurrentState == "" {
		shouldSleep := nextWake.Before(nextSleep)
		if shouldSleep {
			desiredState = "SLEEPING"
		} else {
			desiredState = "AWAKE"
		}
	}

	stateChanged := false

	// Map to keep track of already stored original replicas in status
	originalReplicasMap := make(map[string]int32)
	for _, ts := range schedule.Status.TargetStatuses {
		originalReplicasMap[ts.Name] = ts.OriginalReplicas
	}

	var newTargetStatuses []v1alpha1.TargetStatus

	for _, ref := range schedule.Spec.TargetRefs {
		target, err := r.resolveWorkload(ctx, req.Namespace, ref.Name)
		if apierrors.IsNotFound(err) {
			log.Printf("[WARN] Deployment %s/%s not found. Skipping.", req.Namespace, ref.Name)
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}

		currentReplicas := target.replicas()
		origReplicas := originalReplicasMap[ref.Name]

		// Apply Scale Down
		if desiredState == "SLEEPING" || desiredState == "MANUAL_SLEEP" {
			if currentReplicas > 0 {
				origReplicas = currentReplicas // Save state
				if err := target.scale(ctx, r.Client, 0); err != nil {
					return ctrl.Result{}, err
				}
				log.Printf("[OG] 💤 Scaled DOWN %s %s/%s to 0 replicas", target.kind(), req.Namespace, ref.Name)
				stateChanged = true
			}
			newTargetStatuses = append(newTargetStatuses, v1alpha1.TargetStatus{
				Name:             ref.Name,
				OriginalReplicas: origReplicas,
			})
		}

		// Apply Scale Up
		if desiredState == "AWAKE" || desiredState == "MANUAL_WAKE" {
			want := origReplicas
			if want == 0 {
				want = 1 // Safe fallback
			}
			if currentReplicas != want {
				if err := target.scale(ctx, r.Client, want); err != nil {
					return ctrl.Result{}, err
				}
				log.Printf("[OG] ☀️ Scaled UP %s %s/%s to %d replicas", target.kind(), req.Namespace, ref.Name, want)
				stateChanged = true
			}
			newTargetStatuses = append(newTargetStatuses, v1alpha1.TargetStatus{
				Name:             ref.Name,
				OriginalReplicas: origReplicas,
			})
		}
	}

	if schedule.Status.CurrentState != desiredState {
		schedule.Status.CurrentState = desiredState
		stateChanged = true
	}

	if stateChanged {
		schedule.Status.TargetStatuses = newTargetStatuses
		if err := r.Status().Update(ctx, &schedule); err != nil {
			return ctrl.Result{}, err
		}
	}

	nextEvent := nextSleep
	if desiredState == "SLEEPING" || desiredState == "MANUAL_SLEEP" {
		nextEvent = nextWake
	}
	timeUntilNextEvent := nextEvent.Sub(now)
	localNextEvent := nextEvent.In(loc)

	log.Printf("[SCHEDULER] Next evaluation for %s in %v (at %s)", schedule.Name, timeUntilNextEvent.Round(time.Second), localNextEvent.Format("15:04:05"))
	return ctrl.Result{RequeueAfter: timeUntilNextEvent}, nil
}

func (r *OffhoursScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OffhoursSchedule{}).
		Complete(r)
}