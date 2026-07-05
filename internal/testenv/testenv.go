// Package testenv provides a reusable envtest bootstrap for Tier 2 integration
// tests (PLAN S0.2): a real API server + etcd with Kohen's CRDs installed.
//
// Tests that use it are skipped automatically when KUBEBUILDER_ASSETS is not
// set, so `go test ./...` still passes on machines without envtest binaries.
// CI sets KUBEBUILDER_ASSETS so these tests run there.
package testenv

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// Env is a running test control plane.
type Env struct {
	Cfg    *rest.Config
	Client client.Client
}

// Start boots an envtest control plane with the Kohen CRDs installed and returns
// a client wired to the combined scheme. It registers cleanup on t and skips the
// test if KUBEBUILDER_ASSETS is unset.
func Start(t *testing.T) *Env {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest (Tier 2) test")
	}

	crdPath := filepath.Join(repoRoot(t), "config", "crd", "bases")
	e := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := e.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}

	if err := kohenv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		_ = e.Stop()
		t.Fatalf("registering scheme: %v", err)
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		_ = e.Stop()
		t.Fatalf("creating client: %v", err)
	}

	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Logf("stopping envtest: %v", err)
		}
	})
	return &Env{Cfg: cfg, Client: cl}
}

// repoRoot walks up from the working directory to the module root (the directory
// containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (module root)")
		}
		dir = parent
	}
}
