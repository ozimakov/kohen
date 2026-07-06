package eso_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/secret"
	"github.com/ozimakov/kohen/internal/secret/eso"
	"github.com/ozimakov/kohen/internal/testenv"
)

var esGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"}

func newES(name, targetName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(esGVK)
	u.SetName(name)
	u.SetNamespace("default")
	if targetName != "" {
		_ = unstructured.SetNestedField(u.Object, targetName, "spec", "target", "name")
	}
	return u
}

// createES creates the ExternalSecret and, if ready, sets a Ready=True status
// with a synced revision via the status subresource (as ESO would).
func createES(t *testing.T, env *testenv.Env, u *unstructured.Unstructured, ready bool, synced string) {
	t.Helper()
	ctx := context.Background()
	if err := env.Client.Create(ctx, u); err != nil {
		t.Fatalf("create ExternalSecret: %v", err)
	}
	status := map[string]any{}
	if synced != "" {
		status["syncedResourceVersion"] = synced
	}
	st := "False"
	if ready {
		st = "True"
	}
	status["conditions"] = []any{map[string]any{"type": "Ready", "status": st}}
	_ = unstructured.SetNestedField(u.Object, status, "status")
	if err := env.Client.Status().Update(ctx, u); err != nil {
		t.Fatalf("set ExternalSecret status: %v", err)
	}
}

func makeSecret(t *testing.T, env *testenv.Env, name string, data map[string][]byte) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       data,
	}
	if err := env.Client.Create(context.Background(), s); err != nil {
		t.Fatalf("create secret: %v", err)
	}
}

func TestESOAwaitingExternalSecret(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "absent-es"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonBackendNotReady {
		t.Fatalf("res = %+v, want BackendNotReady (awaiting)", res)
	}
}

func TestESONotReadyBlocks(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	createES(t, env, newES("db-es", "db-secret"), false, "")
	makeSecret(t, env, "db-secret", map[string][]byte{"password": []byte("p")})
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "db-es"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonBackendNotReady {
		t.Fatalf("res = %+v, want BackendNotReady while not Ready", res)
	}
}

func TestESOReadyResolvesTargetSecret(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	createES(t, env, newES("db-es", "db-secret"), true, "rev-1")
	makeSecret(t, env, "db-secret", map[string][]byte{"password": []byte("p")})
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default",
		secret.Ref{SecretName: "db-es", RequiredKeys: []string{"password"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || res.SecretName != "db-secret" || res.VersionToken == "" {
		t.Fatalf("res = %+v, want ready wiring db-secret", res)
	}
}

func TestESOReadyButTargetMissing(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	createES(t, env, newES("db-es", "db-secret"), true, "rev-1")
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "db-es"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonSecretNotFound {
		t.Fatalf("res = %+v, want SecretNotFound when target absent", res)
	}
}

func TestESOKeyMissing(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	createES(t, env, newES("db-es", "db-secret"), true, "rev-1")
	makeSecret(t, env, "db-secret", map[string][]byte{"other": []byte("x")})
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default",
		secret.Ref{SecretName: "db-es", RequiredKeys: []string{"password"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonKeyMissing {
		t.Fatalf("res = %+v, want KeyMissing", res)
	}
}

func TestESODefaultTargetName(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	// No spec.target.name ⇒ target Secret shares the ExternalSecret name.
	createES(t, env, newES("shared-name", ""), true, "rev-1")
	makeSecret(t, env, "shared-name", map[string][]byte{"k": []byte("v")})
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "shared-name"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || res.SecretName != "shared-name" {
		t.Fatalf("res = %+v, want ready wiring shared-name", res)
	}
}

// TestESOTokenTracksSyncedRevision: a change to status.syncedResourceVersion
// (a rotation ESO recorded) changes the token (R8.10).
func TestESOTokenTracksSyncedRevision(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	ctx := context.Background()
	es := newES("rot-es", "rot-secret")
	createES(t, env, es, true, "rev-1")
	makeSecret(t, env, "rot-secret", map[string][]byte{"k": []byte("v")})
	r := eso.New(env.Client)

	res1, err := r.Resolve(ctx, "default", secret.Ref{SecretName: "rot-es"})
	if err != nil {
		t.Fatal(err)
	}

	// ESO records a new synced revision.
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(esGVK)
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "rot-es"}, cur); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cur.Object, "rev-2", "status", "syncedResourceVersion")
	_ = unstructured.SetNestedField(cur.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := env.Client.Status().Update(ctx, cur); err != nil {
		t.Fatal(err)
	}

	res2, err := r.Resolve(ctx, "default", secret.Ref{SecretName: "rot-es"})
	if err != nil {
		t.Fatal(err)
	}
	if res1.VersionToken == res2.VersionToken {
		t.Errorf("token did not change on synced-revision rotation: %q", res2.VersionToken)
	}
}

func TestESONoValueInResolution(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	value := "eso-secret-value"
	createES(t, env, newES("leak-es", "leak-secret"), true, "rev-1")
	makeSecret(t, env, "leak-secret", map[string][]byte{"k": []byte(value)})
	r := eso.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "leak-es"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.VersionToken, value) || strings.Contains(res.Message, value) {
		t.Errorf("resolution leaked secret value: %+v", res)
	}
}
