package rollout_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/rollout"
)

func ptr[T any](v T) *T { return &v }

func TestVersion(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	if got := rollout.Version(full); got != "git:0123456789ab" {
		t.Fatalf("Version = %q, want git:0123456789ab", got)
	}
	if got := rollout.Version("abc"); got != "git:abc" {
		t.Fatalf("Version(short) = %q, want git:abc", got)
	}
}

func TestStatefulSetSupported(t *testing.T) {
	ok := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{
		UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType}}}
	if s, _ := rollout.StatefulSetSupported(ok); !s {
		t.Errorf("RollingUpdate should be supported")
	}
	bad := &appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{
		UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}}}
	if s, msg := rollout.StatefulSetSupported(bad); s || msg == "" {
		t.Errorf("OnDelete should be unsupported")
	}
}

func TestDeploymentProgress(t *testing.T) {
	tests := []struct {
		name         string
		d            *appsv1.Deployment
		wantComplete bool
		wantReason   string
	}{
		{
			name: "complete",
			d: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr[int32](2)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2,
				},
			},
			wantComplete: true, wantReason: kohenv1alpha1.ReasonSynced,
		},
		{
			name: "generation not observed",
			d: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 4},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr[int32](2)},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 3},
			},
			wantComplete: false, wantReason: kohenv1alpha1.ReasonRollingOut,
		},
		{
			name: "updating",
			d: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr[int32](3)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 1,
				},
			},
			wantComplete: false, wantReason: kohenv1alpha1.ReasonRollingOut,
		},
		{
			name: "old replicas pending",
			d: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr[int32](2)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3, Replicas: 3, UpdatedReplicas: 2, AvailableReplicas: 2,
				},
			},
			wantComplete: false, wantReason: kohenv1alpha1.ReasonRollingOut,
		},
		{
			name: "progress deadline exceeded",
			d: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr[int32](2)},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 3,
					Conditions: []appsv1.DeploymentCondition{{
						Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
						Reason: "ProgressDeadlineExceeded", Message: "deadline",
					}},
				},
			},
			wantComplete: false, wantReason: kohenv1alpha1.ReasonProgressDeadlineExceeded,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := rollout.DeploymentProgress(tc.d)
			if p.Complete != tc.wantComplete || p.Reason != tc.wantReason {
				t.Fatalf("got {%v %q}, want {%v %q}", p.Complete, p.Reason, tc.wantComplete, tc.wantReason)
			}
		})
	}
}

func TestStatefulSetProgress(t *testing.T) {
	complete := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr[int32](2)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, UpdatedReplicas: 2, ReadyReplicas: 2,
			CurrentRevision: "r2", UpdateRevision: "r2",
		},
	}
	if p := rollout.StatefulSetProgress(complete); !p.Complete {
		t.Errorf("expected complete, got %+v", p)
	}
	mid := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr[int32](2)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, UpdatedReplicas: 1, ReadyReplicas: 1,
			CurrentRevision: "r1", UpdateRevision: "r2",
		},
	}
	if p := rollout.StatefulSetProgress(mid); p.Complete || p.Reason != kohenv1alpha1.ReasonRollingOut {
		t.Errorf("expected rolling out, got %+v", p)
	}
}
