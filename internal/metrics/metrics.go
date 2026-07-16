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

	// ReconcileDuration is the wall-clock time of the reconcile pipeline.
	ReconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kohen_reconcile_duration_seconds",
		Help:    "ConfigSync reconcile pipeline duration.",
		Buckets: prometheus.DefBuckets,
	})

	// FetchDuration is the wall-clock time of a git fetch (resolve + checkout).
	FetchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kohen_fetch_duration_seconds",
		Help:    "Git fetch (resolve + checkout) duration.",
		Buckets: prometheus.DefBuckets,
	})

	// ConfigVersionInfo exposes the applied config version per ConfigSync as a
	// gauge set to 1 (labelled with the version), so dashboards can read the
	// currently-applied version (R13.1). Cardinality is bounded by clearing the
	// prior series on each update and on deletion.
	ConfigVersionInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kohen_configsync_config_version_info",
		Help: "Applied config version per ConfigSync (value is always 1).",
	}, []string{"namespace", "name", "version"})

	// SecretResolveErrors counts secret-resolution not-ready states by reason
	// (§11.4 SecretsReady). Label cardinality is bounded to reason names — never
	// secret names or values (R8.3).
	SecretResolveErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_secret_resolve_errors_total",
		Help: "Secret resolution not-ready outcomes by reason.",
	}, []string{"reason"})

	// SecretResolveTotal counts secret-resolution outcomes (success|error|pending)
	// per R13.1. Labels are bounded enums — never secret names or values (R8.3).
	SecretResolveTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_secret_resolve_total",
		Help: "Secret resolution outcomes by result (success|error|pending).",
	}, []string{"result"})

	// MaxDegradedExceededTotal counts occurrences of a ConfigSync serving
	// last-good secrets beyond maxDegradedDuration — a security-visible signal
	// (SPEC R8.11).
	MaxDegradedExceededTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kohen_secret_max_degraded_exceeded_total",
		Help: "Times a ConfigSync exceeded maxDegradedDuration serving last-good secrets.",
	})

	// WireErrors counts WorkloadWired failures by §11.4 reason (R13.1, R10.2).
	WireErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_wire_errors_total",
		Help: "Workload wiring failures by reason.",
	}, []string{"reason"})

	// ProgressDeadlineExceeded counts stuck rollouts (R13.1).
	ProgressDeadlineExceeded = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kohen_progress_deadline_exceeded_total",
		Help: "Times a workload rollout hit ProgressDeadlineExceeded.",
	})

	// ApplyTotal counts owned-object apply outcomes (R13.1).
	ApplyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_apply_total",
		Help: "Owned object apply outcomes by result (success|error).",
	}, []string{"result"})

	// PruneTotal counts owned-object prune outcomes (R13.1).
	PruneTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kohen_prune_total",
		Help: "Owned object prune outcomes by result (success|error).",
	}, []string{"result"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal,
		FetchErrors,
		RenderErrors,
		RolloutsTriggered,
		RolloutsSkipped,
		Degraded,
		ReconcileDuration,
		FetchDuration,
		ConfigVersionInfo,
		SecretResolveErrors,
		SecretResolveTotal,
		MaxDegradedExceededTotal,
		WireErrors,
		ProgressDeadlineExceeded,
		ApplyTotal,
		PruneTotal,
	)
}

// SetConfigVersion records version as the current applied version for the named
// ConfigSync, clearing any previous version series to bound cardinality.
func SetConfigVersion(namespace, name, version string) {
	ConfigVersionInfo.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	if version != "" {
		ConfigVersionInfo.WithLabelValues(namespace, name, version).Set(1)
	}
}

// ClearConfigSync removes all per-object series for a deleted ConfigSync.
func ClearConfigSync(namespace, name string) {
	Degraded.DeleteLabelValues(namespace, name)
	ConfigVersionInfo.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
}
