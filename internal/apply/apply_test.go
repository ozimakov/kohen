package apply_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/testenv"
)

func newOwner(t *testing.T, env *testenv.Env, name string) *kohenv1alpha1.ConfigSync {
	t.Helper()
	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source:      kohenv1alpha1.GitSource{URL: "https://github.com/acme/config.git"},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: name},
		},
	}
	if err := env.Client.Create(context.Background(), cs); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	return cs
}

func cm(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data:       data,
	}
}

func TestApplyCreatesWithOwnershipAndData(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	owner := newOwner(t, env, "app1")
	a := apply.New(env.Client, scheme.Scheme)

	if err := a.Apply(ctx, owner, cm("app1-config", map[string]string{"a": "1"})); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app1-config", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Data["a"] != "1" {
		t.Errorf("data = %v", got.Data)
	}
	if got.Labels[apply.ManagedByLabel] != apply.ManagedByValue || got.Labels[apply.OwnerLabel] != "app1" {
		t.Errorf("ownership labels missing: %v", got.Labels)
	}
	ref := metav1.GetControllerOf(got)
	if ref == nil || ref.UID != owner.UID || ref.Kind != "ConfigSync" {
		t.Errorf("controller owner ref = %+v, want ConfigSync %s", ref, owner.UID)
	}
}

func TestApplyIsIdempotentAndUpdates(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	owner := newOwner(t, env, "app2")
	a := apply.New(env.Client, scheme.Scheme)

	if err := a.Apply(ctx, owner, cm("app2-config", map[string]string{"a": "1"})); err != nil {
		t.Fatalf("apply1: %v", err)
	}
	if err := a.Apply(ctx, owner, cm("app2-config", map[string]string{"a": "2", "b": "3"})); err != nil {
		t.Fatalf("apply2: %v", err)
	}
	got := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app2-config", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Data["a"] != "2" || got.Data["b"] != "3" {
		t.Errorf("data not updated: %v", got.Data)
	}
}

func TestApplyRefusesAdoption(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	owner := newOwner(t, env, "app3")
	a := apply.New(env.Client, scheme.Scheme)

	// A pre-existing, user-owned ConfigMap (no Kohen labels).
	pre := cm("app3-config", map[string]string{"user": "data"})
	if err := env.Client.Create(ctx, pre); err != nil {
		t.Fatal(err)
	}

	err := a.Apply(ctx, owner, cm("app3-config", map[string]string{"a": "1"}))
	if err == nil {
		t.Fatal("expected adoption to be refused")
	}
	if r, _ := apply.ReasonOf(err); r != apply.ReasonAlreadyExistsNotOwned {
		t.Fatalf("reason = %v, want AlreadyExistsNotOwned (err: %v)", r, err)
	}
	// User data untouched.
	got := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app3-config", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Data["user"] != "data" {
		t.Errorf("user data was overwritten: %v", got.Data)
	}
}

func TestPruneRemovesUndesiredOwnedObjects(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	owner := newOwner(t, env, "app4")
	other := newOwner(t, env, "other4")
	a := apply.New(env.Client, scheme.Scheme)

	if err := a.Apply(ctx, owner, cm("keep", map[string]string{"a": "1"})); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, owner, cm("drop", map[string]string{"a": "1"})); err != nil {
		t.Fatal(err)
	}
	// A ConfigMap owned by a different ConfigSync must NOT be pruned.
	if err := a.Apply(ctx, other, cm("other-cm", map[string]string{"a": "1"})); err != nil {
		t.Fatal(err)
	}

	if err := a.Prune(ctx, owner, &corev1.ConfigMapList{}, "keep"); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if err := env.Client.Get(ctx, client.ObjectKey{Name: "keep", Namespace: "default"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("keep was deleted: %v", err)
	}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "drop", Namespace: "default"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("drop should be pruned, got err=%v", err)
	}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "other-cm", Namespace: "default"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("other owner's cm was pruned: %v", err)
	}
}
