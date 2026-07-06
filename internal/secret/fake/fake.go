// Package fake provides an in-memory secret.Resolver for tests (PLAN S2.2). It
// lets tests script per-reference resolutions (ready/not-ready, tokens,
// reasons) without a real backend, so the readiness state machine and the
// reconciler's secret gating can be exercised deterministically.
package fake

import (
	"context"
	"sync"

	"github.com/ozimakov/kohen/internal/secret"
)

// Resolver is a scriptable secret.Resolver. It is safe for concurrent use.
type Resolver struct {
	mu sync.Mutex
	// results maps a backing Secret name to the Resolution to return.
	results map[string]secret.Resolution
	// err, when set, is returned from Resolve to simulate an infra failure.
	err error
}

// New returns an empty Resolver that reports every reference as SecretNotFound.
func New() *Resolver {
	return &Resolver{results: map[string]secret.Resolution{}}
}

// Set scripts the Resolution returned for a backing Secret name.
func (r *Resolver) Set(secretName string, res secret.Resolution) *Resolver {
	r.mu.Lock()
	defer r.mu.Unlock()
	res.SecretName = secretName
	r.results[secretName] = res
	return r
}

// SetReady is a convenience for a ready resolution with a version token.
func (r *Resolver) SetReady(secretName, token string) *Resolver {
	return r.Set(secretName, secret.Resolution{Ready: true, VersionToken: token})
}

// SetError makes Resolve return err (simulating a transient infra failure).
func (r *Resolver) SetError(err error) *Resolver {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
	return r
}

// Resolve implements secret.Resolver.
func (r *Resolver) Resolve(_ context.Context, _ string, ref secret.Ref) (secret.Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return secret.Resolution{}, r.err
	}
	if res, ok := r.results[ref.SecretName]; ok {
		return res, nil
	}
	return secret.Resolution{
		Ready:   false,
		Reason:  "SecretNotFound",
		Message: "secret " + ref.SecretName + " not found",
	}, nil
}
