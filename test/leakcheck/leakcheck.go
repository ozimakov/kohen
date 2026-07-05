// Package leakcheck provides a reusable secret-leak assertion helper (PLAN
// S0.2). Tests register known fixture secret values and then assert that those
// values never appear in status, events, logs, or applied objects — enforcing
// SPEC R8.3/TM9 ("no secret value in git, logs, events, metrics, status, or CLI
// output") on every PR that touches reconcile/logging packages, not only in
// Tier 3.
package leakcheck

import (
	"encoding/json"
	"strings"
	"testing"
)

// Scanner holds registered secret values and detects them in arbitrary text or
// marshalled objects.
type Scanner struct {
	secrets []string
}

// New returns a Scanner seeded with the given secret values.
func New(secrets ...string) *Scanner {
	s := &Scanner{}
	s.Add(secrets...)
	return s
}

// Add registers additional secret values. Empty values are ignored.
func (s *Scanner) Add(secrets ...string) {
	for _, sec := range secrets {
		if strings.TrimSpace(sec) == "" {
			continue
		}
		s.secrets = append(s.secrets, sec)
	}
}

// Find returns the registered secrets that appear as substrings of text.
func (s *Scanner) Find(text string) []string {
	var found []string
	for _, sec := range s.secrets {
		if strings.Contains(text, sec) {
			found = append(found, sec)
		}
	}
	return found
}

// AssertClean fails t if any registered secret appears in any of values.
func (s *Scanner) AssertClean(t testing.TB, label string, values ...string) {
	t.Helper()
	for _, v := range values {
		if found := s.Find(v); len(found) > 0 {
			t.Errorf("secret leak in %s: %d registered secret(s) found in %q", label, len(found), v)
		}
	}
}

// AssertObjectClean marshals obj to JSON and asserts no secret appears in it.
// Use it on applied ConfigMaps, workloads, or ConfigSync status.
func (s *Scanner) AssertObjectClean(t testing.TB, label string, obj any) {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("leakcheck marshal %s: %v", label, err)
	}
	s.AssertClean(t, label, string(b))
}
