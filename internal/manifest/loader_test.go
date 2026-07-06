package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ozimakov/kohen/internal/manifest"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const esDoc = `apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: db-es
spec:
  secretStoreRef:
    name: vault
    kind: SecretStore
  target:
    name: db-secret
`

func TestLoadFindsExternalSecret(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"app.yaml":            "key: value\n",
		"secrets/es.yaml":     esDoc,
		"notes.txt":           "ignore me\n",
		"config/settings.ini": "x=1\n",
	})
	objs, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("want 1 ExternalSecret, got %d: %+v", len(objs), objs)
	}
	if objs[0].U.GetName() != "db-es" || objs[0].Source != "secrets/es.yaml" {
		t.Errorf("unexpected object: name=%q source=%q", objs[0].U.GetName(), objs[0].Source)
	}
}

func TestLoadIgnoresNonManifests(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.yaml": "key: value\n",
		"b.json": `{"foo":"bar"}`,
		"c.yml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
	})
	objs, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Fatalf("expected no manifests, got %d", len(objs))
	}
}

func TestLoadMultiDocPicksExternalSecretOnly(t *testing.T) {
	multi := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n---\n" + esDoc
	dir := writeTree(t, map[string]string{"mixed.yaml": multi})
	objs, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].U.GetName() != "db-es" {
		t.Fatalf("want only the ExternalSecret doc, got %+v", objs)
	}
}

func TestLoadNumbersAreJSONTyped(t *testing.T) {
	doc := esDoc + "  refreshInterval: 3600\n"
	dir := writeTree(t, map[string]string{"es.yaml": doc})
	objs, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// SSA requires JSON types: integers decode to float64 via YAMLOrJSONDecoder.
	v, found, err := unstructured.NestedFieldNoCopy(objs[0].U.Object, "spec", "refreshInterval")
	if err != nil || !found {
		t.Fatalf("refreshInterval missing: %v", err)
	}
	if _, ok := v.(float64); !ok {
		t.Errorf("refreshInterval is %T, want float64 (JSON-typed)", v)
	}
}

func TestLoadRejectsEscapingSymlink(t *testing.T) {
	dir := writeTree(t, map[string]string{"real/es.yaml": esDoc})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte(esDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape.yaml")
	if err := os.Symlink(filepath.Join(outside, "secret.yaml"), link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := manifest.Load(dir); err == nil {
		t.Fatal("expected a safety violation for an escaping symlink")
	}
}
