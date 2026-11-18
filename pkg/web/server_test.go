package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testIssuer  = "https://auth.example.test/realms/shop"
	testProject = "shop"
)

var signingKey = mustRSAKey()

func mustRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

func signToken(t *testing.T, key *rsa.PrivateKey, issuer string, groups []string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"iss":    issuer,
		"sub":    "user-1",
		"aud":    "account",
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iat":    time.Now().Unix(),
		"groups": groups,
	})
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypes(v1alpha1.GroupVersion, &v1alpha1.OffhoursSchedule{}, &v1alpha1.OffhoursScheduleList{})
	metav1.AddToGroupVersion(scheme, v1alpha1.GroupVersion)
	return scheme
}

func projectNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{projectNamespaceLabel: project},
	}}
}

func service(namespace, name string, ports ...int32) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	for _, p := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Port: p})
	}
	return svc
}

// recordingTransport answers every probe with 200 and remembers where it went,
// so a test can assert which address the server tried to reach.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, req.URL.String())
	rt.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func newTestServer(t *testing.T, objects ...client.Object) (*Server, *recordingTransport) {
	t.Helper()
	k8sClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.OffhoursSchedule{}).
		Build()
	keySet := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&signingKey.PublicKey}}
	auth := NewAuthenticator(testIssuer, keySet, []string{"project-members", "project-admins"})

	server := NewServer(k8sClient, k8sClient, auth, testProject)
	transport := &recordingTransport{}
	server.healthClient = &http.Client{Transport: transport}
	return server, transport
}

func serve(server *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func wakeRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/apps/shop-web/web-schedule/wake", nil)
	req.Header.Set("HX-Request", "true")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func sleepingSchedule() *v1alpha1.OffhoursSchedule {
	return &v1alpha1.OffhoursSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "web-schedule", Namespace: "shop-web"},
		Spec:       v1alpha1.OffhoursScheduleSpec{TargetRefs: []v1alpha1.TargetRef{{Name: "web"}}},
		Status:     v1alpha1.OffhoursScheduleStatus{CurrentState: "SLEEPING"},
	}
}

func TestProtectedRoutes_RejectAnonymousCallers(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	routes := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		wakeRequest(""),
		httptest.NewRequest(http.MethodPost, "/apps/shop-web/web-schedule/sleep", nil),
	}

	for _, req := range routes {
		t.Run(req.Method+" "+req.URL.Path, func(t *testing.T) {
			rec := serve(server, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestWake_ReturnsForbidden_WhenCallerIsNotAProjectMember(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	token := signToken(t, signingKey, testIssuer, []string{"project-other-members"})

	rec := serve(server, wakeRequest(token))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestWake_ReturnsUnauthorized_WhenTokenComesFromAnotherRealm(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	token := signToken(t, signingKey, "https://auth.example.test/realms/other", []string{"project-members"})

	rec := serve(server, wakeRequest(token))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWake_ReturnsUnauthorized_WhenTokenIsNotSignedByTheRealm(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	token := signToken(t, mustRSAKey(), testIssuer, []string{"project-members"})

	rec := serve(server, wakeRequest(token))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWake_WakesTheSchedule_WhenCallerIsAProjectMember(t *testing.T) {
	schedule := sleepingSchedule()
	server, _ := newTestServer(t, schedule)
	token := signToken(t, signingKey, testIssuer, []string{"project-members"})

	rec := serve(server, wakeRequest(token))

	var got v1alpha1.OffhoursSchedule
	if err := server.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(schedule), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || got.Status.CurrentState != "MANUAL_WAKE" {
		t.Fatalf("status = %d, state = %q, want 200 and MANUAL_WAKE", rec.Code, got.Status.CurrentState)
	}
}

func TestWake_ReturnsForbidden_WhenRequestDoesNotComeFromTheDashboard(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	req := wakeRequest(signToken(t, signingKey, testIssuer, []string{"project-members"}))
	req.Header.Del("HX-Request")

	rec := serve(server, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestDashboard_IsUnavailable_WhenNoIssuerIsConfigured(t *testing.T) {
	k8sClient := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	server := NewServer(k8sClient, k8sClient, nil, testProject)

	rec := serve(server, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestDashboard_ListsSchedules_WhenCallerIsAProjectAdmin(t *testing.T) {
	server, _ := newTestServer(t, sleepingSchedule())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, signingKey, testIssuer, []string{"/project-admins"}))

	rec := serve(server, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web-schedule") {
		t.Fatalf("status = %d, want 200 with the schedule listed", rec.Code)
	}
}

func TestHealth_ProbesTheDeclaredServicePort_WhenTargetBelongsToTheProject(t *testing.T) {
	server, transport := newTestServer(t, projectNamespace("shop-web", testProject), service("shop-web", "web", 8000))

	rec := serve(server, httptest.NewRequest(http.MethodGet, "/health/shop-web/web?port=8000&path=/healthz", nil))

	want := "http://web.shop-web.svc.cluster.local:8000/healthz"
	if rec.Code != http.StatusOK || len(transport.urls) != 1 || transport.urls[0] != want {
		t.Fatalf("status = %d, probes = %v, want 200 and one probe of %s", rec.Code, transport.urls, want)
	}
}

func TestHealth_MakesNoRequest_WhenCallerChoosesTheAddress(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"undeclared port", "/health/shop-web/web?port=6443"},
		{"namespace of another project", "/health/other-db/db?port=5432"},
		{"namespace outside any project", "/health/kube-system/kube-dns?port=53"},
		{"unknown service", "/health/shop-web/missing?port=8000"},
		{"host smuggled in the name", "/health/shop-web/web.evil.example?port=8000"},
		{"absolute URL as path", "/health/shop-web/web?port=8000&path=http://169.254.169.254/latest"},
		{"authority as path", "/health/shop-web/web?port=8000&path=//169.254.169.254/latest"},
		{"userinfo as path", "/health/shop-web/web?port=8000&path=@169.254.169.254"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, transport := newTestServer(t,
				projectNamespace("shop-web", testProject),
				projectNamespace("other-db", "other"),
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
				service("shop-web", "web", 8000),
				service("other-db", "db", 5432),
				service("kube-system", "kube-dns", 53),
			)

			serve(server, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if len(transport.urls) != 0 {
				t.Fatalf("probed %v, want no request", transport.urls)
			}
		})
	}
}

func TestHealth_ReportsSleeping_WithoutProbing_WhenTargetIsAsleep(t *testing.T) {
	server, transport := newTestServer(t, sleepingSchedule(), projectNamespace("shop-web", testProject), service("shop-web", "web", 8000))

	rec := serve(server, httptest.NewRequest(http.MethodGet, "/health/shop-web/web?port=8000", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SLEEPING") || len(transport.urls) != 0 {
		t.Fatalf("status = %d, body = %s, probes = %v", rec.Code, rec.Body.String(), transport.urls)
	}
}
