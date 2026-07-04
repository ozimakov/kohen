// Package render turns a file tree (a resolved config path from a git repo)
// into deterministic ConfigMap data, per SPEC §7.4 and PLAN step S1.2.
//
// Guarantees (SPEC T9): the same input tree always produces byte-identical
// output. The renderer is verbatim — it performs no templating or overlay
// merging (deferred, §19).
//
// Safety (SPEC R7.5–R7.7): the renderer rejects trees that attempt path
// traversal or symlink escapes, excludes recognized secret manifests and
// reserved kohen.* files from ConfigMap keys, and fails closed when the
// rendered object would exceed the ConfigMap size limit.
package render

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ozimakov/kohen/internal/manifest"
)

// DefaultKeySeparator replaces the path separator in nested file paths, since
// ConfigMap keys may not contain "/" (SPEC §18 Q1, resolved here). For example
// the file "app/logging.conf" becomes the key "app__logging.conf".
const DefaultKeySeparator = "__"

// ConfigMapHardLimitBytes is the Kubernetes ~1 MiB total object size ceiling for
// a ConfigMap (data + binaryData + metadata). See SPEC T-LIMIT / R7.7.
const ConfigMapHardLimitBytes = 1 << 20 // 1,048,576

// DefaultSafetyMarginBytes is reserved for object metadata (name, labels,
// managedFields from Server-Side Apply, etc.) that also counts toward the
// object size but is not visible to the renderer. Rendered data + binaryData is
// checked against ConfigMapHardLimitBytes minus this margin.
const DefaultSafetyMarginBytes = 128 * 1024 // 128 KiB

// reservedPrefix marks files reserved for Kohen's own use (SPEC R7.6). Files
// whose base name starts with this prefix are never ConfigMap keys.
const reservedPrefix = "kohen."

// Reason identifies why rendering failed, matching the Rendered condition's
// reasons in SPEC §11.4 so the reconciler (S1.8) can surface it directly.
type Reason string

const (
	// ReasonTreeSafetyViolation indicates a path-traversal or symlink escape.
	ReasonTreeSafetyViolation Reason = "TreeSafetyViolation"
	// ReasonOversize indicates the rendered object exceeds the size limit.
	ReasonOversize Reason = "Oversize"
	// ReasonInvalidKey indicates a file maps to a key Kubernetes would reject.
	ReasonInvalidKey Reason = "InvalidKey"
	// ReasonKeyConflict indicates two files map to the same ConfigMap key.
	ReasonKeyConflict Reason = "KeyConflict"
)

// Error is a rendering failure that carries the SPEC condition reason.
type Error struct {
	Reason Reason
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func newError(reason Reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Msg: fmt.Sprintf(format, args...)}
}

// ReasonOf extracts the render Reason from err, if any.
func ReasonOf(err error) (Reason, bool) {
	var re *Error
	if errors.As(err, &re) {
		return re.Reason, true
	}
	return "", false
}

// Result is the rendered ConfigMap payload.
type Result struct {
	// Data holds UTF-8 file contents (ConfigMap.data).
	Data map[string]string
	// BinaryData holds non-UTF-8 file contents (ConfigMap.binaryData).
	BinaryData map[string][]byte
	// TotalBytes is the conservative serialized size used for the size check.
	TotalBytes int
}

// Keys returns every ConfigMap key (data and binaryData) in sorted order.
func (r *Result) Keys() []string {
	keys := make([]string, 0, len(r.Data)+len(r.BinaryData))
	for k := range r.Data {
		keys = append(keys, k)
	}
	for k := range r.BinaryData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Options configures a Renderer. The zero value is valid and uses defaults.
type Options struct {
	// KeySeparator replaces "/" in nested paths. Defaults to DefaultKeySeparator.
	KeySeparator string
	// MaxTotalBytes is the ConfigMap hard limit. Defaults to
	// ConfigMapHardLimitBytes.
	MaxTotalBytes int
	// SafetyMarginBytes is subtracted from MaxTotalBytes to leave room for
	// object metadata. Defaults to DefaultSafetyMarginBytes.
	SafetyMarginBytes int
}

func (o Options) keySeparator() string {
	if o.KeySeparator == "" {
		return DefaultKeySeparator
	}
	return o.KeySeparator
}

func (o Options) effectiveLimit() int {
	max := o.MaxTotalBytes
	if max <= 0 {
		max = ConfigMapHardLimitBytes
	}
	margin := o.SafetyMarginBytes
	if margin == 0 {
		margin = DefaultSafetyMarginBytes
	}
	if margin >= max {
		return max
	}
	return max - margin
}

// Interface is the rendering contract consumed by the reconciler (S1.8) and any
// other step that needs to turn a file tree into ConfigMap data.
type Interface interface {
	// Render walks the directory tree rooted at root and returns the rendered
	// ConfigMap payload, or a *render.Error on a safety/size/key failure.
	Render(root string) (*Result, error)
}

// Renderer is the default Interface implementation.
type Renderer struct {
	opts Options
}

// New returns a Renderer configured with opts.
func New(opts Options) *Renderer { return &Renderer{opts: opts} }

// candidate is a file selected for inclusion, discovered during the walk.
type candidate struct {
	key      string // ConfigMap key
	realPath string // absolute path to read content from
	size     int64  // stat size, for the fast oversize pre-check
}

// Render implements Interface.
func (r *Renderer) Render(root string) (*Result, error) {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolving config path %q: %w", root, err)
	}
	info, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("stat config path %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("config path %q is not a directory", root)
	}

	candidates, err := r.collect(base)
	if err != nil {
		return nil, err
	}

	// Fast fail: if raw file bytes already blow past the hard limit, do not read
	// everything into memory just to reject it.
	var rawTotal int64
	for _, c := range candidates {
		rawTotal += c.size
	}
	if rawTotal > int64(ConfigMapHardLimitBytes) {
		return nil, newError(ReasonOversize,
			"rendered config is %d bytes which exceeds the ConfigMap limit of %d bytes; reduce the config or split the path",
			rawTotal, ConfigMapHardLimitBytes)
	}

	result := &Result{
		Data:       map[string]string{},
		BinaryData: map[string][]byte{},
	}
	total := 0
	for _, c := range candidates {
		content, err := os.ReadFile(c.realPath)
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", c.realPath, err)
		}
		if utf8.Valid(content) {
			result.Data[c.key] = string(content)
			total += len(c.key) + len(content)
		} else {
			result.BinaryData[c.key] = content
			total += len(c.key) + base64.StdEncoding.EncodedLen(len(content))
		}
	}
	result.TotalBytes = total

	if limit := r.opts.effectiveLimit(); total > limit {
		return nil, newError(ReasonOversize,
			"rendered config is %d bytes which exceeds the safe ConfigMap limit of %d bytes (hard limit %d, safety margin for metadata); reduce the config or split the path",
			total, limit, ConfigMapHardLimitBytes)
	}
	return result, nil
}

// collect walks base and returns the included files, enforcing tree safety,
// exclusions, key validity, and collision detection. Walk order is lexical, so
// collection is deterministic.
func (r *Renderer) collect(base string) ([]candidate, error) {
	sep := r.opts.keySeparator()
	seen := map[string]string{} // key -> rel path, for collision detection
	var out []candidate

	walkErr := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}

		typ := d.Type()

		// Symlinks: allowed only if they resolve within base (SPEC R7.5). A
		// symlink to a directory is skipped (its real path, if inside base, is
		// walked separately) to avoid cycles and duplicate keys.
		if typ&fs.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(path)
			if rerr != nil {
				return newError(ReasonTreeSafetyViolation,
					"symlink %q could not be resolved: %v", relTo(base, path), rerr)
			}
			if !within(base, resolved) {
				return newError(ReasonTreeSafetyViolation,
					"symlink %q escapes the config path", relTo(base, path))
			}
			ti, terr := os.Stat(resolved)
			if terr != nil {
				return fmt.Errorf("stat symlink target %q: %w", resolved, terr)
			}
			if ti.IsDir() {
				return nil
			}
			return r.consider(base, path, sep, seen, &out, ti.Size())
		}

		if d.IsDir() {
			return nil
		}
		if !typ.IsRegular() {
			// Skip devices, sockets, pipes, etc. — not config content.
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		return r.consider(base, path, sep, seen, &out, info.Size())
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// consider evaluates a single regular file (possibly reached via an in-tree
// symlink) for inclusion.
func (r *Renderer) consider(base, path, sep string, seen map[string]string, out *[]candidate, size int64) error {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("computing relative path for %q: %w", path, err)
	}
	rel = filepath.ToSlash(rel)

	// Defense in depth against traversal even though WalkDir stays within base.
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return newError(ReasonTreeSafetyViolation, "path %q escapes the config path", rel)
	}

	if excluded(path, rel) {
		return nil
	}

	key := strings.ReplaceAll(rel, "/", sep)
	if err := validateKey(key); err != nil {
		return newError(ReasonInvalidKey,
			"file %q maps to invalid ConfigMap key %q: %v", rel, key, err)
	}
	if prev, ok := seen[key]; ok {
		return newError(ReasonKeyConflict,
			"files %q and %q both map to ConfigMap key %q; rename one or change the key separator", prev, rel, key)
	}
	seen[key] = rel
	*out = append(*out, candidate{key: key, realPath: path, size: size})
	return nil
}

// excluded reports whether a file must be kept out of the ConfigMap (SPEC R7.6):
// reserved kohen.* files and recognized ExternalSecret manifests.
func excluded(path, rel string) bool {
	name := filepath.Base(rel)
	if strings.HasPrefix(name, reservedPrefix) {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml", ".json":
		data, err := os.ReadFile(path)
		if err != nil {
			// Unreadable here; let the later read surface the error rather than
			// silently excluding real content.
			return false
		}
		return manifest.IsExternalSecret(data)
	}
	return false
}

// maxKeyLen is the Kubernetes limit for a ConfigMap key (a qualified name).
const maxKeyLen = 253

// validateKey enforces the Kubernetes ConfigMap key format:
// non-empty, at most 253 chars, characters in [-._a-zA-Z0-9], and not "." or "..".
func validateKey(key string) error {
	if key == "" {
		return errors.New("key is empty")
	}
	if len(key) > maxKeyLen {
		return fmt.Errorf("key length %d exceeds %d", len(key), maxKeyLen)
	}
	if key == "." || key == ".." {
		return errors.New(`key must not be "." or ".."`)
	}
	for _, ch := range key {
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.':
		default:
			return fmt.Errorf("invalid character %q", ch)
		}
	}
	return nil
}

// within reports whether target is base itself or lies beneath it. Both are
// expected to be symlink-resolved absolute paths.
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

// relTo returns a slash path of target relative to base for error messages,
// falling back to target on failure.
func relTo(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}
