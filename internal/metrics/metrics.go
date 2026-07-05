// Package metrics registers Kohen's Prometheus metrics on the controller-runtime
// registry so every failure state surfaces as a metric, not only logs/conditions
// (SPEC R10.2, R13.1).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconcileTotal counts reconcile outcomes by result (synced|progressing|
	// degraded).
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_reconcile_total",
		Help: "ConfigSync reconcile outcomes by result.",
	}, []string{"result"})

	// FetchErrors counts git fetch/resolve failures by reason (§11.4 Fetched).
	FetchErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_fetch_errors_total",
		Help: "Git fetch/resolve failures by reason.",
	}, []string{"reason"})

	// RenderErrors counts render failures by reason (§11.4 Rendered).
	RenderErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_render_errors_total",
		Help: "Config render failures by reason.",
	}, []string{"reason"})

	// RolloutsTriggered counts version-stamp changes that trigger a rollout.
	RolloutsTriggered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kohen_rollouts_triggered_total",
		Help: "Rollouts triggered by a config-version change.",
	})

	// RolloutsSkipped counts reconciles where the stamp already matched
	// (no spurious rollout, R-ROLLOUT.2).
	RolloutsSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kohen_rollouts_skipped_total",
		Help: "Reconciles where the version stamp already matched.",
	})

	// Degraded is a gauge of currently-degraded ConfigSyncs.
	Degraded = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kohen_configsync_degraded",
		Help: "1 when a ConfigSync is degraded, 0 otherwise.",
	}, []string{"namespace", "name"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal,
		FetchErrors,
		RenderErrors,
		RolloutsTriggered,
		RolloutsSkipped,
		Degraded,
	)
}
