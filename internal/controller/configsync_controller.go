// Package controller contains the ConfigSync reconciler (SPEC §7.3, §10): the
// fetch → render → apply → prune → wire → stamp/rollout pipeline with a
// fail-safe (keep last-good on failure, never prune on fetch failure) and a
// finalizer that unwires the workload and prunes owned objects on deletion.
package controller

import (
	"context"
	"fmt"
	"net"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/apply"
	"github.com/ozimakov/kohen/internal/config"
	"github.com/ozimakov/kohen/internal/git"
	"github.com/ozimakov/kohen/internal/metrics"
	"github.com/ozimakov/kohen/internal/redact"
	"github.com/ozimakov/kohen/internal/render"
	"github.com/ozimakov/kohen/internal/rollout"
	"github.com/ozimakov/kohen/internal/wire"
)

// FinalizerName gates cleanup of Kohen-owned objects and workload unwiring on
// ConfigSync deletion (SPEC R11.3, R-WIRE.6).
const FinalizerName = "kohen.dev/finalizer"

// progressingRequeue is a short requeue used while a rollout is in progress.
const progressingRequeue = 10 * time.Second

// ConfigSyncReconciler reconciles ConfigSync objects.
type ConfigSyncReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Fetcher  git.Source
	Renderer render.Interface
	Applier  *apply.Applier
	Wirer    *wire.Wirer
	Redactor *redact.Redactor
	Config   *config.Config
}

// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kohen.dev,resources=configsyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;patch

// Reconcile runs the ConfigSync pipeline.
func (r *ConfigSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cs kohenv1alpha1.ConfigSync
	if err := r.Get(ctx, req.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cs.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cs)
	}

	if !controllerutil.ContainsFinalizer(&cs, FinalizerName) {
		controllerutil.AddFinalizer(&cs, FinalizerName)
		if err := r.Update(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Force-sync annotation: clear it so re-setting an identical value re-triggers
	// and callers get a completion signal (SPEC §6.1).
	if _, ok := cs.Annotations[kohenv1alpha1.AnnotationSyncNow]; ok {
		delete(cs.Annotations, kohenv1alpha1.AnnotationSyncNow)
		if err := r.Update(ctx, &cs); err != nil {
			return ctrl.Result{}, err
		}
		r.event(&cs, corev1.EventTypeNormal, "SyncNow", "processed force-sync annotation")
		return ctrl.Result{Requeue: true}, nil
	}

	result, syncErr := r.sync(ctx, &cs)

	recordReconcileResult(&cs)

	if err := r.Status().Update(ctx, &cs); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "updating status")
		return ctrl.Result{}, err
	}
	return result, syncErr
}

// sync executes the pipeline, mutating cs.Status. It returns a retryable error
// only for transient failures that should trigger controller-runtime backoff.
func (r *ConfigSyncReconciler) sync(ctx context.Context, cs *kohenv1alpha1.ConfigSync) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	cs.Status.ObservedGeneration = cs.Generation
	steady := ctrl.Result{RequeueAfter: cs.Spec.SyncInterval()}

	// R-SINGLETON: at most one ConfigSync per workload.
	if other, conflict := r.singletonConflict(ctx, cs); conflict {
		msg := fmt.Sprintf("workload %s/%s is already targeted by ConfigSync %q",
			cs.Spec.WorkloadRef.Kind, cs.Spec.WorkloadRef.Name, other)
		setCondition(cs, kohenv1alpha1.ConditionWorkloadWired, metav1.ConditionFalse, kohenv1alpha1.ReasonSingletonViolation, msg)
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, msg)
		r.event(cs, corev1.EventTypeWarning, kohenv1alpha1.ReasonSingletonViolation, msg)
		return steady, nil
	}

	// Credentials (R-AUTH.6, R8.3).
	cred, err := r.loadCredential(ctx, cs)
	if err != nil {
		setCondition(cs, kohenv1alpha1.ConditionFetched, metav1.ConditionFalse, kohenv1alpha1.ReasonAuthFailed, err.Error())
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, "credential error")
		r.event(cs, corev1.EventTypeWarning, kohenv1alpha1.ReasonAuthFailed, err.Error())
		return ctrl.Result{}, err
	}

	// Fetch (never prune on failure — keep last-good).
	res, err := r.Fetcher.Fetch(ctx, git.Reference{
		URL:  cs.Spec.Source.URL,
		Ref:  cs.Spec.Source.Ref,
		Path: cs.Spec.Path,
	}, cred)
	if err != nil {
		reason := gitConditionReason(err)
		metrics.FetchErrors.WithLabelValues(reason).Inc()
		setCondition(cs, kohenv1alpha1.ConditionFetched, metav1.ConditionFalse, reason, err.Error())
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, "fetch failed, serving last-good")
		r.event(cs, corev1.EventTypeWarning, reason, err.Error())
		return ctrl.Result{}, err
	}
	defer func() {
		if cerr := res.Cleanup(); cerr != nil {
			log.V(1).Info("cleanup checkout", "error", cerr.Error())
		}
	}()
	cs.Status.SourceCommit = res.Commit
	setCondition(cs, kohenv1alpha1.ConditionFetched, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced,
		"fetched commit "+rollout.ShortCommit(res.Commit))

	// Render (keep last-good on failure).
	rendered, err := r.Renderer.Render(res.Dir)
	if err != nil {
		reason := renderConditionReason(err)
		metrics.RenderErrors.WithLabelValues(reason).Inc()
		setCondition(cs, kohenv1alpha1.ConditionRendered, metav1.ConditionFalse, reason, err.Error())
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, "render failed, serving last-good")
		r.event(cs, corev1.EventTypeWarning, reason, err.Error())
		return steady, nil
	}
	setCondition(cs, kohenv1alpha1.ConditionRendered, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced,
		fmt.Sprintf("rendered %d keys (%d bytes)", len(rendered.Data)+len(rendered.BinaryData), rendered.TotalBytes))

	// Apply the ConfigMap (fully owned, SSA) and prune stale owned ConfigMaps.
	cmName := cs.Spec.ConfigMapName()
	if err := r.Applier.Apply(ctx, cs, buildConfigMap(cs, cmName, rendered)); err != nil {
		reason := applyConditionReason(err)
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, err.Error())
		r.event(cs, corev1.EventTypeWarning, reason, err.Error())
		if reason == string(apply.ReasonAlreadyExistsNotOwned) {
			return steady, nil // terminal until the user resolves the conflict
		}
		return ctrl.Result{}, err
	}
	if err := r.Applier.Prune(ctx, cs, &corev1.ConfigMapList{}, cmName); err != nil {
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, err.Error())
		return ctrl.Result{}, err
	}

	// Version and workload wiring / stamping.
	version := rollout.Version(res.Commit)
	cs.Status.ConfigVersion = version
	rolloutComplete := r.wireAndStamp(ctx, cs, cmName, version)

	// Overall readiness.
	r.computeReady(cs)
	if !rolloutComplete && workloadWired(cs) {
		return ctrl.Result{RequeueAfter: progressingRequeue}, nil
	}
	return steady, nil
}

// wireAndStamp wires the workload and stamps the version per rollout mode,
// setting WorkloadWired and RolloutComplete. Returns whether the rollout is
// complete. Object sync continues even when the workload cannot be wired.
func (r *ConfigSyncReconciler) wireAndStamp(ctx context.Context, cs *kohenv1alpha1.ConfigSync, cmName, version string) bool {
	kind := cs.Spec.WorkloadRef.Kind
	ns := cs.Namespace
	name := cs.Spec.WorkloadRef.Name

	// Existence + strategy support (R-ROLLOUT.5).
	currentStamp, reason, msg, ok := r.inspectWorkload(ctx, kind, ns, name)
	if !ok {
		setCondition(cs, kohenv1alpha1.ConditionWorkloadWired, metav1.ConditionFalse, reason, msg)
		r.event(cs, corev1.EventTypeWarning, reason, msg)
		return false
	}

	mountPath := cs.Spec.Wiring.MountPath
	if mountPath == "" {
		mountPath = "/etc/kohen/config"
	}
	spec := wire.Spec{
		Kind: kind, Name: name, Namespace: ns,
		Container: cs.Spec.Wiring.Container,
		MountPath: mountPath,
		ConfigMap: cmName,
	}
	if cs.Spec.Rollout != kohenv1alpha1.RolloutNone {
		spec.ConfigSHA = version // auto: stamp the pod template (triggers rollout)
	}
	if err := r.Wirer.Wire(ctx, spec); err != nil {
		reason := wireConditionReason(err)
		setCondition(cs, kohenv1alpha1.ConditionWorkloadWired, metav1.ConditionFalse, reason, err.Error())
		r.event(cs, corev1.EventTypeWarning, reason, err.Error())
		return false
	}
	setCondition(cs, kohenv1alpha1.ConditionWorkloadWired, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, "owned fields merged")

	if cs.Spec.Rollout == kohenv1alpha1.RolloutNone {
		if err := rollout.StampNoRestart(ctx, r.Client, kind, ns, name, version); err != nil {
			setCondition(cs, kohenv1alpha1.ConditionRolloutComplete, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, err.Error())
			return false
		}
		cs.Status.WorkloadVersion = version
		setCondition(cs, kohenv1alpha1.ConditionRolloutComplete, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, "rollout disabled (none)")
		return true
	}

	cs.Status.WorkloadVersion = version
	// Rollout triggered vs skipped-on-match (R-ROLLOUT.2, R13.1).
	if currentStamp == version {
		metrics.RolloutsSkipped.Inc()
	} else {
		metrics.RolloutsTriggered.Inc()
		r.event(cs, corev1.EventTypeNormal, "RolloutTriggered", "config version "+version+" stamped")
	}
	p := r.rolloutProgress(ctx, kind, ns, name)
	status := metav1.ConditionFalse
	if p.Complete {
		status = metav1.ConditionTrue
	}
	setCondition(cs, kohenv1alpha1.ConditionRolloutComplete, status, p.Reason, p.Message)
	cs.Status.RolloutInProgress = !p.Complete
	return p.Complete
}

// inspectWorkload verifies the workload exists and its strategy is supported,
// and returns the current pod-template config-sha stamp (for rollout
// triggered-vs-skipped accounting).
func (r *ConfigSyncReconciler) inspectWorkload(ctx context.Context, kind, ns, name string) (stamp, reason, msg string, ok bool) {
	key := client.ObjectKey{Namespace: ns, Name: name}
	switch kind {
	case "StatefulSet":
		var ss appsv1.StatefulSet
		if err := r.Get(ctx, key, &ss); err != nil {
			if apierrors.IsNotFound(err) {
				return "", kohenv1alpha1.ReasonWorkloadNotFound, fmt.Sprintf("StatefulSet %q not found", name), false
			}
			return "", kohenv1alpha1.ReasonDegraded, err.Error(), false
		}
		if supported, m := rollout.StatefulSetSupported(&ss); !supported {
			return "", kohenv1alpha1.ReasonUnsupportedStrategy, m, false
		}
		return ss.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA], "", "", true
	case "Deployment":
		var dp appsv1.Deployment
		if err := r.Get(ctx, key, &dp); err != nil {
			if apierrors.IsNotFound(err) {
				return "", kohenv1alpha1.ReasonWorkloadNotFound, fmt.Sprintf("Deployment %q not found", name), false
			}
			return "", kohenv1alpha1.ReasonDegraded, err.Error(), false
		}
		return dp.Spec.Template.Annotations[kohenv1alpha1.AnnotationConfigSHA], "", "", true
	default:
		return "", kohenv1alpha1.ReasonUnsupportedStrategy, "unsupported workload kind " + kind, false
	}
}

// rolloutProgress reads the workload and evaluates its rollout state.
func (r *ConfigSyncReconciler) rolloutProgress(ctx context.Context, kind, ns, name string) rollout.Progress {
	key := client.ObjectKey{Namespace: ns, Name: name}
	switch kind {
	case "StatefulSet":
		var ss appsv1.StatefulSet
		if err := r.Get(ctx, key, &ss); err != nil {
			return rollout.Progress{Complete: false, Reason: kohenv1alpha1.ReasonRollingOut, Message: "re-reading workload"}
		}
		return rollout.StatefulSetProgress(&ss)
	default:
		var dp appsv1.Deployment
		if err := r.Get(ctx, key, &dp); err != nil {
			return rollout.Progress{Complete: false, Reason: kohenv1alpha1.ReasonRollingOut, Message: "re-reading workload"}
		}
		return rollout.DeploymentProgress(&dp)
	}
}

// finalize unwires the workload and prunes owned objects, then drops the
// finalizer (R11.3, R-WIRE.6).
func (r *ConfigSyncReconciler) finalize(ctx context.Context, cs *kohenv1alpha1.ConfigSync) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cs, FinalizerName) {
		return ctrl.Result{}, nil
	}
	// Only unwire if this ConfigSync legitimately owns the workload wiring.
	// The wire field manager is shared across syncs, so a singleton loser (which
	// never wired) must NOT unwire — that would retract the incumbent's fields on
	// the shared workload (H-A / R-WIRE.6, R-SINGLETON).
	if _, conflict := r.singletonConflict(ctx, cs); !conflict {
		if err := r.Wirer.Unwire(ctx, cs.Spec.WorkloadRef.Kind, cs.Namespace, cs.Spec.WorkloadRef.Name); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Prune is owner-scoped, so it only removes this sync's own ConfigMaps.
	if err := r.Applier.Prune(ctx, cs, &corev1.ConfigMapList{}); err != nil {
		return ctrl.Result{}, err
	}
	// Drop the per-object degraded gauge so deleted syncs don't leak a series.
	metrics.Degraded.DeleteLabelValues(cs.Namespace, cs.Name)
	controllerutil.RemoveFinalizer(cs, FinalizerName)
	if err := r.Update(ctx, cs); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// singletonConflict reports whether another live ConfigSync targeting the same
// workload should win over cs. Only the loser (the newer sync) is reported in
// conflict, so the incumbent keeps reconciling (SPEC R-SINGLETON, §10).
func (r *ConfigSyncReconciler) singletonConflict(ctx context.Context, cs *kohenv1alpha1.ConfigSync) (string, bool) {
	var list kohenv1alpha1.ConfigSyncList
	if err := r.List(ctx, &list, client.InNamespace(cs.Namespace)); err != nil {
		return "", false
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == cs.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.WorkloadRef == cs.Spec.WorkloadRef && winsOver(other, cs) {
			return other.Name, true
		}
	}
	return "", false
}

// winsOver returns whether a should own a shared workload in preference to b:
// the older ConfigSync wins; ties break on name for determinism.
func winsOver(a, b *kohenv1alpha1.ConfigSync) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}

// computeReady derives the overall Ready condition from the sub-conditions.
func (r *ConfigSyncReconciler) computeReady(cs *kohenv1alpha1.ConfigSync) {
	for _, t := range []string{
		kohenv1alpha1.ConditionFetched,
		kohenv1alpha1.ConditionRendered,
		kohenv1alpha1.ConditionWorkloadWired,
	} {
		if !conditionTrue(cs, t) {
			r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonDegraded, t+" is not satisfied")
			return
		}
	}
	if !conditionTrue(cs, kohenv1alpha1.ConditionRolloutComplete) {
		r.setReady(cs, metav1.ConditionFalse, kohenv1alpha1.ReasonProgressing, "rollout in progress")
		return
	}
	r.setReady(cs, metav1.ConditionTrue, kohenv1alpha1.ReasonSynced, "config version "+cs.Status.ConfigVersion+" applied and rolled out")
}

func (r *ConfigSyncReconciler) setReady(cs *kohenv1alpha1.ConfigSync, status metav1.ConditionStatus, reason, msg string) {
	setCondition(cs, kohenv1alpha1.ConditionReady, status, reason, msg)
}

func (r *ConfigSyncReconciler) event(cs *kohenv1alpha1.ConfigSync, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(cs, eventType, reason, msg)
	}
}

func conditionTrue(cs *kohenv1alpha1.ConfigSync, condType string) bool {
	for _, c := range cs.Status.Conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func workloadWired(cs *kohenv1alpha1.ConfigSync) bool {
	return conditionTrue(cs, kohenv1alpha1.ConditionWorkloadWired)
}

// SetupWithManager wires defaults and registers the reconciler.
func (r *ConfigSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	if r.Config == nil {
		r.Config = config.Default()
	}
	if r.Redactor == nil {
		r.Redactor = redact.New()
	}
	if r.Applier == nil {
		r.Applier = apply.New(r.Client, r.Scheme)
	}
	if r.Wirer == nil {
		r.Wirer = wire.New(r.Client)
	}
	if r.Renderer == nil {
		r.Renderer = render.New(render.Options{})
	}
	if r.Fetcher == nil {
		// Wire a real resolver so hostname-based SSRF is guarded, not only
		// IP-literal hosts (R-AUTH.7 / TM5).
		r.Fetcher = git.NewClient(git.Options{
			AllowList: r.Config.SourceAllowList,
			Resolver:  net.DefaultResolver,
		})
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kohenv1alpha1.ConfigSync{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapWorkload("Deployment"))).
		Watches(&appsv1.StatefulSet{}, handler.EnqueueRequestsFromMapFunc(r.mapWorkload("StatefulSet"))).
		Named("configsync").
		Complete(r)
}

// mapWorkload maps a changed workload back to the ConfigSync(s) targeting it, so
// rollout progress and drift (e.g. another manager stripping Kohen's fields) are
// reconciled promptly rather than only on the poll interval.
func (r *ConfigSyncReconciler) mapWorkload(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list kohenv1alpha1.ConfigSyncList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range list.Items {
			cs := &list.Items[i]
			if cs.Spec.WorkloadRef.Kind == kind && cs.Spec.WorkloadRef.Name == obj.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cs)})
			}
		}
		return reqs
	}
}

// recordReconcileResult exports the reconcile outcome and degraded gauge.
func recordReconcileResult(cs *kohenv1alpha1.ConfigSync) {
	result := "degraded"
	degraded := 1.0
	if c := findCondition(cs, kohenv1alpha1.ConditionReady); c != nil {
		switch {
		case c.Status == metav1.ConditionTrue:
			result, degraded = "synced", 0
		case c.Reason == kohenv1alpha1.ReasonProgressing:
			result, degraded = "progressing", 0
		}
	}
	metrics.ReconcileTotal.WithLabelValues(result).Inc()
	metrics.Degraded.WithLabelValues(cs.Namespace, cs.Name).Set(degraded)
}

func findCondition(cs *kohenv1alpha1.ConfigSync, condType string) *metav1.Condition {
	for i := range cs.Status.Conditions {
		if cs.Status.Conditions[i].Type == condType {
			return &cs.Status.Conditions[i]
		}
	}
	return nil
}
