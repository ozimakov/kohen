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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/wire"
)

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

// Version returns the config version for a resolved commit. In Phase 1 (no
// env-surfaced secrets) the normative format is "git:<short-commit>" (R-CONS);
// Phase 2 appends "-sec:<hash>" when secrets are env-surfaced.
func Version(commit string) string {
	return "git:" + ShortCommit(commit)
}

// StatefulSetSupported reports whether a StatefulSet's update strategy can be
// driven by a version stamp. OnDelete is unsupported (R-ROLLOUT.5).
func StatefulSetSupported(s *appsv1.StatefulSet) (bool, string) {
	if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return false, "StatefulSet uses the OnDelete update strategy, which cannot be triggered by a version stamp"
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
	return c.Patch(ctx, obj, client.Apply, client.FieldOwner(wire.FieldManager))
}
