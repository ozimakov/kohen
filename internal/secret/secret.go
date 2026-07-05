// Package secret is Kohen's backend-independent secret resolution framework
// (PLAN S2.2, SPEC §8). It defines the Resolver contract that concrete backends
// (native Secret — S2.3; ESO ExternalSecret — S2.4) implement, and the
// asymmetric readiness state machine that turns per-reference resolutions into
// an aggregate decision the reconciler acts on.
//
// Safety properties enforced here (SPEC §8.5):
//   - Fail closed on unresolved (R8.4): an unready reference blocks advancing to
//     its config version.
//   - Asymmetric readiness (R8.9): a reference with no prior wired-and-rolled
//     version fails closed (never wire crash-bound pods); a reference that has a
//     prior good version fails safe (keep last-good, mark Degraded, requeue).
//   - Bounded degradation (R8.11): once a reference has served last-good for
//     longer than maxDegradedDuration, resolution surfaces MaxDegradedExceeded
//     (security-visible) and still refuses to advance.
//   - Version tokens are metadata-only (R8.10) and only env-surfaced tokens fold
//     into the config version (R8.5, R-CONS) — that folding happens in the
//     reconciler via rollout.VersionWithSecrets using Decision.EnvTokens.
//
// Resolution results never carry secret values, so nothing in this package can
// leak material (R8.3).
package secret

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// Backend identifies a resolution backend.
type Backend string

const (
	// BackendExternalSecret resolves via an External Secrets Operator object.
	BackendExternalSecret Backend = Backend(kohenv1alpha1.BackendExternalSecret)
	// BackendNativeSecret resolves a pre-existing native Secret.
	BackendNativeSecret Backend = Backend(kohenv1alpha1.BackendNativeSecret)
)

// Ref is the normalized input to a Resolver: the backing object name and the
// keys that must be present for the reference to be considered resolved. It
// carries no surfacing detail — surfacing is the reconciler/wire concern.
type Ref struct {
	// Name is the ConfigSync-local reference name (for messages/status).
	Name string
	// SecretName is the backing object name (ExternalSecret name for ESO —
	// whose target Secret shares the name by default — or native Secret name).
	SecretName string
	// RequiredKeys must all be present in the backing Secret for readiness.
	// Env references require their single key; file references require only
	// that the Secret exists (nil/empty).
	RequiredKeys []string
}

// Resolution is a Resolver's per-reference output. It never contains secret
// values (R8.3): VersionToken is derived from metadata only (R8.10).
type Resolution struct {
	// Ready reports whether the backing Secret exists with all required keys
	// and (for ESO) the ExternalSecret reports Ready=True.
	Ready bool
	// SecretName is the resolved backing Secret to wire into the workload.
	SecretName string
	// VersionToken is a metadata-derived rotation token (R8.10). Stable while
	// the secret is unchanged; changes on rotation.
	VersionToken string
	// Reason is a §11.4 SecretsReady reason when not ready (SecretNotFound,
	// KeyMissing, BackendNotReady).
	Reason string
	// Message is a redaction-safe explanation when not ready.
	Message string
}

// Resolver resolves a single reference against the cluster for a namespace.
// Implementations MUST return a non-nil error only for unexpected/transient
// infrastructure failures (e.g. an API error); an absent Secret, a missing key,
// or a not-yet-ready backend are normal states expressed as Resolution{Ready:
// false} with a Reason.
type Resolver interface {
	Resolve(ctx context.Context, namespace string, ref Ref) (Resolution, error)
}

// EvaluatedRef couples a reference's resolution with the surfacing and prior
// state the readiness policy needs.
type EvaluatedRef struct {
	// Name is the reference name (for messages/status).
	Name string
	// EnvSurfaced reports whether the reference is surfaced as an env var
	// (only env-surfaced tokens fold into the config version — R8.5).
	EnvSurfaced bool
	// RolloutOnRotate reports whether env-rotation should advance the version
	// (SPEC R8.5, surface.rolloutOnRotate). Ignored for file surfacing.
	RolloutOnRotate bool
	// Established reports whether this reference has a prior wired-and-rolled
	// good version (drives the asymmetric first-vs-subsequent policy — R8.9).
	Established bool
	// Resolution is the resolver's output for this reference.
	Resolution Resolution
}

// Decision is the aggregate outcome the reconciler acts on.
type Decision struct {
	// AllReady reports whether every reference resolved. When true the
	// reconciler may wire the secret surfaces and stamp the desired version.
	AllReady bool
	// SecretsReady is the SecretsReady condition status.
	SecretsReady bool
	// Reason/Message describe the SecretsReady condition (§11.4).
	Reason  string
	Message string
	// ReadyReason maps the decision to the overall Ready condition reason:
	// Synced when ready, Progressing while awaiting first resolution, Degraded
	// while serving last-good (incl. MaxDegradedExceeded).
	ReadyReason string
	// MaxDegradedExceeded reports that a reference has served last-good beyond
	// maxDegradedDuration; the reconciler emits a security-visible event/metric
	// (R8.11) and still refuses to advance.
	MaxDegradedExceeded bool
	// EnvTokens are the version tokens of env-surfaced references that
	// participate in rollout (rolloutOnRotate=true), populated only when
	// AllReady. Folded into the config version by the reconciler (R-CONS).
	EnvTokens []string
}

// Evaluate applies the asymmetric readiness policy (R8.4/R8.9/R8.11) to the
// evaluated references and returns the aggregate Decision. It is pure: the
// reconciler supplies degradedSince (when the ConfigSync first entered
// serving-last-good for the current outage; zero if not currently degraded),
// maxDegraded, and now.
func Evaluate(refs []EvaluatedRef, degradedSince time.Time, maxDegraded time.Duration, now time.Time) Decision {
	if len(refs) == 0 {
		return Decision{AllReady: true, SecretsReady: true, Reason: kohenv1alpha1.ReasonSynced, ReadyReason: kohenv1alpha1.ReasonSynced,
			Message: "no secret references"}
	}

	var unready, firstUnready []EvaluatedRef
	for _, r := range refs {
		if !r.Resolution.Ready {
			unready = append(unready, r)
			if !r.Established {
				firstUnready = append(firstUnready, r)
			}
		}
	}

	if len(unready) == 0 {
		return Decision{
			AllReady:     true,
			SecretsReady: true,
			Reason:       kohenv1alpha1.ReasonSynced,
			ReadyReason:  kohenv1alpha1.ReasonSynced,
			Message:      fmt.Sprintf("all %d secret references resolved", len(refs)),
			EnvTokens:    envTokens(refs),
		}
	}

	// First-resolution fail-closed dominates: if any unready reference has no
	// prior good version, refuse to wire/stamp/roll (R8.9) so we never roll
	// pods that would crash on a missing secret.
	if len(firstUnready) > 0 {
		return Decision{
			AllReady:     false,
			SecretsReady: false,
			Reason:       kohenv1alpha1.ReasonAwaitingFirstResolution,
			ReadyReason:  kohenv1alpha1.ReasonProgressing,
			Message:      "awaiting first resolution: " + describe(firstUnready),
		}
	}

	// Otherwise every unready reference has a prior good version: fail safe —
	// keep last-good, mark Degraded, requeue (R8.9), bounded by R8.11.
	if !degradedSince.IsZero() && maxDegraded > 0 && now.Sub(degradedSince) > maxDegraded {
		return Decision{
			AllReady:            false,
			SecretsReady:        false,
			Reason:              kohenv1alpha1.ReasonMaxDegradedExceeded,
			ReadyReason:         kohenv1alpha1.ReasonDegraded,
			MaxDegradedExceeded: true,
			Message: fmt.Sprintf("serving last-good for over %s; not advancing: %s",
				maxDegraded, describe(unready)),
		}
	}
	return Decision{
		AllReady:     false,
		SecretsReady: false,
		Reason:       kohenv1alpha1.ReasonDegradedServingLastGood,
		ReadyReason:  kohenv1alpha1.ReasonDegraded,
		Message:      "serving last-good, awaiting recovery: " + describe(unready),
	}
}

// envTokens returns the sorted version tokens of env-surfaced references that
// participate in rollout (rolloutOnRotate=true). Used to fold secret rotation
// into the config version (R-CONS, R8.5).
func envTokens(refs []EvaluatedRef) []string {
	var out []string
	for _, r := range refs {
		if r.EnvSurfaced && r.RolloutOnRotate {
			out = append(out, r.Name+"="+r.Resolution.VersionToken)
		}
	}
	sort.Strings(out)
	return out
}

// describe renders a short, redaction-safe summary of unready references.
func describe(refs []EvaluatedRef) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		reason := r.Resolution.Reason
		if reason == "" {
			reason = kohenv1alpha1.ReasonBackendNotReady
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", r.Name, reason))
	}
	return strings.Join(parts, ", ")
}
