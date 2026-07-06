//go:build e2e

// Operator lifecycle suite (PLAN S3.3, SPEC A12): Helm upgrade keeps syncs
// converging; uninstall leaves user workloads and objects in place.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

func helmChartPath() string {
	if v := os.Getenv("KOHEN_CHART_PATH"); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "./deploy/helm/kohen"
	}
	return filepath.Join(wd, "deploy", "helm", "kohen")
}

func installMethod() string {
	if v := os.Getenv("KOHEN_INSTALL_METHOD"); v != "" {
		return v
	}
	return "helm"
}

// TestU3OperatorUpgrade verifies a Helm upgrade keeps an in-flight ConfigSync
// converging (A12).
func TestU3OperatorUpgrade(t *testing.T) {
	if installMethod() != "helm" {
		t.Skip("upgrade test requires KOHEN_INSTALL_METHOD=helm")
	}
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-upgrade"
	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-sync", Namespace: ns},
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
	t.Cleanup(func() { _ = c.Delete(ctx, cs) })
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 120*time.Second)

	args := []string{
		"upgrade", helmReleaseName(), helmChartPath(),
		"--namespace", operatorNamespace(),
		"--reuse-values",
		"--set", fmt.Sprintf("podAnnotations.kohen\\.dev/upgraded-at=%d", time.Now().Unix()),
		"--wait", "--timeout", "4m",
	}
	if img := os.Getenv("KOHEN_IMAGE"); img != "" {
		parts := strings.SplitN(img, ":", 2)
		args = append(args, "--set", "image.repository="+parts[0])
		if len(parts) == 2 {
			args = append(args, "--set", "image.tag="+parts[1])
		}
		args = append(args, "--set", "image.pullPolicy=Never")
	}
	if out, err := exec.Command("helm", args...).CombinedOutput(); err != nil {
		t.Fatalf("helm upgrade: %v\n%s", err, out)
	}

	commitFile(t, ns, "gitserver", 18470, "svc/app.yaml", "greeting: post-upgrade\n")
	eventually(t, 120*time.Second, "configmap updated after upgrade", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: post-upgrade\n" {
			return fmt.Errorf("data = %q", cm.Data["app.yaml"])
		}
		return nil
	})
	configSyncReady(t, c, key, 120*time.Second)
}

// TestU3OperatorUninstall verifies helm uninstall removes the operator but
// leaves the workload, ConfigMap, and wiring intact (A12). Run last in the
// lifecycle job — it removes Kohen from the cluster.
func TestU3OperatorUninstall(t *testing.T) {
	if installMethod() != "helm" {
		t.Skip("uninstall test requires KOHEN_INSTALL_METHOD=helm")
	}
	if os.Getenv("KOHEN_ALLOW_UNINSTALL") != "true" {
		t.Skip("set KOHEN_ALLOW_UNINSTALL=true to run destructive uninstall test")
	}
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-uninstall"
	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "uninstall-sync", Namespace: ns},
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

	var stamp string
	{
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			t.Fatalf("get deploy: %v", err)
		}
		stamp = d.Spec.Template.Annotations[configSHAAnnotation]
		if stamp == "" {
			t.Fatal("expected version stamp before uninstall")
		}
	}

	out, err := exec.Command("helm", "uninstall", helmReleaseName(),
		"--namespace", operatorNamespace(), "--wait").CombinedOutput()
	if err != nil {
		t.Fatalf("helm uninstall: %v\n%s", err, out)
	}

	eventually(t, 60*time.Second, "operator deployment removed", func() error {
		d := &appsv1.Deployment{}
		err := c.Get(ctx, client.ObjectKey{Namespace: operatorNamespace(), Name: operatorDeployName()}, d)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("operator still present: %v", err)
	})

	// User objects must remain.
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("workload should survive uninstall: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app-config"}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("configmap should survive uninstall: %v", err)
	}
	d := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
		t.Fatalf("get deploy: %v", err)
	}
	if got := d.Spec.Template.Annotations[configSHAAnnotation]; got != stamp {
		t.Fatalf("stamp changed after uninstall: %q -> %q", stamp, got)
	}
	if len(d.Spec.Template.Spec.Volumes) == 0 {
		t.Fatal("workload wiring should remain after operator uninstall")
	}
}
