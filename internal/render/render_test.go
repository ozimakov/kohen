package render

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree writes files described by rel-slash-path -> content into root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestRenderFlatAndNested(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.yaml":       "server: on\n",
		"logging.conf":   "level=info\n",
		"sub/nested.txt": "hello",
		"sub/deep/x.txt": "deep",
		"empty.txt":      "",
	})

	res, err := New(Options{}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := map[string]string{
		"app.yaml":         "server: on\n",
		"logging.conf":     "level=info\n",
		"sub__nested.txt":  "hello",
		"sub__deep__x.txt": "deep",
		"empty.txt":        "",
	}
	if !reflect.DeepEqual(res.Data, want) {
		t.Fatalf("Data mismatch:\n got %#v\nwant %#v", res.Data, want)
	}
	if len(res.BinaryData) != 0 {
		t.Fatalf("expected no binaryData, got %#v", res.BinaryData)
	}
}

func TestRenderBinaryData(t *testing.T) {
	root := t.TempDir()
	// Invalid UTF-8 goes to binaryData.
	bin := []byte{0x00, 0xff, 0xfe, 0x01}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{"ok.txt": "text"})

	res, err := New(Options{}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, ok := res.Data["ok.txt"]; !ok {
		t.Fatalf("expected ok.txt in data")
	}
	got, ok := res.BinaryData["blob.bin"]
	if !ok {
		t.Fatalf("expected blob.bin in binaryData; data=%v", res.Data)
	}
	if !bytes.Equal(got, bin) {
		t.Fatalf("binary content mismatch: got %v want %v", got, bin)
	}
}

func TestRenderExclusions(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app.yaml":             "a: 1\n",
		"external-secret.yaml": "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata:\n  name: db\n",
		"kohen.yaml":           "internal: true\n",
		"kohen.state":          "x",
		"nested/es.yml":        "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n",
	})

	res, err := New(Options{}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := res.Keys(); !reflect.DeepEqual(got, []string{"app.yaml"}) {
		t.Fatalf("expected only app.yaml, got %v", got)
	}
}

func TestRenderCustomSeparator(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a/b.txt": "x"})

	res, err := New(Options{KeySeparator: "."}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, ok := res.Data["a.b.txt"]; !ok {
		t.Fatalf("expected key a.b.txt, got %v", res.Keys())
	}
}

func TestRenderKeyCollision(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/b.txt":  "one",
		"a__b.txt": "two",
	})

	_, err := New(Options{}).Render(root)
	assertReason(t, err, ReasonKeyConflict)
}

func TestRenderInvalidKey(t *testing.T) {
	root := t.TempDir()
	// A space is not a valid ConfigMap key character.
	if err := os.WriteFile(filepath.Join(root, "bad key.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(Options{}).Render(root)
	assertReason(t, err, ReasonInvalidKey)
}

func TestRenderSymlinkEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	writeTree(t, root, map[string]string{"ok.txt": "fine"})
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := New(Options{}).Render(root)
	assertReason(t, err, ReasonTreeSafetyViolation)
}

func TestRenderSymlinkWithinAllowed(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real.txt": "content"})
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	res, err := New(Options{}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Data["link.txt"] != "content" || res.Data["real.txt"] != "content" {
		t.Fatalf("expected both real.txt and link.txt = content, got %v", res.Data)
	}
}

func TestRenderSymlinkDirWithinSkipped(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"realdir/a.txt": "x"})
	// A symlink to an in-tree directory must not duplicate keys or error.
	if err := os.Symlink(filepath.Join(root, "realdir"), filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	res, err := New(Options{}).Render(root)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := res.Keys(); !reflect.DeepEqual(got, []string{"realdir__a.txt"}) {
		t.Fatalf("expected only realdir__a.txt, got %v", got)
	}
}

func TestRenderOversizePreciseLimit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"big.txt": strings.Repeat("x", 500)})

	// Force a tiny effective limit so the precise (post-read) check fires.
	_, err := New(Options{MaxTotalBytes: 200}).Render(root)
	assertReason(t, err, ReasonOversize)
}

func TestRenderOversizeHardLimit(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", ConfigMapHardLimitBytes+1024)
	writeTree(t, root, map[string]string{"big.txt": big})

	_, err := New(Options{}).Render(root)
	assertReason(t, err, ReasonOversize)
}

func TestRenderDeterministic(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt":     "1",
		"b/c.txt":   "2",
		"b/d/e.txt": "3",
	})
	r := New(Options{})
	first, err := r.Render(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Render(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("non-deterministic render:\n%#v\n%#v", first, second)
	}
}

func TestRenderRejectsNonDir(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{}).Render(f); err == nil {
		t.Fatal("expected error rendering a non-directory path")
	}
}

func assertReason(t *testing.T, err error, want Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with reason %q, got nil", want)
	}
	got, ok := ReasonOf(err)
	if !ok {
		t.Fatalf("error %v is not a *render.Error", err)
	}
	if got != want {
		t.Fatalf("reason = %q, want %q (err: %v)", got, want, err)
	}
}
