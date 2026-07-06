package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/manifest"
)

// externalSecretPruneVersions are the ExternalSecret API versions Kohen lists
// when pruning removed manifests. Both are tried (tolerating unserved ones) so
// pruning works whether a cluster serves ESO v1, v1beta1, or both.
var externalSecretPruneVersions = []string{"v1", "v1beta1"}

// manifestOutcome reports the result of the apply-if-present engine to the
// caller so it can decide whether to fail closed terminally (a config/guard
// error the user must fix) or requeue (a transient apply failure).
type manifestOutcome struct {
	ok       bool
	terminal bool
	err      error
}

// applyManifests applies recognized secret manifests (ExternalSecrets) found in
// the config tree as Kohen-owned objects, and prunes owned manifests that were
// removed from git (SPEC §8.2, R8.8). Guard rails (R-AUTH.4/R-AUTH.5) reject
// disallowed kinds, cross-namespace targets, and non-allow-listed secret
// stores, failing closed. It sets the ManifestsApplied condition.
func (r *ConfigSyncReconciler) applyManifests(ctx context.Context, cs *kohenv1alpha1.ConfigSync, dir string) manifestOutcome {
	objs, err := manifest.Load(dir)
	if err != nil {
		msg := r.redactMsg(err.Error())
		setCondition(cs, kohenv1alpha1.ConditionManifestsApplied, metav1.ConditionFalse, kohenv1alpha1.ReasonManifestInvalid, msg)
		r.event(cs, corev1.EventTypeWarning, kohenv1alpha1.ReasonManifestInvalid, err.Error())
		return manifestOutcome{ok: false, terminal: true}
	}

	// Validate the whole set first (all-or-nothing): a guard violation on any
	// manifest fails closed without partially applying its siblings.
	guard := manifest.Guard{Namespace: cs.Namespace, StoreAllowList: r.storeAllowList()}
	for i := range objs {
		if gerr := guard.Check(objs[i].U); gerr != nil {
			reason := guardConditionReason(gerr)
			msg := fmt.Sprintf("%s (%s)", gerr.Error(), objs[i].Source)
			setCondition(cs, kohenv1alpha1.ConditionManifestsApplied, metav1.ConditionFalse, reason, r.redactMsg(msg))
			r.event(cs, corev1.EventTypeWarning, reason, msg)
			return manifestOutcome{ok: false, terminal: true}
		}
	}

	applied := make([]string, 0, len(objs))
	for i := range objs {
		u := objs[i].U
		// Namespace locality: default the (allowed) empty namespace to the
		// ConfigSync's so the owned object is created in-namespace (R-AUTH.5).
		if u.GetNamespace() == "" {
			u.SetNamespace(cs.Namespace)
		}
		if aerr := r.Applier.Apply(ctx, cs, u); aerr != nil {
			reason := kohenv1alpha1.ReasonManifestApplyFailed
			terminal := false
			if ar, ok := apply.ReasonOf(aerr); ok && ar == apply.ReasonAlreadyExistsNotOwned {
				// A pre-existing, un-owned object: terminal until the user
				// resolves it (no adoption, R8.8).
				terminal = true
			}
			setCondition(cs, kohenv1alpha1.ConditionManifestsApplied, metav1.ConditionFalse, reason, r.redactMsg(aerr.Error()))
			r.event(cs, corev1.EventTypeWarning, reason, aerr.Error())
			return manifestOutcome{ok: false, terminal: terminal, err: aerr}
		}
		applied = append(applied, u.GetName())
	}

	// Prune owned ExternalSecrets that were removed from git.
	for _, v := range externalSecretPruneVersions {
		gvk := schema.GroupVersionKind{Group: manifest.ExternalSecretsGroup, Version: v, Kind: manifest.ExternalSecretKind + "List"}
		if perr := r.Applier.PruneKind(ctx, cs, gvk, applied...); perr != nil {
			setCondition(cs, kohenv1alpha1.ConditionManifestsApplied, metav1.ConditionFalse, kohenv1alpha1.ReasonManifestApplyFailed, r.redactMsg(perr.Error()))
			r.event(cs, corev1.EventTypeWarning, kohenv1alpha1.ReasonManifestApplyFailed, perr.Error())
			return manifestOutcome{ok: false, terminal: false, err: perr}
		}
	}

	sort.Strings(applied)
	msg := "no secret manifests to apply"
	if len(applied) > 0 {
		msg = fmt.Sprintf("applied %d secret manifest(s): %v", len(applied), applied)
	}
	setCondition(cs, kohenv1alpha1.ConditionManifestsApplied, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, msg)
	return manifestOutcome{ok: true}
}

// storeAllowList returns the operator's secret-store allow-list (R-AUTH.4).
func (r *ConfigSyncReconciler) storeAllowList() []string {
	if r.Config == nil {
		return nil
	}
	return r.Config.SecretStoreAllowList
}

// guardConditionReason maps a manifest guard-rail rejection to a §11.4 reason.
func guardConditionReason(err error) string {
	reason, _ := manifest.GuardReasonOf(err)
	switch reason {
	case manifest.ReasonKindNotAllowed:
		return kohenv1alpha1.ReasonManifestKindNotAllowed
	case manifest.ReasonNamespaceViolation:
		return kohenv1alpha1.ReasonManifestNamespaceViolation
	case manifest.ReasonStoreNotAllowed:
		return kohenv1alpha1.ReasonStoreNotAllowed
	default:
		return kohenv1alpha1.ReasonManifestInvalid
	}
}
