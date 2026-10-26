package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	targetAsleep = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "offhours_target_asleep",
		Help: "1 while the target workload is scaled down by the controller, else 0.",
	}, []string{"namespace", "schedule", "target"})

	targetOriginalReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "offhours_target_original_replicas",
		Help: "Replica count the target had before sleeping, restored on wake.",
	}, []string{"namespace", "schedule", "target"})

	targetAsleepSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "offhours_target_asleep_seconds_total",
		Help: "Cumulative seconds the target spent asleep.",
	}, []string{"namespace", "schedule", "target"})

	transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "offhours_transitions_total",
		Help: "Sleep and wake transitions attempted, by direction and result.",
	}, []string{"namespace", "schedule", "direction", "result"})

	tracker = &sleepTracker{
		lastSeen: map[string]time.Time{},
		targets:  map[string]map[string]struct{}{},
	}
)

func init() {
	metrics.Registry.MustRegister(targetAsleep, targetOriginalReplicas, targetAsleepSeconds, transitions)
}

// sleepTracker lives in memory only: after a restart the counter resumes from
// the next observation, which Prometheus treats as a normal counter reset.
type sleepTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	targets  map[string]map[string]struct{}
}

func targetName(kind, name string) string { return kind + "/" + name }

func scheduleKey(namespace, schedule string) string { return namespace + "/" + schedule }

func (s *sleepTracker) observe(namespace, schedule, target string, asleep bool, originalReplicas int32, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sk := scheduleKey(namespace, schedule)
	if s.targets[sk] == nil {
		s.targets[sk] = map[string]struct{}{}
	}
	s.targets[sk][target] = struct{}{}

	// Reconciles are sparse while asleep (the next one is the wake), so time
	// accrued since the previous observation is credited even on the wake call.
	key := sk + "/" + target
	if last, ok := s.lastSeen[key]; ok {
		targetAsleepSeconds.WithLabelValues(namespace, schedule, target).Add(now.Sub(last).Seconds())
	}
	if asleep {
		s.lastSeen[key] = now
	} else {
		delete(s.lastSeen, key)
	}

	value := 0.0
	if asleep {
		value = 1
	}
	targetAsleep.WithLabelValues(namespace, schedule, target).Set(value)
	targetOriginalReplicas.WithLabelValues(namespace, schedule, target).Set(float64(originalReplicas))
}

// prune drops the gauge series of targets no longer part of the schedule.
func (s *sleepTracker) prune(namespace, schedule string, keep map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sk := scheduleKey(namespace, schedule)
	for target := range s.targets[sk] {
		if _, ok := keep[target]; ok {
			continue
		}
		s.forgetTarget(namespace, schedule, target)
		delete(s.targets[sk], target)
	}
}

func (s *sleepTracker) forgetSchedule(namespace, schedule string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sk := scheduleKey(namespace, schedule)
	for target := range s.targets[sk] {
		s.forgetTarget(namespace, schedule, target)
	}
	delete(s.targets, sk)
}

func (s *sleepTracker) forgetTarget(namespace, schedule, target string) {
	labels := prometheus.Labels{"namespace": namespace, "schedule": schedule, "target": target}
	targetAsleep.Delete(labels)
	targetOriginalReplicas.Delete(labels)
	delete(s.lastSeen, scheduleKey(namespace, schedule)+"/"+target)
}

func recordTransition(namespace, schedule, direction string, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	transitions.WithLabelValues(namespace, schedule, direction, result).Inc()
}
