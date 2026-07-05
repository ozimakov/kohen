// Package controller contains the ConfigSync reconciler (SPEC §7, §10). In the
// S1.4 skeleton the reconcile body is a no-op that records observedGeneration
// and a Ready condition; S1.5–S1.8 replace the body with the fetch → render →
// apply → wire → roll out pipeline.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
)

// ConfigSyncReconciler reconciles ConfigSync objects.
type ConfigSyncReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;patch

// Reconcile implements the ConfigSync control loop.
func (r *ConfigSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cs kohenv1alpha1.ConfigSync
	if err := r.Get(ctx, req.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// S1.4 skeleton: no-op reconcile. The real pipeline lands in S1.5–S1.8.
	changed := false
	if cs.Status.ObservedGeneration != cs.Generation {
		cs.Status.ObservedGeneration = cs.Generation
		changed = true
	}
	if meta.SetStatusCondition(&cs.Status.Conditions, metav1.Condition{
		Type:               kohenv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             kohenv1alpha1.ReasonSynced,
		Message:            "reconcile skeleton: no-op",
		ObservedGeneration: cs.Generation,
	}) {
		changed = true
	}

	if changed {
		if err := r.Status().Update(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		log.V(1).Info("status updated", "generation", cs.Generation)
	}

	return ctrl.Result{RequeueAfter: cs.Spec.SyncInterval()}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *ConfigSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kohenv1alpha1.ConfigSync{}).
		Named("configsync").
		Complete(r)
}
