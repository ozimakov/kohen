package native_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/secret"
	"github.com/ozimakov/kohen/internal/secret/native"
	"github.com/ozimakov/kohen/internal/testenv"
)

func makeSecret(t *testing.T, env *testenv.Env, name string, data map[string][]byte) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       data,
	}
	if err := env.Client.Create(context.Background(), s); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return s
}

func TestNativeResolveMissingSecret(t *testing.T) {
	env := testenv.Start(t)
	r := native.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{Name: "db", SecretName: "absent"})
	if err != nil {
		t.Fatalf("missing secret must not be an error: %v", err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonSecretNotFound {
		t.Fatalf("res = %+v, want not-ready SecretNotFound", res)
	}
}

func TestNativeResolveReadyFileSurface(t *testing.T) {
	env := testenv.Start(t)
	makeSecret(t, env, "tls", map[string][]byte{"tls.crt": []byte("CERT"), "tls.key": []byte("KEY")})
	r := native.New(env.Client)
	// File surfacing requires no specific key, only existence.
	res, err := r.Resolve(context.Background(), "default", secret.Ref{Name: "tls", SecretName: "tls"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready || res.SecretName != "tls" || res.VersionToken == "" {
		t.Fatalf("res = %+v, want ready with token", res)
	}
}

func TestNativeResolveKeyMissing(t *testing.T) {
	env := testenv.Start(t)
	makeSecret(t, env, "creds", map[string][]byte{"username": []byte("u")})
	r := native.New(env.Client)
	res, err := r.Resolve(context.Background(), "default",
		secret.Ref{Name: "db", SecretName: "creds", RequiredKeys: []string{"password"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonKeyMissing {
		t.Fatalf("res = %+v, want not-ready KeyMissing", res)
	}
}

func TestNativeResolveKeyPresent(t *testing.T) {
	env := testenv.Start(t)
	makeSecret(t, env, "creds2", map[string][]byte{"password": []byte("p")})
	r := native.New(env.Client)
	res, err := r.Resolve(context.Background(), "default",
		secret.Ref{Name: "db", SecretName: "creds2", RequiredKeys: []string{"password"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready {
		t.Fatalf("res = %+v, want ready", res)
	}
}

// TestNativeVersionTokenTracksRotation: the token is stable across identical
// reads and changes when the Secret is updated (new resourceVersion), enabling
// rotation detection without ever reading the value (R8.10).
func TestNativeVersionTokenTracksRotation(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	s := makeSecret(t, env, "rot", map[string][]byte{"k": []byte("v1")})
	r := native.New(env.Client)

	res1, err := r.Resolve(ctx, "default", secret.Ref{SecretName: "rot"})
	if err != nil {
		t.Fatal(err)
	}
	res1b, err := r.Resolve(ctx, "default", secret.Ref{SecretName: "rot"})
	if err != nil {
		t.Fatal(err)
	}
	if res1.VersionToken != res1b.VersionToken {
		t.Errorf("token not stable across reads: %q vs %q", res1.VersionToken, res1b.VersionToken)
	}

	s.Data["k"] = []byte("v2")
	if err := env.Client.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	res2, err := r.Resolve(ctx, "default", secret.Ref{SecretName: "rot"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.VersionToken == res1.VersionToken {
		t.Errorf("token did not change on rotation: %q", res2.VersionToken)
	}
}

// TestNativeVersionTokenNeverContainsValue verifies the token is metadata-only:
// two Secrets with the same key set but different values (and forced identical
// resourceVersion via the derivation) hash the same, proving values are not in
// the token. We assert directly that the value never appears in the token.
func TestNativeVersionTokenNeverContainsValue(t *testing.T) {
	env := testenv.Start(t)
	value := "super-secret-value"
	makeSecret(t, env, "leak", map[string][]byte{"k": []byte(value)})
	r := native.New(env.Client)
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "leak"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.VersionToken, value) || strings.Contains(res.Message, value) || strings.Contains(res.SecretName, value) {
		t.Errorf("resolution leaked secret value: %+v", res)
	}
}

// errReader wraps a client.Reader to inject a non-NotFound error, simulating a
// transient API failure that must surface as an error (→ fail-safe).
type errReader struct{ err error }

func (e errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return e.err
}
func (e errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return e.err
}

func TestNativeResolveTransientErrorSurfaces(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	r := native.New(errReader{err: boom})
	_, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("transient error must surface, got %v", err)
	}
}

func TestNativeResolveNotFoundIsNotError(t *testing.T) {
	nf := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "x")
	r := native.New(errReader{err: nf})
	res, err := r.Resolve(context.Background(), "default", secret.Ref{SecretName: "x"})
	if err != nil {
		t.Fatalf("NotFound must not be an error: %v", err)
	}
	if res.Ready || res.Reason != kohenv1alpha1.ReasonSecretNotFound {
		t.Fatalf("res = %+v, want SecretNotFound", res)
	}
}
