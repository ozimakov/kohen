package v1alpha1_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/testenv"
)

func validConfigSync(name, ns string) *kohenv1alpha1.ConfigSync {
	return &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source:      kohenv1alpha1.GitSource{URL: "https://github.com/acme/config.git"},
			Path:        "services/app/prod",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
		},
	}
}

func TestConfigSyncDefaults(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	// Create via the minimal-object path a user experiences with `kubectl apply`
	// (optional fields omitted), so the API server applies its schema defaults.
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kohen.dev/v1alpha1",
		"kind":       "ConfigSync",
		"metadata":   map[string]any{"name": "defaults", "namespace": "default"},
		"spec": map[string]any{
			"source":      map[string]any{"url": "https://github.com/acme/config.git"},
			"path":        "services/app/prod",
			"workloadRef": map[string]any{"kind": "Deployment", "name": "app"},
		},
	}}
	if err := env.Client.Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := &kohenv1alpha1.ConfigSync{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "defaults", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Static defaults applied by the API server (SPEC §11.2).
	if got.Spec.Source.Ref != "main" {
		t.Errorf("source.ref = %q, want main", got.Spec.Source.Ref)
	}
	if got.Spec.Wiring.MountPath != "/etc/kohen/config" {
		t.Errorf("wiring.mountPath = %q, want /etc/kohen/config", got.Spec.Wiring.MountPath)
	}
	if got.Spec.Rollout != kohenv1alpha1.RolloutAuto {
		t.Errorf("rollout = %q, want auto", got.Spec.Rollout)
	}
	if got.Spec.Sync.Interval.Duration != 30*time.Second {
		t.Errorf("sync.interval = %v, want 30s", got.Spec.Sync.Interval.Duration)
	}

	// Dynamic default applied in code (SPEC §11.2).
	if name := got.Spec.ConfigMapName(); name != "app-config" {
		t.Errorf("ConfigMapName() = %q, want app-config", name)
	}
}

func TestConfigSyncValidation(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*kohenv1alpha1.ConfigSync)
	}{
		{"missing url", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.Source.URL = "" }},
		{"missing path", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.Path = "" }},
		{"absolute path", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.Path = "/etc/passwd" }},
		{"traversal path", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.Path = "a/../../b" }},
		{"bad workload kind", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.WorkloadRef.Kind = "DaemonSet" }},
		{"bad rollout", func(cs *kohenv1alpha1.ConfigSync) { cs.Spec.Rollout = "sometimes" }},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := validConfigSync("invalid", "default")
			cs.Name = "invalid-" + strconv.Itoa(i)
			tc.mutate(cs)
			if err := env.Client.Create(ctx, cs); err == nil {
				t.Fatalf("expected creation to be rejected for %q", tc.name)
			}
		})
	}
}

func TestConfigSyncValidAccepted(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	cs := validConfigSync("valid", "default")
	if err := env.Client.Create(ctx, cs); err != nil {
		t.Fatalf("valid ConfigSync rejected: %v", err)
	}
}
