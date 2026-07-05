//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// forceSync toggles the sync-now annotation to trigger an immediate reconcile.
func forceSync(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	eventually(t, 30*time.Second, "force sync", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(context.Background(), key, got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[kohenv1alpha1.AnnotationSyncNow] = fmt.Sprintf("%d", time.Now().UnixNano())
		return c.Update(context.Background(), got)
	})
}

// TestU1RolloutNone is scenario 2's rollout:none half: a config change updates
// the mounted volume (ConfigMap) and the object-level version, with no rollout
// (pod template stays unstamped, generation unchanged).
func TestU1RolloutNone(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-none"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "none-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
			Rollout:     kohenv1alpha1.RolloutNone,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	// Volume wired, but pod template NOT stamped; version lives on the object.
	var objV1 string
	eventually(t, 60*time.Second, "wired without pod-template stamp", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			return err
		}
		if len(d.Spec.Template.Spec.Volumes) == 0 {
			return fmt.Errorf("no volume wired")
		}
		if s := d.Spec.Template.Annotations[configSHAAnnotation]; s != "" {
			return fmt.Errorf("pod template unexpectedly stamped: %q", s)
		}
		objV1 = d.Annotations[configSHAAnnotation]
		if objV1 == "" {
			return fmt.Errorf("object not stamped with version")
		}
		return nil
	})
	// Settle initial wiring, then record the ReplicaSet count. "No rollout" is
	// most faithfully "no new ReplicaSet is created" — a generation bump from a
	// non-pod-template field would not roll pods, so we assert on ReplicaSets.
	waitStableGeneration(t, c, ns, "app", 8*time.Second, 60*time.Second)
	rsBefore := replicaSetCount(t, c, ns, "app")

	// Commit a change ⇒ ConfigMap updates, object version advances, NO rollout.
	commitFile(t, ns, "gitserver", 18444, "svc/app.yaml", "greeting: hello-v2\n")
	eventually(t, 90*time.Second, "configmap + object version advance", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: hello-v2\n" {
			return fmt.Errorf("configmap not updated: %q", cm.Data["app.yaml"])
		}
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			return err
		}
		if d.Annotations[configSHAAnnotation] == objV1 {
			return fmt.Errorf("object version not advanced")
		}
		return nil
	})
	consistently(t, 15*time.Second, "no rollout in rollout:none", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			return err
		}
		if s := d.Spec.Template.Annotations[configSHAAnnotation]; s != "" {
			return fmt.Errorf("pod template got stamped in none mode: %q", s)
		}
		if got := replicaSetCount(t, c, ns, "app"); got != rsBefore {
			return fmt.Errorf("a rollout happened in none mode: replicasets %d -> %d", rsBefore, got)
		}
		return nil
	})
}

// replicaSetCount returns the number of ReplicaSets owned by the named Deployment
// (a new ReplicaSet is the ground-truth signal that a rollout occurred).
func replicaSetCount(t *testing.T, c client.Client, ns, deployment string) int {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: deployment}, d); err != nil {
		t.Fatalf("get deploy %s: %v", deployment, err)
	}
	var list appsv1.ReplicaSetList
	if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatalf("list replicasets: %v", err)
	}
	n := 0
	for i := range list.Items {
		for _, owner := range list.Items[i].OwnerReferences {
			if owner.UID == d.UID {
				n++
			}
		}
	}
	return n
}

// TestU1Rollback is scenario 5 (UC6): pinning spec.source.ref to a prior commit
// restores that version's config and rolls the workload back to it.
func TestU1Rollback(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-rollback"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	sha1 := commitFile(t, ns, "gitserver", 18445, "rb/app.yaml", "greeting: rb-v1\n")
	commitFile(t, ns, "gitserver", 18445, "rb/app.yaml", "greeting: rb-v2\n")

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "rb-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "rb",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	eventually(t, 60*time.Second, "latest (v2) rendered", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: rb-v2\n" {
			return fmt.Errorf("expected v2, got %q", cm.Data["app.yaml"])
		}
		return nil
	})

	// Pin to the prior commit ⇒ v1 restored and re-rolled.
	eventually(t, 30*time.Second, "pin ref to sha1", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		got.Spec.Source.Ref = sha1
		return c.Update(ctx, got)
	})
	eventually(t, 90*time.Second, "rollback to v1", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: rb-v1\n" {
			return fmt.Errorf("expected v1 after rollback, got %q", cm.Data["app.yaml"])
		}
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		if !strings.HasPrefix(got.Status.SourceCommit, sha1[:8]) && got.Status.SourceCommit != sha1 {
			return fmt.Errorf("sourceCommit = %q, want %q", got.Status.SourceCommit, sha1)
		}
		return nil
	})
}

// TestU1GitOutage is scenario 6: a git outage degrades the ConfigSync while the
// workload stays healthy on last-good config; recovery auto-heals it.
func TestU1GitOutage(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-outage"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "outage-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	// Cause the outage: scale the git server to zero.
	scaleDeployment(t, c, ns, "gitserver", 0)

	// Fetched goes False and Ready degrades; but the last-good ConfigMap remains
	// and the workload keeps its wiring.
	forceSync(t, c, key)
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionFetched,
		metav1.ConditionFalse, kohenv1alpha1.ReasonFetchFailed, 90*time.Second)
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, cm); err != nil {
		t.Fatalf("last-good configmap should remain during outage: %v", err)
	}
	d := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
		t.Fatalf("workload should stay healthy during outage: %v", err)
	}
	if len(d.Spec.Template.Spec.Volumes) == 0 {
		t.Fatalf("workload wiring lost during outage")
	}

	// Recover: scale the git server back up ⇒ auto-recovers to Ready.
	scaleDeployment(t, c, ns, "gitserver", 1)
	waitDeployReady(t, c, ns, "gitserver", 120*time.Second)
	forceSync(t, c, key)
	configSyncReady(t, c, key, 120*time.Second)
}

func scaleDeployment(t *testing.T, c client.Client, ns, name string, replicas int32) {
	t.Helper()
	eventually(t, 30*time.Second, fmt.Sprintf("scale %s to %d", name, replicas), func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
			return err
		}
		d.Spec.Replicas = &replicas
		return c.Update(context.Background(), d)
	})
}

// TestU1ErrorUX is scenario 7: documented, actionable failure conditions —
// missing workload, oversize config, and an unsupported rollout strategy.
func TestU1ErrorUX(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-errux"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	mkSync := func(name, path, kind, workload string, rollout kohenv1alpha1.RolloutMode) *kohenv1alpha1.ConfigSync {
		return &kohenv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: kohenv1alpha1.ConfigSyncSpec{
				Source: kohenv1alpha1.GitSource{
					URL:           gitURL(ns, "gitserver"),
					Ref:           "main",
					AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
				},
				Path:        path,
				WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: kind, Name: workload},
				Rollout:     rollout,
				Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
			},
		}
	}

	t.Run("workload_not_found", func(t *testing.T) {
		cs := mkSync("ghost-sync", "svc", "Deployment", "ghost", kohenv1alpha1.RolloutAuto)
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		waitConditionReason(t, c, client.ObjectKeyFromObject(cs), kohenv1alpha1.ConditionWorkloadWired,
			metav1.ConditionFalse, kohenv1alpha1.ReasonWorkloadNotFound, 90*time.Second)
	})

	t.Run("unsupported_strategy", func(t *testing.T) {
		deployStatefulSet(t, c, ns, "ondelete-ss", appsv1.OnDeleteStatefulSetStrategyType)
		cs := mkSync("ss-sync", "svc", "StatefulSet", "ondelete-ss", kohenv1alpha1.RolloutAuto)
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		waitConditionReason(t, c, client.ObjectKeyFromObject(cs), kohenv1alpha1.ConditionWorkloadWired,
			metav1.ConditionFalse, kohenv1alpha1.ReasonUnsupportedStrategy, 90*time.Second)
	})

	t.Run("oversize", func(t *testing.T) {
		deployDeployment(t, c, ns, "big-app")
		// > 1 MiB ConfigMap limit ⇒ Rendered=False/Oversize.
		big := strings.Repeat("x", 1_300_000)
		commitFile(t, ns, "gitserver", 18446, "big/big.yaml", big)
		cs := mkSync("big-sync", "big", "Deployment", "big-app", kohenv1alpha1.RolloutAuto)
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		waitConditionReason(t, c, client.ObjectKeyFromObject(cs), kohenv1alpha1.ConditionRendered,
			metav1.ConditionFalse, kohenv1alpha1.ReasonOversize, 90*time.Second)
	})
}

// TestU1PrivateRepoAuth is scenario 4: private-repo HTTPS auth — correct
// credentials sync; wrong credentials surface Degraded/AuthFailed.
func TestU1PrivateRepoAuth(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-auth"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", []corev1.EnvVar{
		{Name: "AUTH_USER", Value: "git"},
		{Name: "AUTH_PASS", Value: "s3cr3t"},
	})
	deployDeployment(t, c, ns, "good-app")
	deployDeployment(t, c, ns, "token-app")
	deployDeployment(t, c, ns, "bad-app")

	// SSH-key auth (ssh-privatekey/known_hosts) is validated at the unit tier
	// (internal/git/auth_test.go: TestAuthMethodSSH); the e2e here exercises the
	// two HTTPS credential forms end-to-end: username/password and token.
	createCredentialSecret(t, c, ns, "good-creds", map[string][]byte{
		"username":                 []byte("git"),
		"password":                 []byte("s3cr3t"),
		"insecure-skip-tls-verify": []byte("true"),
	})
	createCredentialSecret(t, c, ns, "token-creds", map[string][]byte{
		"username":                 []byte("git"),
		"token":                    []byte("s3cr3t"),
		"insecure-skip-tls-verify": []byte("true"),
	})
	createCredentialSecret(t, c, ns, "bad-creds", map[string][]byte{
		"username":                 []byte("git"),
		"password":                 []byte("wrong"),
		"insecure-skip-tls-verify": []byte("true"),
	})

	mkSync := func(name, workload, secret string) *kohenv1alpha1.ConfigSync {
		return &kohenv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: kohenv1alpha1.ConfigSyncSpec{
				Source: kohenv1alpha1.GitSource{
					URL:           gitURL(ns, "gitserver"),
					Ref:           "main",
					AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: secret},
				},
				Path:        "svc",
				WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: workload},
				Rollout:     kohenv1alpha1.RolloutAuto,
				Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
			},
		}
	}

	t.Run("valid_credentials_sync", func(t *testing.T) {
		cs := mkSync("good-sync", "good-app", "good-creds")
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		configSyncReady(t, c, client.ObjectKeyFromObject(cs), 120*time.Second)
	})

	t.Run("token_credential_sync", func(t *testing.T) {
		cs := mkSync("token-sync", "token-app", "token-creds")
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		configSyncReady(t, c, client.ObjectKeyFromObject(cs), 120*time.Second)
	})

	t.Run("bad_credentials_degraded", func(t *testing.T) {
		cs := mkSync("bad-sync", "bad-app", "bad-creds")
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		key := client.ObjectKeyFromObject(cs)
		waitConditionReason(t, c, key, kohenv1alpha1.ConditionFetched,
			metav1.ConditionFalse, kohenv1alpha1.ReasonAuthFailed, 90*time.Second)
		// An actionable, redacted AuthFailed event must be emitted (R8.3/TM9);
		// the credential value must never appear in status or events.
		assertAuthFailedEvent(t, c, ns, cs.Name)
		assertNoSecretLeak(t, c, ns, key, cs.Name, "wrong")
	})
}

// assertAuthFailedEvent waits for a Warning AuthFailed event on the ConfigSync.
func assertAuthFailedEvent(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	eventually(t, 90*time.Second, "AuthFailed event", func() error {
		var events corev1.EventList
		if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == "ConfigSync" && e.InvolvedObject.Name == name &&
				e.Reason == kohenv1alpha1.ReasonAuthFailed && e.Type == corev1.EventTypeWarning {
				return nil
			}
		}
		return fmt.Errorf("no Warning/AuthFailed event for %s", name)
	})
}

// assertNoSecretLeak fails if secret appears in the ConfigSync status conditions
// or in any event message for the object.
func assertNoSecretLeak(t *testing.T, c client.Client, ns string, key client.ObjectKey, name, secret string) {
	t.Helper()
	got := &kohenv1alpha1.ConfigSync{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get configsync: %v", err)
	}
	for _, cond := range got.Status.Conditions {
		if strings.Contains(cond.Message, secret) {
			t.Fatalf("secret leaked into condition %s: %q", cond.Type, cond.Message)
		}
	}
	var events corev1.EventList
	if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "ConfigSync" && e.InvolvedObject.Name == name &&
			strings.Contains(e.Message, secret) {
			t.Fatalf("secret leaked into event %s: %q", e.Reason, e.Message)
		}
	}
}

// TestU1GitOpsCoexistence is scenario 9 (A10): a second SSA manager (simulating
// an Argo CD / Flux controller in server-side-apply mode) managing the same
// Deployment coexists with Kohen — neither strips the other's fields, and there
// is no flapping.
func TestU1GitOpsCoexistence(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-gitops"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "gitops-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	// Simulate the GitOps controller: a server-side apply (force) of the app's
	// own fields (replicas + a label) with a distinct field manager, NOT listing
	// Kohen's volume/mount/stamp. SSA keyed-list merge must preserve them.
	gitopsApply(t, c, ns, "app", 3)

	// Kohen's fields survive the GitOps apply.
	assertKohenWired(t, c, ns, "app")

	// Simulate an ongoing GitOps reconcile loop with self-heal (repeated forced
	// SSA of the app-owned fields) interleaved with Kohen reconciles, and assert
	// neither manager ever strips the other's fields — i.e. no flapping (A10).
	deadline := time.Now().Add(24 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		gitopsApply(t, c, ns, "app", 3) // GitOps re-applies its desired state
		if i%2 == 0 {
			forceSync(t, c, key) // Kohen re-reconciles
		}
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			t.Fatalf("get deploy: %v", err)
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 3 {
			t.Fatalf("gitops replicas retracted: %v", d.Spec.Replicas)
		}
		if d.Spec.Template.Labels["managed-by-gitops"] != "true" {
			t.Fatalf("gitops label retracted")
		}
		if len(d.Spec.Template.Spec.Volumes) == 0 {
			t.Fatalf("kohen volume retracted by gitops apply")
		}
		if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 {
			t.Fatalf("kohen mount retracted by gitops apply")
		}
		if d.Spec.Template.Annotations[configSHAAnnotation] == "" {
			t.Fatalf("kohen stamp retracted by gitops apply")
		}
		time.Sleep(3 * time.Second)
	}
}

func gitopsApply(t *testing.T, c client.Client, ns, name string, replicas int64) {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{
					"app":               name,
					"managed-by-gitops": "true",
				}},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":  "app",
						"image": "registry.k8s.io/pause:3.9",
					}},
				},
			},
		},
	}}
	if err := c.Patch(context.Background(), u, client.Apply,
		client.FieldOwner("gitops-controller"), client.ForceOwnership); err != nil {
		t.Fatalf("gitops SSA apply: %v", err)
	}
}

func assertKohenWired(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	eventually(t, 60*time.Second, "kohen fields present after gitops apply", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
			return err
		}
		if len(d.Spec.Template.Spec.Volumes) == 0 {
			return fmt.Errorf("kohen volume missing")
		}
		if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 {
			return fmt.Errorf("kohen mount missing")
		}
		if d.Spec.Template.Annotations[configSHAAnnotation] == "" {
			return fmt.Errorf("kohen stamp missing")
		}
		return nil
	})
}
