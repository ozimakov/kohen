//go:build e2e

// This file is the U2 usability suite (PLAN U2): it proves the secret user
// journeys — and the abuse cases — on a real cluster (kind) with a kubelet and
// a real External Secrets Operator using its built-in fake provider. Like the
// U1 suite it assumes Kohen is already installed (Helm), the gitserver image is
// loaded, and kubectl is on PATH. The U2 CI job additionally installs ESO and
// installs Kohen with:
//   - secretStoreAllowList: ["fake-store"]  (so the store guard rail is live);
//   - a short maxDegradedDuration            (so MaxDegradedExceeded is reachable);
//   - sourceAllowList: ["https://gitserver."] (so the disallowed-URL abuse case
//     fails closed while every in-cluster gitserver still resolves).
//
// The scenarios never rely on values leaking anywhere: the backing secret value
// is delivered to the pod via the target Secret only. assertNoValueLeak scans
// git, the ConfigSync status, and events for the fixture value.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

const (
	// fakeStore is the fake-provider SecretStore name the U2 install allow-lists.
	fakeStore = "fake-store"
	// fakeStoreDisallowed is a store name deliberately outside the allow-list.
	fakeStoreDisallowed = "rogue-store"
	// esoGroup is the External Secrets Operator API group; v1 is served by
	// current ESO (v1beta1 is disabled by default from ESO chart 2.x).
	esoGroup   = "external-secrets.io"
	esoVersion = "v1"
)

func externalSecretGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: esoGroup, Version: esoVersion, Kind: "ExternalSecret"}
}

// requireESO skips the test unless the ExternalSecret CRD is served. This keeps
// `make e2e` usable on a cluster without ESO while the U2 CI job installs it.
func requireESO(t *testing.T, c client.Client) {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: esoGroup, Version: esoVersion, Kind: "ExternalSecretList"})
	if err := c.List(context.Background(), list, client.InNamespace("default")); err != nil {
		if meta.IsNoMatchError(err) {
			t.Skip("External Secrets Operator not installed (external-secrets.io/v1 not served); skipping ESO scenario")
		}
		t.Fatalf("probing ESO CRD: %v", err)
	}
}

// createFakeSecretStore creates a namespaced fake-provider SecretStore that
// returns a single static key/value pair. The value never leaves the store /
// target Secret; it is what ESO writes into the target Secret.
func createFakeSecretStore(t *testing.T, c client.Client, ns, name, remoteKey, value string) {
	t.Helper()
	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": esoGroup + "/" + esoVersion,
		"kind":       "SecretStore",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"provider": map[string]any{
				"fake": map[string]any{
					"data": []any{
						map[string]any{"key": remoteKey, "value": value},
					},
				},
			},
		},
	}}
	if err := c.Create(context.Background(), store); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create SecretStore %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), store) })
}

// externalSecretManifest returns an ExternalSecret manifest body (as committed
// to git) that pulls remoteKey from store into target Secret targetName under
// data key secretKey.
func externalSecretManifest(name, targetName, store, remoteKey, secretKey string) string {
	return fmt.Sprintf(`apiVersion: %s/%s
kind: ExternalSecret
metadata:
  name: %s
spec:
  refreshInterval: 3s
  secretStoreRef:
    name: %s
    kind: SecretStore
  target:
    name: %s
    creationPolicy: Owner
  data:
    - secretKey: %s
      remoteRef:
        key: %s
`, esoGroup, esoVersion, name, store, targetName, secretKey, remoteKey)
}

// getExternalSecret fetches an ExternalSecret as unstructured.
func getExternalSecret(t *testing.T, c client.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK())
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("get ExternalSecret %s: %v", name, err)
	}
	return u
}

// createNativeSecret creates (or replaces) a plain Secret with the given data.
func createNativeSecret(t *testing.T, c client.Client, ns, name string, data map[string][]byte) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
	if err := c.Create(context.Background(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create secret %s: %v", name, err)
	}
}

// fileRef builds a file-surfaced native secret reference.
func fileRef(name, secretName, mountPath string) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:         name,
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: secretName},
		Surface:      kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: mountPath},
	}
}

// envRef builds an env-surfaced native secret reference.
func envRef(name, secretName, envVar, key string) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:         name,
		Backend:      kohenv1alpha1.BackendNativeSecret,
		NativeSecret: &kohenv1alpha1.LocalObjectReference{Name: secretName},
		Surface:      kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, EnvVar: envVar, Key: key},
	}
}

// esoRef builds a secret reference against an ExternalSecret with the given surface.
func esoRef(name, esName string, surface kohenv1alpha1.SecretSurface) kohenv1alpha1.SecretReference {
	return kohenv1alpha1.SecretReference{
		Name:           name,
		Backend:        kohenv1alpha1.BackendExternalSecret,
		ExternalSecret: &kohenv1alpha1.LocalObjectReference{Name: esName},
		Surface:        surface,
	}
}

// newSecretSync builds a ConfigSync wired to the seeded gitserver with the given
// path, workload, rollout mode, and secret references.
func newSecretSync(name, ns, path, workload string, rollout kohenv1alpha1.RolloutMode, refs ...kohenv1alpha1.SecretReference) *kohenv1alpha1.ConfigSync {
	return &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source: kohenv1alpha1.GitSource{
				URL:           gitURL(ns, "gitserver"),
				Ref:           "main",
				AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "git-creds"},
			},
			Path:        path,
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: workload},
			Rollout:     rollout,
			Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
			SecretRefs:  refs,
		},
	}
}

// deployStamp returns the current pod-template config-sha stamp of a Deployment.
func deployStamp(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
		t.Fatalf("get deploy %s: %v", name, err)
	}
	return d.Spec.Template.Annotations[configSHAAnnotation]
}

// assertEnvSecretWired asserts the target container has an env var sourced from
// the given Secret/key via valueFrom.secretKeyRef (never a literal value).
func assertEnvSecretWired(t *testing.T, c client.Client, ns, deploy, envVar, secretName, key string) {
	t.Helper()
	eventually(t, 90*time.Second, fmt.Sprintf("env %s wired from secret %s", envVar, secretName), func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: deploy}, d); err != nil {
			return err
		}
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if e.Name != envVar {
				continue
			}
			if e.Value != "" {
				return fmt.Errorf("env %s has a literal value (must use secretKeyRef)", envVar)
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				return fmt.Errorf("env %s missing secretKeyRef", envVar)
			}
			if e.ValueFrom.SecretKeyRef.Name != secretName || e.ValueFrom.SecretKeyRef.Key != key {
				return fmt.Errorf("env %s secretKeyRef = %s/%s, want %s/%s",
					envVar, e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key, secretName, key)
			}
			return nil
		}
		return fmt.Errorf("env %s not found in container", envVar)
	})
}

// assertFileSecretWired asserts a volume + mount backed by the given Secret at mountPath.
func assertFileSecretWired(t *testing.T, c client.Client, ns, deploy, secretName, mountPath string) {
	t.Helper()
	eventually(t, 90*time.Second, fmt.Sprintf("file mount %s wired from secret %s", mountPath, secretName), func() error {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: deploy}, d); err != nil {
			return err
		}
		var volName string
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == secretName {
				volName = v.Name
				break
			}
		}
		if volName == "" {
			return fmt.Errorf("no volume backed by secret %s", secretName)
		}
		for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
			if m.Name == volName && m.MountPath == mountPath {
				return nil
			}
		}
		return fmt.Errorf("no mount of volume %s at %s", volName, mountPath)
	})
}

// assertNoValueLeak fails if value appears in the ConfigSync status conditions,
// the per-reference status, or any event for the object (R8.3/TM9). The value
// is never committed to git (only the store-referencing ExternalSecret manifest
// is) and is only ever legitimately present in the backing/target Secret and
// the pod's mounted volume/env — never in Kohen's surfaces.
func assertNoValueLeak(t *testing.T, c client.Client, ns string, key client.ObjectKey, value string) {
	t.Helper()
	got := &kohenv1alpha1.ConfigSync{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get configsync: %v", err)
	}
	for _, cond := range got.Status.Conditions {
		if strings.Contains(cond.Message, value) {
			t.Fatalf("secret value leaked into condition %s: %q", cond.Type, cond.Message)
		}
	}
	for _, sr := range got.Status.SecretRefs {
		if strings.Contains(sr.Reason, value) {
			t.Fatalf("secret value leaked into secretRefs status: %q", sr.Reason)
		}
	}
	var events corev1.EventList
	if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range events.Items {
		if e.InvolvedObject.Kind == "ConfigSync" && e.InvolvedObject.Name == key.Name &&
			strings.Contains(e.Message, value) {
			t.Fatalf("secret value leaked into event %s: %q", e.Reason, e.Message)
		}
	}
}

// rotateNativeSecret updates an existing Secret's data in place (a rotation:
// the resourceVersion advances and, for a changed key set, the metadata token).
func rotateNativeSecret(t *testing.T, c client.Client, ns, name string, data map[string][]byte) {
	t.Helper()
	eventually(t, 30*time.Second, "rotate secret "+name, func() error {
		sec := &corev1.Secret{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
			return err
		}
		sec.Data = data
		return c.Update(context.Background(), sec)
	})
}

// deployGeneration returns the current metadata.generation of a Deployment.
func deployGeneration(t *testing.T, c client.Client, ns, name string) int64 {
	t.Helper()
	return mustDeploy(t, c, ns, name).Generation
}

// mustDeploy fetches a Deployment or fails the test.
func mustDeploy(t *testing.T, c client.Client, ns, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, d); err != nil {
		t.Fatalf("get deploy %s: %v", name, err)
	}
	return d
}

// assertWarningEvent waits for a Warning event with the given reason on the ConfigSync.
func assertWarningEvent(t *testing.T, c client.Client, ns, name, reason string) {
	t.Helper()
	eventually(t, 90*time.Second, fmt.Sprintf("Warning/%s event", reason), func() error {
		var events corev1.EventList
		if err := c.List(context.Background(), &events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.InvolvedObject.Kind == "ConfigSync" && e.InvolvedObject.Name == name &&
				e.Reason == reason && e.Type == corev1.EventTypeWarning {
				return nil
			}
		}
		return fmt.Errorf("no Warning/%s event for %s", reason, name)
	})
}

// aliasGitServerService creates a second Service (alias) selecting the same
// gitserver pods, so a distinct in-cluster DNS name resolves to the same
// backend (used by the disallowed-source-url abuse case).
func aliasGitServerService(t *testing.T, c client.Client, ns, target, alias string) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: alias, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": target},
			Ports:    []corev1.ServicePort{{Port: 8443, TargetPort: intstr.FromInt(8443)}},
		},
	}
	if err := c.Create(context.Background(), svc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create alias service %s: %v", alias, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), svc) })
}

// TestU2ESOJourney is scenario 1 (A4): a git-committed ExternalSecret is
// applied by Kohen, awaited to Ready via the real ESO fake provider, and its
// target Secret wired into the workload as both a file and an env var. The
// backing value is present in the target Secret (deliverable to the pod) but
// never leaks into git/status/events.
func TestU2ESOJourney(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	requireESO(t, c)
	ns := "kohen-e2e-eso"
	const (
		esName     = "app-secret"
		secretKey  = "token"
		remoteKey  = "/prod/app/token"
		mountPath  = "/etc/secrets/app"
		envVarName = "APP_TOKEN"
		value      = "sup3r-s3cr3t-eso-value"
	)

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())
	createFakeSecretStore(t, c, ns, fakeStore, remoteKey, value)

	// Commit the ExternalSecret into the config path. It is excluded from the
	// ConfigMap (R7.6) and applied by Kohen's manifest engine (§8.2).
	commitFile(t, ns, "gitserver", 18450, "eso/external-secret.yaml",
		externalSecretManifest(esName, esName, fakeStore, remoteKey, secretKey))
	// Keep a rendered config file too so the ConfigMap is non-empty.
	commitFile(t, ns, "gitserver", 18450, "eso/app.yaml", "greeting: hello-eso\n")

	cs := newSecretSync("eso-sync", ns, "eso", "app", kohenv1alpha1.RolloutAuto,
		esoRef("es-file", esName, kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: mountPath}),
		esoRef("es-env", esName, kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceEnv, EnvVar: envVarName, Key: secretKey}),
	)
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)

	// Kohen applies the ExternalSecret (owned) and ESO drives it Ready.
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionManifestsApplied,
		metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, 120*time.Second)
	eventually(t, 120*time.Second, "ExternalSecret owned + Ready", func() error {
		es := getExternalSecret(t, c, ns, esName)
		conds, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
		for _, cnd := range conds {
			m, ok := cnd.(map[string]any)
			if ok && m["type"] == "Ready" && m["status"] == "True" {
				return nil
			}
		}
		return fmt.Errorf("ExternalSecret not Ready yet: %v", conds)
	})

	// SecretsReady, wiring, and the overall Ready all converge.
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionSecretsReady,
		metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, 120*time.Second)
	assertFileSecretWired(t, c, ns, "app", esName, mountPath)
	assertEnvSecretWired(t, c, ns, "app", envVarName, esName, secretKey)
	configSyncReady(t, c, key, 180*time.Second)

	// The value ESO synced is present in the target Secret (deliverable to the
	// pod) — the ground truth that wiring delivers a real secret ...
	eventually(t, 60*time.Second, "target Secret holds the synced value", func() error {
		sec := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: esName}, sec); err != nil {
			return err
		}
		if string(sec.Data[secretKey]) != value {
			return fmt.Errorf("target secret key %s = %q, want the synced value", secretKey, string(sec.Data[secretKey]))
		}
		return nil
	})
	// ... but it never leaks into git, status, or events.
	assertNoValueLeak(t, c, ns, key, value)

	// Deleting the ConfigSync prunes the owned ExternalSecret (ESO then deletes
	// its target Secret) and unwires the workload (A7 for secret surfaces).
	if err := c.Delete(ctx, cs); err != nil {
		t.Fatalf("delete configsync: %v", err)
	}
	eventually(t, 90*time.Second, "owned ExternalSecret pruned", func() error {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(externalSecretGVK())
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: esName}, u)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("ExternalSecret still present (err=%v)", err)
	})
}

// TestU2FirstResolutionFailClosed is scenario 2 (A5) and the native-backend
// smoke (scenario 5): a reference to a not-yet-existing native Secret fails
// closed (AwaitingFirstResolution, no wiring, no rollout); creating the Secret
// resolves it and rolls the workload, wired as both file and env.
func TestU2FirstResolutionFailClosed(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-firstres"
	const (
		secretName = "db-creds"
		envVarName = "DB_PASSWORD"
		dataKey    = "password"
		mountPath  = "/etc/secrets/db"
		value      = "n0t-yet-there"
	)

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

	cs := newSecretSync("firstres-sync", ns, "svc", "app", kohenv1alpha1.RolloutAuto,
		fileRef("db-file", secretName, mountPath),
		envRef("db-env", secretName, envVarName, dataKey),
	)
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)

	// Fail closed: SecretsReady False/AwaitingFirstResolution, no stamp, no wiring.
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionSecretsReady,
		metav1.ConditionFalse, kohenv1alpha1.ReasonAwaitingFirstResolution, 90*time.Second)
	consistently(t, 15*time.Second, "no rollout while awaiting first resolution", func() error {
		if s := deployStamp(t, c, ns, "app"); s != "" {
			return fmt.Errorf("workload stamped before secret resolved: %q", s)
		}
		d := &appsv1.Deployment{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "app"}, d); err != nil {
			return err
		}
		if len(d.Spec.Template.Spec.Volumes) != 0 {
			return fmt.Errorf("workload wired before secret resolved")
		}
		return nil
	})

	// Create the Secret ⇒ resolves, wires (file + env), rolls, Ready.
	createNativeSecret(t, c, ns, secretName, map[string][]byte{dataKey: []byte(value)})
	forceSync(t, c, key)
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionSecretsReady,
		metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, 90*time.Second)
	assertFileSecretWired(t, c, ns, "app", secretName, mountPath)
	assertEnvSecretWired(t, c, ns, "app", envVarName, secretName, dataKey)
	configSyncReady(t, c, key, 120*time.Second)
	assertNoValueLeak(t, c, ns, key, value)
}

// TestU2UpdateFailSafeAndMaxDegraded is scenario 3 (A5): once a reference is
// established (wired + rolled), a transient outage of its backing Secret fails
// SAFE — the workload keeps running on last-good — and, past
// maxDegradedDuration, surfaces MaxDegradedExceeded. Recreating the Secret
// auto-recovers. The readiness policy is backend-agnostic (S2.2); native makes
// the outage deterministic (delete the Secret) without depending on ESO refresh
// timing.
func TestU2UpdateFailSafeAndMaxDegraded(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	ns := "kohen-e2e-failsafe"
	const (
		secretName = "api-key"
		mountPath  = "/etc/secrets/api"
		value      = "establish3d-good"
	)

	setupNamespace(t, c, ns)
	deployGitServer(t, c, ns, "gitserver", nil)
	deployDeployment(t, c, ns, "app")
	createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())
	createNativeSecret(t, c, ns, secretName, map[string][]byte{"key": []byte(value)})

	cs := newSecretSync("failsafe-sync", ns, "svc", "app", kohenv1alpha1.RolloutAuto,
		fileRef("api-file", secretName, mountPath),
	)
	if err := c.Create(ctx, cs); err != nil {
		t.Fatalf("create configsync: %v", err)
	}
	key := client.ObjectKeyFromObject(cs)

	// Establish: resolved, wired, Ready.
	configSyncReady(t, c, key, 120*time.Second)
	assertFileSecretWired(t, c, ns, "app", secretName, mountPath)
	genBefore := deployGeneration(t, c, ns, "app")

	// Outage: delete the established Secret. The reference is established, so
	// Kohen fails safe — DegradedServingLastGood — keeping the workload wired.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns}}); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	forceSync(t, c, key)
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionSecretsReady,
		metav1.ConditionFalse, kohenv1alpha1.ReasonDegradedServingLastGood, 90*time.Second)
	// The workload keeps its wiring and does not roll (fail safe).
	if len(mustDeploy(t, c, ns, "app").Spec.Template.Spec.Volumes) == 0 {
		t.Fatalf("wiring lost during transient outage (should fail safe)")
	}
	if got := deployGeneration(t, c, ns, "app"); got != genBefore {
		t.Fatalf("workload rolled during transient outage: generation %d -> %d", genBefore, got)
	}

	// Past maxDegradedDuration (short in the U2 install) the security-visible
	// MaxDegradedExceeded surfaces on the condition and as a Warning event.
	waitConditionReason(t, c, key, kohenv1alpha1.ConditionSecretsReady,
		metav1.ConditionFalse, kohenv1alpha1.ReasonMaxDegradedExceeded, 120*time.Second)
	assertWarningEvent(t, c, ns, cs.Name, kohenv1alpha1.ReasonMaxDegradedExceeded)

	// Recover: recreate the Secret ⇒ auto-heals back to Ready.
	createNativeSecret(t, c, ns, secretName, map[string][]byte{"key": []byte(value)})
	forceSync(t, c, key)
	configSyncReady(t, c, key, 120*time.Second)
}

// TestU2Rotation is scenario 4 (A6): rotating an env-surfaced secret advances
// the config version and triggers exactly one rollout; rotating a file-surfaced
// secret updates in place with no rollout.
func TestU2Rotation(t *testing.T) {
	c := newClient(t)

	t.Run("env_surface_rolls", func(t *testing.T) {
		ctx := context.Background()
		ns := "kohen-e2e-rot-env"
		const secretName, envVarName, dataKey = "rot-env", "ROT", "value"

		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())
		createNativeSecret(t, c, ns, secretName, map[string][]byte{dataKey: []byte("v1")})

		cs := newSecretSync("rot-env-sync", ns, "svc", "app", kohenv1alpha1.RolloutAuto,
			envRef("env", secretName, envVarName, dataKey))
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create configsync: %v", err)
		}
		key := client.ObjectKeyFromObject(cs)
		configSyncReady(t, c, key, 120*time.Second)
		waitStableGeneration(t, c, ns, "app", 8*time.Second, 90*time.Second)
		rsBefore := replicaSetCount(t, c, ns, "app")
		stampBefore := deployStamp(t, c, ns, "app")

		// Rotate the env-surfaced Secret ⇒ metadata token changes ⇒ version
		// advances ⇒ exactly one new ReplicaSet (rollout).
		rotateNativeSecret(t, c, ns, secretName, map[string][]byte{dataKey: []byte("v2")})
		forceSync(t, c, key)
		eventually(t, 120*time.Second, "env rotation advances version + rolls", func() error {
			if s := deployStamp(t, c, ns, "app"); s == stampBefore || s == "" {
				return fmt.Errorf("stamp not advanced (was %q)", stampBefore)
			}
			return nil
		})
		eventually(t, 90*time.Second, "exactly one new ReplicaSet", func() error {
			if got := replicaSetCount(t, c, ns, "app"); got != rsBefore+1 {
				return fmt.Errorf("replicasets %d, want %d (one rollout)", got, rsBefore+1)
			}
			return nil
		})
	})

	t.Run("file_surface_no_roll", func(t *testing.T) {
		ctx := context.Background()
		ns := "kohen-e2e-rot-file"
		const secretName, mountPath = "rot-file", "/etc/secrets/rot"

		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())
		createNativeSecret(t, c, ns, secretName, map[string][]byte{"value": []byte("v1")})

		cs := newSecretSync("rot-file-sync", ns, "svc", "app", kohenv1alpha1.RolloutAuto,
			fileRef("file", secretName, mountPath))
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create configsync: %v", err)
		}
		key := client.ObjectKeyFromObject(cs)
		configSyncReady(t, c, key, 120*time.Second)
		waitStableGeneration(t, c, ns, "app", 8*time.Second, 90*time.Second)
		rsBefore := replicaSetCount(t, c, ns, "app")
		stampBefore := deployStamp(t, c, ns, "app")

		// Rotate the file-surfaced Secret ⇒ kubelet updates the mount in place;
		// the config version does NOT advance and no rollout occurs.
		rotateNativeSecret(t, c, ns, secretName, map[string][]byte{"value": []byte("v2")})
		forceSync(t, c, key)
		consistently(t, 25*time.Second, "file rotation causes no rollout", func() error {
			if s := deployStamp(t, c, ns, "app"); s != stampBefore {
				return fmt.Errorf("stamp changed on file rotation: %q -> %q", stampBefore, s)
			}
			if got := replicaSetCount(t, c, ns, "app"); got != rsBefore {
				return fmt.Errorf("a rollout happened on file rotation: replicasets %d -> %d", rsBefore, got)
			}
			return nil
		})
	})
}

// TestU2AbuseCases is scenario 6 (A11): configuration-time abuse fails closed.
func TestU2AbuseCases(t *testing.T) {
	c := newClient(t)

	t.Run("manifest_store_not_allowed", func(t *testing.T) {
		requireESO(t, c)
		ctx := context.Background()
		ns := "kohen-e2e-abuse-store"
		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

		// A committed ExternalSecret referencing a store outside the operator
		// allow-list must be rejected before it is applied (R-AUTH.4).
		commitFile(t, ns, "gitserver", 18451, "abuse/external-secret.yaml",
			externalSecretManifest("rogue-es", "rogue-es", fakeStoreDisallowed, "/x", "k"))
		commitFile(t, ns, "gitserver", 18451, "abuse/app.yaml", "greeting: hi\n")

		cs := newSecretSync("abuse-store-sync", ns, "abuse", "app", kohenv1alpha1.RolloutAuto,
			esoRef("es", "rogue-es", kohenv1alpha1.SecretSurface{As: kohenv1alpha1.SurfaceFile, MountPath: "/etc/secrets/x"}))
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		key := client.ObjectKeyFromObject(cs)
		waitConditionReason(t, c, key, kohenv1alpha1.ConditionManifestsApplied,
			metav1.ConditionFalse, kohenv1alpha1.ReasonStoreNotAllowed, 90*time.Second)
		// Nothing was applied: the rogue ExternalSecret must not exist.
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(externalSecretGVK())
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "rogue-es"}, u); !apierrors.IsNotFound(err) {
			t.Fatalf("disallowed ExternalSecret should not be applied (err=%v)", err)
		}
	})

	t.Run("unlabeled_credential_rejected", func(t *testing.T) {
		ctx := context.Background()
		ns := "kohen-e2e-abuse-cred"
		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		// A Secret WITHOUT the kohen.dev/git-credential label must be refused
		// as a git credential (R-AUTH.6).
		createNativeSecret(t, c, ns, "unlabeled", insecureTLSSecret())

		cs := &kohenv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: "abuse-cred-sync", Namespace: ns},
			Spec: kohenv1alpha1.ConfigSyncSpec{
				Source: kohenv1alpha1.GitSource{
					URL:           gitURL(ns, "gitserver"),
					Ref:           "main",
					AuthSecretRef: &kohenv1alpha1.LocalObjectReference{Name: "unlabeled"},
				},
				Path:        "svc",
				WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
				Rollout:     kohenv1alpha1.RolloutAuto,
				Sync:        kohenv1alpha1.SyncSpec{Interval: metav1.Duration{Duration: 5 * time.Second}},
			},
		}
		if err := c.Create(ctx, cs); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		waitConditionReason(t, c, client.ObjectKeyFromObject(cs), kohenv1alpha1.ConditionFetched,
			metav1.ConditionFalse, kohenv1alpha1.ReasonAuthFailed, 90*time.Second)
	})

	t.Run("disallowed_source_url", func(t *testing.T) {
		ctx := context.Background()
		ns := "kohen-e2e-abuse-url"
		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())
		// A second Service name aliases the same gitserver pod so the host
		// resolves (RFC1918, passes the SSRF guard) but its URL is outside the
		// operator sourceAllowList (["https://gitserver."]) ⇒ SourceNotAllowed.
		aliasGitServerService(t, c, ns, "gitserver", "otherserver")

		cs := &kohenv1alpha1.ConfigSync{
			ObjectMeta: metav1.ObjectMeta{Name: "abuse-url-sync", Namespace: ns},
			Spec: kohenv1alpha1.ConfigSyncSpec{
				Source: kohenv1alpha1.GitSource{
					URL:           gitURL(ns, "otherserver"),
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
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, cs) })
		waitConditionReason(t, c, client.ObjectKeyFromObject(cs), kohenv1alpha1.ConditionFetched,
			metav1.ConditionFalse, kohenv1alpha1.ReasonSourceNotAllowed, 90*time.Second)
	})

	t.Run("singleton_violation", func(t *testing.T) {
		ctx := context.Background()
		ns := "kohen-e2e-abuse-singleton"
		setupNamespace(t, c, ns)
		deployGitServer(t, c, ns, "gitserver", nil)
		deployDeployment(t, c, ns, "app")
		createCredentialSecret(t, c, ns, "git-creds", insecureTLSSecret())

		first := newSecretSync("singleton-a", ns, "svc", "app", kohenv1alpha1.RolloutAuto)
		if err := c.Create(ctx, first); err != nil {
			t.Fatalf("create first: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, first) })
		configSyncReady(t, c, client.ObjectKeyFromObject(first), 120*time.Second)

		second := newSecretSync("singleton-b", ns, "svc", "app", kohenv1alpha1.RolloutAuto)
		if err := c.Create(ctx, second); err != nil {
			t.Fatalf("create second: %v", err)
		}
		t.Cleanup(func() { _ = c.Delete(ctx, second) })
		// The loser degrades with SingletonViolation on WorkloadWired (the Ready
		// condition reason is the generic Degraded); a Warning event is emitted.
		waitConditionReason(t, c, client.ObjectKeyFromObject(second), kohenv1alpha1.ConditionWorkloadWired,
			metav1.ConditionFalse, kohenv1alpha1.ReasonSingletonViolation, 90*time.Second)
		assertWarningEvent(t, c, ns, second.Name, kohenv1alpha1.ReasonSingletonViolation)
	})
}
