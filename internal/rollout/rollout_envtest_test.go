package rollout_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/rollout"
	"github.com/ozimakov/kohen/internal/testenv"
)

func TestStampNoRestartStampsObjectNotPodTemplate(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "none-app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "none-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "none-app"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx:1"}}},
			},
		},
	}
	if err := env.Client.Create(ctx, dep); err != nil {
		t.Fatal(err)
	}

	if err := rollout.StampNoRestart(ctx, env.Client, "Deployment", "default", "none-app", "git:abc123"); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got := &appsv1.Deployment{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "none-app", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	// Stamp is on the workload object metadata...
	if got.Annotations[kohenv1alpha1.AnnotationConfigSHA] != "git:abc123" {
		t.Errorf("object annotation = %q, want git:abc123", got.Annotations[kohenv1alpha1.AnnotationConfigSHA])
	}
	// ...and NOT on the pod template (no restart).
	if _, ok := got.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("pod template must not carry the stamp in none mode: %v", got.Spec.Template.Annotations)
	}

	if err := rollout.ClearStamp(ctx, env.Client, "Deployment", "default", "none-app"); err != nil {
		t.Fatalf("clear stamp: %v", err)
	}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: "none-app", Namespace: "default"}, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[kohenv1alpha1.AnnotationConfigSHA]; ok {
		t.Errorf("ClearStamp left object annotation: %v", got.Annotations)
	}
}
