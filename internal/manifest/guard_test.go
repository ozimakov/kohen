package manifest_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ozimakov/kohen/internal/manifest"
)

func externalSecret(name, ns, store string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("external-secrets.io/v1beta1")
	u.SetKind("ExternalSecret")
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	if store != "" {
		_ = unstructured.SetNestedField(u.Object, store, "spec", "secretStoreRef", "name")
	}
	return u
}

func TestGuardAcceptsLocalAllowedStore(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault"}}
	if err := g.Check(externalSecret("db", "team-a", "vault")); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}
	// Empty namespace is allowed (defaulted at apply time).
	if err := g.Check(externalSecret("db", "", "vault")); err != nil {
		t.Errorf("namespace-less manifest rejected: %v", err)
	}
}

func TestGuardRejectsForeignNamespace(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a"}
	err := g.Check(externalSecret("db", "team-b", ""))
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonNamespaceViolation {
		t.Fatalf("reason = %v, want NamespaceViolation (err %v)", r, err)
	}
}

func TestGuardRejectsDisallowedKind(t *testing.T) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Secret")
	u.SetName("raw")
	u.SetNamespace("team-a")
	g := manifest.Guard{Namespace: "team-a"}
	err := g.Check(u)
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonKindNotAllowed {
		t.Fatalf("reason = %v, want KindNotAllowed (err %v)", r, err)
	}
}

func TestGuardRejectsClusterScopedSecretCR(t *testing.T) {
	// A ClusterExternalSecret (cluster-scoped) must never be applied from a
	// namespaced path (R-AUTH.4): only ExternalSecret is allow-listed.
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("external-secrets.io/v1beta1")
	u.SetKind("ClusterExternalSecret")
	u.SetName("cluster-es")
	g := manifest.Guard{Namespace: "team-a"}
	err := g.Check(u)
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonKindNotAllowed {
		t.Fatalf("reason = %v, want KindNotAllowed (err %v)", r, err)
	}
}

func TestGuardRejectsDisallowedStore(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault"}}
	err := g.Check(externalSecret("db", "team-a", "rogue-store"))
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonStoreNotAllowed {
		t.Fatalf("reason = %v, want StoreNotAllowed (err %v)", r, err)
	}
}

func TestGuardNoStoreAllowListPermitsAnyStore(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a"}
	if err := g.Check(externalSecret("db", "team-a", "anything")); err != nil {
		t.Errorf("with no store allow-list, any store is permitted: %v", err)
	}
}

// TestGuardRejectsDisallowedPerEntryStore: a benign top-level secretStoreRef
// must not mask a disallowed per-key sourceRef.storeRef (R-AUTH.4/TM2).
func TestGuardRejectsDisallowedPerEntryStore(t *testing.T) {
	u := externalSecret("db", "team-a", "vault")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"secretKey": "password",
			"sourceRef": map[string]any{
				"storeRef": map[string]any{"name": "rogue-store", "kind": "SecretStore"},
			},
		},
	}, "spec", "data")
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault"}}
	err := g.Check(u)
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonStoreNotAllowed {
		t.Fatalf("reason = %v, want StoreNotAllowed for per-entry store bypass (err %v)", r, err)
	}
}

// TestGuardAcceptsAllPerEntryStoresAllowed: when every referenced store (top
// level and per-entry) is allow-listed, the manifest passes.
func TestGuardAcceptsAllPerEntryStoresAllowed(t *testing.T) {
	u := externalSecret("db", "team-a", "vault")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"secretKey": "password",
			"sourceRef": map[string]any{"storeRef": map[string]any{"name": "vault2"}},
		},
	}, "spec", "dataFrom")
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault", "vault2"}}
	if err := g.Check(u); err != nil {
		t.Errorf("all stores allow-listed but rejected: %v", err)
	}
}

// TestGuardRejectsGeneratorWhenStoreAllowListSet: a generatorRef has no store
// to allow-list, so it fails closed under a store policy (R-AUTH.4).
func TestGuardRejectsGeneratorWhenStoreAllowListSet(t *testing.T) {
	u := externalSecret("db", "team-a", "")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"sourceRef": map[string]any{
				"generatorRef": map[string]any{"apiVersion": "generators.external-secrets.io/v1alpha1", "kind": "Password", "name": "pw"},
			},
		},
	}, "spec", "data")
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault"}}
	err := g.Check(u)
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonStoreNotAllowed {
		t.Fatalf("reason = %v, want StoreNotAllowed for generatorRef (err %v)", r, err)
	}
}

// TestGuardRejectsNoStoreRefWhenAllowListSet: a manifest with no verifiable
// store reference cannot be confirmed and fails closed under a store policy.
func TestGuardRejectsNoStoreRefWhenAllowListSet(t *testing.T) {
	u := externalSecret("db", "team-a", "") // no top-level store, no data
	g := manifest.Guard{Namespace: "team-a", StoreAllowList: []string{"vault"}}
	err := g.Check(u)
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonStoreNotAllowed {
		t.Fatalf("reason = %v, want StoreNotAllowed for missing store (err %v)", r, err)
	}
}

func TestGuardRejectsNameless(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a"}
	err := g.Check(externalSecret("", "team-a", "vault"))
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonNameMissing {
		t.Fatalf("reason = %v, want ManifestInvalid (err %v)", r, err)
	}
}
