//go:build e2e

// Package e2e contains the U1 usability suite (PLAN U1): it proves the
// config-only user journeys on a real cluster (kind) with a real kubelet — the
// experience, not units. It assumes:
//   - KUBECONFIG points at a cluster where Kohen is already installed (Helm) with
//     allowInsecureGitTLS=true and an empty sourceAllowList;
//   - the throwaway gitserver image (GITSERVER_IMAGE) is loaded into the cluster;
//   - kubectl is on PATH (used for port-forwarding the gitserver admin endpoint).
//
// Each test owns its own namespace and git server(s) so scenarios stay isolated
// and can run in any order. Helpers here are shared with e2e_scenarios_test.go.
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

const configSHAAnnotation = "kohen.dev/config-sha"

func gitserverImage() string {
	if v := os.Getenv("GITSERVER_IMAGE"); v != "" {
		return v
	}
	return "kohen-e2e-gitserver:latest"
}

func newClient(t *testing.T) client.Client {
	t.Helper()
	scheme := clientgoscheme.Scheme
	if err := kohenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	cfg, err := config.GetConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func eventually(t *testing.T, timeout time.Duration, desc string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s: %v", desc, last)
}

// consistently asserts fn stays nil for the whole window (used for A3: no churn).
func consistently(t *testing.T, window time.Duration, desc string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		time.Sleep(3 * time.Second)
	}
}

func gitURL(ns, svc string) string {
	return fmt.Sprintf("https://%s.%s.svc:8443/config/.git", svc, ns)
}

func setupNamespace(t *testing.T, c client.Client, ns string) {
	t.Helper()
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := c.Create(context.Background(), obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
}

// deployGitServer deploys a gitserver instance (and Service) named svc in ns and
// waits until it is ready. env passes extra container env (e.g. AUTH_USER).
func deployGitServer(t *testing.T, c client.Client, ns, svc string, env []corev1.EnvVar) {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": svc}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:            "gitserver",
					Image:           gitserverImage(),
					ImagePullPolicy: corev1.PullNever, // loaded into kind
					Env:             env,
					Ports:           []corev1.ContainerPort{{ContainerPort: 8443}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz", Port: intstr.FromInt(8443), Scheme: corev1.URISchemeHTTPS,
						}},
						InitialDelaySeconds: 2, PeriodSeconds: 3,
					},
				}}},
			},
		},
	}
	if err := c.Create(ctx, d); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create gitserver deploy: %v", err)
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svc, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 8443, TargetPort: intstr.FromInt(8443)}},
		},
	}
	if err := c.Create(ctx, service); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create gitserver svc: %v", err)
	}
	waitDeployReady(t, c, ns, svc, 120*time.Second)
}

func waitDeployReady(t *testing.T, c client.Client, ns, name string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, name+" ready", func() error {
		got := &appsv1.Deployment{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas < 1 {
			return fmt.Errorf("%s not ready", name)
		}
		return nil
	})
}

// deployDeployment creates a minimal pause Deployment to act as the wiring target.
func deployDeployment(t *testing.T, c client.Client, ns, name string) {
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
					Name:  "app",
					Image: "registry.k8s.io/pause:3.9",
				}}},
			},
		},
	}
	if err := c.Create(context.Background(), d); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create deployment %s: %v", name, err)
	}
}

// deployStatefulSet creates a minimal StatefulSet with the given update strategy.
func deployStatefulSet(t *testing.T, c client.Client, ns, name string, strategy appsv1.StatefulSetUpdateStrategyType) {
	t.Helper()
	labels := map[string]string{"app": name}
	replicas := int32(1)
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			Replicas:       &replicas,
			ServiceName:    name,
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: strategy},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "app",
					Image: "registry.k8s.io/pause:3.9",
				}}},
			},
		},
	}
	if err := c.Create(context.Background(), ss); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create statefulset %s: %v", name, err)
	}
}

// createCredentialSecret creates a git-credential-labeled Secret with data.
func createCredentialSecret(t *testing.T, c client.Client, ns, name string, data map[string][]byte) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{kohenv1alpha1.LabelGitCredential: "true"},
		},
		Data: data,
	}
	if err := c.Create(context.Background(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

// insecureTLSSecret is an anonymous credential that tells the operator to trust
// the gitserver's self-signed cert (gated by allowInsecureGitTLS=true).
func insecureTLSSecret() map[string][]byte {
	return map[string][]byte{"insecure-skip-tls-verify": []byte("true")}
}

// commitFile updates path in the gitserver repo via its admin endpoint, reached
// through a kubectl port-forward on localPort, and returns the new commit SHA.
func commitFile(t *testing.T, ns, svc string, localPort int, path, content string) string {
	t.Helper()
	pf := exec.Command("kubectl", "port-forward", "-n", ns,
		"svc/"+svc, fmt.Sprintf("%d:8443", localPort))
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(3 * time.Second)

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // e2e self-signed
	url := fmt.Sprintf("https://127.0.0.1:%d/admin/commit?path=%s", localPort, path)
	var lastErr error
	for i := 0; i < 12; i++ {
		resp, err := hc.Post(url, "text/plain", strings.NewReader(content))
		if err == nil && resp.StatusCode == http.StatusOK {
			sha, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return strings.TrimSpace(string(sha))
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("commit %s failed: %v", path, lastErr)
	return ""
}

// configSyncReady waits until the ConfigSync is Ready with populated status.
func configSyncReady(t *testing.T, c client.Client, key client.ObjectKey, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, "ConfigSync Ready", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(context.Background(), key, got); err != nil {
			return err
		}
		if got.Status.SourceCommit == "" || got.Status.ConfigVersion == "" {
			return fmt.Errorf("status not populated: %+v", got.Status)
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, kohenv1alpha1.ConditionReady) {
			return fmt.Errorf("not Ready yet: %+v", got.Status.Conditions)
		}
		return nil
	})
}

// waitConditionReason waits until the given condition has the expected status and reason.
func waitConditionReason(t *testing.T, c client.Client, key client.ObjectKey,
	condType string, status metav1.ConditionStatus, reason string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, fmt.Sprintf("%s=%s/%s", condType, status, reason), func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(context.Background(), key, got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, condType)
		if cond == nil {
			return fmt.Errorf("%s not set: %+v", condType, got.Status.Conditions)
		}
		if cond.Status != status || cond.Reason != reason {
			return fmt.Errorf("%s = %s/%s, want %s/%s", condType, cond.Status, cond.Reason, status, reason)
		}
		return nil
	})
}

// TestU1ConfigSyncJourney is scenario 1 (Day-1 wiring, A1), scenario 2 (config
// change ⇒ exactly one rollout + no-change ⇒ no rollout, A2/A3), scenario 3
// (status/version readable, UC7), scenario 10 (force sync), and scenario 8
// (delete ⇒ prune + unwire, A7) — the canonical end-to-end journey.
func TestU1ConfigSyncJourney(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-core"

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "demo")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-sync", Namespace: ns},
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
	key := client.ObjectKeyFromObject(cs)

	// Scenario 1 (A1): default-named ConfigMap appears with rendered content.
	eventually(t, 90*time.Second, "configmap created", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: hello-v1\n" {
			return fmt.Errorf("configmap data = %q", cm.Data["app.yaml"])
		}
		return nil
	})

	// Scenario 1 (A1): workload wired at the default mount path and stamped.
	var v1 string
	eventually(t, 90*time.Second, "workload wired + stamped", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, d); err != nil {
			return err
		}
		if len(d.Spec.Template.Spec.Volumes) == 0 {
			return fmt.Errorf("no volume wired")
		}
		if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 ||
			d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/etc/kohen/config" {
			return fmt.Errorf("mount not wired: %+v", d.Spec.Template.Spec.Containers[0].VolumeMounts)
		}
		v1 = d.Spec.Template.Annotations[configSHAAnnotation]
		if v1 == "" {
			return fmt.Errorf("pod template not stamped")
		}
		return nil
	})

	// Scenario 3 (UC7): status readable + Ready once the real rollout completes.
	configSyncReady(t, c, key, 180*time.Second)

	// Scenario 2 (A2): commit a config change ⇒ new version ⇒ rollout.
	commitFile(t, ns, "gitserver", 18443, "svc/app.yaml", "greeting: hello-v2\n")
	eventually(t, 120*time.Second, "configmap updated to v2", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: hello-v2\n" {
			return fmt.Errorf("configmap not updated: %q", cm.Data["app.yaml"])
		}
		return nil
	})
	var v2 string
	eventually(t, 120*time.Second, "new version stamped", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, d); err != nil {
			return err
		}
		v2 = d.Spec.Template.Annotations[configSHAAnnotation]
		if v2 == "" || v2 == v1 {
			return fmt.Errorf("version not advanced: was %q now %q", v1, v2)
		}
		return nil
	})

	// Scenario 2 (A3): a follow-up reconcile with no change ⇒ no rollout. The
	// stamp and the generation must stay put across several sync intervals.
	var genAfterV2 int64
	{
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, d); err != nil {
			t.Fatalf("get deploy: %v", err)
		}
		genAfterV2 = d.Generation
	}
	consistently(t, 20*time.Second, "no rollout without a config change", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, d); err != nil {
			return err
		}
		if got := d.Spec.Template.Annotations[configSHAAnnotation]; got != v2 {
			return fmt.Errorf("stamp churned: %q -> %q", v2, got)
		}
		if d.Generation != genAfterV2 {
			return fmt.Errorf("generation churned: %d -> %d", genAfterV2, d.Generation)
		}
		return nil
	})

	// Scenario 10: force-sync annotation is processed and cleared.
	eventually(t, 30*time.Second, "force-sync applied", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[kohenv1alpha1.AnnotationSyncNow] = "1"
		return c.Update(ctx, got)
	})
	eventually(t, 30*time.Second, "sync-now annotation removed", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		if _, ok := got.Annotations[kohenv1alpha1.AnnotationSyncNow]; ok {
			return fmt.Errorf("annotation still present")
		}
		return nil
	})

	// Scenario 8 (A7): delete ConfigSync ⇒ ConfigMap pruned, workload unwired but
	// still present.
	if err := c.Delete(ctx, cs); err != nil {
		t.Fatalf("delete configsync: %v", err)
	}
	eventually(t, 90*time.Second, "owned configmap pruned", func() error {
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo-config"}, cm)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("configmap still present (err=%v)", err)
	})
	eventually(t, 90*time.Second, "workload unwired and intact", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, d); err != nil {
			return fmt.Errorf("workload should still exist: %w", err)
		}
		if len(d.Spec.Template.Spec.Volumes) != 0 {
			return fmt.Errorf("volume not retracted: %+v", d.Spec.Template.Spec.Volumes)
		}
		return nil
	})
}
