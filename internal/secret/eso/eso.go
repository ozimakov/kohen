// Package eso resolves backend=externalSecret references against the External
// Secrets Operator (SPEC §8.3, PLAN S2.4). ESO is the authority: it syncs the
// backing store (Vault/AWS/GCP/…) into a Kubernetes Secret via an
// ExternalSecret. Kohen only awaits readiness and wires the resulting Secret —
// it never reads or produces the material (P2).
//
// Readiness gates on the ExternalSecret's Ready=True condition (R8.9). The
// rotation version token is metadata-only (R8.10): the ExternalSecret's
// status.syncedResourceVersion when present, else the target Secret's
// resourceVersion plus its key set. No secret value is ever read (R8.3).
package eso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kohenv1alpha1 "github.com/ozimakov/kohen/api/v1alpha1"
	"github.com/ozimakov/kohen/internal/manifest"
	"github.com/ozimakov/kohen/internal/secret"
)

const tokenLen = 16

// candidateVersions are the ExternalSecret API versions tried, in order, so the
// resolver works whether a cluster serves ESO v1, v1beta1, or both.
var candidateVersions = []string{"v1", "v1beta1"}

// Resolver resolves ESO ExternalSecret references via a (typically uncached)
// reader.
type Resolver struct {
	reader client.Reader
}

// New returns a Resolver backed by reader (pass an uncached reader so
// referenced objects need not be cached — TM8/T6).
func New(reader client.Reader) *Resolver {
	return &Resolver{reader: reader}
}

// Resolve implements secret.Resolver. An absent ExternalSecret, a not-yet-Ready
// ExternalSecret, an absent target Secret, or a missing key are normal
// not-ready states (Reason set, nil error). Only unexpected API errors return
// an error so the caller fails safe (R8.9).
func (r *Resolver) Resolve(ctx context.Context, namespace string, ref secret.Ref) (secret.Resolution, error) {
	es, served, err := r.getExternalSecret(ctx, namespace, ref.SecretName)
	if err != nil {
		return secret.Resolution{}, err
	}
	if !served {
		return secret.Resolution{
			Reason:  kohenv1alpha1.ReasonBackendNotReady,
			Message: fmt.Sprintf("External Secrets Operator CRD (%s/%s) is not served in this cluster", manifest.ExternalSecretsGroup, manifest.ExternalSecretKind),
		}, nil
	}
	if es == nil {
		return secret.Resolution{
			Reason:  kohenv1alpha1.ReasonBackendNotReady,
			Message: fmt.Sprintf("awaiting ExternalSecret %q", ref.SecretName),
		}, nil
	}
	if !esReady(es) {
		return secret.Resolution{
			Reason:  kohenv1alpha1.ReasonBackendNotReady,
			Message: fmt.Sprintf("ExternalSecret %q is not Ready", ref.SecretName),
		}, nil
	}

	// The target Secret ESO writes; defaults to the ExternalSecret's name.
	targetName := esTargetName(es, ref.SecretName)
	var s corev1.Secret
	if gerr := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: targetName}, &s); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return secret.Resolution{
				Reason:  kohenv1alpha1.ReasonSecretNotFound,
				Message: fmt.Sprintf("ExternalSecret %q Ready but target secret %q not found", ref.SecretName, targetName),
			}, nil
		}
		return secret.Resolution{}, gerr
	}

	keys := secretKeys(&s)
	if missing := missingKeys(keys, ref.RequiredKeys); len(missing) > 0 {
		return secret.Resolution{
			Reason: kohenv1alpha1.ReasonKeyMissing,
			Message: fmt.Sprintf("target secret %q missing required key(s): %s",
				targetName, strings.Join(missing, ", ")),
		}, nil
	}

	return secret.Resolution{
		Ready:        true,
		SecretName:   targetName,
		VersionToken: versionToken(es, s.ResourceVersion, keys),
	}, nil
}

// getExternalSecret fetches the ExternalSecret, trying each candidate API
// version. served reports whether any candidate version is served by the
// cluster (distinguishing "ESO not installed" from "manifest not yet present").
// It returns (nil, true, nil) when the object is absent (awaiting) and an error
// only for unexpected API failures.
func (r *Resolver) getExternalSecret(ctx context.Context, namespace, name string) (u *unstructured.Unstructured, served bool, err error) {
	for _, v := range candidateVersions {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group: manifest.ExternalSecretsGroup, Version: v, Kind: manifest.ExternalSecretKind,
		})
		gerr := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
		switch {
		case gerr == nil:
			return obj, true, nil
		case apierrors.IsNotFound(gerr):
			return nil, true, nil // version served, object absent: awaiting
		case meta.IsNoMatchError(gerr):
			continue // version not served: try the next
		default:
			return nil, true, gerr
		}
	}
	return nil, false, nil // no candidate version served: ESO not installed
}

// esReady reports whether the ExternalSecret's status has a Ready=True condition.
func esReady(u *unstructured.Unstructured) bool {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "Ready" && cm["status"] == "True" {
			return true
		}
	}
	return false
}

// esTargetName returns spec.target.name, defaulting to the ExternalSecret's own
// name (ESO's default target Secret name).
func esTargetName(u *unstructured.Unstructured, fallback string) string {
	if name, found, _ := unstructured.NestedString(u.Object, "spec", "target", "name"); found && name != "" {
		return name
	}
	return fallback
}

// versionToken derives a metadata-only rotation token (R8.10): the
// ExternalSecret's status.syncedResourceVersion when present, else falling back
// to the target Secret's resourceVersion; the key set is always folded in.
// Never derived from values. Using synced-revision-when-present avoids spurious
// version churn from benign metadata writes that bump the Secret's rv.
func versionToken(es *unstructured.Unstructured, secretRV string, keys []string) string {
	rev, _, _ := unstructured.NestedString(es.Object, "status", "syncedResourceVersion")
	if rev == "" {
		rev = secretRV
	}
	h := sha256.New()
	h.Write([]byte(rev))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(keys, ",")))
	return hex.EncodeToString(h.Sum(nil))[:tokenLen]
}

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
