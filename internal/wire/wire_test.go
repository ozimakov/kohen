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
