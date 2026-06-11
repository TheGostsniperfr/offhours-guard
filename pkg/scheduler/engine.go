package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/thegostsniperfr/offhours-guard/pkg/k8s"
	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Engine struct {
	k8sClient *k8s.Client
	parser    cron.Parser
	location  *time.Location
}

func NewEngine(client *k8s.Client) *Engine {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		log.Println("[WARN] Could not load Europe/Paris timezone, falling back to Local")
		loc = time.Local
	}

	return &Engine{
		k8sClient: client,
		parser:    cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		location:  loc,
	}
}

func (e *Engine) Start(ctx context.Context) {
	log.Println("[SCHEDULER] Starting background reconciliation loop...")

	now := time.Now()
	nextMinute := now.Truncate(time.Minute).Add(time.Minute)
	time.Sleep(time.Until(nextMinute))

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Run the first check immediately at second :00
	e.checkDeployments(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("[SCHEDULER] Stopping engine...")
			return
		case <-ticker.C:
			e.checkDeployments(ctx)
		}
	}
}

// checkDeployments scans the K8s cluster for managed apps
func (e *Engine) checkDeployments(ctx context.Context) {
	// Note: In K3s, listing all deployments is lightning fast.
	// For massive clusters (10k+ apps), we would use a LabelSelector instead.
	deployments, err := e.k8sClient.ClientSet.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[ERROR] Failed to list deployments: %v\n", err)
		return
	}

	now := time.Now().In(e.location)
	currentMinute := now.Truncate(time.Minute)

	for _, deploy := range deployments.Items {
		annotations := deploy.GetAnnotations()
		
		// Skip apps that don't have the OG annotation
		if annotations == nil || annotations[k8s.AnnotationEnabled] != "true" {
			continue
		}

		sleepCron := annotations[k8s.AnnotationSleepAt]
		wakeCron := annotations[k8s.AnnotationWakeAt]

		// Check if the current minute is a trigger time
		if e.isTimeForAction(sleepCron, currentMinute) {
			log.Printf("[SCHEDULER] Triggering SLEEP schedule for %s/%s\n", deploy.Namespace, deploy.Name)
			err := e.k8sClient.ScaleApp(ctx, deploy.Namespace, deploy.Name, "sleep")
			if err != nil {
				log.Printf("[ERROR] %v\n", err)
			}
		} else if e.isTimeForAction(wakeCron, currentMinute) {
			log.Printf("[SCHEDULER] Triggering WAKE schedule for %s/%s\n", deploy.Namespace, deploy.Name)
			err := e.k8sClient.ScaleApp(ctx, deploy.Namespace, deploy.Name, "wake")
			if err != nil {
				log.Printf("[ERROR] %v\n", err)
			}
		}
	}
}

// isTimeForAction evaluates if a cron expression matches the current exact minute
func (e *Engine) isTimeForAction(cronExpr string, currentMinute time.Time) bool {
	if cronExpr == "" {
		return false
	}

	schedule, err := e.parser.Parse(cronExpr)
	if err != nil {
		return false // Invalid cron format provided by developer, safely ignore
	}

	// Logic: We calculate the next scheduled execution starting from exactly 1 minute ago.
	// If that next execution equals the current minute, it means it's time to fire!
	oneMinuteAgo := currentMinute.Add(-1 * time.Minute)
	nextRun := schedule.Next(oneMinuteAgo)

	return nextRun.Equal(currentMinute)
}