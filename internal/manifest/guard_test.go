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

func TestGuardRejectsNameless(t *testing.T) {
	g := manifest.Guard{Namespace: "team-a"}
	err := g.Check(externalSecret("", "team-a", "vault"))
	if r, _ := manifest.GuardReasonOf(err); r != manifest.ReasonNameMissing {
		t.Fatalf("reason = %v, want ManifestInvalid (err %v)", r, err)
	}
}
