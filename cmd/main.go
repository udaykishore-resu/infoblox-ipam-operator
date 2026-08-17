// Command infoblox-ipam-operator runs the controller manager: it watches
// IPSpaceClaim objects cluster-wide and reconciles them against Infoblox
// Universal DDI. Designed to run as a Deployment with leader election so
// exactly one replica reconciles at a time during upgrades/failover.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ipamv1alpha1 "github.com/udaykishore-resu/infoblox-ipam-operator/api/v1alpha1"
	"github.com/udaykishore-resu/infoblox-ipam-operator/internal/controller"
	"github.com/udaykishore-resu/infoblox-ipam-operator/internal/infoblox"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ipamv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		infobloxBaseURL      string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "enable leader election for controller manager HA")
	flag.StringVar(&infobloxBaseURL, "infoblox-base-url", "https://csp.infoblox.com", "Infoblox Universal DDI Portal base URL")
	flag.Parse()

	logger := zap.New(zap.UseDevMode(false))
	ctrl.SetLogger(logger)

	// Token is read from the environment (mounted via a Secret in-cluster),
	// never accepted as a flag, so it never lands in process listings or
	// Deployment specs in plaintext.
	infobloxToken := os.Getenv("INFOBLOX_API_TOKEN")
	if infobloxToken == "" {
		logger.Error(nil, "INFOBLOX_API_TOKEN environment variable is required")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "infoblox-ipam-operator-lock",
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	infobloxClient := infoblox.NewClient(infobloxBaseURL, infobloxToken)

	if err := (&controller.IPSpaceClaimReconciler{
		Client:         mgr.GetClient(),
		InfobloxClient: infobloxClient,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "IPSpaceClaim")
		os.Exit(1)
	}

	if err := (&controller.DNSRecordClaimReconciler{
		Client:         mgr.GetClient(),
		InfobloxClient: infobloxClient,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "DNSRecordClaim")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting infoblox-ipam-operator", "infobloxBaseURL", infobloxBaseURL)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "problem running manager")
		os.Exit(1)
	}
}
