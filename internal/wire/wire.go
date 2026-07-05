// Package wire merges Kohen-owned fields into a target workload via Server-Side
// Apply, engineered to coexist with GitOps (SPEC §6.2, R-WIRE.1–R-WIRE.6).
//
// Kohen claims ownership of ONLY the fields it injects — its named config
// volume, the target container's volumeMount, and the kohen.dev/config-sha
// annotation — using the dedicated field manager "kohen" and never force-taking
// fields owned by another manager (R-WIRE.4). Unwire retracts exactly those
// fields by applying an empty owned set (R-WIRE.6).
package wire

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// FieldManager is Kohen's dedicated SSA field manager (R-WIRE.1).
const FieldManager = "kohen"

// VolumeName is the deterministic name of the Kohen-injected config volume.
const VolumeName = "kohen-config"

// secretVolumePrefix namespaces file-surfaced secret volumes so they never
// collide with the config volume or unrelated workload volumes.
const secretVolumePrefix = "kohen-secret-"

// SecretVolumeName returns the deterministic volume name for a file-surfaced
// secret reference. refName is a DNS-1123 label (≤50 chars, enforced by the
// API), so the result stays within the 63-char volume-name limit.
func SecretVolumeName(refName string) string {
	return secretVolumePrefix + refName
}

// Reason classifies wiring errors for WorkloadWired status mapping (§11.4).
type Reason string

const (
	ReasonWorkloadNotFound  Reason = "WorkloadNotFound"
	ReasonContainerNotFound Reason = "ContainerNotFound"
	ReasonApplyConflict     Reason = "ApplyConflict"
	ReasonWireFailed        Reason = "WireFailed"
)

// Error is a typed wiring error.
type Error struct {
	Reason Reason
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Reason, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf extracts a wiring Reason from err, if present.
func ReasonOf(err error) (Reason, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}

// SecretFile is a file-surfaced secret reference: the backing Secret is mounted
// as a volume at MountPath in the target container (SPEC §8.4). Kohen never
// uses subPath, so kubelet delivers rotations in place (R8.5).
type SecretFile struct {
	// RefName is the ConfigSync-local reference name; it yields the
	// deterministic volume name via SecretVolumeName.
	RefName string
	// SecretName is the backing Secret to mount.
	SecretName string
	// MountPath is where the Secret is mounted in the target container.
	MountPath string
}

// SecretEnv is an env-surfaced secret reference: a discrete env entry with
// valueFrom.secretKeyRef (SPEC §8.4, R-WIRE.2 — never envFrom).
type SecretEnv struct {
	// EnvVar is the environment variable name injected into the container.
	EnvVar string
	// SecretName is the backing Secret.
	SecretName string
	// Key is the Secret data key exposed via the env var.
	Key string
}

// Spec describes the desired wiring for a workload.
type Spec struct {
	Kind      string // Deployment | StatefulSet
	Name      string
	Namespace string
	Container string // explicit container; empty ⇒ first container
	MountPath string
	ConfigMap string
	// ConfigSHA, when non-empty, is stamped as the kohen.dev/config-sha pod
	// template annotation (version stamp; S1.7).
	ConfigSHA string
	// SecretFiles are file-surfaced secret references mounted as volumes.
	SecretFiles []SecretFile
	// SecretEnv are env-surfaced secret references injected as env entries.
	SecretEnv []SecretEnv
}

// Wirer performs workload wiring via SSA.
type Wirer struct {
	client       client.Client
	fieldManager string
}

// New returns a Wirer bound to c.
func New(c client.Client) *Wirer {
	return &Wirer{client: c, fieldManager: FieldManager}
}

func gvkFor(kind string) (schema.GroupVersionKind, bool) {
	switch kind {
	case "Deployment":
		return appsv1.SchemeGroupVersion.WithKind("Deployment"), true
	case "StatefulSet":
		return appsv1.SchemeGroupVersion.WithKind("StatefulSet"), true
	default:
		return schema.GroupVersionKind{}, false
	}
}

// ResolveContainer reads the workload and returns the effective target container
// name, applying the "first container" default (R-WIRE.3). It also validates the
// workload exists.
func (w *Wirer) ResolveContainer(ctx context.Context, kind, ns, name, requested string) (string, error) {
	gvk, ok := gvkFor(kind)
	if !ok {
		return "", &Error{Reason: ReasonWireFailed, Msg: "unsupported workload kind " + kind}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &Error{Reason: ReasonWorkloadNotFound, Msg: fmt.Sprintf("%s %q not found", kind, name)}
		}
		return "", &Error{Reason: ReasonWireFailed, Msg: "reading workload", Err: err}
	}

	containers, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		return "", &Error{Reason: ReasonContainerNotFound, Msg: "workload has no containers"}
	}
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := cm["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	if requested == "" {
		if len(names) == 0 {
			return "", &Error{Reason: ReasonContainerNotFound, Msg: "workload has no named containers"}
		}
		return names[0], nil
	}
	for _, n := range names {
		if n == requested {
			return requested, nil
		}
	}
	return "", &Error{Reason: ReasonContainerNotFound,
		Msg: fmt.Sprintf("container %q not found in %s %q", requested, kind, name)}
}

// Wire injects the config volume, the target container's volumeMount, and
// (optionally) the version-stamp annotation via SSA of only Kohen-owned fields.
func (w *Wirer) Wire(ctx context.Context, s Spec) error {
	gvk, ok := gvkFor(s.Kind)
	if !ok {
		return &Error{Reason: ReasonWireFailed, Msg: "unsupported workload kind " + s.Kind}
	}
	container, err := w.ResolveContainer(ctx, s.Kind, s.Namespace, s.Name, s.Container)
	if err != nil {
		return err
	}

	obj := w.base(gvk, s.Namespace, s.Name)

	// Volumes: the config ConfigMap plus one Secret volume per file surface.
	volumes := []any{
		map[string]any{
			"name":      VolumeName,
			"configMap": map[string]any{"name": s.ConfigMap},
		},
	}
	// VolumeMounts on the target container: the config mount plus each secret
	// file mount. SSA merges these by name/mountPath so Kohen co-owns exactly
	// its entries alongside another manager's (R-WIRE.2, R-WIRE.4).
	mounts := []any{
		map[string]any{
			"name":      VolumeName,
			"mountPath": s.MountPath,
		},
	}
	for _, f := range s.SecretFiles {
		vol := SecretVolumeName(f.RefName)
		volumes = append(volumes, map[string]any{
			"name":   vol,
			"secret": map[string]any{"secretName": f.SecretName},
		})
		mounts = append(mounts, map[string]any{
			"name":      vol,
			"mountPath": f.MountPath,
			"readOnly":  true,
		})
	}

	targetContainer := map[string]any{
		"name":         container,
		"volumeMounts": mounts,
	}
	// Env: discrete entries with valueFrom.secretKeyRef, merged by name.
	if env := secretEnvEntries(s.SecretEnv); len(env) > 0 {
		targetContainer["env"] = env
	}

	spec := map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"volumes":    volumes,
				"containers": []any{targetContainer},
			},
		},
	}
	if s.ConfigSHA != "" {
		tmpl := spec["template"].(map[string]any)
		tmpl["metadata"] = map[string]any{
			"annotations": map[string]any{
				kohenv1alpha1.AnnotationConfigSHA: s.ConfigSHA,
			},
		}
	}
	obj.Object["spec"] = spec

	return w.apply(ctx, obj, "wiring workload")
}

// Unwire retracts exactly Kohen's owned fields by applying an empty owned set
// (R-WIRE.6), leaving the rest of the workload untouched.
func (w *Wirer) Unwire(ctx context.Context, kind, ns, name string) error {
	gvk, ok := gvkFor(kind)
	if !ok {
		return &Error{Reason: ReasonWireFailed, Msg: "unsupported workload kind " + kind}
	}
	// SSA apply is an upsert; guard against materializing a missing workload.
	// A missing workload during unwire is a no-op — nothing to retract.
	existing := w.base(gvk, ns, name)
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return &Error{Reason: ReasonWireFailed, Msg: "reading workload for unwire", Err: err}
	}

	obj := w.base(gvk, ns, name)
	return w.apply(ctx, obj, "unwiring workload")
}

// secretEnvEntries builds the SSA-mergeable env[] entries for env-surfaced
// secrets. Each entry uses valueFrom.secretKeyRef so the value is delivered by
// the kubelet and never passes through Kohen (R8.3, R-WIRE.2).
func secretEnvEntries(envs []SecretEnv) []any {
	if len(envs) == 0 {
		return nil
	}
	out := make([]any, 0, len(envs))
	for _, e := range envs {
		out = append(out, map[string]any{
			"name": e.EnvVar,
			"valueFrom": map[string]any{
				"secretKeyRef": map[string]any{
					"name": e.SecretName,
					"key":  e.Key,
				},
			},
		})
	}
	return out
}

func (w *Wirer) base(gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ns)
	obj.SetName(name)
	return obj
}

func (w *Wirer) apply(ctx context.Context, obj *unstructured.Unstructured, what string) error {
	// No ForceOwnership: Kohen never steals fields owned by another manager
	// (R-WIRE.4); genuine conflicts surface as ApplyConflict.
	err := w.client.Patch(ctx, obj, client.Apply, client.FieldOwner(w.fieldManager))
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return &Error{Reason: ReasonWorkloadNotFound, Msg: what + ": workload not found"}
	case apierrors.IsConflict(err):
		return &Error{Reason: ReasonApplyConflict, Msg: what + ": field owned by another manager", Err: err}
	default:
		return &Error{Reason: ReasonWireFailed, Msg: what, Err: err}
	}
}
