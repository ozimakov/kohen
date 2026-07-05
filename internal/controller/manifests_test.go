package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/config"
	"github.com/ozimakov/kohen/internal/controller"
	"github.com/ozimakov/kohen/internal/git"
	"github.com/ozimakov/kohen/internal/redact"
	"github.com/ozimakov/kohen/internal/render"
	"github.com/ozimakov/kohen/internal/secret"
	"github.com/ozimakov/kohen/internal/secret/eso"
	"github.com/ozimakov/kohen/internal/secret/native"
	"github.com/ozimakov/kohen/internal/testenv"
	"github.com/ozimakov/kohen/internal/wire"
)

// esManifest returns an ExternalSecret manifest body for a git fixture.
func esManifest(name, targetName, store, namespace string) string {
	nsLine := ""
	if namespace != "" {
		nsLine = "\n  namespace: " + namespace
	}
	return fmt.Sprintf(`apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: %s%s
spec:
  secretStoreRef:
    name: %s
    kind: SecretStore
  target:
    name: %s
`, name, nsLine, store, targetName)
}

// newReconcilerReal builds a reconciler with the real native + ESO resolvers,
// for S2.4 manifest/ESO integration tests.
func newReconcilerReal(env *testenv.Env, f git.Source, cfg *config.Config) *controller.ConfigSyncReconciler {
	return &controller.ConfigSyncReconciler{
		Client:   env.Client,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(200),
		Fetcher:  f,
		Renderer: render.New(render.Options{}),
		Applier:  apply.New(env.Client, scheme.Scheme),
		Wirer:    wire.New(env.Client),
		Redactor: redact.New(),
		Config:   cfg,
		Resolvers: map[secret.Backend]secret.Resolver{
			secret.BackendNativeSecret:   native.New(env.Client),
			secret.BackendExternalSecret: eso.New(env.Client),
		},
	}
}

func getES(t *testing.T, env *testenv.Env, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"})
	if err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, u); err != nil {
		t.Fatalf("get ExternalSecret %q: %v", name, err)
	}
	return u
}

func extSecretRef(name, esName, envVar, key string) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:           name,
		Backend:        kohenv1alpha1.BackendExternalSecret,
		ExternalSecret: &kohenv1alpha1.LocalObjectReference{Name: esName},
		Surface:        kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, EnvVar: envVar, Key: key},
	}
}

func manifestsAppliedReason(cs *kohenv1alpha1.ConfigSync) string {
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionManifestsApplied)
	if c == nil {
		return "<absent>"
	}
	return c.Reason
}

// TestManifestAppliedAndExcluded: a committed ExternalSecret is applied as an
// owned object and excluded from ConfigMap keys (SPEC §8.2, R7.6, R8.8).
func TestManifestAppliedAndExcluded(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	makeDeployment(t, env, "mapp")
	cs := makeConfigSync(t, env, "msync", "mapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{
		"app.yaml":           "k: v\n",
		"secrets/db-es.yaml": esManifest("db-es", "db-secret", "vault", ""),
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "aaaa0000aaaa0000"}, config.Default())
	reconcileN(t, r, key, 2)

	es := getES(t, env, "db-es")
	if es.GetLabels()[apply.ManagedByLabel] != apply.ManagedByValue {
		t.Errorf("applied ExternalSecret missing ownership label: %v", es.GetLabels())
	}
	if es.GetNamespace() != "default" {
		t.Errorf("ExternalSecret namespace = %q, want default", es.GetNamespace())
	}
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionManifestsApplied) != metav1.ConditionTrue {
		t.Errorf("ManifestsApplied should be True: %+v", cs.Status.Conditions)
	}
	// Excluded from ConfigMap keys.
	cm := &corev1.ConfigMap{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: "mapp-config", Namespace: "default"}, cm); err != nil {
		t.Fatal(err)
	}
	for k := range cm.Data {
		if strings.Contains(k, "db-es") {
			t.Errorf("ExternalSecret leaked into ConfigMap keys: %v", cm.Data)
		}
	}
}

// TestManifestPrunedOnRemoval: removing the manifest from git prunes the owned
// ExternalSecret (R8.8).
func TestManifestPrunedOnRemoval(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	makeDeployment(t, env, "papp")
	cs := makeConfigSync(t, env, "psync", "papp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	withES := fixtureDir(t, map[string]string{
		"app.yaml": "k: v\n",
		"es.yaml":  esManifest("gone-es", "gone-secret", "vault", ""),
	})
	f := &fakeFetcher{dir: withES, commit: "bbbb0000bbbb0000"}
	r := newReconcilerReal(env, f, config.Default())
	reconcileN(t, r, key, 2)
	getES(t, env, "gone-es") // exists

	// Next commit drops the manifest.
	f.dir = fixtureDir(t, map[string]string{"app.yaml": "k: v\n"})
	f.commit = "cccc0000cccc0000"
	reconcileN(t, r, key, 1)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"})
	err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gone-es"}, u)
	if err == nil {
		t.Errorf("ExternalSecret not pruned after removal from git")
	}
}

// TestManifestGuardRailForeignNamespace: an ExternalSecret targeting another
// namespace fails closed (R-AUTH.5).
func TestManifestGuardRailForeignNamespace(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	makeDeployment(t, env, "gapp")
	cs := makeConfigSync(t, env, "gsync", "gapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{
		"app.yaml": "k: v\n",
		"es.yaml":  esManifest("cross-es", "x", "vault", "other-ns"),
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "dddd0000dddd0000"}, config.Default())
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if manifestsAppliedReason(cs) != kohenv1alpha1.ReasonManifestNamespaceViolation {
		t.Errorf("reason = %q, want ManifestNamespaceViolation", manifestsAppliedReason(cs))
	}
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Errorf("Ready should be False on guard-rail violation")
	}
}

// TestManifestGuardRailStoreNotAllowed: a secretStoreRef outside the operator
// allow-list fails closed (R-AUTH.4).
func TestManifestGuardRailStoreNotAllowed(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	makeDeployment(t, env, "sapp")
	cs := makeConfigSync(t, env, "ssync", "sapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	cfg := config.Default()
	cfg.SecretStoreAllowList = []string{"vault"}
	dir := fixtureDir(t, map[string]string{
		"app.yaml": "k: v\n",
		"es.yaml":  esManifest("rogue-es", "x", "rogue-store", ""),
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "eeee0000eeee0000"}, cfg)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if manifestsAppliedReason(cs) != kohenv1alpha1.ReasonStoreNotAllowed {
		t.Errorf("reason = %q, want StoreNotAllowed", manifestsAppliedReason(cs))
	}
}

// TestManifestNoAdoption: a pre-existing, un-owned ExternalSecret is never
// adopted/overwritten (R8.8); the reconcile fails closed terminally.
func TestManifestNoAdoption(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	ctx := context.Background()
	makeDeployment(t, env, "aapp")
	cs := makeConfigSync(t, env, "async", "aapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	// A foreign, un-owned ExternalSecret already exists with the same name.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1beta1", Kind: "ExternalSecret"})
	foreign.SetName("db-es")
	foreign.SetNamespace("default")
	_ = unstructured.SetNestedField(foreign.Object, "vault", "spec", "secretStoreRef", "name")
	if err := env.Client.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	dir := fixtureDir(t, map[string]string{
		"app.yaml": "k: v\n",
		"es.yaml":  esManifest("db-es", "db-secret", "vault", ""),
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "abcd1234abcd1234"}, config.Default())
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if manifestsAppliedReason(cs) != kohenv1alpha1.ReasonManifestApplyFailed {
		t.Errorf("reason = %q, want ManifestApplyFailed (no adoption)", manifestsAppliedReason(cs))
	}
	// The foreign object must be untouched (no ownership labels stamped).
	got := getES(t, env, "db-es")
	if got.GetLabels()[apply.ManagedByLabel] == apply.ManagedByValue {
		t.Errorf("Kohen adopted a pre-existing ExternalSecret: %v", got.GetLabels())
	}
}

// TestManifestMultipleApplied: several ExternalSecrets across files are all
// applied and owned.
func TestManifestMultipleApplied(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	makeDeployment(t, env, "multapp")
	cs := makeConfigSync(t, env, "multsync", "multapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	multi := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n---\n" + esManifest("es-b", "sec-b", "vault", "")
	dir := fixtureDir(t, map[string]string{
		"app.yaml":     "k: v\n",
		"a/es-a.yaml":  esManifest("es-a", "sec-a", "vault", ""),
		"b/mixed.yaml": multi,
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "5678abcd5678abcd"}, config.Default())
	reconcileN(t, r, key, 2)

	getES(t, env, "es-a")
	getES(t, env, "es-b")
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionManifestsApplied) != metav1.ConditionTrue {
		t.Errorf("ManifestsApplied should be True for multiple manifests: %+v", cs.Status.Conditions)
	}
}

// TestESOJourneyFirstResolutionThenReady exercises UC3 at Tier 2: the committed
// ExternalSecret is applied, first resolution fails closed until ESO reports
// Ready and the target Secret exists, then the workload is wired + stamped.
func TestESOJourneyFirstResolutionThenReady(t *testing.T) {
	env := testenv.Start(t, "test/crds")
	ctx := context.Background()
	makeDeployment(t, env, "esoapp")
	refs := []kohenv1alpha1.SecretReference{extSecretRef("db", "db-es", "DB_PASSWORD", "password")}
	cs := makeConfigSyncWithRefs(t, env, "esosync", "esoapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{
		"app.yaml": "k: v\n",
		"es.yaml":  esManifest("db-es", "db-secret", "vault", ""),
	})
	r := newReconcilerReal(env, &fakeFetcher{dir: dir, commit: "f0f0f0f0f0f0f0f0"}, config.Default())
	reconcileN(t, r, key, 2)

	// The ExternalSecret was applied, but it is not Ready yet ⇒ fail closed.
	getES(t, env, "db-es")
	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Fatalf("first resolution should await ESO readiness, reason = %q", secretsReadyReason(cs))
	}
	if got := podTemplateStamp(t, env, "esoapp"); got != "" {
		t.Errorf("workload stamped before ESO Ready: %q", got)
	}

	// ESO reconciles: marks the ExternalSecret Ready and writes the target Secret.
	es := getES(t, env, "db-es")
	_ = unstructured.SetNestedField(es.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	_ = unstructured.SetNestedField(es.Object, "rev-1", "status", "syncedResourceVersion")
	if err := env.Client.Status().Update(ctx, es); err != nil {
		t.Fatal(err)
	}
	value := "eso-delivered-secret"
	if err := env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte(value)},
	}); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionSecretsReady) != metav1.ConditionTrue {
		t.Fatalf("SecretsReady should be True after ESO Ready: %+v", cs.Status.Conditions)
	}
	stamp := podTemplateStamp(t, env, "esoapp")
	if !strings.HasPrefix(stamp, "git:f0f0f0f0f0f0-sec:") {
		t.Errorf("stamp = %q, want env-folded version after ESO resolution", stamp)
	}
	// The env surface wires the ESO target Secret; no value leaks.
	d := getDeploy(t, env, "esoapp")
	if len(d.Spec.Template.Spec.Containers[0].Env) == 0 {
		t.Errorf("ESO target secret not wired as env: %+v", d.Spec.Template.Spec.Containers[0])
	}
}
