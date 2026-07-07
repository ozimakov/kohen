//go:build e2e

// Security conformance suite (PLAN S3.1, SPEC A9): pod hardening and RBAC
// least-privilege on a live cluster. Complements the abuse-case regression in
// secrets_test.go (A11).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

const (
	defaultOperatorNS     = "kohen-system"
	defaultOperatorDeploy = "kohen"
	defaultHelmRelease    = "kohen"
)

func operatorNamespace() string {
	if v := os.Getenv("KOHEN_OPERATOR_NAMESPACE"); v != "" {
		return v
	}
	return defaultOperatorNS
}

func operatorDeployName() string {
	if v := os.Getenv("KOHEN_OPERATOR_DEPLOY"); v != "" {
		return v
	}
	return defaultOperatorDeploy
}

func helmReleaseName() string {
	if v := os.Getenv("KOHEN_HELM_RELEASE"); v != "" {
		return v
	}
	return defaultHelmRelease
}

func rbacScope() string {
	if v := os.Getenv("KOHEN_SCOPE"); v != "" {
		return v
	}
	return "cluster"
}

// TestU3PodSecurityConformance asserts the running operator pod matches the
// hardened defaults from the Helm chart (A9): non-root, read-only rootfs.
func TestU3PodSecurityConformance(t *testing.T) {
	out, err := exec.Command("kubectl", "-n", operatorNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=kohen",
		"-o", "jsonpath={.items[0].spec.containers[0].securityContext}").Output()
	if err != nil {
		t.Fatalf("get operator pod securityContext: %v\n%s", err, out)
	}
	ctx := string(out)
	if !strings.Contains(ctx, `"runAsNonRoot":true`) {
		t.Fatalf("operator container must runAsNonRoot: %s", ctx)
	}
	if !strings.Contains(ctx, `"readOnlyRootFilesystem":true`) {
		t.Fatalf("operator container must have readOnlyRootFilesystem: %s", ctx)
	}
	podSC, err := exec.Command("kubectl", "-n", operatorNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=kohen",
		"-o", "jsonpath={.items[0].spec.securityContext.runAsNonRoot}").Output()
	if err != nil {
		t.Fatalf("get pod securityContext: %v", err)
	}
	if strings.TrimSpace(string(podSC)) != "true" {
		t.Fatalf("operator pod securityContext.runAsNonRoot = %q, want true", podSC)
	}
}

// TestU3RBACConformance verifies reconcile fails when a required RBAC rule is
// removed and recovers when restored (A9). Uses the cluster- or namespace-scoped
// Role/ClusterRole installed by Helm (KOHEN_SCOPE).
func TestU3RBACConformance(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	// Namespaced operators only watch their release namespace; cluster-scoped
	// operators reconcile any namespace.
	ns := "kohen-e2e-rbac"
	gitSvc, workload := "gitserver", "app"
	if rbacScope() == "namespaced" {
		ns = operatorNamespace()
		gitSvc, workload = "gitserver-rbac", "app-rbac"
	}
	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, gitSvc, nil)
	deployDeployment(t, c, ns, workload)
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "rbac-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, gitSvc),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: workload},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, cs) })
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	roleName := helmReleaseName()
	roleGVK := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	roleKey := client.ObjectKey{Name: roleName}
	if rbacScope() == "namespaced" {
		roleGVK.Kind = "Role"
		roleKey.Namespace = operatorNamespace()
	}

	role := &unstructured.Unstructured{}
	role.SetGroupVersionKind(roleGVK)
	if err := c.Get(ctx, roleKey, role); err != nil {
		t.Fatalf("get %s %s: %v", roleGVK.Kind, roleName, err)
	}
	origRules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	t.Cleanup(func() {
		_ = unstructured.SetNestedSlice(role.Object, origRules, "rules")
		_ = c.Update(ctx, role)
	})

	// Drop workload patch permission — stamping must fail while git fetch/render still works.
	stripped := stripRule(origRules, "apps", "deployments", "patch")
	if err := unstructured.SetNestedSlice(role.Object, stripped, "rules"); err != nil {
		t.Fatalf("set stripped rules: %v", err)
	}
	if err := c.Update(ctx, role); err != nil {
		t.Fatalf("patch %s: %v", roleGVK.Kind, err)
	}
	// Allow the informer cache to observe the RBAC change.
	time.Sleep(5 * time.Second)

	commitFile(t, ns, gitSvc, 18460, "svc/app.yaml", "greeting: rbac-stripped\n")
	eventually(t, 120*time.Second, "workload wiring fails without patch RBAC", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		cond := metaFindCondition(got.Status.Conditions, kohenv1alpha1.ConditionWorkloadWired)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return fmt.Errorf("WorkloadWired not False: %+v", got.Status.Conditions)
		}
		return nil
	})

	if err := unstructured.SetNestedSlice(role.Object, origRules, "rules"); err != nil {
		t.Fatalf("restore rules: %v", err)
	}
	if err := c.Update(ctx, role); err != nil {
		t.Fatalf("restore %s: %v", roleGVK.Kind, err)
	}
	time.Sleep(5 * time.Second)

	// Force a reconcile and expect recovery.
	got := &kohenv1alpha1.ConfigSync{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("get configsync: %v", err)
	}
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations[kohenv1alpha1.AnnotationSyncNow] = "rbac-restore"
	if err := c.Update(ctx, got); err != nil {
		t.Fatalf("force sync: %v", err)
	}
	configSyncReady(t, c, key, 120*time.Second)
}

func stripRule(rules []any, group, resource, verb string) []any {
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			out = append(out, r)
			continue
		}
		groups, _ := m["apiGroups"].([]any)
		resources, _ := m["resources"].([]any)
		verbs, _ := m["verbs"].([]any)
		if !sliceHas(groups, group) || !sliceHas(resources, resource) {
			out = append(out, r)
			continue
		}
		newVerbs := make([]any, 0, len(verbs))
		for _, v := range verbs {
			if s, _ := v.(string); s == verb {
				continue
			}
			newVerbs = append(newVerbs, v)
		}
		if len(newVerbs) == 0 {
			continue
		}
		clone := map[string]any{}
		for k, v := range m {
			clone[k] = v
		}
		clone["verbs"] = newVerbs
		out = append(out, clone)
	}
	return out
}

func sliceHas(items []any, want string) bool {
	for _, it := range items {
		if s, _ := it.(string); s == want {
			return true
		}
	}
	return false
}

// TestU3NamespacedScopeIsolation is run when KOHEN_SCOPE=namespaced: a
// ConfigSync in a namespace outside the operator watch must not reconcile.
func TestU3NamespacedScopeIsolation(t *testing.T) {
	if rbacScope() != "namespaced" {
		t.Skip("namespaced-scope isolation applies only when KOHEN_SCOPE=namespaced")
	}
	ctx := context.Background()
	c := newClient(t)
	outside := "kohen-e2e-outside-watch"
	setupNamespace(t, c, outside)
	deployGitServer(t, c, outside, "gitserver", nil)
	deployDeployment(t, c, outside, "app")
	createCredentialSecret(t, c, outside, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "outside-sync", Namespace: outside},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(outside, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 3 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, cs) })

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cs), got); err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status.SourceCommit != "" {
			t.Fatalf("ConfigSync outside watch namespace reconciled: status=%+v", got.Status)
		}
		time.Sleep(3 * time.Second)
	}
}

func metaFindCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}
