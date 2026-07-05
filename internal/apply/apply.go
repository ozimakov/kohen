// Package apply implements Kohen's Server-Side Apply engine for objects it fully
// owns (ConfigMaps and, in Phase 2, Kohen-applied secret manifests).
//
// It enforces the SPEC contract for owned objects:
//   - all writes use SSA with the dedicated field manager "kohen" (R-WIRE.1, T3);
//   - every applied object carries ownership labels and a controller owner
//     reference for GC (R8.8);
//   - Kohen never adopts or overwrites a pre-existing object it does not own —
//     there is no adoption mode (R8.8);
//   - objects that vanish from the desired set are pruned (R8.8).
//
// Workload wiring (partial ownership, GitOps coexistence) is a different concern
// handled in S1.6 and does NOT use this fully-owning applier.
package apply

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// FieldManager is Kohen's dedicated SSA field manager (R-WIRE.1).
	FieldManager = "kohen"
	// ManagedByLabel/Value mark objects Kohen manages (R8.8).
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "kohen"
	// OwnerLabel scopes pruning to a single ConfigSync (namespace-local).
	OwnerLabel = "kohen.dev/owner"
)

// Reason classifies apply errors for status mapping.
type Reason string

const (
	// ReasonAlreadyExistsNotOwned means a pre-existing object would be adopted;
	// refused per R8.8.
	ReasonAlreadyExistsNotOwned Reason = "AlreadyExistsNotOwned"
	// ReasonApplyFailed is a generic apply/prune failure.
	ReasonApplyFailed Reason = "ApplyFailed"
)

// Error is a typed apply error.
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

// ReasonOf extracts the Reason from an error, if present.
func ReasonOf(err error) (Reason, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}

// Applier server-side-applies and prunes Kohen-owned objects.
type Applier struct {
	client       client.Client
	scheme       *runtime.Scheme
	fieldManager string
}

// New returns an Applier bound to c and scheme.
func New(c client.Client, scheme *runtime.Scheme) *Applier {
	return &Applier{client: c, scheme: scheme, fieldManager: FieldManager}
}

// Apply server-side-applies obj as an object fully owned by owner. It stamps
// ownership labels and a controller owner reference, and refuses to overwrite a
// pre-existing object that Kohen does not already own (R8.8).
func (a *Applier) Apply(ctx context.Context, owner client.Object, obj client.Object) error {
	if err := a.ensureGVK(obj); err != nil {
		return &Error{Reason: ReasonApplyFailed, Msg: "resolving object kind", Err: err}
	}

	// No-adoption guard: if the object already exists and is not owned by this
	// ConfigSync, refuse rather than hijack a user- or third-party-managed object.
	existing := obj.DeepCopyObject().(client.Object)
	switch err := a.client.Get(ctx, client.ObjectKeyFromObject(obj), existing); {
	case err == nil:
		if !ownedBy(existing, owner) {
			return &Error{
				Reason: ReasonAlreadyExistsNotOwned,
				Msg: fmt.Sprintf("%s %q already exists and is not owned by Kohen; refusing to adopt",
					obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName()),
			}
		}
	case apierrors.IsNotFound(err):
		// create path
	default:
		return &Error{Reason: ReasonApplyFailed, Msg: "checking existing object", Err: err}
	}

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedByLabel] = ManagedByValue
	labels[OwnerLabel] = owner.GetName()
	obj.SetLabels(labels)

	if err := controllerutil.SetControllerReference(owner, obj, a.scheme); err != nil {
		return &Error{Reason: ReasonApplyFailed, Msg: "setting owner reference", Err: err}
	}

	// SSA: clear managed fields and resourceVersion so this is a pure apply.
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	if err := a.client.Patch(ctx, obj, client.Apply,
		client.FieldOwner(a.fieldManager), client.ForceOwnership); err != nil {
		return &Error{Reason: ReasonApplyFailed, Msg: "server-side apply", Err: err}
	}
	return nil
}

// Prune deletes objects of the list's kind that Kohen owns for this owner but
// whose names are not in keep. The caller supplies an empty typed list (e.g.
// &corev1.ConfigMapList{}).
func (a *Applier) Prune(ctx context.Context, owner client.Object, list client.ObjectList, keep ...string) error {
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}

	if err := a.client.List(ctx, list,
		client.InNamespace(owner.GetNamespace()),
		client.MatchingLabels{ManagedByLabel: ManagedByValue, OwnerLabel: owner.GetName()},
	); err != nil {
		return &Error{Reason: ReasonApplyFailed, Msg: "listing owned objects", Err: err}
	}

	items, err := meta.ExtractList(list)
	if err != nil {
		return &Error{Reason: ReasonApplyFailed, Msg: "extracting list", Err: err}
	}
	for _, it := range items {
		o, ok := it.(client.Object)
		if !ok {
			continue
		}
		if _, keepIt := keepSet[o.GetName()]; keepIt {
			continue
		}
		if err := a.client.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			return &Error{Reason: ReasonApplyFailed, Msg: "pruning " + o.GetName(), Err: err}
		}
	}
	return nil
}

func (a *Applier) ensureGVK(obj client.Object) error {
	if !obj.GetObjectKind().GroupVersionKind().Empty() {
		return nil
	}
	gvks, _, err := a.scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}
	if len(gvks) == 0 {
		return fmt.Errorf("no registered kind for %T", obj)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	return nil
}

// ownedBy reports whether existing is already owned by Kohen for this owner,
// via both the ownership label and a controller owner reference (by UID).
func ownedBy(existing, owner client.Object) bool {
	labels := existing.GetLabels()
	if labels[ManagedByLabel] != ManagedByValue || labels[OwnerLabel] != owner.GetName() {
		return false
	}
	if ctrl := metav1.GetControllerOf(existing); ctrl != nil && ctrl.UID == owner.GetUID() {
		return true
	}
	// Label-owned but controller ref not yet observed (e.g. mid-migration): treat
	// as owned only if the owner UID is empty (owner not persisted) — otherwise
	// require the controller ref to match to avoid cross-owner hijack.
	return owner.GetUID() == ""
}
