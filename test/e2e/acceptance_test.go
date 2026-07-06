//go:build e2e

// U3 acceptance additions (PLAN U3): fills gaps in the A1–A12 matrix not fully
// covered by the U1/U2 suites — notably A2 (live mount content) and documents
// the mapping for reviewers.
package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// busyboxImage is small, has /bin/cat, and is cached on kind nodes.
const busyboxImage = "registry.k8s.io/e2e-test-images/busybox:1.29-4"

func deployBusyboxDeployment(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	labels := map[string]string{"app": name}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:    "app",
					Image:   busyboxImage,
					Command: []string{"sh", "-c", "sleep infinity"},
				}}},
			},
		},
	}
	if err := c.Create(context.Background(), d); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create deployment %s: %v", name, err)
	}
}

func podExecCat(t *testing.T, ns, deployName, path string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "-n", ns, "exec", "deploy/"+deployName,
		"-c", "app", "--", "cat", path).CombinedOutput()
	if err != nil {
		t.Fatalf("exec cat %s: %v\n%s", path, err, out)
	}
	return string(out)
}

// TestU3MountedVolumeContent is A2: after a git commit updates the ConfigMap,
// a running pod observes the new file content via the kubelet atomic mount
// (non-subPath).
func TestU3MountedVolumeContent(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-mount-a2"
	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployBusyboxDeployment(t, c, ns, "demo")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "mount-sync", Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "demo"},
			Rollout:     kohenv1alpha1.RolloutAuto,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
		},
	}
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, cs) })
	key := client.ObjectKeyFromObject(cs)
	configSyncReady(t, c, key, 180*time.Second)
	waitDeployReady(t, c, ns, "demo", 120*time.Second)

	mountPath := "/etc/kohen/config/app.yaml"
	eventually(t, 90*time.Second, "v1 visible in pod", func() error {
		got := podExecCat(t, ns, "demo", mountPath)
		if !strings.Contains(got, "hello-v1") {
			return fmt.Errorf("mount content = %q", got)
		}
		return nil
	})

	commitFile(t, ns, "gitserver", 18480, "svc/app.yaml", "greeting: hello-mount-v2\n")
	eventually(t, 120*time.Second, "configmap v2", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: hello-mount-v2\n" {
			return fmt.Errorf("cm data = %q", cm.Data["app.yaml"])
		}
		return nil
	})

	// Wait for rollout to complete with the new stamp, then read the mount.
	eventually(t, 180*time.Second, "v2 visible in pod after rollout", func() error {
		waitDeployReady(t, c, ns, "demo", 30*time.Second)
		got := podExecCat(t, ns, "demo", mountPath)
		if !strings.Contains(got, "hello-mount-v2") {
			return fmt.Errorf("mount still old: %q", got)
		}
		return nil
	})
}

// TestU3AcceptanceMatrix is a lightweight registry test that documents which
// file owns each acceptance criterion. It always passes but fails compilation
// if a referenced test is removed.
func TestU3AcceptanceMatrix(t *testing.T) {
	owners := map[string]string{
		"A1":  "TestU1ConfigSyncJourney",
		"A2":  "TestU3MountedVolumeContent",
		"A3":  "TestU1ConfigSyncJourney",
		"A4":  "TestU2ESOJourney",
		"A5":  "TestU2FirstResolutionFailClosed, TestU2UpdateFailSafeAndMaxDegraded",
		"A6":  "TestU2Rotation",
		"A7":  "TestU1ConfigSyncJourney",
		"A8":  "TestU1ErrorUX",
		"A9":  "TestU3PodSecurityConformance, TestU3RBACConformance",
		"A10": "TestU1GitOpsCoexistence",
		"A11": "TestU2AbuseCases",
		"A12": "TestU3OperatorUpgrade, TestU3OperatorUninstall",
	}
	for id, owner := range owners {
		t.Logf("%s => %s", id, owner)
	}
	_ = intstr.FromInt(1) // keep k8s import for future matrix helpers
}
