// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package main

// nolint:gci
import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/pflag"
	extpolicyv1 "github.com/stolostron/governance-policy-propagator/api/v1"
	apiRuntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"open-cluster-management.io/addon-framework/pkg/lease"

	// Import all Kubernetes client auth plugins to ensure that exec-entrypoint and run can make use of them.
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	policyv1 "github.com/stolostron/cert-policy-controller/api/v1"
	controllers "github.com/stolostron/cert-policy-controller/controllers"
	"github.com/stolostron/cert-policy-controller/pkg/common"
	"github.com/stolostron/cert-policy-controller/version"
	//+kubebuilder:scaffold:imports
)

var (
	metricsHost       = "0.0.0.0"
	metricsPort int32 = 8383
	scheme            = apiRuntime.NewScheme()
	setupLog          = ctrl.Log.WithName("setup")
)

var log = logf.Log.WithName("cmd")

// errNoNamespace indicates that a namespace could not be found for the current
// environment. This was taken from operator-sdk v0.19.4.
var errNoNamespace = fmt.Errorf("namespace not found for current environment")

func printVersion() {
	log.Info(fmt.Sprintf("Operator Version: %s", version.Version))
	log.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
}

// nolint:wsl
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(extpolicyv1.AddToScheme(scheme))

	utilruntime.Must(policyv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// getWatchNamespace returns the Namespace the operator should be watching for changes.
// This was taken from operator-sdk v0.19.4.
func getWatchNamespace() (string, error) {
	// WatchNamespaceEnvVar is the constant for env variable WATCH_NAMESPACE
	// which specifies the Namespace to watch.
	// An empty value means the operator is running with cluster scope.
	watchNamespaceEnvVar := "WATCH_NAMESPACE"

	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", watchNamespaceEnvVar)
	}

	return ns, nil
}

// getOperatorNamespace returns the namespace the operator should be running in.
// This was partially taken from operator-sdk v0.19.4.
func getOperatorNamespace() (string, error) {
	nsBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		if os.IsNotExist(err) {
			return "", errNoNamespace
		}

		return "", fmt.Errorf("failed to retrieve operator namespace: %w", err)
	}

	ns := strings.TrimSpace(string(nsBytes))
	log.V(1).Info("Found namespace", "Namespace", ns)

	return ns, nil
}

func startLeaseController(generatedClient kubernetes.Interface, hubConfigSecretName string,
	hubConfigSecretNs string, clusterName string) {
	operatorNs, err := getOperatorNamespace()
	if err != nil {
		if errors.Is(err, errNoNamespace) {
			log.Info("Skipping lease; not running in a cluster.")
		} else {
			log.Error(err, "Failed to get operator namespace")
			os.Exit(1)
		}
	} else {
		hubCfg, _ := common.LoadHubConfig(hubConfigSecretNs, hubConfigSecretName)

		log.Info("Starting lease controller to report status")
		leaseUpdater := lease.NewLeaseUpdater(
			generatedClient,
			"cert-policy-controller",
			operatorNs,
		).WithHubLeaseConfig(hubCfg, clusterName)

		go leaseUpdater.Start(context.TODO())
	}
}

func main() {
	var eventOnParent, defaultDuration, clusterName, hubConfigSecretNs, hubConfigSecretName, probeAddr string
	var frequency uint
	var enableLease, enableLeaderElection, legacyLeaderElection bool

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	//nolint:gomnd
	pflag.UintVar(&frequency, "update-frequency", 10,
		"The status update frequency (in seconds) of a mutation policy",
	)
	pflag.StringVar(&eventOnParent, "parent-event", "ifpresent",
		"to also send status events on parent policy. options are: yes/no/ifpresent",
	)
	pflag.StringVar(&defaultDuration, "default-duration", "672h",
		"The default minimum duration allowed for certificatepolicies to be compliant, must be in golang time format",
	)
	pflag.BoolVar(&enableLease, "enable-lease", false,
		"If enabled, the controller will start the lease controller to report its status",
	)
	pflag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.",
	)
	pflag.BoolVar(&legacyLeaderElection, "legacy-leader-elect", false,
		"Use a legacy leader election method for controller manager instead of the lease API.",
	)
	pflag.StringVar(&hubConfigSecretNs, "hubconfig-secret-ns",
		"open-cluster-management-agent-addon", "Namespace for hub config kube-secret",
	)
	pflag.StringVar(&hubConfigSecretName, "hubconfig-secret-name",
		"cert-policy-controller-hub-kubeconfig", "Name of the hub config kube-secret",
	)
	pflag.StringVar(&clusterName, "cluster-name", "default-cluster", "Name of the cluster")
	flag.StringVar(&probeAddr, "health-probe-bind-address",
		":8081", "The address the probe endpoint binds to.",
	)

	pflag.Parse()

	var duration time.Duration

	logf.SetLogger(zap.New())
	printVersion()

	namespace, err := getWatchNamespace()
	if err != nil {
		log.Error(err, "Failed to get watch namespace")
		os.Exit(1)
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "")
		os.Exit(1)
	}

	options := ctrl.Options{
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "cert-policy-controller.open-cluster-management.io",
		MetricsBindAddress:     fmt.Sprintf("%s:%d", metricsHost, metricsPort),
		Namespace:              namespace,
		Scheme:                 scheme,
	}

	if strings.Contains(namespace, ",") {
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(namespace, ","))
	}

	if legacyLeaderElection {
		// If legacyLeaderElection is enabled, then that means the lease API is not available.
		// In this case, use the legacy leader election method of a ConfigMap.
		options.LeaderElectionResourceLock = "configmaps"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	setupLog.Info("Registering components")

	if err = (&controllers.Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("certificatepolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CertificatePolicy")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	var generatedClient kubernetes.Interface = kubernetes.NewForConfigOrDie(mgr.GetConfig())

	common.Initialize(&generatedClient, cfg)
	_ = controllers.Initialize(&generatedClient, mgr, namespace, eventOnParent, duration) /* #nosec G104 */
	// PeriodicallyExecCertificatePolicies is the go-routine that periodically checks the policies and
	// does the needed work to make sure the desired state is achieved
	go controllers.PeriodicallyExecCertificatePolicies(frequency, true)

	if enableLease {
		startLeaseController(generatedClient, hubConfigSecretName, hubConfigSecretNs, clusterName)
	} else {
		log.Info("Status reporting is not enabled")
	}

	setupLog.Info("Starting the manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
