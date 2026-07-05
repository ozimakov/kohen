package manifest

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GuardReason classifies a guard-rail rejection for status/condition mapping
// (SPEC R-AUTH.4, R-AUTH.5, TM2/TM3).
type GuardReason string

const (
	// ReasonKindNotAllowed: the manifest is not an allow-listed namespaced kind.
	ReasonKindNotAllowed GuardReason = "ManifestKindNotAllowed"
	// ReasonNamespaceViolation: the manifest targets another namespace (R-AUTH.5).
	ReasonNamespaceViolation GuardReason = "ManifestNamespaceViolation"
	// ReasonStoreNotAllowed: the ExternalSecret's secretStoreRef is not in the
	// operator's allow-list (R-AUTH.4).
	ReasonStoreNotAllowed GuardReason = "StoreNotAllowed"
	// ReasonNameMissing: the manifest has no metadata.name (cannot be owned).
	ReasonNameMissing GuardReason = "ManifestInvalid"
)

// GuardError is a typed guard-rail rejection.
type GuardError struct {
	Reason GuardReason
	Msg    string
}

func (e *GuardError) Error() string { return string(e.Reason) + ": " + e.Msg }

// GuardReasonOf extracts the GuardReason from err, if present.
func GuardReasonOf(err error) (GuardReason, bool) {
	var e *GuardError
	if errors.As(err, &e) {
		return e.Reason, true
	}
	return "", false
}

// Guard enforces the apply-if-present guard rails (SPEC R-AUTH.4/R-AUTH.5,
// TM2/TM3): only namespaced allow-listed kinds (v1: ExternalSecret) may be
// applied, an applied object may not target another namespace, and — when an
// allow-list is configured — its secretStoreRef name must be permitted. A
// cluster-scoped secret CR (no namespace field on its schema) is never
// applicable because only ExternalSecret (namespaced) is allow-listed.
type Guard struct {
	// Namespace is the ConfigSync's namespace; applied objects must live here.
	Namespace string
	// StoreAllowList, when non-empty, restricts permissible secretStoreRef
	// names (R-AUTH.4). Empty means no store restriction.
	StoreAllowList []string
}

// Check validates a single loaded object against the guard rails, returning a
// *GuardError on violation.
func (g Guard) Check(u *unstructured.Unstructured) error {
	gvk := u.GroupVersionKind()
	if gvk.Kind != ExternalSecretKind || gvk.Group != ExternalSecretsGroup {
		return &GuardError{
			Reason: ReasonKindNotAllowed,
			Msg: fmt.Sprintf("manifest %s/%s is not an allow-listed kind (only %s.%s may be applied)",
				gvk.GroupVersion().String(), gvk.Kind, ExternalSecretKind, ExternalSecretsGroup),
		}
	}
	if u.GetName() == "" {
		return &GuardError{Reason: ReasonNameMissing, Msg: "manifest has no metadata.name"}
	}
	// Namespace locality: empty is allowed (defaulted to the ConfigSync ns at
	// apply time); an explicit foreign namespace is rejected (R-AUTH.5).
	if ns := u.GetNamespace(); ns != "" && ns != g.Namespace {
		return &GuardError{
			Reason: ReasonNamespaceViolation,
			Msg: fmt.Sprintf("manifest %q targets namespace %q, but must be in the ConfigSync namespace %q",
				u.GetName(), ns, g.Namespace),
		}
	}
	if len(g.StoreAllowList) > 0 {
		stores, hasGenerator := collectStoreRefs(u)
		// A generator source has no store to allow-list; when a store policy is
		// in force it cannot be verified, so fail closed (R-AUTH.4).
		if hasGenerator {
			return &GuardError{
				Reason: ReasonStoreNotAllowed,
				Msg: fmt.Sprintf("manifest %q uses a generatorRef, which cannot be validated against the secret-store allow-list",
					u.GetName()),
			}
		}
		// No verifiable store reference at all: cannot confirm the source, fail
		// closed rather than apply an unconstrained manifest.
		if len(stores) == 0 {
			return &GuardError{
				Reason: ReasonStoreNotAllowed,
				Msg:    fmt.Sprintf("manifest %q has no secretStoreRef to validate against the allow-list", u.GetName()),
			}
		}
		// Every referenced store — top-level and per-entry sourceRef — must be
		// permitted (R-AUTH.4/TM2): a benign top-level ref must not mask a
		// disallowed per-key override.
		for _, s := range stores {
			if !allowed(s, g.StoreAllowList) {
				return &GuardError{
					Reason: ReasonStoreNotAllowed,
					Msg: fmt.Sprintf("manifest %q references secret store %q which is not in the operator allow-list",
						u.GetName(), s),
				}
			}
		}
	}
	return nil
}

// collectStoreRefs gathers every secret-store name an ExternalSecret references
// — the top-level spec.secretStoreRef and each spec.data[]/spec.dataFrom[]
// sourceRef.storeRef — and reports whether any entry uses a generatorRef (which
// references no store). Enumerating all of them prevents an allow-listed
// top-level store from masking a disallowed per-entry override (R-AUTH.4/TM2).
func collectStoreRefs(u *unstructured.Unstructured) (names []string, hasGenerator bool) {
	if name, found, _ := unstructured.NestedString(u.Object, "spec", "secretStoreRef", "name"); found && name != "" {
		names = append(names, name)
	}
	for _, field := range []string{"data", "dataFrom"} {
		entries, _, _ := unstructured.NestedSlice(u.Object, "spec", field)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			src, ok := em["sourceRef"].(map[string]any)
			if !ok {
				continue
			}
			if _, hasGen := src["generatorRef"]; hasGen {
				hasGenerator = true
			}
			if storeRef, ok := src["storeRef"].(map[string]any); ok {
				if name, ok := storeRef["name"].(string); ok && name != "" {
					names = append(names, name)
				}
			}
		}
	}
	return names, hasGenerator
}

func allowed(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
