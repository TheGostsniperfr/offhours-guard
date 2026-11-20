package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// projectNamespaceLabel is set by cnp-project-base on the system namespace and
// on every app namespace of a project.
const projectNamespaceLabel = "cnp.3istor.com/project"

//go:embed templates/*
var templateFS embed.FS

type Server struct {
	k8sClient client.Client
	// Uncached: Services and Namespaces are read once per health probe, which
	// is cheaper than a cluster-wide informer in a 48Mi pod.
	apiReader    client.Reader
	auth         *Authenticator
	projectName  string
	healthClient *http.Client
	templates    *template.Template
}

// NewServer serves the dashboard behind auth. projectName, when set, limits
// health probes to that project's namespaces; auth nil keeps every route but
// /health closed.
func NewServer(k8sClient client.Client, apiReader client.Reader, auth *Authenticator, projectName string) *Server {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatalf("[FATAL] Failed to parse embedded templates: %v", err)
	}

	return &Server{
		k8sClient:    k8sClient,
		apiReader:    apiReader,
		auth:         auth,
		projectName:  projectName,
		healthClient: &http.Client{Timeout: 2 * time.Second},
		templates:    tmpl,
	}
}

func (s *Server) Start(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.auth.requireMember(s.handleDashboard))
	// Gatus calls it in-cluster without a user: it stays open, and only ever
	// probes a Service of this project on a port the Service declares.
	mux.HandleFunc("GET /health/{namespace}/{name}", s.handleGatusProxy)
	mux.HandleFunc("POST /apps/{namespace}/{name}/wake", s.auth.requireMember(s.handleWakeOverride))
	mux.HandleFunc("POST /apps/{namespace}/{name}/sleep", s.auth.requireMember(s.handleSleepOverride))

	return mux
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

	if v1alpha1.IsAsleep(schedule.Status.CurrentState) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"SLEEPING","info":"Service is stopped on schedule (Managed by Offhours-Guard)"}`))
		return
	}

	s.checkRealAppHealth(w, r, name, namespace)
}

var errHealthTargetRejected = errors.New("health target rejected")

// healthTargetURL builds the probe URL from the cluster, not from the caller:
// the host is a Service of this project and the port one it declares. Only
// the path on that Service comes from the query.
func (s *Server) healthTargetURL(ctx context.Context, namespace, name, portParam, pathParam string) (string, error) {
	if len(validation.IsDNS1123Label(namespace)) > 0 || len(validation.IsDNS1123Label(name)) > 0 {
		return "", fmt.Errorf("%w: invalid namespace or name", errHealthTargetRejected)
	}

	if s.projectName != "" {
		var ns corev1.Namespace
		if err := s.apiReader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			return "", fmt.Errorf("%w: namespace %s: %v", errHealthTargetRejected, namespace, err)
		}
		if ns.Labels[projectNamespaceLabel] != s.projectName {
			return "", fmt.Errorf("%w: namespace %s is not in project %s", errHealthTargetRejected, namespace, s.projectName)
		}
	}

	var svc corev1.Service
	if err := s.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: service %s/%s not found", errHealthTargetRejected, namespace, name)
		}
		return "", fmt.Errorf("get service %s/%s: %w", namespace, name, err)
	}
	if len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("%w: service %s/%s declares no port", errHealthTargetRejected, namespace, name)
	}

	port := svc.Spec.Ports[0].Port
	if portParam != "" {
		requested, err := strconv.ParseInt(portParam, 10, 32)
		if err != nil || !serviceDeclaresPort(&svc, int32(requested)) {
			return "", fmt.Errorf("%w: port %q is not declared by service %s/%s", errHealthTargetRejected, portParam, namespace, name)
		}
		port = int32(requested)
	}

	if pathParam == "" {
		pathParam = "/"
	}
	target, err := url.Parse(pathParam)
	if err != nil || !strings.HasPrefix(pathParam, "/") || strings.HasPrefix(pathParam, "//") ||
		target.Scheme != "" || target.Host != "" || target.User != nil {
		return "", fmt.Errorf("%w: path must be an absolute path", errHealthTargetRejected)
	}

	probe := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace), strconv.Itoa(int(port))),
		Path:     target.Path,
		RawPath:  target.RawPath,
		RawQuery: target.RawQuery,
	}
	return probe.String(), nil
}

func serviceDeclaresPort(svc *corev1.Service, port int32) bool {
	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			return true
		}
	}
	return false
}

func (s *Server) checkRealAppHealth(w http.ResponseWriter, r *http.Request, name, namespace string) {
	w.Header().Set("Content-Type", "application/json")

	probeURL, err := s.healthTargetURL(r.Context(), namespace, name, r.URL.Query().Get("port"), r.URL.Query().Get("path"))
	if err != nil {
		log.Printf("[WEB] Health probe refused for %s/%s: %v", namespace, name, err)
		if errors.Is(err, errHealthTargetRejected) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"UNKNOWN","error":"Not a monitored service of this project"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"UNKNOWN","error":"Could not resolve the service"}`))
		return
	}

	resp, err := s.healthClient.Get(probeURL)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"UNHEALTHY","error":"App is down or unreachable"}`))
		return
	}
	defer resp.Body.Close()

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
	// The session is a cookie, so a cross-site form could post here. Browsers
	// only let the page's own scripts add this header without a CORS preflight.
	if r.Header.Get("HX-Request") != "true" {
		http.Error(w, "missing HX-Request header", http.StatusForbidden)
		return
	}

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