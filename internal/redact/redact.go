// Package redact provides a centralized redacting logger so secret material
// never reaches logs, events, or status (SPEC R8.3, TM9).
//
// The operator registers sensitive values (git tokens, SSH keys, resolved
// secret bytes) with a Redactor as it loads them; the Redactor is wired into the
// logging pipeline as a logr.LogSink wrapper that replaces any occurrence of a
// registered value — in messages, string values, and error text — with a fixed
// placeholder.
package redact

import (
	"strings"
	"sync"

	"github.com/go-logr/logr"
)

// Placeholder replaces redacted secret material.
const Placeholder = "[REDACTED]"

// minSecretLen avoids redacting trivially short values that would match common
// substrings and garble unrelated logs.
const minSecretLen = 5

// Redactor holds registered sensitive values and redacts them from strings. It
// is safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  map[string]struct{}
	replacer *strings.Replacer
}

// New returns an empty Redactor.
func New() *Redactor {
	return &Redactor{secrets: map[string]struct{}{}, replacer: strings.NewReplacer()}
}

// Add registers one or more sensitive values. Values shorter than minSecretLen
// and multi-line secrets' individual short lines are ignored; whole values and
// each of their non-trivial lines are registered so that partial logging still
// redacts.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, s := range secrets {
		for _, candidate := range append([]string{s}, strings.Split(s, "\n")...) {
			candidate = strings.TrimSpace(candidate)
			if len(candidate) < minSecretLen {
				continue
			}
			if _, ok := r.secrets[candidate]; !ok {
				r.secrets[candidate] = struct{}{}
				changed = true
			}
		}
	}
	if changed {
		r.rebuild()
	}
}

// rebuild recreates the replacer. Caller must hold the write lock.
func (r *Redactor) rebuild() {
	pairs := make([]string, 0, len(r.secrets)*2)
	for s := range r.secrets {
		pairs = append(pairs, s, Placeholder)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// String returns s with every registered secret replaced by Placeholder.
func (r *Redactor) String(s string) string {
	r.mu.RLock()
	rep := r.replacer
	r.mu.RUnlock()
	return rep.Replace(s)
}

// NewLogger wraps base so that all output is redacted through r.
func NewLogger(base logr.Logger, r *Redactor) logr.Logger {
	return logr.New(&sink{inner: base.GetSink(), r: r})
}

// sink is a logr.LogSink that redacts messages, string values, and error text.
type sink struct {
	inner logr.LogSink
	r     *Redactor
}

func (s *sink) Init(info logr.RuntimeInfo) { s.inner.Init(info) }

func (s *sink) Enabled(level int) bool { return s.inner.Enabled(level) }

func (s *sink) Info(level int, msg string, kv ...any) {
	s.inner.Info(level, s.r.String(msg), s.redactKV(kv)...)
}

func (s *sink) Error(err error, msg string, kv ...any) {
	s.inner.Error(s.redactErr(err), s.r.String(msg), s.redactKV(kv)...)
}

func (s *sink) WithValues(kv ...any) logr.LogSink {
	return &sink{inner: s.inner.WithValues(s.redactKV(kv)...), r: s.r}
}

func (s *sink) WithName(name string) logr.LogSink {
	return &sink{inner: s.inner.WithName(name), r: s.r}
}

// redactKV redacts string and error values (keys are assumed static identifiers).
func (s *sink) redactKV(kv []any) []any {
	if len(kv) == 0 {
		return kv
	}
	out := make([]any, len(kv))
	for i, v := range kv {
		switch tv := v.(type) {
		case string:
			out[i] = s.r.String(tv)
		case error:
			out[i] = s.redactErr(tv)
		default:
			out[i] = v
		}
	}
	return out
}

// redactErr returns an error whose message is redacted.
func (s *sink) redactErr(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: s.r.String(err.Error())}
}

type redactedError struct{ msg string }

func (e *redactedError) Error() string { return e.msg }
