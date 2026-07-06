// Package native resolves backend=nativeSecret references (SPEC §8.3, PLAN
// S2.3): a Secret created out-of-band (cert-manager, kubectl, another
// controller) that Kohen only awaits and wires — never produces or mutates
// (P2).
//
// The resolver reads the backing Secret's metadata and key set only; it never
// returns, logs, or hashes secret values (R8.3). The rotation version token is
// derived purely from metadata — the Secret's resourceVersion plus its observed
// key set (R8.10) — so a rotation (new resourceVersion) advances the token
// without ever touching the material.
package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/secret"
)

// tokenLen bounds the hex version token; metadata-derived, never a value hash.
const tokenLen = 16

// Resolver resolves native Secrets via a (typically uncached) reader.
type Resolver struct {
	reader client.Reader
}

// New returns a Resolver backed by reader. Callers SHOULD pass an uncached
// reader (e.g. manager.GetAPIReader) so arbitrary referenced Secrets can be
// read on demand without caching all Secret material in the operator (TM8, T6).
func New(reader client.Reader) *Resolver {
	return &Resolver{reader: reader}
}

// Resolve implements secret.Resolver. A missing Secret or a missing required
// key are normal not-ready states (Reason set, nil error); only unexpected API
// errors are returned as errors so the caller can treat them as transient
// backend outages (R8.9).
func (r *Resolver) Resolve(ctx context.Context, namespace string, ref secret.Ref) (secret.Resolution, error) {
	var s corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: ref.SecretName}
	if err := r.reader.Get(ctx, key, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return secret.Resolution{
				Reason:  kohenv1alpha1.ReasonSecretNotFound,
				Message: fmt.Sprintf("native secret %q not found", ref.SecretName),
			}, nil
		}
		// Transient/unexpected: surface as an error so the caller fails safe.
		return secret.Resolution{}, err
	}

	keys := secretKeys(&s)
	if missing := missingKeys(keys, ref.RequiredKeys); len(missing) > 0 {
		return secret.Resolution{
			Reason: kohenv1alpha1.ReasonKeyMissing,
			Message: fmt.Sprintf("native secret %q missing required key(s): %s",
				ref.SecretName, strings.Join(missing, ", ")),
		}, nil
	}

	return secret.Resolution{
		Ready:        true,
		SecretName:   ref.SecretName,
		VersionToken: versionToken(s.ResourceVersion, keys),
	}, nil
}

// secretKeys returns the sorted set of data keys present in the Secret,
// covering both Data and StringData (the latter appears only pre-persistence,
// defensively included).
func secretKeys(s *corev1.Secret) []string {
	set := make(map[string]struct{}, len(s.Data)+len(s.StringData))
	for k := range s.Data {
		set[k] = struct{}{}
	}
	for k := range s.StringData {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// missingKeys returns the required keys absent from the present set.
func missingKeys(present, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(present))
	for _, k := range present {
		have[k] = struct{}{}
	}
	var missing []string
	for _, k := range required {
		if _, ok := have[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// versionToken derives a stable, metadata-only rotation token (R8.10) from the
// Secret's resourceVersion and its observed key set. It intentionally does NOT
// read any value: a rotation bumps resourceVersion, which changes the token.
func versionToken(resourceVersion string, keys []string) string {
	h := sha256.New()
	h.Write([]byte(resourceVersion))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(keys, ",")))
	return hex.EncodeToString(h.Sum(nil))[:tokenLen]
}
