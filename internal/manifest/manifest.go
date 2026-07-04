// Package manifest provides lightweight recognition of Kubernetes manifests
// that Kohen treats specially when reading a git config tree.
//
// In v1 the only recognized apply-if-present manifest is the External Secrets
// Operator ExternalSecret (SPEC R7.6, §8.2). Recognition is intentionally
// minimal: it inspects only the apiVersion/kind of each YAML document and never
// interprets the object's contents. The renderer (S1.2) uses this to exclude
// such files from ConfigMap keys; the apply engine (S2.4) will reuse it to
// decide which files to apply.
package manifest

import (
	"bytes"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// ExternalSecretsGroup is the API group of the External Secrets Operator.
const ExternalSecretsGroup = "external-secrets.io"

// ExternalSecretKind is the kind of an ExternalSecret resource.
const ExternalSecretKind = "ExternalSecret"

// typeMeta mirrors the two fields we need to classify a manifest. Unknown YAML
// fields are ignored by the decoder, so arbitrary config documents decode into
// a zero value without error.
type typeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// IsExternalSecret reports whether data contains at least one YAML document that
// declares an External Secrets Operator ExternalSecret. Multi-document YAML
// (separated by `---`) is supported; a match in any document classifies the
// whole file. Content that is not valid YAML (e.g. binary blobs or `.properties`
// files) is treated as a non-match.
func IsExternalSecret(data []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var tm typeMeta
		if err := dec.Decode(&tm); err != nil {
			// EOF or a document that does not decode into a mapping: stop and
			// rely on any matches found so far. We never treat a parse failure
			// as a match, so unrecognized content falls through to ConfigMap.
			return false
		}
		if tm.Kind == ExternalSecretKind && groupOf(tm.APIVersion) == ExternalSecretsGroup {
			return true
		}
	}
}

// groupOf returns the API group portion of an apiVersion string
// ("external-secrets.io/v1beta1" -> "external-secrets.io"). A core-group
// apiVersion with no slash (e.g. "v1") yields the empty string.
func groupOf(apiVersion string) string {
	if i := strings.IndexByte(apiVersion, '/'); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}
