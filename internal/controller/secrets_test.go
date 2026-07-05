package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/ozimakov/kohen/internal/secret/native"
	"github.com/ozimakov/kohen/internal/testenv"
	"github.com/ozimakov/kohen/internal/wire"
	"github.com/ozimakov/kohen/test/leakcheck"
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
	scan := leakcheck.New(token)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if strings.Contains(cs.Status.ConfigVersion, token) {
		t.Errorf("version token leaked verbatim into configVersion: %q", cs.Status.ConfigVersion)
	}
	if s := podTemplateStamp(t, env, "tokapp"); strings.Contains(s, token) {
		t.Errorf("version token leaked into workload stamp: %q", s)
	}
	// Broaden: no condition message or per-ref status may carry the token.
	scan.AssertObjectClean(t, "configsync status", cs)
}

// TestSecretEstablishmentRequiresWiring: a reference that resolves but whose
// workload is never wired (workload missing) must NOT become established, so a
// later outage fails closed (AwaitingFirstResolution) rather than emitting the
// security-visible MaxDegradedExceeded signal (R8.9/R8.11).
func TestSecretEstablishmentRequiresWiring(t *testing.T) {
	env := testenv.Start(t)
	// Deliberately no workload: wiring will fail with WorkloadNotFound.
	refs := []kohenv1alpha1.SecretReference{envRef("db", "est-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "estsync", "ghostwl", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("est-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "eeee1111eeee2222"}, fake)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if len(cs.Status.SecretRefs) != 1 || cs.Status.SecretRefs[0].Established {
		t.Fatalf("ref must not be established when wiring failed: %+v", cs.Status.SecretRefs)
	}

	// Drop the secret. Because it never wired, this is still a first resolution.
	fake.Set("est-secret", secret.Resolution{Ready: false, Reason: kohenv1alpha1.ReasonSecretNotFound, Message: "gone"})
	rec := r.Recorder.(*record.FakeRecorder)
	reconcileN(t, r, key, 1)

	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("SecretsReady reason = %q, want AwaitingFirstResolution", secretsReadyReason(cs))
	}
	for drain := true; drain; {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, kohenv1alpha1.ReasonMaxDegradedExceeded) {
				t.Errorf("spurious MaxDegradedExceeded event for a never-wired secret: %q", ev)
			}
		default:
			drain = false
		}
	}
}

// TestSecretNewRefFailsClosedKeepsLastGood: adding a new (unready) reference to
// an already-healthy ConfigSync fails the aggregate closed and preserves the
// last-good wiring/stamp (R8.9).
func TestSecretNewRefFailsClosedKeepsLastGood(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "growapp")
	refs := []kohenv1alpha1.SecretReference{envRef("a", "a-secret", "AAA", "k")}
	cs := makeConfigSyncWithRefs(t, env, "growsync", "growapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("a-secret", "rv-a")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "1234abcd1234abcd"}, fake)
	reconcileN(t, r, key, 2)
	goodStamp := podTemplateStamp(t, env, "growapp")
	if goodStamp == "" {
		t.Fatal("expected a good stamp after first resolution")
	}

	// Add a second, not-yet-resolvable reference.
	cs = getCS(t, env, key)
	cs.Spec.SecretRefs = append(cs.Spec.SecretRefs, envRef("b", "b-secret", "BBB", "k"))
	if err := env.Client.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 1)

	cs = getCS(t, env, key)
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("adding an unready ref should fail closed, reason = %q", secretsReadyReason(cs))
	}
	// Last-good preserved: stamp unchanged.
	if got := podTemplateStamp(t, env, "growapp"); got != goodStamp {
		t.Errorf("last-good stamp changed while a new ref was unresolved: %q -> %q", goodStamp, got)
	}
}

// getDeploy fetches the named Deployment for surface assertions.
func getDeploy(t *testing.T, env *testenv.Env, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, d); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return d
}

// TestSecretFileSurfaceReachesWorkload: a resolved file reference is mounted as
// a read-only Secret volume in the target container (SPEC §8.4, S2.3).
func TestSecretFileSurfaceReachesWorkload(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "wfileapp")
	refs := []kohenv1alpha1.SecretReference{fileRef("tls", "tls-secret", "/etc/tls")}
	cs := makeConfigSyncWithRefs(t, env, "wfilesync", "wfileapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("tls-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "abababababab0000"}, fake)
	reconcileN(t, r, key, 2)

	d := getDeploy(t, env, "wfileapp")
	vol := wire.SecretVolumeName("tls")
	var mounted bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == vol && v.Secret != nil && v.Secret.SecretName == "tls-secret" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("secret volume not injected: %+v", d.Spec.Template.Spec.Volumes)
	}
	main := d.Spec.Template.Spec.Containers[0]
	var found bool
	for _, m := range main.VolumeMounts {
		if m.Name == vol && m.MountPath == "/etc/tls" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("secret mount not injected read-only: %+v", main.VolumeMounts)
	}
}

// TestSecretEnvSurfaceReachesWorkload: a resolved env reference is injected as a
// discrete env entry with valueFrom.secretKeyRef (SPEC §8.4, R-WIRE.2).
func TestSecretEnvSurfaceReachesWorkload(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "wenvapp")
	refs := []kohenv1alpha1.SecretReference{envRef("db", "db-secret", "DB_PASSWORD", "password")}
	cs := makeConfigSyncWithRefs(t, env, "wenvsync", "wenvapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	fake := fakesecret.New().SetReady("db-secret", "rv-1")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "cdcdcdcdcdcd0000"}, fake)
	reconcileN(t, r, key, 2)

	d := getDeploy(t, env, "wenvapp")
	main := d.Spec.Template.Spec.Containers[0]
	var e *corev1.EnvVar
	for i := range main.Env {
		if main.Env[i].Name == "DB_PASSWORD" {
			e = &main.Env[i]
		}
	}
	if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("env entry not injected via secretKeyRef: %+v", main.Env)
	}
	if e.ValueFrom.SecretKeyRef.Name != "db-secret" || e.ValueFrom.SecretKeyRef.Key != "password" {
		t.Errorf("secretKeyRef = %+v, want db-secret/password", e.ValueFrom.SecretKeyRef)
	}
	if e.Value != "" {
		t.Errorf("env entry must not carry an inline value: %q", e.Value)
	}
}

// TestSecretSurfaceEndToEndNative exercises the real native resolver against an
// actual Secret: resolution → surfacing → version folding, with a leak scan
// over the workload and status (S2.3, R8.3).
func TestSecretSurfaceEndToEndNative(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "e2eapp")
	value := "top-secret-password"
	if err := env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-secret", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte(value)},
	}); err != nil {
		t.Fatal(err)
	}
	refs := []kohenv1alpha1.SecretReference{envRef("db", "e2e-secret", "DB_PASSWORD", "password")}
	cs := makeConfigSyncWithRefs(t, env, "e2esync", "e2eapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "e2e0e2e0e2e00000"}, native.New(env.Client))
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionSecretsReady) != metav1.ConditionTrue {
		t.Fatalf("SecretsReady should be True with a real resolvable secret: %+v", cs.Status.Conditions)
	}
	stamp := podTemplateStamp(t, env, "e2eapp")
	if !strings.HasPrefix(stamp, "git:e2e0e2e0e2e0-sec:") {
		t.Errorf("stamp = %q, want env-folded version", stamp)
	}
	d := getDeploy(t, env, "e2eapp")
	main := d.Spec.Template.Spec.Containers[0]
	if len(main.Env) == 0 || main.Env[0].ValueFrom == nil {
		t.Errorf("env not wired end-to-end: %+v", main.Env)
	}

	// No secret value may appear in the workload, status, or events (R8.3/TM9).
	scan := leakcheck.New(value)
	scan.AssertObjectClean(t, "deployment", d)
	scan.AssertObjectClean(t, "configsync status", cs)
	rec := r.Recorder.(*record.FakeRecorder)
	for drain := true; drain; {
		select {
		case ev := <-rec.Events:
			scan.AssertClean(t, "event", ev)
		default:
			drain = false
		}
	}
}

// TestSecretMissingKeyFailsClosedNative: the native resolver reports KeyMissing
// for an env reference whose key is absent, failing closed (R8.4/A5).
func TestSecretMissingKeyFailsClosedNative(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "keyapp")
	if err := env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "key-secret", Namespace: "default"},
		Data:       map[string][]byte{"other": []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	refs := []kohenv1alpha1.SecretReference{envRef("db", "key-secret", "DB", "password")}
	cs := makeConfigSyncWithRefs(t, env, "keysync", "keyapp", kohenv1alpha1.RolloutAuto, refs)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconcilerWithResolver(env, &fakeFetcher{dir: dir, commit: "fefefefefefe0000"}, native.New(env.Client))
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	// First-resolution failure fails closed: the aggregate condition reports
	// AwaitingFirstResolution while the specific KeyMissing reason surfaces on
	// the per-reference status.
	if secretsReadyReason(cs) != kohenv1alpha1.ReasonAwaitingFirstResolution {
		t.Errorf("SecretsReady reason = %q, want AwaitingFirstResolution", secretsReadyReason(cs))
	}
	if len(cs.Status.SecretRefs) != 1 || cs.Status.SecretRefs[0].Reason != kohenv1alpha1.ReasonKeyMissing {
		t.Errorf("per-ref reason = %+v, want KeyMissing", cs.Status.SecretRefs)
	}
	if got := podTemplateStamp(t, env, "keyapp"); got != "" {
		t.Errorf("workload stamped despite missing key: %q", got)
	}
}
