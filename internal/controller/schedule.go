package controller

import (
	"context"
	"log"
	"time"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type OffhoursScheduleReconciler struct {
	client.Client
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
		var deployment appsv1.Deployment
		depName := types.NamespacedName{Namespace: req.Namespace, Name: ref.Name}
		
		if err := r.Get(ctx, depName, &deployment); err != nil {
			log.Printf("[WARN] Deployment %s/%s not found. Skipping.", req.Namespace, ref.Name)
			continue
		}

		currentReplicas := *deployment.Spec.Replicas
		origReplicas := originalReplicasMap[ref.Name]

		// Apply Scale Down
		if desiredState == "SLEEPING" || desiredState == "MANUAL_SLEEP" {
			if currentReplicas > 0 {
				origReplicas = currentReplicas // Save state
				zero := int32(0)
				deployment.Spec.Replicas = &zero
				if err := r.Update(ctx, &deployment); err != nil {
					return ctrl.Result{}, err
				}
				log.Printf("[OG] 💤 Scaled DOWN %s/%s to 0 replicas", req.Namespace, ref.Name)
				stateChanged = true
			}
			newTargetStatuses = append(newTargetStatuses, v1alpha1.TargetStatus{
				Name:             ref.Name,
				OriginalReplicas: origReplicas,
			})
		}

		// Apply Scale Up
		if desiredState == "AWAKE" || desiredState == "MANUAL_WAKE" {
			target := origReplicas
			if target == 0 {
				target = 1 // Safe fallback
			}
			if currentReplicas != target {
				deployment.Spec.Replicas = &target
				if err := r.Update(ctx, &deployment); err != nil {
					return ctrl.Result{}, err
				}
				log.Printf("[OG] ☀️ Scaled UP %s/%s to %d replicas", req.Namespace, ref.Name, target)
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