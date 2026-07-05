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
