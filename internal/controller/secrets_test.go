package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	fakesecret "github.com/ozimakov/kohen/internal/secret/fake"
	"github.com/ozimakov/kohen/internal/testenv"
	"github.com/ozimakov/kohen/internal/wire"
)

// newReconcilerWithResolver builds a reconciler wired with a fake native
// resolver so the secret framework can be exercised deterministically.
func newReconcilerWithResolver(env *testenv.Env, f git.Source, resolver secret.Resolver) *controller.ConfigSyncReconciler {
	return &controller.ConfigSyncReconciler{
		Client:   env.Client,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(200),
		Fetcher:  f,
		Renderer: render.New(render.Options{}),
		Applier:  apply.New(env.Client, scheme.Scheme),
		Wirer:    wire.New(env.Client),
		Redactor: redact.New(),
		Config:   config.Default(),
		Resolvers: map[secret.Backend]secret.Resolver{
			secret.BackendNativeSecret:   resolver,
			secret.BackendExternalSecret: resolver,
		},
	}
}

func makeConfigSyncWithRefs(t *testing.T, env *testenv.Env, name, workload string, rollout kohenv1alpha1.RolloutMode, refs []kohenv1alpha1.SecretReference) *kohenv1alpha1.ConfigSync {
	t.Helper()
	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source:      kohenv1alpha1.GitSource{URL: "https://github.com/acme/config.git", Ref: "main"},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: workload},
			Rollout:     rollout,
			Wiring:      kohenv1alpha1.Wiring{MountPath: "/etc/kohen/config"},
			SecretRefs:  refs,
		},
	}
	if err := env.Client.Create(context.Background(), cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	return cs
}

func envRef(name, secretName, envVar, key string) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:         name,
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: secretName},
		Surface:      kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, EnvVar: envVar, Key: key},
	}
}

func fileRef(name, secretName, mountPath string) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:         name,
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: secretName},
		Surface:      kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: mountPath},
	}
}

func podTemplateStamp(t *testing.T, env *testenv.Env, name string) string {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]
}

func secretsReadyReason(cs *kohenv1alpha1.ConfigSync) string {
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionSecretsReady)
	if c == nil {
		return "<absent>"
	}
	return c.Reason
}

// TestSecretFirstResolutionBlocksStamping: an unready first-resolution reference
// fails closed — no wiring, no stamp, no rollout — until it resolves (R8.9, A5).
func TestSecretFirstResolutionBlocksStamping(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "secapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "db-secret", "DB_PASSWORD", "password")}
	cs := makeConfigSyncWithRefs(t, env, "secsync", "secapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New() // db-secret not scripted ⇒ SecretNotFound
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "abcdefabcdef0000"}, fake)
	reconcileN(t, r, key, 2)

	// Fail closed: no stamp and no wiring.
	if got := podTemplateStamp(t, env, "secapp"); got != "" {
		t.Errorf("workload stamped despite unresolved first-resolution secret: %q", got)
	}
	d := &appsv1.Deployment{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: "secapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("workload wired despite unresolved first-resolution secret: %+v", d.Spec.Template.Spec.Volumes)
	}
	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("SecretsReady reason = %q, want AwaitingFirstResolution", secretsReadyReason(cs))
	}
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Errorf("Ready should be False while awaiting first resolution")
	}

	// Now the secret resolves ⇒ wire + stamp with the env-inclusive version.
	fake.SetReady("db-secret", "rv-1")
	reconcileN(t, r, key, 2)
	stamp := podTemplateStamp(t, env, "secapp")
	if !strings.HasPrefix(stamp, "git:abcdefabcdef-sec:") {
		t.Errorf("stamp = %q, want git:abcdefabcdef-sec:<hash>", stamp)
	}
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionSecretsReady) != metav1.ConditionTrue {
		t.Errorf("SecretsReady should be True after resolution: %+v", cs.Status.Conditions)
	}
}

// TestSecretEnvRotationAdvancesVersion: rotating an env-surfaced secret changes
// its token, advancing the config version and re-stamping (R8.5, A6).
func TestSecretEnvRotationAdvancesVersion(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "rotapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "rot-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "rotsync", "rotapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("rot-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "1111222233334444"}, fake)
	reconcileN(t, r, key, 2)
	v1 := podTemplateStamp(t, env, "rotapp")

	// Rotate ⇒ new token ⇒ new version.
	fake.SetReady("rot-secret", "rv-2")
	reconcileN(t, r, key, 1)
	v2 := podTemplateStamp(t, env, "rotapp")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Errorf("env rotation did not advance version: %q -> %q", v1, v2)
	}
}

// TestSecretFileRotationNoVersionChange: file-surfaced rotation is delivered by
// kubelet in place and MUST NOT advance the version or roll (R8.5, A6).
func TestSecretFileRotationNoVersionChange(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "fileapp")
	refs := []kohenv1alpha1.SecretReference{fileRef("tls", "tls-secret", "/etc/tls")}
	cs := makeConfigSyncWithRefs(t, env, "filesync", "fileapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("tls-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "aaaabbbbccccdddd"}, fake)
	reconcileN(t, r, key, 2)
	v1 := podTemplateStamp(t, env, "fileapp")
	if v1 != "git:aaaabbbbcccc" {
		t.Errorf("file-only version = %q, want git:aaaabbbbcccc (no -sec)", v1)
	}

	// Rotate the file-surfaced secret ⇒ version unchanged.
	fake.SetReady("tls-secret", "rv-2")
	reconcileN(t, r, key, 1)
	if v2 := podTemplateStamp(t, env, "fileapp"); v2 != v1 {
		t.Errorf("file rotation changed the version: %q -> %q", v1, v2)
	}
}

// TestSecretUpdateFailSafe: with a prior good version running, a transient
// not-ready backend keeps last-good (Degraded), then recovers (R8.9, A5).
func TestSecretUpdateFailSafe(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "fsapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "fs-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "fssync", "fsapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("fs-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "5555666677778888"}, fake)
	reconcileN(t, r, key, 2)
	goodStamp := podTemplateStamp(t, env, "fsapp")
	if goodStamp == "" {
		t.Fatal("expected a good stamp after first resolution")
	}

	// Backend goes transiently not-ready.
	fake.Set("fs-secret", secret.Resolution{Ready: false, Reason: kohenv1alpha1.ReasonBackendNotReady, Message: "provider timeout"})
	reconcileN(t, r, key, 1)

	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonDegradedServingLastGood {
		t.Errorf("SecretsReady reason = %q, want DegradedServingLastGood", secretsReadyReason(cs))
	}
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Errorf("Ready should be False (Degraded) while serving last-good")
	}
	// Last-good preserved: stamp unchanged, wiring intact.
	if got := podTemplateStamp(t, env, "fsapp"); got != goodStamp {
		t.Errorf("stamp changed during fail-safe: %q -> %q", goodStamp, got)
	}
	d := &appsv1.Deployment{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: "fsapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) == 0 {
		t.Errorf("last-good wiring retracted during fail-safe")
	}

	// Recover.
	fake.SetReady("fs-secret", "rv-1")
	reconcileN(t, r, key, 1)
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionSecretsReady) != metav1.ConditionTrue {
		t.Errorf("SecretsReady should recover to True: %+v", cs.Status.Conditions)
	}
}

// TestSecretMaxDegradedExceeded: serving last-good beyond maxDegradedDuration
// surfaces MaxDegradedExceeded (R8.11).
func TestSecretMaxDegradedExceeded(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "mdapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "md-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "mdsync", "mdapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("md-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "9999888877776666"}, fake)
	// Tiny budget so the second degraded reconcile trips the ceiling.
	r.Config = &config.Config{MaxDegradedDuration: metav1.Duration{Duration: time.Nanosecond}}
	reconcileN(t, r, key, 2)

	fake.Set("md-secret", secret.Resolution{Ready: false, Reason: kohenv1alpha1.ReasonBackendNotReady, Message: "down"})
	reconcileN(t, r, key, 1) // enters DegradedServingLastGood (degradedSince=now)
	time.Sleep(2 * time.Millisecond)
	reconcileN(t, r, key, 1) // now - degradedSince > 1ns ⇒ MaxDegradedExceeded

	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonMaxDegradedExceeded {
		t.Errorf("SecretsReady reason = %q, want MaxDegradedExceeded", secretsReadyReason(cs))
	}
}

// TestSecretInvalidSurface: a surface whose declared `as` does not match its
// fields (which CEL cannot catch) fails closed with InvalidSurface (R11.1).
func TestSecretInvalidSurface(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "invapp")
	// as=env but only mountPath set: CEL sees the file field-set and admits it,
	// but the declared mode is env — the reconciler must reject it.
	ref := kohenv1alpha1.SecretReference{
		Name:         "bad",
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: "s"},
		Surface:      kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, MountPath: "/etc/x"},
	}
	cs := makeConfigSyncWithRefs(t, env, "invsync", "invapp", kohenv1alpha1.RolloutAuto, []kohenv1alpha1.SecretReference{ref})
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("s", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "0000111122223333"}, fake)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if len(cs.Status.SecretRefs) != 1 || cs.Status.SecretRefs[0].Reason != kohenv1alpha1.ReasonInvalidSurface {
		t.Errorf("expected InvalidSurface per-ref reason, got %+v", cs.Status.SecretRefs)
	}
}

// TestSecretTokenNotLeaked: version tokens must never appear verbatim in status
// (only the hashed -sec component); resolution carries no values (R8.3, R8.10).
func TestSecretTokenNotLeaked(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "tokapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "tok-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "toksync", "tokapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	const token = "distinctive-resource-version-777"
	fake := fakesecret.New().SetReady("tok-secret", token)
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "abcabcabcabc0000"}, fake)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if strings.Contains(cs.Status.ConfigVersion, token) {
		t.Errorf("version token leaked verbatim into configVersion: %q", cs.Status.ConfigVersion)
	}
	if s := podTemplateStamp(t, env, "tokapp"); strings.Contains(s, token) {
		t.Errorf("version token leaked into workload stamp: %q", s)
	}
}
