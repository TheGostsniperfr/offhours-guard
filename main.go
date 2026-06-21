package main

import (
	"context"
	"log"
	"os"
	"strings"
	"k8s.io/client-go/kubernetes"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	"github.com/thegostsniperfr/offhours-guard/internal/controller"
	"github.com/thegostsniperfr/offhours-guard/pkg/web"
	"github.com/robfig/cron/v3"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/labels"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// 1. Setup Scheme
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	
	scheme.AddKnownTypes(v1alpha1.GroupVersion, &v1alpha1.OffhoursSchedule{}, &v1alpha1.OffhoursScheduleList{})
	metav1.AddToGroupVersion(scheme, v1alpha1.GroupVersion)

	projectName := getProjectNameFromNamespace()
	var cacheOpts cache.Options

	if projectName != "" {
		log.Printf("[INFO] Multi-tenancy active. Filtering cache for project: %s", projectName)
		selector := labels.SelectorFromSet(labels.Set{"project": projectName})
		
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&v1alpha1.OffhoursSchedule{}: {
				Label: selector,
			},
			&appsv1.Deployment{}: {
				Label: selector,
			},
		}
	}

	// 3. Initialize Manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8083",
		},
		Cache: cacheOpts, 
	})
	if err != nil {
		log.Fatalf("Unable to start manager: %v", err)
		os.Exit(1)
	}

	// 4. Register our Controller
	reconciler := &controller.OffhoursScheduleReconciler{
		Client:     mgr.GetClient(),
		CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}

	if err = reconciler.SetupWithManager(mgr); err != nil {
		log.Fatalf("Unable to create controller: %v", err)
		os.Exit(1)
	}

	// 5. Start our Web UI Server
	webServer := web.NewServer(mgr.GetClient())
	ctx := ctrl.SetupSignalHandler()
	
	go func() {
		if err := webServer.Start(ctx, ":8082"); err != nil {
			log.Fatalf("Failed to run Web UI: %v", err)
		}
	}()

	// 6. Start the Operator!
	log.Println("🛡️ Starting Offhours-Guard Operator & Web UI...")
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("Problem running manager: %v", err)
		os.Exit(1)
	}
}

func getProjectNameFromNamespace() string {
	// 1. Read the current namespace injected by Kubernetes into the pod
	nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		log.Println("[INFO] Running outside the cluster or unable to read namespace file, skipping auto-detection.")
		return ""
	}
	namespace := strings.TrimSpace(string(nsBytes))

	// 2. Create a temporary Kubernetes client to query the API
	config, err := ctrl.GetConfig()
	if err != nil {
		log.Printf("[WARN] Failed to get Kubernetes config: %v", err)
		return ""
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("[WARN] Failed to create Kubernetes client: %v", err)
		return ""
	}

	// 3. Retrieve metadata for the current namespace
	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
	if err != nil {
		log.Printf("[WARN] Failed to fetch metadata for namespace %s: %v", namespace, err)
		return ""
	}

	// 4. Read the "project" label
	projectName := ns.Labels["project"]
	return projectName
}