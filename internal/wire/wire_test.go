package wire_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/testenv"
	"github.com/ozimakov/kohen/internal/wire"
)

func deployment(name string, containers ...corev1.Container) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: containers},
			},
		},
	}
}

func getDeploy(t *testing.T, env *testenv.Env, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := env.Client.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, d); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return d
}

func findContainer(d *appsv1.Deployment, name string) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == name {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

func hasVolume(d *appsv1.Deployment, name string) bool {
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func TestWireDefaultContainer(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app",
		corev1.Container{Name: "main", Image: "nginx:1"},
		corev1.Container{Name: "sidecar", Image: "envoy:1"})); err != nil {
		t.Fatal(err)
	}

	w := wire.New(env.Client)
	err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "app", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "app-config",
	})
	if err != nil {
		t.Fatalf("wire: %v", err)
	}

	d := getDeploy(t, env, "app")
	if !hasVolume(d, wire.VolumeName) {
		t.Fatalf("config volume not injected: %+v", d.Spec.Template.Spec.Volumes)
	}
	main := findContainer(d, "main")
	if main == nil || len(main.VolumeMounts) != 1 || main.VolumeMounts[0].MountPath != "/etc/kohen/config" {
		t.Fatalf("volumeMount not injected into first container: %+v", main)
	}
	if main.Image != "nginx:1" {
		t.Errorf("existing image mutated: %q", main.Image)
	}
	// Non-target container is untouched.
	if side := findContainer(d, "sidecar"); side == nil || len(side.VolumeMounts) != 0 {
		t.Errorf("sidecar mutated: %+v", side)
	}
	// Volume points at the ConfigMap.
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == wire.VolumeName {
			if v.ConfigMap == nil || v.ConfigMap.Name != "app-config" {
				t.Errorf("volume source = %+v, want configMap app-config", v.VolumeSource)
			}
		}
	}
}

func TestWireExplicitContainerAndStamp(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app2",
		corev1.Container{Name: "main", Image: "nginx:1"},
		corev1.Container{Name: "worker", Image: "worker:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "app2", Namespace: "default",
		Container: "worker", MountPath: "/cfg", ConfigMap: "c", ConfigSHA: "abc123",
	}); err != nil {
		t.Fatalf("wire: %v", err)
	}
	d := getDeploy(t, env, "app2")
	if worker := findContainer(d, "worker"); worker == nil || len(worker.VolumeMounts) != 1 {
		t.Fatalf("worker not wired: %+v", worker)
	}
	if main := findContainer(d, "main"); main == nil || len(main.VolumeMounts) != 0 {
		t.Errorf("main should be untouched: %+v", main)
	}
	if got := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; got != "abc123" {
		t.Errorf("config-sha annotation = %q, want abc123", got)
	}
}

func TestWireWorkloadNotFound(t *testing.T) {
	env := testenv.Start(t)
	w := wire.New(env.Client)
	err := w.Wire(context.Background(), wire.Spec{
		Kind: "Deployment", Name: "ghost", Namespace: "default", MountPath: "/x", ConfigMap: "c",
	})
	if r, _ := wire.ReasonOf(err); r != wire.ReasonWorkloadNotFound {
		t.Fatalf("reason = %v, want WorkloadNotFound (err %v)", r, err)
	}
}

func TestWireContainerNotFound(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app3",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "app3", Namespace: "default",
		Container: "nope", MountPath: "/x", ConfigMap: "c",
	})
	if r, _ := wire.ReasonOf(err); r != wire.ReasonContainerNotFound {
		t.Fatalf("reason = %v, want ContainerNotFound (err %v)", r, err)
	}
}

func TestWireIsIdempotent(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app4",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	s := wire.Spec{Kind: "Deployment", Name: "app4", Namespace: "default", MountPath: "/cfg", ConfigMap: "c"}
	if err := w.Wire(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := w.Wire(ctx, s); err != nil {
		t.Fatalf("second wire: %v", err)
	}
	d := getDeploy(t, env, "app4")
	if main := findContainer(d, "main"); main == nil || len(main.VolumeMounts) != 1 {
		t.Fatalf("expected exactly one volumeMount, got %+v", main)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("expected exactly one volume, got %+v", d.Spec.Template.Spec.Volumes)
	}
}

func TestUnwireRetractsOwnedFields(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app5",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "app5", Namespace: "default",
		MountPath: "/cfg", ConfigMap: "c", ConfigSHA: "v1",
	}); err != nil {
		t.Fatal(err)
	}

	if err := w.Unwire(ctx, "Deployment", "default", "app5"); err != nil {
		t.Fatalf("unwire: %v", err)
	}

	d := getDeploy(t, env, "app5")
	if hasVolume(d, wire.VolumeName) {
		t.Errorf("volume not retracted: %+v", d.Spec.Template.Spec.Volumes)
	}
	main := findContainer(d, "main")
	if main == nil {
		t.Fatal("container was removed by unwire")
	}
	if len(main.VolumeMounts) != 0 {
		t.Errorf("volumeMount not retracted: %+v", main.VolumeMounts)
	}
	if main.Image != "nginx:1" {
		t.Errorf("image mutated by unwire: %q", main.Image)
	}
	if _, ok := d.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("config-sha annotation not retracted: %v", d.Spec.Template.Annotations)
	}
}

func TestUnwireMissingWorkloadIsNoOp(t *testing.T) {
	env := testenv.Start(t)
	w := wire.New(env.Client)
	if err := w.Unwire(context.Background(), "Deployment", "default", "ghost"); err != nil {
		t.Fatalf("unwire missing should be no-op, got %v", err)
	}
}

func findMount(c *corev1.Container, path string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == path {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

func findEnv(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// TestWireSecretFileSurface mounts a file-surfaced secret as a read-only volume
// alongside the config volume (SPEC §8.4).
func TestWireSecretFileSurface(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("secfile",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "secfile", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
		SecretFiles: []wire.SecretFile{{RefName: "tls", SecretName: "tls-secret", MountPath: "/etc/tls"}},
	}); err != nil {
		t.Fatalf("wire: %v", err)
	}
	d := getDeploy(t, env, "secfile")
	vol := wire.SecretVolumeName("tls")
	if !hasVolume(d, vol) {
		t.Fatalf("secret volume %q not injected: %+v", vol, d.Spec.Template.Spec.Volumes)
	}
	var found bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == vol {
			found = true
			if v.Secret == nil || v.Secret.SecretName != "tls-secret" {
				t.Errorf("secret volume source = %+v, want secret tls-secret", v.VolumeSource)
			}
		}
	}
	if !found {
		t.Fatal("secret volume missing")
	}
	main := findContainer(d, "main")
	m := findMount(main, "/etc/tls")
	if m == nil || m.Name != vol || m.ReadOnly != true {
		t.Errorf("secret mount not injected read-only: %+v", main.VolumeMounts)
	}
	// The config mount still exists.
	if findMount(main, "/etc/kohen/config") == nil {
		t.Errorf("config mount lost: %+v", main.VolumeMounts)
	}
}

// TestWireSecretEnvSurface injects an env-surfaced secret as a discrete env
// entry with valueFrom.secretKeyRef (R-WIRE.2 — never envFrom).
func TestWireSecretEnvSurface(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("secenv",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "secenv", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
		SecretEnv: []wire.SecretEnv{{EnvVar: "DB_PASSWORD", SecretName: "db-secret", Key: "password"}},
	}); err != nil {
		t.Fatalf("wire: %v", err)
	}
	d := getDeploy(t, env, "secenv")
	main := findContainer(d, "main")
	e := findEnv(main, "DB_PASSWORD")
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

// TestWireSecretSurfacePruneOnRemoval verifies that dropping a secret ref from
// the wire spec retracts exactly its volume/mount/env (SSA prune of owned
// fields), leaving the config wiring intact (R-WIRE.6 semantics).
func TestWireSecretSurfacePruneOnRemoval(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("secprune",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	w := wire.New(env.Client)
	full := wire.Spec{
		Kind: "Deployment", Name: "secprune", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
		SecretFiles: []wire.SecretFile{{RefName: "tls", SecretName: "tls-secret", MountPath: "/etc/tls"}},
		SecretEnv:   []wire.SecretEnv{{EnvVar: "DB_PASSWORD", SecretName: "db-secret", Key: "password"}},
	}
	if err := w.Wire(ctx, full); err != nil {
		t.Fatal(err)
	}
	// Re-wire without the secret surfaces.
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "secprune", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
	}); err != nil {
		t.Fatal(err)
	}
	d := getDeploy(t, env, "secprune")
	if hasVolume(d, wire.SecretVolumeName("tls")) {
		t.Errorf("secret volume not pruned: %+v", d.Spec.Template.Spec.Volumes)
	}
	main := findContainer(d, "main")
	if findMount(main, "/etc/tls") != nil {
		t.Errorf("secret mount not pruned: %+v", main.VolumeMounts)
	}
	if findEnv(main, "DB_PASSWORD") != nil {
		t.Errorf("secret env not pruned: %+v", main.Env)
	}
	// Config wiring survives.
	if !hasVolume(d, wire.VolumeName) || findMount(main, "/etc/kohen/config") == nil {
		t.Errorf("config wiring lost during prune: %+v", d.Spec.Template.Spec)
	}
}

// TestWireSecretSurfaceCoexistsWithForeignListEntries verifies Kohen's
// surface env/volumeMount/volume entries co-own the keyed lists alongside
// another manager's entries, and that pruning Kohen's entries leaves the
// foreign entries intact (R-WIRE.2 mergeable lists, R-WIRE.4 no force-take).
func TestWireSecretSurfaceCoexistsWithForeignListEntries(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	// A workload where another manager owns an env var, a volume, and a mount.
	d := deployment("coexist", corev1.Container{
		Name:  "main",
		Image: "nginx:1",
		Env:   []corev1.EnvVar{{Name: "FOREIGN_ENV", Value: "keep-me"}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "foreign-vol", MountPath: "/foreign"},
		},
	})
	d.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "foreign-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	d.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	if err := env.Client.Patch(ctx, d, client.Apply, client.FieldOwner("argocd"), client.ForceOwnership); err != nil {
		t.Fatalf("simulated gitops apply: %v", err)
	}

	w := wire.New(env.Client)
	full := wire.Spec{
		Kind: "Deployment", Name: "coexist", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
		SecretFiles: []wire.SecretFile{{RefName: "tls", SecretName: "tls-secret", MountPath: "/etc/tls"}},
		SecretEnv:   []wire.SecretEnv{{EnvVar: "DB_PASSWORD", SecretName: "db-secret", Key: "password"}},
	}
	if err := w.Wire(ctx, full); err != nil {
		t.Fatalf("wire alongside foreign entries: %v", err)
	}

	got := getDeploy(t, env, "coexist")
	main := findContainer(got, "main")
	if findEnv(main, "FOREIGN_ENV") == nil || findEnv(main, "DB_PASSWORD") == nil {
		t.Fatalf("foreign and kohen env should coexist: %+v", main.Env)
	}
	if findMount(main, "/foreign") == nil || findMount(main, "/etc/tls") == nil {
		t.Fatalf("foreign and kohen mounts should coexist: %+v", main.VolumeMounts)
	}
	if !hasVolume(got, "foreign-vol") || !hasVolume(got, wire.SecretVolumeName("tls")) {
		t.Fatalf("foreign and kohen volumes should coexist: %+v", got.Spec.Template.Spec.Volumes)
	}

	// Prune Kohen's secret surfaces; the foreign entries must survive.
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "coexist", Namespace: "default",
		MountPath: "/etc/kohen/config", ConfigMap: "c",
	}); err != nil {
		t.Fatalf("re-wire without surfaces: %v", err)
	}
	got = getDeploy(t, env, "coexist")
	main = findContainer(got, "main")
	if findEnv(main, "FOREIGN_ENV") == nil {
		t.Errorf("prune retracted foreign env: %+v", main.Env)
	}
	if findEnv(main, "DB_PASSWORD") != nil {
		t.Errorf("prune left kohen env: %+v", main.Env)
	}
	if findMount(main, "/foreign") == nil {
		t.Errorf("prune retracted foreign mount: %+v", main.VolumeMounts)
	}
	if !hasVolume(got, "foreign-vol") {
		t.Errorf("prune retracted foreign volume: %+v", got.Spec.Template.Spec.Volumes)
	}
}

// TestWireCoexistsWithOtherManager verifies Kohen wires without disturbing
// fields owned by a different SSA manager (GitOps coexistence, R-WIRE.4).
func TestWireCoexistsWithOtherManager(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, deployment("app6",
		corev1.Container{Name: "main", Image: "nginx:1"})); err != nil {
		t.Fatal(err)
	}
	// Another manager updates the image via SSA (like Argo CD ServerSideApply).
	patch := deployment("app6", corev1.Container{Name: "main", Image: "nginx:2"})
	patch.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	if err := env.Client.Patch(ctx, patch, client.Apply, client.FieldOwner("argocd"), client.ForceOwnership); err != nil {
		t.Fatalf("simulated gitops apply: %v", err)
	}

	w := wire.New(env.Client)
	if err := w.Wire(ctx, wire.Spec{
		Kind: "Deployment", Name: "app6", Namespace: "default", MountPath: "/cfg", ConfigMap: "c",
	}); err != nil {
		t.Fatalf("wire alongside other manager: %v", err)
	}
	d := getDeploy(t, env, "app6")
	main := findContainer(d, "main")
	if main == nil || main.Image != "nginx:2" {
		t.Errorf("other manager's image not preserved: %+v", main)
	}
	if len(main.VolumeMounts) != 1 {
		t.Errorf("kohen mount missing: %+v", main.VolumeMounts)
	}
}
