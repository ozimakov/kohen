// Package rollout computes the Kohen config version, stamps it per the sync's
// rollout mode, and evaluates rollout progress (SPEC §9, R-CONS, R-VERSION,
// R-ROLLOUT.1–.6).
//
// Version stamping locations (R-VERSION):
//   - rollout: auto — pod-template annotation kohen.dev/config-sha (triggers a
//     rolling restart); performed by package wire during workload wiring.
//   - rollout: none — the workload OBJECT's annotation (no pod-template change,
//     so no restart); performed here by StampNoRestart.
package rollout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// StampFieldManager owns ONLY the workload object-level config-sha annotation
// written in rollout: none mode. It is deliberately distinct from the wire
// package's field manager (which owns the pod-template volume/mount/annotation):
// two Server-Side Apply requests by the *same* manager replace that manager's
// entire owned field set, so sharing a manager between the object-metadata stamp
// and the pod-template wiring would make each apply retract the other's fields
// (stripping the config volume and churning the pod template every reconcile).
const StampFieldManager = "kohen-stamp"

// ShortCommitLen is the number of leading hex characters used in the version
// string (long enough to be collision-resistant, short enough to be readable).
const ShortCommitLen = 12

// ShortCommit returns the abbreviated commit SHA used in version strings.
func ShortCommit(commit string) string {
	if len(commit) <= ShortCommitLen {
		return commit
	}
	return commit[:ShortCommitLen]
}

// SecretHashLen is the number of leading hex characters of the env-surfaced
// secret-token hash used in the config version's "-sec:" component. Long enough
// to be collision-resistant, short enough to stay readable in status/metadata.
const SecretHashLen = 12

// Version returns the config version for a resolved commit with no env-surfaced
// secrets. The normative format is "git:<short-commit>" (R-CONS).
func Version(commit string) string {
	return "git:" + ShortCommit(commit)
}

// VersionWithSecrets returns the config version extended with the env-surfaced
// secret component when envTokens is non-empty:
//
//	git:<short-commit>-sec:<hash of env-surfaced secret version tokens>
//
// per SPEC R-CONS. The tokens are sorted and joined before hashing so the
// result is deterministic and independent of reference ordering. Only
// env-surfaced tokens participate — file-surfaced rotation is delivered by
// kubelet and MUST NOT change the version (R8.5). Tokens are metadata-derived,
// never secret values (R8.10).
func VersionWithSecrets(commit string, envTokens []string) string {
	base := Version(commit)
	if len(envTokens) == 0 {
		return base
	}
	sorted := append([]string(nil), envTokens...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return base + "-sec:" + hex.EncodeToString(sum[:])[:SecretHashLen]
}

// StatefulSetSupported reports whether a StatefulSet's update strategy can be
// driven by a version stamp. OnDelete is unsupported (R-ROLLOUT.5).
func StatefulSetSupported(s *appsv1.StatefulSet) (bool, string) {
	if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return false, "StatefulSet uses the OnDelete update strategy, which cannot be triggered by a version stamp"
	}
	return true, ""
}

// DeploymentSupported reports whether a Deployment's strategy is safe to drive
// with a version stamp. Recreate causes a full-downtime restart on every config
// change, so Kohen refuses to stamp it and surfaces UnsupportedStrategy rather
// than silently taking the workload down (R-ROLLOUT.5).
func DeploymentSupported(d *appsv1.Deployment) (bool, string) {
	if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return false, "Deployment uses the Recreate strategy, which would cause full downtime on every config change; use RollingUpdate or rollout: none"
	}
	return true, ""
}

// Progress describes rollout state, with a reason aligned to §11.4.
type Progress struct {
	Complete bool
	Reason   string
	Message  string
}

func rollingOut(msg string) Progress {
	return Progress{Complete: false, Reason: kohenv1alpha1.ReasonRollingOut, Message: msg}
}

// DeploymentProgress evaluates a Deployment's rollout, mirroring `kubectl rollout
// status` semantics.
func DeploymentProgress(d *appsv1.Deployment) Progress {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing &&
			c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return Progress{Complete: false, Reason: kohenv1alpha1.ReasonProgressDeadlineExceeded, Message: c.Message}
		}
	}
	if d.Status.ObservedGeneration < d.Generation {
		return rollingOut("waiting for the deployment controller to observe the updated generation")
	}
	if d.Status.UpdatedReplicas < desired {
		return rollingOut(fmt.Sprintf("%d of %d replicas updated", d.Status.UpdatedReplicas, desired))
	}
	if d.Status.Replicas > d.Status.UpdatedReplicas {
		return rollingOut(fmt.Sprintf("%d old replicas pending termination", d.Status.Replicas-d.Status.UpdatedReplicas))
	}
	if d.Status.AvailableReplicas < desired {
		return rollingOut(fmt.Sprintf("%d of %d updated replicas available", d.Status.AvailableReplicas, desired))
	}
	return Progress{Complete: true, Reason: kohenv1alpha1.ReasonSynced, Message: "rollout complete"}
}

// StatefulSetProgress evaluates a StatefulSet's rollout.
func StatefulSetProgress(s *appsv1.StatefulSet) Progress {
	desired := int32(1)
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	if s.Status.ObservedGeneration < s.Generation {
		return rollingOut("waiting for the statefulset controller to observe the updated generation")
	}
	if s.Status.UpdatedReplicas < desired {
		return rollingOut(fmt.Sprintf("%d of %d replicas updated", s.Status.UpdatedReplicas, desired))
	}
	if s.Status.ReadyReplicas < desired {
		return rollingOut(fmt.Sprintf("%d of %d replicas ready", s.Status.ReadyReplicas, desired))
	}
	if s.Status.UpdateRevision != "" && s.Status.CurrentRevision != s.Status.UpdateRevision {
		return rollingOut("waiting for the updated revision to become current")
	}
	return Progress{Complete: true, Reason: kohenv1alpha1.ReasonSynced, Message: "rollout complete"}
}

// StampNoRestart records the config version on the workload OBJECT's metadata
// (rollout: none) via SSA of the Kohen-owned annotation, without touching the
// pod template so no restart is triggered (R-VERSION).
func StampNoRestart(ctx context.Context, c client.Client, kind, ns, name, version string) error {
	var gvk schema.GroupVersionKind
	switch kind {
	case "Deployment":
		gvk = appsv1.SchemeGroupVersion.WithKind("Deployment")
	case "StatefulSet":
		gvk = appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	default:
		return fmt.Errorf("unsupported workload kind %q", kind)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ns)
	obj.SetName(name)
	obj.SetAnnotations(map[string]string{kohenv1alpha1.AnnotationConfigSHA: version})
	return c.Patch(ctx, obj, client.Apply, client.FieldOwner(StampFieldManager))
}
