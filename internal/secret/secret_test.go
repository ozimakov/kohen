package secret_test

import (
	"testing"
	"time"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/secret"
)

func ready(name string, env, rollout, established bool, token string) secret.EvaluatedRef {
	return secret.EvaluatedRef{
		Name: name, EnvSurfaced: env, RolloutOnRotate: rollout, Established: established,
		Resolution: secret.Resolution{Ready: true, VersionToken: token},
	}
}

func notReady(name string, env, established bool, reason string) secret.EvaluatedRef {
	return secret.EvaluatedRef{
		Name: name, EnvSurfaced: env, RolloutOnRotate: true, Established: established,
		Resolution: secret.Resolution{Ready: false, Reason: reason, Message: name + " not ready"},
	}
}

func TestEvaluateNoRefs(t *testing.T) {
	d := secret.Evaluate(nil, time.Time{}, time.Minute, time.Now())
	if !d.AllReady || !d.SecretsReady {
		t.Fatalf("no refs should be ready: %+v", d)
	}
	if d.ReadyReason != kohenv1alpha1.ReasonSynced {
		t.Errorf("ReadyReason = %q, want Synced", d.ReadyReason)
	}
	if len(d.EnvTokens) != 0 {
		t.Errorf("EnvTokens = %v, want empty", d.EnvTokens)
	}
}

func TestEvaluateAllReadyEnvTokens(t *testing.T) {
	refs := []secret.EvaluatedRef{
		ready("db", true, true, false, "tok-db"),    // env, participates
		ready("cache", true, false, false, "tok-c"), // env but rolloutOnRotate=false ⇒ excluded
		ready("tls", false, true, false, "tok-tls"), // file ⇒ excluded
	}
	d := secret.Evaluate(refs, time.Time{}, time.Minute, time.Now())
	if !d.AllReady {
		t.Fatalf("want AllReady, got %+v", d)
	}
	if len(d.EnvTokens) != 1 || d.EnvTokens[0] != "db=tok-db" {
		t.Errorf("EnvTokens = %v, want [db=tok-db]", d.EnvTokens)
	}
}

func TestEvaluateFirstResolutionFailClosed(t *testing.T) {
	refs := []secret.EvaluatedRef{
		ready("db", true, true, true, "tok"),                             // established, ready
		notReady("new", true, false, kohenv1alpha1.ReasonSecretNotFound), // first resolution, not ready
	}
	d := secret.Evaluate(refs, time.Time{}, time.Minute, time.Now())
	if d.AllReady || d.SecretsReady {
		t.Fatalf("first-resolution failure must not be ready: %+v", d)
	}
	if d.Reason != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("Reason = %q, want AwaitingFirstResolution", d.Reason)
	}
	if d.ReadyReason != kohenv1alpha1.ReasonProgressing {
		t.Errorf("ReadyReason = %q, want Progressing", d.ReadyReason)
	}
}

func TestEvaluateSubsequentFailSafe(t *testing.T) {
	refs := []secret.EvaluatedRef{
		notReady("db", true, true, kohenv1alpha1.ReasonBackendNotReady), // established ⇒ fail safe
	}
	now := time.Now()
	d := secret.Evaluate(refs, now.Add(-time.Minute), 15*time.Minute, now)
	if d.AllReady {
		t.Fatalf("want not ready, got %+v", d)
	}
	if d.Reason != kohenv1alpha1.ReasonDegradedServingLastGood {
		t.Errorf("Reason = %q, want DegradedServingLastGood", d.Reason)
	}
	if d.ReadyReason != kohenv1alpha1.ReasonDegraded {
		t.Errorf("ReadyReason = %q, want Degraded", d.ReadyReason)
	}
	if d.MaxDegradedExceeded {
		t.Errorf("MaxDegradedExceeded set too early")
	}
}

func TestEvaluateMaxDegradedExceeded(t *testing.T) {
	refs := []secret.EvaluatedRef{
		notReady("db", true, true, kohenv1alpha1.ReasonBackendNotReady),
	}
	now := time.Now()
	d := secret.Evaluate(refs, now.Add(-20*time.Minute), 15*time.Minute, now)
	if !d.MaxDegradedExceeded {
		t.Fatalf("want MaxDegradedExceeded, got %+v", d)
	}
	if d.Reason != kohenv1alpha1.ReasonMaxDegradedExceeded {
		t.Errorf("Reason = %q, want MaxDegradedExceeded", d.Reason)
	}
	if d.AllReady {
		t.Errorf("must not advance when max degraded exceeded")
	}
}

func TestEvaluateFirstResolutionBeatsFailSafe(t *testing.T) {
	// A mix of an established-unready and a first-resolution-unready ref must
	// fail closed (the stricter policy wins) even past maxDegradedDuration.
	refs := []secret.EvaluatedRef{
		notReady("old", true, true, kohenv1alpha1.ReasonBackendNotReady),
		notReady("new", true, false, kohenv1alpha1.ReasonSecretNotFound),
	}
	now := time.Now()
	d := secret.Evaluate(refs, now.Add(-time.Hour), 15*time.Minute, now)
	if d.Reason != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("Reason = %q, want AwaitingFirstResolution", d.Reason)
	}
	if d.MaxDegradedExceeded {
		t.Errorf("first-resolution path must not set MaxDegradedExceeded")
	}
}
