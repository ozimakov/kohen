package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/config"
	"github.com/ozimakov/kohen/internal/controller"
	"github.com/ozimakov/kohen/internal/git"
	"github.com/ozimakov/kohen/internal/redact"
	"github.com/ozimakov/kohen/internal/render"
	"github.com/ozimakov/kohen/internal/testenv"
	"github.com/ozimakov/kohen/internal/wire"
	"github.com/ozimakov/kohen/test/leakcheck"
)

// fakeFetcher returns a prepared directory as if fetched from git.
type fakeFetcher struct {
	dir    string
	commit string
	err    error
}

func (f *fakeFetcher) Fetch(_ context.Context, _ git.Reference, _ *git.Credential) (*git.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &git.Result{Commit: f.commit, Dir: f.dir, WorktreeDir: f.dir, Cleanup: func() error { return nil }}, nil
}

func fixtureDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newReconciler(env *testenv.Env, f git.Source) *controller.ConfigSyncReconciler {
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
	}
}

func makeConfigSync(t *testing.T, env *testenv.Env, name, workload string, rolloutMode kohenv1alpha1.RolloutMode) *kohenv1alpha1.ConfigSync {
	t.Helper()
	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source:      kohenv1alpha1.GitSource{URL: "https://github.com/acme/config.git", Ref: "main"},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: workload},
			Rollout:     rolloutMode,
			Wiring:      kohenv1alpha1.Wiring{MountPath: "/etc/kohen/config"},
		},
	}
	if err := env.Client.Create(context.Background(), cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	return cs
}

func makeDeployment(t *testing.T, env *testenv.Env, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:1"}}},
			},
		},
	}
	if err := env.Client.Create(context.Background(), d); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	return d
}

func reconcileN(t *testing.T, r *controller.ConfigSyncReconciler, key client.ObjectKey, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Logf("reconcile %d returned err (may be expected): %v", i, err)
		}
	}
}

func getCS(t *testing.T, env *testenv.Env, key client.ObjectKey) *kohenv1alpha1.ConfigSync {
	t.Helper()
	cs := &kohenv1alpha1.ConfigSync{}
	if err := env.Client.Get(context.Background(), key, cs); err != nil {
		t.Fatalf("get cs: %v", err)
	}
	return cs
}

func condStatus(cs *kohenv1alpha1.ConfigSync, condType string) metav1.ConditionStatus {
	c := meta.FindStatusCondition(cs.Status.Conditions, condType)
	if c == nil {
		return "<absent>"
	}
	return c.Status
}

func TestPipelineHappyPathAuto(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "app")
	cs := makeConfigSync(t, env, "sync", "app", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "key: value\n", "db/settings.conf": "x=1\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "abcdef1234567890"})

	reconcileN(t, r, key, 2) // finalizer, then pipeline

	// ConfigMap created with rendered data + ownership.
	cm := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app-config", Namespace: "default"}, cm); err != nil {
		t.Fatalf("configmap not created: %v", err)
	}
	if cm.Data["app.yaml"] != "key: value\n" || cm.Data["db__settings.conf"] != "x=1\n" {
		t.Errorf("configmap data = %v", cm.Data)
	}
	if cm.Labels[apply.ManagedByLabel] != apply.ManagedByValue {
		t.Errorf("configmap missing ownership label: %v", cm.Labels)
	}

	// Workload wired (volume + mount + stamp).
	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 || d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/etc/kohen/config" {
		t.Errorf("workload not wired: %+v", d.Spec.Template.Spec)
	}
	wantVersion := "git:abcdef123456"
	if got := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; got != wantVersion {
		t.Errorf("pod template stamp = %q, want %q", got, wantVersion)
	}

	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionFetched) != metav1.ConditionTrue ||
		condStatus(cs, kohenv1alpha1.ConditionRendered) != metav1.ConditionTrue ||
		condStatus(cs, kohenv1alpha1.ConditionWorkloadWired) != metav1.ConditionTrue {
		t.Errorf("expected Fetched/Rendered/WorkloadWired True: %+v", cs.Status.Conditions)
	}
	if cs.Status.SourceCommit != "abcdef1234567890" || cs.Status.ConfigVersion != wantVersion {
		t.Errorf("status commit/version = %q/%q", cs.Status.SourceCommit, cs.Status.ConfigVersion)
	}
	// Rollout not complete yet (no deployment controller in envtest).
	if condStatus(cs, kohenv1alpha1.ConditionRolloutComplete) != metav1.ConditionFalse {
		t.Errorf("expected RolloutComplete False while progressing")
	}

	// Simulate the deployment controller finishing the rollout.
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "app", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = 1
	d.Status.UpdatedReplicas = 1
	d.Status.AvailableReplicas = 1
	d.Status.ReadyReplicas = 1
	if err := env.Client.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 1)

	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionRolloutComplete) != metav1.ConditionTrue {
		t.Errorf("expected RolloutComplete True after status update: %+v", cs.Status.Conditions)
	}
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionTrue {
		t.Errorf("expected Ready True, got %+v", meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionReady))
	}
	if cs.Status.WorkloadVersion != wantVersion {
		t.Errorf("workloadVersion = %q, want %q", cs.Status.WorkloadVersion, wantVersion)
	}
}

func TestPipelineRolloutNoneStampsObject(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "napp")
	cs := makeConfigSync(t, env, "nsync", "napp", kohenv1alpha1.RolloutNone)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "0011223344556677"})
	reconcileN(t, r, key, 2)

	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "napp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	want := "git:001122334455"
	if d.Annotations[kohenv1alpha1.AnnotationConfigSHA] != want {
		t.Errorf("object stamp = %q, want %q", d.Annotations[kohenv1alpha1.AnnotationConfigSHA], want)
	}
	if _, ok := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("pod template must not be stamped in none mode")
	}
	// The config volume/mount must still be present: the object-level stamp must
	// not retract the pod-template wiring (C1 — the two SSA applies must not use
	// the same field manager with disjoint field sets).
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("config volume stripped in none mode: %+v", d.Spec.Template.Spec.Volumes)
	}
	if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 ||
		d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/etc/kohen/config" {
		t.Fatalf("config mount stripped in none mode: %+v", d.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
	// Steady state: extra reconciles must not churn the pod template (no restart).
	genBefore := d.Generation
	reconcileN(t, r, key, 2)
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "napp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if d.Generation != genBefore {
		t.Errorf("none mode churned the workload: generation %d -> %d", genBefore, d.Generation)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("config volume not stable across reconciles: %+v", d.Spec.Template.Spec.Volumes)
	}
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionTrue {
		t.Errorf("expected Ready True in none mode, got %+v", cs.Status.Conditions)
	}
}

func TestPipelineFetchFailureKeepsLastGood(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "fapp")
	cs := makeConfigSync(t, env, "fsync", "fapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "good: config\n"})
	good := &fakeFetcher{dir: dir, commit: "aaaabbbbccccdddd"}
	r := newReconciler(env, good)
	reconcileN(t, r, key, 2)

	// Sanity: ConfigMap applied.
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fapp-config", Namespace: "default"}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("configmap not applied: %v", err)
	}

	// Now git goes down.
	r.Fetcher = &fakeFetcher{err: &git.Error{Reason: git.ReasonFetchFailed, Msg: "connection refused"}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected fetch error to be returned for backoff")
	}

	// Fail-safe: last-good ConfigMap must NOT be pruned.
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "fapp-config", Namespace: "default"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("last-good configmap was pruned on fetch failure: %v", err)
	}
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionFetched) != metav1.ConditionFalse {
		t.Errorf("expected Fetched False, got %+v", cs.Status.Conditions)
	}
	if cs.Status.SourceCommit != "aaaabbbbccccdddd" {
		t.Errorf("last-good sourceCommit lost: %q", cs.Status.SourceCommit)
	}
}

func TestPipelineRenderFailure(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "rapp")
	cs := makeConfigSync(t, env, "rsync", "rapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	// A symlink escaping the tree triggers a tree-safety render error (R7.5).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "1234123412341234"})
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionRendered) != metav1.ConditionFalse {
		t.Errorf("expected Rendered False, got %+v", cs.Status.Conditions)
	}
	if c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionRendered); c == nil || c.Reason != kohenv1alpha1.ReasonTreeSafetyViolation {
		t.Errorf("expected TreeSafetyViolation reason, got %+v", c)
	}
}

func TestPipelineRefusesAdoption(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "aapp")
	// Pre-existing user ConfigMap.
	pre := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "aapp-config", Namespace: "default"},
		Data:       map[string]string{"user": "owned"},
	}
	if err := env.Client.Create(ctx, pre); err != nil {
		t.Fatal(err)
	}
	cs := makeConfigSync(t, env, "async", "aapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "deadbeefdeadbeef"})
	reconcileN(t, r, key, 2)

	// User data preserved.
	got := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "aapp-config", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Data["user"] != "owned" {
		t.Errorf("user configmap was overwritten: %v", got.Data)
	}
	cs = getCS(t, env, key)
	if condStatus(cs, kohenv1alpha1.ConditionReady) != metav1.ConditionFalse {
		t.Errorf("expected Ready False on adoption refusal")
	}
}

func TestPipelineSingletonViolation(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "sapp")
	first := makeConfigSync(t, env, "first", "sapp", kohenv1alpha1.RolloutAuto)
	second := makeConfigSync(t, env, "second", "sapp", kohenv1alpha1.RolloutAuto)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "1111222233334444"})

	reconcileN(t, r, client.ObjectKeyFromObject(first), 2)
	reconcileN(t, r, client.ObjectKeyFromObject(second), 2)

	cs := getCS(t, env, client.ObjectKeyFromObject(second))
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionWorkloadWired)
	if c == nil || c.Reason != kohenv1alpha1.ReasonSingletonViolation {
		t.Errorf("expected SingletonViolation on second sync, got %+v", c)
	}
}

func TestSingletonIncumbentKeepsWorking(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "shared")
	// "aa-first" is created first and sorts first ⇒ it must win.
	first := makeConfigSync(t, env, "aa-first", "shared", kohenv1alpha1.RolloutAuto)
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "aaaa111122223333"})
	reconcileN(t, r, client.ObjectKeyFromObject(first), 2)

	second := makeConfigSync(t, env, "zz-second", "shared", kohenv1alpha1.RolloutAuto)
	reconcileN(t, r, client.ObjectKeyFromObject(second), 2)
	// Re-reconcile the incumbent now that a rival exists.
	reconcileN(t, r, client.ObjectKeyFromObject(first), 1)

	csFirst := getCS(t, env, client.ObjectKeyFromObject(first))
	if c := meta.FindStatusCondition(csFirst.Status.Conditions, kohenv1alpha1.ConditionWorkloadWired); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("incumbent should stay WorkloadWired=True, got %+v", c)
	}
	csSecond := getCS(t, env, client.ObjectKeyFromObject(second))
	if c := meta.FindStatusCondition(csSecond.Status.Conditions, kohenv1alpha1.ConditionWorkloadWired); c == nil || c.Reason != kohenv1alpha1.ReasonSingletonViolation {
		t.Errorf("newcomer should be SingletonViolation, got %+v", c)
	}
}

func TestSingletonLoserDeletionKeepsIncumbentWired(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "shared2")
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "abcabcabcabc1234"})

	// Incumbent wins and wires the workload.
	incumbent := makeConfigSync(t, env, "aa-owner", "shared2", kohenv1alpha1.RolloutAuto)
	reconcileN(t, r, client.ObjectKeyFromObject(incumbent), 2)

	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "shared2", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("incumbent did not wire the workload: %+v", d.Spec.Template.Spec.Volumes)
	}

	// A losing duplicate targets the same workload, then is deleted.
	loser := makeConfigSync(t, env, "zz-loser", "shared2", kohenv1alpha1.RolloutAuto)
	reconcileN(t, r, client.ObjectKeyFromObject(loser), 2)
	loser = getCS(t, env, client.ObjectKeyFromObject(loser))
	if err := env.Client.Delete(ctx, loser); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, client.ObjectKeyFromObject(loser), 2)

	// The incumbent's wiring MUST survive the loser's finalizer (H-A): a shared
	// field manager means an unconditional Unwire would retract the incumbent's
	// volume/mount on the shared workload.
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "shared2", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("incumbent workload was unwired by loser deletion: %+v", d.Spec.Template.Spec.Volumes)
	}
	if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 {
		t.Errorf("incumbent mount removed by loser deletion")
	}
}

func TestSyncNowAnnotationCleared(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "snapp")
	cs := makeConfigSync(t, env, "snsync", "snapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "5a5a5a5a5a5a5a5a"})

	reconcileN(t, r, key, 1) // add finalizer

	cs = getCS(t, env, key)
	cs.Annotations = map[string]string{kohenv1alpha1.AnnotationSyncNow: "now"}
	if err := env.Client.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 1) // should clear the annotation

	cs = getCS(t, env, key)
	if _, ok := cs.Annotations[kohenv1alpha1.AnnotationSyncNow]; ok {
		t.Errorf("sync-now annotation was not cleared: %v", cs.Annotations)
	}
}

func TestPipelineRedactsSecretsInStatusAndEvents(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "redapp")

	const token = "supersecrettoken1234567890"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gitcreds", Namespace: "default",
			Labels: map[string]string{kohenv1alpha1.LabelGitCredential: "true"},
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := env.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}

	cs := makeConfigSync(t, env, "redsync", "redapp", kohenv1alpha1.RolloutAuto)
	cs = getCS(t, env, client.ObjectKeyFromObject(cs))
	cs.Spec.Source.AuthSecretRef = &kohenv1alpha1.LocalObjectReference{Name: "gitcreds"}
	if err := env.Client.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(cs)

	// The fetch fails with an error message that leaks the token (as a go-git
	// error echoing a credentialed URL might).
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{
		dir:    dir,
		commit: "1212121212121212",
		err:    &git.Error{Reason: git.ReasonFetchFailed, Msg: "clone https://x:" + token + "@host failed"},
	})
	rec := r.Recorder.(*record.FakeRecorder)
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionFetched)
	if c == nil {
		t.Fatal("expected a Fetched condition")
	}
	if strings.Contains(c.Message, token) {
		t.Errorf("token leaked into status condition message: %q", c.Message)
	}
	if !strings.Contains(c.Message, redact.Placeholder) {
		t.Errorf("expected redaction placeholder in status message, got %q", c.Message)
	}

	drain := true
	for drain {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, token) {
				t.Errorf("token leaked into event: %q", ev)
			}
		default:
			drain = false
		}
	}
}

func TestPipelineNoSecretLeakAcrossSinks(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "leakapp")

	const token = "distinctive-leak-token-9f8e7d6c"
	scan := leakcheck.New(token)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "leakcreds", Namespace: "default",
			Labels: map[string]string{kohenv1alpha1.LabelGitCredential: "true"},
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := env.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	cs := makeConfigSync(t, env, "leaksync", "leakapp", kohenv1alpha1.RolloutAuto)
	cs = getCS(t, env, client.ObjectKeyFromObject(cs))
	cs.Spec.Source.AuthSecretRef = &kohenv1alpha1.LocalObjectReference{Name: "leakcreds"}
	if err := env.Client.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "cafebabecafebabe"})
	rec := r.Recorder.(*record.FakeRecorder)
	reconcileN(t, r, key, 2)

	// Happy path: scan every persisted sink for the fixture token.
	cs = getCS(t, env, key)
	scan.AssertObjectClean(t, "configsync status", cs)
	cm := &corev1.ConfigMap{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "leakapp-config", Namespace: "default"}, cm); err != nil {
		t.Fatal(err)
	}
	scan.AssertObjectClean(t, "configmap", cm)
	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "leakapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	scan.AssertObjectClean(t, "workload", d)

	// Failure path: an error echoing the token must not leak into status/events.
	r.Fetcher = &fakeFetcher{err: &git.Error{Reason: git.ReasonFetchFailed, Msg: "clone https://x:" + token + "@h failed"}}
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	cs = getCS(t, env, key)
	scan.AssertObjectClean(t, "configsync status after failure", cs)
	for drain := true; drain; {
		select {
		case ev := <-rec.Events:
			scan.AssertClean(t, "event", ev)
		default:
			drain = false
		}
	}
}

func TestPipelineWorkloadNotFound(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	cs := makeConfigSync(t, env, "wsync", "ghost", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "5555666677778888"})
	reconcileN(t, r, key, 2)

	// ConfigMap still applied (object sync continues).
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "ghost-config", Namespace: "default"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("configmap should still be applied when workload missing: %v", err)
	}
	cs = getCS(t, env, key)
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionWorkloadWired)
	if c == nil || c.Reason != kohenv1alpha1.ReasonWorkloadNotFound {
		t.Errorf("expected WorkloadNotFound, got %+v", c)
	}
}

func TestPipelineFinalizerCleanup(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "dapp")
	cs := makeConfigSync(t, env, "dsync", "dapp", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "99aabbccddeeff00"})
	reconcileN(t, r, key, 2)

	// Delete the ConfigSync → finalizer runs cleanup.
	cs = getCS(t, env, key)
	if err := env.Client.Delete(ctx, cs); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 2)

	// ConfigSync gone (finalizer removed).
	if err := env.Client.Get(ctx, key, &kohenv1alpha1.ConfigSync{}); !apierrors.IsNotFound(err) {
		t.Errorf("configsync should be gone, got %v", err)
	}
	// Owned ConfigMap pruned.
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "dapp-config", Namespace: "default"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("owned configmap should be pruned on delete, got %v", err)
	}
	// Workload unwired.
	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "dapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("volume not retracted on delete: %+v", d.Spec.Template.Spec.Volumes)
	}
	if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("mount not retracted on delete")
	}
	if _, ok := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("pod-template stamp not retracted on delete: %v", d.Spec.Template.Annotations)
	}
}

func TestPipelineFinalizerClearsNoneStamp(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "none-dapp")
	cs := makeConfigSync(t, env, "none-dsync", "none-dapp", kohenv1alpha1.RolloutNone)
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "99aabbccddeeff00"})
	reconcileN(t, r, key, 2)

	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "none-dapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if d.Annotations[kohenv1alpha1.AnnotationConfigSHA] == "" {
		t.Fatal("expected object-level stamp before delete")
	}

	cs = getCS(t, env, key)
	if err := env.Client.Delete(ctx, cs); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, key, 2)

	if err := env.Client.Get(ctx, client.ObjectKey{Name: "none-dapp", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("object stamp (kohen-stamp) not retracted on delete: %v", d.Annotations)
	}
}

func TestPipelineRolloutNoneResetsRolloutInProgress(t *testing.T) {
	env := testenv.Start(t)
	makeDeployment(t, env, "rip-app")
	cs := makeConfigSync(t, env, "rip-sync", "rip-app", kohenv1alpha1.RolloutNone)
	key := client.ObjectKeyFromObject(cs)
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "aabbccddeeff0011"})

	// Seed a stale rolloutInProgress before reconcile (e.g. after auto→none).
	cs = getCS(t, env, key)
	cs.Status.RolloutInProgress = true
	if err := env.Client.Status().Update(context.Background(), cs); err != nil {
		t.Fatal(err)
	}

	reconcileN(t, r, key, 2)
	cs = getCS(t, env, key)
	if cs.Status.RolloutInProgress {
		t.Errorf("rolloutInProgress still true after rollout:none sync")
	}
}

func TestPipelineSkipsWireWhenStampMatches(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "skip-app")
	cs := makeConfigSync(t, env, "skip-sync", "skip-app", kohenv1alpha1.RolloutAuto)
	key := client.ObjectKeyFromObject(cs)
	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "ccddeeff00112233"})
	reconcileN(t, r, key, 2)

	d := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "skip-app", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	genBefore := d.Generation
	rvBefore := d.ResourceVersion

	// Same version again — stamp matches ⇒ no workload write (R-ROLLOUT.2).
	reconcileN(t, r, key, 1)
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "skip-app", Namespace: "default"}, d); err != nil {
		t.Fatal(err)
	}
	if d.Generation != genBefore {
		t.Errorf("generation advanced on stamp-match reconcile: %d → %d", genBefore, d.Generation)
	}
	if d.ResourceVersion != rvBefore {
		t.Errorf("resourceVersion changed on stamp-match reconcile: %s → %s", rvBefore, d.ResourceVersion)
	}
}

func TestPipelineCredentialMissingLabel(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	makeDeployment(t, env, "capp")
	// Secret without the required credential label.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("hunter2secret")},
	}
	if err := env.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	cs := makeConfigSync(t, env, "csync", "capp", kohenv1alpha1.RolloutAuto)
	cs = getCS(t, env, client.ObjectKeyFromObject(cs))
	cs.Spec.Source.AuthSecretRef = &kohenv1alpha1.LocalObjectReference{Name: "creds"}
	if err := env.Client.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(cs)

	dir := fixtureDir(t, map[string]string{"app.yaml": "a: b\n"})
	r := newReconciler(env, &fakeFetcher{dir: dir, commit: "abcabcabcabcabca"})
	reconcileN(t, r, key, 2)

	cs = getCS(t, env, key)
	c := meta.FindStatusCondition(cs.Status.Conditions, kohenv1alpha1.ConditionFetched)
	if c == nil || c.Reason != kohenv1alpha1.ReasonAuthFailed {
		t.Errorf("expected AuthFailed for unlabeled credential secret, got %+v", c)
	}
}
