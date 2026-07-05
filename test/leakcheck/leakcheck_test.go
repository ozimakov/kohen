package leakcheck_test

import (
	"testing"

	"github.com/ozimakov/kohen/test/leakcheck"
)

func TestScannerFind(t *testing.T) {
	s := leakcheck.New("hunter2token", "another-secret")
	if got := s.Find("log line with hunter2token in it"); len(got) != 1 {
		t.Errorf("expected 1 finding, got %v", got)
	}
	if got := s.Find("clean line"); len(got) != 0 {
		t.Errorf("expected no findings, got %v", got)
	}
	if got := s.Find("both hunter2token and another-secret"); len(got) != 2 {
		t.Errorf("expected 2 findings, got %v", got)
	}
}

func TestScannerIgnoresEmpty(t *testing.T) {
	s := leakcheck.New("", "  ")
	if got := s.Find("anything at all"); len(got) != 0 {
		t.Errorf("empty secrets must not match: %v", got)
	}
}

func TestAssertObjectClean(t *testing.T) {
	s := leakcheck.New("topsecret-value")
	// A clean object passes.
	s.AssertObjectClean(t, "clean", map[string]string{"key": "value"})

	// A leaking object fails: verify via a sub-recorder.
	rec := &recordingT{}
	s.AssertObjectClean(rec, "dirty", map[string]string{"password": "topsecret-value"})
	if !rec.failed {
		t.Errorf("expected AssertObjectClean to flag a leaking object")
	}
}

// recordingT captures whether an assertion failed without failing the parent.
type recordingT struct {
	testing.TB
	failed bool
}

func (r *recordingT) Helper()                   {}
func (r *recordingT) Errorf(string, ...any)     { r.failed = true }
func (r *recordingT) Fatalf(f string, a ...any) { r.failed = true }
