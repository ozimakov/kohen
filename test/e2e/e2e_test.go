//go:build e2e

// Package e2e contains the U1 usability suite (PLAN U1): it proves the
// config-only user journeys on a real cluster (kind) with a real kubelet — the
// experience, not units. It assumes:
//   - KUBECONFIG points at a cluster where Kohen is already installed (Helm) with
//     allowInsecureGitTLS=true and an empty sourceAllowList;
//   - the throwaway gitserver image (GITSERVER_IMAGE) is loaded into the cluster;
//   - kubectl is on PATH (used for port-forwarding the gitserver admin endpoint).
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
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

const (
	e2eNamespace = "kohen-e2e"
	appName      = "demo"
	gitService   = "gitserver"
)

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

func gitURL() string {
	return fmt.Sprintf("https://%s.%s.svc:8443/config/.git", gitService, e2eNamespace)
}

func TestU1ConfigSyncJourney(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	setupNamespace(t, c)
	deployGitServer(t, c)
	deploySampleApp(t, c)
	createCredentialSecret(t, c)

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-sync", Namespace: e2eNamespace},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        "svc",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: appName},
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
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: "demo-config"}, cm); err != nil {
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
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: appName}, d); err != nil {
			return err
		}
		if len(d.Spec.Template.Spec.Volumes) == 0 {
			return fmt.Errorf("no volume wired")
		}
		if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) == 0 ||
			d.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/etc/kohen/config" {
			return fmt.Errorf("mount not wired: %+v", d.Spec.Template.Spec.Containers[0].VolumeMounts)
		}
		v1 = d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]
		if v1 == "" {
			return fmt.Errorf("pod template not stamped")
		}
		return nil
	})

	// Scenario 3 (UC7): status readable + Ready once the real rollout completes.
	eventually(t, 180*time.Second, "ConfigSync Ready", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
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

	// Scenario 2 (A2/A3): commit a config change ⇒ new version ⇒ rollout.
	commitChange(t, "svc/app.yaml", "greeting: hello-v2\n")
	eventually(t, 120*time.Second, "configmap updated to v2", func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: "demo-config"}, cm); err != nil {
			return err
		}
		if cm.Data["app.yaml"] != "greeting: hello-v2\n" {
			return fmt.Errorf("configmap not updated: %q", cm.Data["app.yaml"])
		}
		return nil
	})
	eventually(t, 120*time.Second, "new version stamped", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: appName}, d); err != nil {
			return err
		}
		if v2 := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; v2 == "" || v2 == v1 {
			return fmt.Errorf("version not advanced: was %q now %q", v1, v2)
		}
		return nil
	})

	// Scenario 10: force-sync annotation is processed and cleared.
	eventually(t, 30*time.Second, "force-sync cleared", func() error {
		got := &kohenv1alpha1.ConfigSync{}
		if err := c.Get(ctx, key, got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations[kohenv1alpha1.AnnotationSyncNow] = "1"
		if err := c.Update(ctx, got); err != nil {
			return err
		}
		return nil
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
		err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: "demo-config"}, cm)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("configmap still present (err=%v)", err)
	})
	eventually(t, 90*time.Second, "workload unwired and intact", func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: appName}, d); err != nil {
			return fmt.Errorf("workload should still exist: %w", err)
		}
		if len(d.Spec.Template.Spec.Volumes) != 0 {
			return fmt.Errorf("volume not retracted: %+v", d.Spec.Template.Spec.Volumes)
		}
		return nil
	})
}

func setupNamespace(t *testing.T, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e2eNamespace}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), ns)
	})
}

func deployGitServer(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": gitService}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gitService, Namespace: e2eNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:            "gitserver",
					Image:           gitserverImage(),
					ImagePullPolicy: corev1.PullNever, // loaded into kind
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
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: gitService, Namespace: e2eNamespace},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 8443, TargetPort: intstr.FromInt(8443)}},
		},
	}
	if err := c.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create gitserver svc: %v", err)
	}
	eventually(t, 120*time.Second, "gitserver ready", func() error {
		got := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: gitService}, got); err != nil {
			return err
		}
		if got.Status.ReadyReplicas < 1 {
			return fmt.Errorf("gitserver not ready")
		}
		return nil
	})
}

func deploySampleApp(t *testing.T, c client.Client) {
	t.Helper()
	labels := map[string]string{"app": appName}
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: appName, Namespace: e2eNamespace},
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
		t.Fatalf("create sample app: %v", err)
	}
}

func createCredentialSecret(t *testing.T, c client.Client) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "git-creds", Namespace: e2eNamespace,
			Labels: map[string]string{kohenv1alpha1.LabelGitCredential: "true"},
		},
		// Anonymous access over TLS the operator must be told to trust (the
		// gitserver uses a self-signed cert). Gated by allowInsecureGitTLS=true.
		Data: map[string][]byte{"insecure-skip-tls-verify": []byte("true")},
	}
	if err := c.Create(context.Background(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create credential secret: %v", err)
	}
}

// commitChange updates a file in the gitserver repo via its admin endpoint,
// reached through a kubectl port-forward.
func commitChange(t *testing.T, path, content string) {
	t.Helper()
	pf := exec.Command("kubectl", "port-forward", "-n", e2eNamespace, "svc/"+gitService, "18443:8443")
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(3 * time.Second)

	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // e2e self-signed
	url := "https://127.0.0.1:18443/admin/commit?path=" + path
	var lastErr error
	for i := 0; i < 10; i++ {
		resp, err := hc.Post(url, "text/plain", strings.NewReader(content))
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("commit change failed: %v", lastErr)
}
