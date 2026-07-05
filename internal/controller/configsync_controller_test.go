package controller_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/controller"
	"github.com/ozimakov/kohen/internal/testenv"
)

func TestReconcileNoOpSetsReady(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	cs := &kohenv1alpha1.ConfigSync{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: kohenv1alpha1.ConfigSyncSpec{
			Source:      kohenv1alpha1.GitSource{URL: "https://github.com/acme/config.git"},
			Path:        "svc/app",
			WorkloadRef: kohenv1alpha1.WorkloadReference{Kind: "Deployment", Name: "app"},
		},
	}
	if err := env.Client.Create(ctx, cs); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &controller.ConfigSyncReconciler{Client: env.Client, Scheme: scheme.Scheme}
	key := client.ObjectKeyFromObject(cs)

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected periodic requeue, got %v", res.RequeueAfter)
	}

	got := &kohenv1alpha1.ConfigSync{}
	if err := env.Client.Get(ctx, key, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, kohenv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want True", cond)
	}
	if cond.Reason != kohenv1alpha1.ReasonSynced {
		t.Errorf("Ready reason = %q, want %q", cond.Reason, kohenv1alpha1.ReasonSynced)
	}
}

func TestReconcileMissingObjectNoError(t *testing.T) {
	env := testenv.Start(t)
	r := &controller.ConfigSyncReconciler{Client: env.Client, Scheme: scheme.Scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "ghost", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected nil error for missing object, got %v", err)
	}
}
