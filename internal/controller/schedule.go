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

	var deployment appsv1.Deployment
	depName := types.NamespacedName{Namespace: req.Namespace, Name: schedule.Spec.TargetRef.Name}
	if err := r.Get(ctx, depName, &deployment); err != nil {
		log.Printf("[WARN] Deployment %s not found. Retrying in 30s.", depName.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

	// Check if a cron boundary was reached exactly during the current minute
	oneMinuteAgo := currentMinute.Add(-1 * time.Minute)
	sleepTriggered := sleepCron.Next(oneMinuteAgo).Equal(currentMinute)
	wakeTriggered := wakeCron.Next(oneMinuteAgo).Equal(currentMinute)

	// Determine desired state
	desiredState := schedule.Status.CurrentState

	if sleepTriggered {
		desiredState = "SLEEPING"
		log.Printf("[OG] ⏰ Sleep cron schedule triggered at %s", currentMinute.Format("15:04"))
	} else if wakeTriggered {
		desiredState = "AWAKE"
		log.Printf("[OG] ⏰ Wake cron schedule triggered at %s", currentMinute.Format("15:04"))
	} else if schedule.Status.CurrentState == "" {
		// Initial bootstrap
		shouldSleep := nextWake.Before(nextSleep)
		if shouldSleep {
			desiredState = "SLEEPING"
		} else {
			desiredState = "AWAKE"
		}
	}

	stateChanged := false

	// Apply Scale Down (SLEEP / MANUAL_SLEEP)
	if desiredState == "SLEEPING" || desiredState == "MANUAL_SLEEP" {
		if *deployment.Spec.Replicas > 0 {
			schedule.Status.OriginalReplicas = *deployment.Spec.Replicas
			zero := int32(0)
			deployment.Spec.Replicas = &zero
			if err := r.Update(ctx, &deployment); err != nil {
				return ctrl.Result{}, err
			}
			log.Printf("[OG] 💤 Scaled DOWN %s/%s to 0 replicas (State: %s)", req.Namespace, depName.Name, desiredState)
		}
		if schedule.Status.CurrentState != desiredState {
			schedule.Status.CurrentState = desiredState
			stateChanged = true
		}
	}

	// Apply Scale Up (WAKE / MANUAL_WAKE)
	if desiredState == "AWAKE" || desiredState == "MANUAL_WAKE" {
		target := schedule.Status.OriginalReplicas
		if target == 0 {
			target = 1
		}
		if *deployment.Spec.Replicas != target {
			deployment.Spec.Replicas = &target
			if err := r.Update(ctx, &deployment); err != nil {
				return ctrl.Result{}, err
			}
			log.Printf("[OG] ☀️ Scaled UP %s/%s to %d replicas (State: %s)", req.Namespace, depName.Name, target, desiredState)
		}
		if schedule.Status.CurrentState != desiredState {
			schedule.Status.CurrentState = desiredState
			stateChanged = true
		}
	}

	if stateChanged {
		if err := r.Status().Update(ctx, &schedule); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Calculate next wake/sleep event to sleep the controller
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