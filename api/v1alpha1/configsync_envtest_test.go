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

func envSurface(envVar, key string) kohenv1alpha1.SecretSurface {
	return kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, EnvVar: envVar, Key: key}
}

func fileSurface(mountPath string) kohenv1alpha1.SecretSurface {
	return kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: mountPath}
}

func extRef(name string, surface kohenv1alpha1.SecretSurface) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:           name,
		Backend:        kohenv1alpha1.BackendExternalSecret,
		ExternalSecret: &kohenv1alpha1.LocalObjectReference{Name: name + "-es"},
		Surface:        surface,
	}
}

func nativeRef(name string, surface kohenv1alpha1.SecretSurface) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:         name,
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: name + "-secret"},
		Surface:      surface,
	}
}

func TestSecretRefsValidationRejected(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*kohenv1alpha1.ConfigSync)
	}{
		{"externalSecret backend without ref", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{{
				Name: "db", Backend: kohenv1alpha1.BackendExternalSecret, Surface: envSurface("DB", "password"),
			}}
		}},
		{"nativeSecret backend with externalSecret ref", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{{
				Name: "db", Backend: kohenv1alpha1.BackendNativeSecret,
				ExternalSecret: &kohenv1alpha1.LocalObjectReference{Name: "x"},
				Surface:        envSurface("DB", "password"),
			}}
		}},
		{"file surface without mountPath", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("tls", kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile}),
			}
		}},
		{"env surface without envVar/key", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				extRef("db", kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv}),
			}
		}},
		{"file surface with envVar", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("tls", kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: "/etc/tls", EnvVar: "X"}),
			}
		}},
		{"duplicate file mountPaths", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("tls1", fileSurface("/etc/secret")),
				nativeRef("tls2", fileSurface("/etc/secret")),
			}
		}},
		{"duplicate env vars", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				extRef("a", envSurface("TOKEN", "k1")),
				extRef("b", envSurface("TOKEN", "k2")),
			}
		}},
		{"duplicate reference names", func(cs *kohenv1alpha1.ConfigSync) {
			// name is the load-bearing identity (volume name, status key, token
			// key); listType=map+listMapKey=name makes the API server reject
			// duplicates (R8.12).
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("dup", fileSurface("/etc/a")),
				extRef("dup", envSurface("TOKEN", "k")),
			}
		}},
		{"file mount equals wiring mountPath", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.Wiring.MountPath = "/etc/kohen/config"
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("tls", fileSurface("/etc/kohen/config")),
			}
		}},
		{"env surface with rollout none", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.Rollout = kohenv1alpha1.RolloutNone
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				extRef("db", envSurface("DB", "password")),
			}
		}},
		{"bad backend enum", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{{
				Name: "db", Backend: "vault", Surface: envSurface("DB", "password"),
			}}
		}},
		{"bad surface enum", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{{
				Name: "db", Backend: kohenv1alpha1.BackendNativeSecret,
				NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: "s"},
				Surface:      kohenv1alpha1.SecretSurface{As: "stdout"},
			}}
		}},
		{"name not a DNS label", func(cs *kohenv1alpha1.ConfigSync) {
			cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
				nativeRef("Db_Password", fileSurface("/etc/tls")),
			}
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := validConfigSync("secref-bad", "default")
			cs.Name = "secref-bad-" + strconv.Itoa(i)
			tc.mutate(cs)
			if err := env.Client.Create(ctx, cs); err == nil {
				t.Fatalf("expected rejection for %q", tc.name)
			}
		})
	}
}

func TestSecretRefsValidAccepted(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	cs := validConfigSync("secref-ok", "default")
	cs.Spec.SecretRefs = []kohenv1alpha1.SecretReference{
		extRef("db-password", envSurface("DB_PASSWORD", "password")),
		nativeRef("tls", fileSurface("/etc/tls")),
	}
	if err := env.Client.Create(ctx, cs); err != nil {
		t.Fatalf("valid secretRefs rejected: %v", err)
	}
}

func TestSecretSurfaceRolloutOnRotateDefault(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	// Omit rolloutOnRotate so the API server applies its default (SPEC §11.2).
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kohen.dev/v1alpha1",
		"kind":       "ConfigSync",
		"metadata":   map[string]any{"name": "secref-default", "namespace": "default"},
		"spec": map[string]any{
			"source":      map[string]any{"url": "https://github.com/acme/config.git"},
			"path":        "services/app/prod",
			"workloadRef": map[string]any{"kind": "Deployment", "name": "app"},
			"secretRefs": []any{map[string]any{
				"name":           "db-password",
				"backend":        "externalSecret",
				"externalSecret": map[string]any{"name": "checkout-db"},
				"surface":        map[string]any{"as": "env", "envVar": "DB_PASSWORD", "key": "password"},
			}},
		},
	}}
	if err := env.Client.Create(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &kohenv1alpha1.ConfigSync{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "secref-default", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Spec.SecretRefs) != 1 {
		t.Fatalf("secretRefs len = %d, want 1", len(got.Spec.SecretRefs))
	}
	s := got.Spec.SecretRefs[0].Surface
	if s.RolloutOnRotate == nil || !*s.RolloutOnRotate {
		t.Errorf("rolloutOnRotate default = %v, want true", s.RolloutOnRotate)
	}
	if !s.ShouldRolloutOnRotate() {
		t.Errorf("ShouldRolloutOnRotate() = false, want true")
	}
}
