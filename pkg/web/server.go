package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed templates/*
var templateFS embed.FS

type Server struct {
	k8sClient client.Client
	templates *template.Template
}

func NewServer(k8sClient client.Client) *Server {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatalf("[FATAL] Failed to parse embedded templates: %v", err)
	}

	return &Server{
		k8sClient: k8sClient,
		templates: tmpl,
	}
}

func (s *Server) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /health/{namespace}/{name}", s.handleGatusProxy)
	mux.HandleFunc("POST /apps/{namespace}/{name}/wake", s.handleWakeOverride)
	mux.HandleFunc("POST /apps/{namespace}/{name}/sleep", s.handleSleepOverride)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("[WEB] UI Dashboard and Gatus Proxy serving on %s\n", addr)
	return srv.ListenAndServe()
}

// Sub-structure to hold target details
type TargetDetail struct {
	Name             string
	OriginalReplicas int32
	CurrentReplicas  int32
}

type AppCardData struct {
	ScheduleName string 
	Namespace    string
	State        string
	SleepAt      string
	WakeAt       string
	Timezone     string
	Targets      []TargetDetail 
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var schedules v1alpha1.OffhoursScheduleList

	if err := s.k8sClient.List(ctx, &schedules); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var cards []AppCardData
	for _, sch := range schedules.Items {
		origMap := make(map[string]int32)
		for _, ts := range sch.Status.TargetStatuses {
			origMap[ts.Name] = ts.OriginalReplicas
		}

		var targets []TargetDetail
		for _, ref := range sch.Spec.TargetRefs {
			var deploy appsv1.Deployment
			_ = s.k8sClient.Get(ctx, types.NamespacedName{Namespace: sch.Namespace, Name: ref.Name}, &deploy)

			current := int32(0)
			if deploy.Spec.Replicas != nil {
				current = *deploy.Spec.Replicas
			}

			orig := origMap[ref.Name]
			if orig == 0 {
				orig = current
			}

			targets = append(targets, TargetDetail{
				Name:             ref.Name,
				OriginalReplicas: orig,
				CurrentReplicas:  current,
			})
		}

		cards = append(cards, AppCardData{
			ScheduleName: sch.Name,
			Namespace:    sch.Namespace,
			State:        sch.Status.CurrentState,
			SleepAt:      sch.Spec.SleepAt,
			WakeAt:       sch.Spec.WakeAt,
			Timezone:     sch.Spec.Timezone,
			Targets:      targets,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	_ = s.templates.ExecuteTemplate(w, "index.html", cards)
}

func (s *Server) handleGatusProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	namespace := r.PathValue("namespace")

	schedule, err := s.findScheduleForTarget(ctx, namespace, name)
	if err != nil {
		s.checkRealAppHealth(w, r, name, namespace)
		return
	}

	if schedule.Status.CurrentState == "SLEEPING" || schedule.Status.CurrentState == "MANUAL_SLEEP" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"SLEEPING","info":"Service is stopped on schedule (Managed by Offhours-Guard)"}`))
		return
	}

	s.checkRealAppHealth(w, r, name, namespace)
}

func (s *Server) checkRealAppHealth(w http.ResponseWriter, r *http.Request, name, namespace string) {
	port := r.URL.Query().Get("port")
	if port == "" {
		port = "80"
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}

	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:%s%s", name, namespace, port, path)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"UNHEALTHY","error":"App is down or unreachable"}`))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	if resp.StatusCode >= 500 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"UNHEALTHY","error":"App returned 5xx"}`))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"HEALTHY","info":"App is running fine"}`))
}

func (s *Server) handleWakeOverride(w http.ResponseWriter, r *http.Request) {
	s.triggerManualAction(w, r, "wake")
}

func (s *Server) handleSleepOverride(w http.ResponseWriter, r *http.Request) {
	s.triggerManualAction(w, r, "sleep")
}

func (s *Server) triggerManualAction(w http.ResponseWriter, r *http.Request, action string) {
	ctx := r.Context()
	name := r.PathValue("name")
	namespace := r.PathValue("namespace")

	var schedule v1alpha1.OffhoursSchedule
	
	err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &schedule)
	
	if err == nil {
		if action == "sleep" {
			schedule.Status.CurrentState = "MANUAL_SLEEP"
			log.Printf("[WEB] Manual SLEEP requested for schedule %s/%s", namespace, name)
		} else {
			schedule.Status.CurrentState = "MANUAL_WAKE"
			log.Printf("[WEB] Manual WAKE requested for schedule %s/%s", namespace, name)
		}
		
		if err := s.k8sClient.Status().Update(ctx, &schedule); err != nil {
			log.Printf("[ERROR] Failed to update status for schedule %s/%s: %v", namespace, name, err)
		}
	} else {
		log.Printf("[ERROR] Schedule %s/%s not found for manual action: %v", namespace, name, err)
	}

	time.Sleep(300 * time.Millisecond)
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) findScheduleForTarget(ctx context.Context, namespace, targetName string) (*v1alpha1.OffhoursSchedule, error) {
	var list v1alpha1.OffhoursScheduleList
	if err := s.k8sClient.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for _, sch := range list.Items {
		for _, ref := range sch.Spec.TargetRefs {
			if ref.Name == targetName {
				return &sch, nil
			}
		}
	}

	return nil, fmt.Errorf("no schedule found managing target %s", targetName)
}