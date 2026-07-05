package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/config"
	"github.com/ozimakov/kohen/internal/metrics"
	"github.com/ozimakov/kohen/internal/secret"
)

// resolveSecrets runs the resolution framework over spec.secretRefs, sets the
// per-reference status and the SecretsReady condition, and returns the
// aggregate readiness Decision (SPEC §8, R8.4/R8.9/R8.11). It mutates cs.Status
// but does NOT wire or stamp — the caller gates advancing on Decision.AllReady.
//
// Prior status is read before it is overwritten: a reference is "established"
// (eligible for update fail-safe) only if it was previously resolved AND the
// workload already carries a rolled version; degradedSince is derived from the
// SecretsReady condition's last transition so bounded degradation (R8.11) is
// measured from the start of the outage, not reset each reconcile.
func (r *ConfigSyncReconciler) resolveSecrets(ctx context.Context, cs *kohenv1alpha1.ConfigSync, now time.Time) secret.Decision {
	refs := cs.Spec.SecretRefs
	if len(refs) == 0 {
		cs.Status.SecretRefs = nil
		d := secret.Evaluate(nil, time.Time{}, 0, now)
		setCondition(cs, kohenv1alpha1.ConditionSecretsReady, metav1.ConditionTrue, d.Reason, d.Message)
		return d
	}

	// Establishment is sticky (persisted per reference): a reference is eligible
	// for update fail-safe (R8.9) only once it has been resolved as part of an
	// applied version. Reading current Resolved would incorrectly demote a
	// reference back to first-resolution the moment it goes transiently
	// not-ready, so we carry the prior sticky value forward instead.
	priorEstablished := map[string]bool{}
	for _, s := range cs.Status.SecretRefs {
		priorEstablished[s.Name] = s.Established
	}

	degradedSince := now
	if c := findCondition(cs, kohenv1alpha1.ConditionSecretsReady); c != nil && c.Status == metav1.ConditionFalse {
		degradedSince = c.LastTransitionTime.Time
	}

	maxDegraded := config.DefaultMaxDegradedDuration
	if r.Config != nil {
		maxDegraded = r.Config.MaxDegradedDuration.Duration
	}

	evals := make([]secret.EvaluatedRef, 0, len(refs))
	newStatus := make([]kohenv1alpha1.SecretRefStatus, 0, len(refs))
	for i := range refs {
		ref := &refs[i]
		res := r.resolveOne(ctx, cs.Namespace, ref)
		if !res.Ready {
			metrics.SecretResolveErrors.WithLabelValues(knownResolveReason(res.Reason)).Inc()
		}
		evals = append(evals, secret.EvaluatedRef{
			Name:            ref.Name,
			EnvSurfaced:     ref.Surface.As == kohenv1alpha1.SurfaceEnv,
			RolloutOnRotate: ref.Surface.ShouldRolloutOnRotate(),
			Established:     priorEstablished[ref.Name],
			Resolution:      res,
		})
		st := kohenv1alpha1.SecretRefStatus{
			Name: ref.Name, Resolved: res.Ready, Backend: ref.Backend,
			Established: priorEstablished[ref.Name],
		}
		if !res.Ready {
			st.Reason = res.Reason
		}
		newStatus = append(newStatus, st)
	}

	cs.Status.SecretRefs = newStatus

	decision := secret.Evaluate(evals, degradedSince, maxDegraded, now)

	status := metav1.ConditionTrue
	if !decision.SecretsReady {
		status = metav1.ConditionFalse
	}
	setCondition(cs, kohenv1alpha1.ConditionSecretsReady, status, decision.Reason, r.redactMsg(decision.Message))
	return decision
}

// markSecretsEstablished sets the sticky Established marker on every current
// secret reference. It is called only after the workload has actually been
// wired for the resolved version, so establishment tracks "wired" (not merely
// "resolved") per R8.9 — preventing a reference that resolved but never wired
// (e.g. the workload was missing) from later failing safe and emitting a
// spurious MaxDegradedExceeded security signal.
func markSecretsEstablished(cs *kohenv1alpha1.ConfigSync) {
	for i := range cs.Status.SecretRefs {
		cs.Status.SecretRefs[i].Established = true
	}
}

// knownResolveReason bounds the SecretResolveErrors metric label to the §11.4
// reason set (plus a catch-all) so a backend returning an arbitrary reason
// cannot blow up metric cardinality (R8.3).
func knownResolveReason(reason string) string {
	switch reason {
	case kohenv1alpha1.ReasonSecretNotFound,
		kohenv1alpha1.ReasonKeyMissing,
		kohenv1alpha1.ReasonBackendNotReady,
		kohenv1alpha1.ReasonInvalidSurface:
		return reason
	default:
		return "Other"
	}
}

// resolveOne validates the surface and dispatches to the backend resolver.
func (r *ConfigSyncReconciler) resolveOne(ctx context.Context, ns string, ref *kohenv1alpha1.SecretReference) secret.Resolution {
	if reason, msg := validateSurface(ref); reason != "" {
		return secret.Resolution{Reason: reason, Message: msg}
	}
	resolver := r.Resolvers[secret.Backend(ref.Backend)]
	if resolver == nil {
		return secret.Resolution{
			Reason:  kohenv1alpha1.ReasonBackendNotReady,
			Message: fmt.Sprintf("no resolver configured for backend %q", ref.Backend),
		}
	}
	res, err := resolver.Resolve(ctx, ns, secret.Ref{
		Name:         ref.Name,
		SecretName:   ref.SecretName(),
		RequiredKeys: requiredKeys(ref),
	})
	if err != nil {
		return secret.Resolution{
			Reason:  kohenv1alpha1.ReasonBackendNotReady,
			Message: r.redactMsg(err.Error()),
		}
	}
	return res
}

// requiredKeys returns the Secret data keys that must be present for the
// reference to be considered resolved. Env surfacing requires its single key;
// file surfacing requires only that the Secret exists.
func requiredKeys(ref *kohenv1alpha1.SecretReference) []string {
	if ref.Surface.As == kohenv1alpha1.SurfaceEnv && ref.Surface.Key != "" {
		return []string{ref.Surface.Key}
	}
	return nil
}

// validateSurface verifies that surface.as matches the fields present. CEL
// enforces the field-set exclusivity but cannot reference the reserved `as`
// keyword, so this closes the remaining gap (R11.1). Returns a §11.4-style
// reason and message when invalid, or ("","") when valid.
func validateSurface(ref *kohenv1alpha1.SecretReference) (reason, msg string) {
	s := ref.Surface
	switch s.As {
	case kohenv1alpha1.SurfaceFile:
		if s.MountPath == "" || s.EnvVar != "" || s.Key != "" {
			return kohenv1alpha1.ReasonInvalidSurface,
				fmt.Sprintf("secretRef %q: surface.as=file requires mountPath and no envVar/key", ref.Name)
		}
	case kohenv1alpha1.SurfaceEnv:
		if s.EnvVar == "" || s.Key == "" || s.MountPath != "" {
			return kohenv1alpha1.ReasonInvalidSurface,
				fmt.Sprintf("secretRef %q: surface.as=env requires envVar and key and no mountPath", ref.Name)
		}
	default:
		return kohenv1alpha1.ReasonInvalidSurface,
			fmt.Sprintf("secretRef %q: unknown surface.as %q", ref.Name, s.As)
	}
	return "", ""
}
