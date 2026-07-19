// Command operator runs the Kohen controller manager (SPEC §7, §12).
package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/config"
	"github.com/ozimakov/kohen/internal/controller"
	"github.com/ozimakov/kohen/internal/redact"
	"github.com/ozimakov/kohen/internal/version"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kohenv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		enableLeader    bool
		configPath      string
		leaderNamespace string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint.")
	flag.BoolVar(&enableLeader, "leader-elect", false, "Enable leader election for HA.")
	flag.StringVar(&leaderNamespace, "leader-election-namespace", "", "Namespace for the leader election lease (defaults to the pod namespace).")
	flag.StringVar(&configPath, "config", "", "Path to the operator config file (SPEC §12).")
	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	redactor := redact.New()
	ctrl.SetLogger(redact.NewLogger(zap.New(zap.UseFlagOptions(&zapOpts)), redactor))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("kohen operator", "version", version.Version, "commit", version.Commit)

	opCfg, err := config.Load(configPath)
	if err != nil {
		setupLog.Error(err, "loading operator config")
		os.Exit(1)
	}
	setupLog.Info("operator config loaded",
		"sourceAllowList", len(opCfg.SourceAllowList),
		"maxDegradedDuration", opCfg.MaxDegradedDuration.Duration.String())

	mgrOpts := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeader,
		LeaderElectionID:        "kohen-operator.kohen.dev",
		LeaderElectionNamespace: leaderNamespace,
	}
	// Only cache git-credential Secrets, never all Secret material: this bounds
	// the compromised-operator blast radius and memory footprint (SPEC TM8, T6).
	// Kohen only ever reads label-gated credential Secrets (R-AUTH.6).
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Label: labels.SelectorFromSet(labels.Set{kohenv1alpha1.LabelGitCredential: "true"}),
			},
		},
	}
	// Namespaced scope: restrict the cache/watch to a single namespace so the
	// operator needs only namespaced RBAC (SPEC §16 install scopes).
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{ns: {}}
		setupLog.Info("running in namespaced scope", "namespace", ns)
	}
	mgrOpts.Cache = cacheOpts

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "creating manager")
		os.Exit(1)
	}

	if err := (&controller.ConfigSyncReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("configsync-controller"),
		Redactor: redactor,
		Config:   opCfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setting up ConfigSync controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding readyz check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
