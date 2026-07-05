package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapNameDefault(t *testing.T) {
	s := &ConfigSyncSpec{WorkloadRef: WorkloadReference{Name: "checkout"}}
	if got := s.ConfigMapName(); got != "checkout-config" {
		t.Fatalf("ConfigMapName() = %q, want checkout-config", got)
	}
	s.ConfigMap.Name = "explicit"
	if got := s.ConfigMapName(); got != "explicit" {
		t.Fatalf("ConfigMapName() = %q, want explicit", got)
	}
}

func TestSyncIntervalDefault(t *testing.T) {
	var s ConfigSyncSpec
	if got := s.SyncInterval(); got != DefaultSyncInterval {
		t.Fatalf("SyncInterval() = %v, want %v", got, DefaultSyncInterval)
	}
	s.Sync.Interval = metav1.Duration{Duration: 5 * time.Minute}
	if got := s.SyncInterval(); got != 5*time.Minute {
		t.Fatalf("SyncInterval() = %v, want 5m", got)
	}
}

func TestShouldRolloutOnRotate(t *testing.T) {
	// Unset defaults to true (SPEC §11.2).
	var s SecretSurface
	if !s.ShouldRolloutOnRotate() {
		t.Errorf("unset ShouldRolloutOnRotate() = false, want true")
	}
	f := false
	s.RolloutOnRotate = &f
	if s.ShouldRolloutOnRotate() {
		t.Errorf("explicit false ShouldRolloutOnRotate() = true, want false")
	}
	tr := true
	s.RolloutOnRotate = &tr
	if !s.ShouldRolloutOnRotate() {
		t.Errorf("explicit true ShouldRolloutOnRotate() = false, want true")
	}
}

func TestSecretReferenceSecretName(t *testing.T) {
	tests := []struct {
		name string
		ref  SecretReference
		want string
	}{
		{
			name: "externalSecret",
			ref:  SecretReference{Backend: BackendExternalSecret, ExternalSecret: &LocalObjectReference{Name: "checkout-db"}},
			want: "checkout-db",
		},
		{
			name: "nativeSecret",
			ref:  SecretReference{Backend: BackendNativeSecret, NativeSecret: &LocalObjectReference{Name: "checkout-tls"}},
			want: "checkout-tls",
		},
		{
			name: "externalSecret missing ref",
			ref:  SecretReference{Backend: BackendExternalSecret},
			want: "",
		},
		{
			name: "unknown backend",
			ref:  SecretReference{Backend: "other", NativeSecret: &LocalObjectReference{Name: "x"}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.SecretName(); got != tc.want {
				t.Errorf("SecretName() = %q, want %q", got, tc.want)
			}
		})
	}
}
