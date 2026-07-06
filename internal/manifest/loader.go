package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Object is a recognized apply-if-present manifest parsed from the config tree,
// paired with its source path (relative to the config root) for messages.
type Object struct {
	// U is the parsed object. Values are JSON-typed (numbers are float64) so it
	// is safe for Server-Side Apply.
	U *unstructured.Unstructured
	// Source is the slash path of the file the object was parsed from.
	Source string
}

// Load walks the config tree rooted at root and returns every recognized
// apply-if-present manifest (v1: ExternalSecret) it finds (SPEC §8.2, R7.6).
// Non-manifest documents are ignored (they are ConfigMap content, handled by
// the renderer). Symlinks that escape the root are rejected as a safety
// violation (R7.5), mirroring the renderer's tree contract.
//
// Multi-document files are supported: only the recognized ExternalSecret
// documents in a file are returned. Files that are not valid YAML/JSON are
// skipped (they cannot be manifests).
func Load(root string) ([]Object, error) {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving config path %q: %w", root, err)
	}
	var out []Object
	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base || d.IsDir() {
			return nil
		}
		// Only YAML/JSON files can be manifests.
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		// Reject symlinks that escape the config root (R7.5, defense in depth).
		real := path
		if d.Type()&fs.ModeSymlink != 0 {
			r, rerr := filepath.EvalSymlinks(path)
			if rerr != nil || !within(base, r) {
				return fmt.Errorf("symlink %q escapes the config path", relOrPath(base, path))
			}
			real = r
		}
		objs, perr := parseFile(real)
		if perr != nil {
			return perr
		}
		src := relOrPath(base, path)
		for _, u := range objs {
			out = append(out, Object{U: u, Source: src})
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// parseFile decodes all YAML/JSON documents in a file and returns those that
// are recognized ExternalSecret manifests. A document that fails to decode is
// treated as a non-manifest (skipped) so plain config never blocks the walk.
func parseFile(path string) ([]*unstructured.Unstructured, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()

	dec := k8syaml.NewYAMLOrJSONDecoder(f, 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if derr := dec.Decode(&m); derr != nil {
			// io.EOF (or a stream that no longer decodes) ends the file.
			break
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		gvk := u.GroupVersionKind()
		if gvk.Kind == ExternalSecretKind && gvk.Group == ExternalSecretsGroup {
			out = append(out, u)
		}
	}
	return out, nil
}

// within reports whether target is base itself or lies beneath it.
func within(base, target string) bool {
	if target == base {
		return true
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func relOrPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
