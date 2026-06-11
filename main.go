package main

import (
	"log"
	"os"

	"github.com/thegostsniperfr/offhours-guard/api/v1alpha1"
	"github.com/thegostsniperfr/offhours-guard/internal/controller"
	"github.com/thegostsniperfr/offhours-guard/pkg/web"
	"github.com/robfig/cron/v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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

	// 2. Initialize Manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8083", // Change to 8083 to leave 8082 for our Web UI!
		},
	})
	if err != nil {
		log.Fatalf("Unable to start manager: %v", err)
		os.Exit(1)
	}

	// 3. Register our Controller
	reconciler := &controller.OffhoursScheduleReconciler{
		Client:     mgr.GetClient(),
		CronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}

	if err = reconciler.SetupWithManager(mgr); err != nil {
		log.Fatalf("Unable to create controller: %v", err)
		os.Exit(1)
	}

	// 🔴 4. Start our beautiful Web UI Server
	webServer := web.NewServer(mgr.GetClient())
	ctx := ctrl.SetupSignalHandler()
	
	go func() {
		if err := webServer.Start(ctx, ":8082"); err != nil {
			log.Fatalf("Failed to run Web UI: %v", err)
		}
	}()

	// 5. Start the Operator!
	log.Println("🛡️ Starting Offhours-Guard Operator & Web UI...")
	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("Problem running manager: %v", err)
		os.Exit(1)
	}
}