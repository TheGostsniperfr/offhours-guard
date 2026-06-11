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

// Start launches the Go 1.26+ enhanced HTTP router
func (s *Server) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	// Pro Go 1.26 Routing syntax
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

type AppCardData struct {
	Name             string
	Namespace        string
	State            string
	OriginalReplicas int32
	CurrentReplicas  int32
	SleepAt          string
	WakeAt           string
	Timezone         string
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
		var deploy appsv1.Deployment
		_ = s.k8sClient.Get(ctx, types.NamespacedName{Namespace: sch.Namespace, Name: sch.Spec.TargetRef.Name}, &deploy)

		current := int32(0)
		if deploy.Spec.Replicas != nil {
			current = *deploy.Spec.Replicas
		}

		cards = append(cards, AppCardData{
			Name:             sch.Spec.TargetRef.Name,
			Namespace:        sch.Namespace,
			State:            sch.Status.CurrentState,
			OriginalReplicas: sch.Status.OriginalReplicas,
			CurrentReplicas:  current,
			SleepAt:          sch.Spec.SleepAt,
			WakeAt:           sch.Spec.WakeAt,
			Timezone:         sch.Spec.Timezone,
		})
	}

	w.Header().Set("Content-Type", "text/html")
	_ = s.templates.ExecuteTemplate(w, "index.html", cards)
}

func (s *Server) handleGatusProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	namespace := r.PathValue("namespace")

	var schedule v1alpha1.OffhoursSchedule
	err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name + "-schedule"}, &schedule)

	// If no schedule exists, we simply report actual app health
	if err != nil {
		s.checkRealAppHealth(w, r, name, namespace)
		return
	}

	// 1. If the app is scheduled to sleep, we tell Gatus that everything is fine (Honest 200 OK)
	if schedule.Status.CurrentState == "SLEEPING" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"SLEEPING","info":"Service is stopped on schedule (Managed by Offhours-Guard)"}`))
		return
	}

	// 2. Otherwise, we check if the actual app is healthy and running
	s.checkRealAppHealth(w, r, name, namespace)
}

func (s *Server) checkRealAppHealth(w http.ResponseWriter, r *http.Request, name, namespace string) {
	// Querying the internal Kubernetes Service address of the app
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local", name, namespace)
	
	// Fallback to public domain during local development outside of the K3s cluster
	if r.Host == "localhost:8082" || r.Host == "127.0.0.1:8082" {
		url = fmt.Sprintf("https://%s.3istor.com", name)
	}

	clientHttp := http.Client{Timeout: 3 * time.Second}
	resp, err := clientHttp.Get(url)

	w.Header().Set("Content-Type", "application/json")
	if err != nil || resp.StatusCode >= 500 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"UNHEALTHY","error":"App is down or unreachable"}`))
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

	// Update the CRD State to MANUAL_SLEEP or MANUAL_WAKE. 
	var schedule v1alpha1.OffhoursSchedule
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name + "-schedule"}, &schedule); err == nil {
		if action == "sleep" {
			schedule.Status.CurrentState = "MANUAL_SLEEP"
		} else {
			schedule.Status.CurrentState = "MANUAL_WAKE"
		}
		_ = s.k8sClient.Status().Update(ctx, &schedule)
	}

	// Let Kubernetes process the state change event
	time.Sleep(300 * time.Millisecond)

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}