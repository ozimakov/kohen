// Package git fetches and resolves configuration content from a git repository
// deterministically and securely (PLAN step S1.1, SPEC §7).
//
// A Source resolves a branch/tag/commit ref to a concrete commit SHA, fetches
// the tree, and exposes the requested subpath on the local filesystem together
// with the resolved commit. Security guards (scheme allow-list, SSRF IP
// blocking, operator source allow-list, TLS/host-key verification on by
// default) are enforced before any network access — see url.go and auth.go.
package git

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
)

// DefaultRef is the ref used when a Reference leaves it empty (SPEC §11.2).
const DefaultRef = "main"

// Reference identifies what to fetch: a repository URL, a ref (branch, tag, or
// commit SHA), and a subpath within the tree.
type Reference struct {
	// URL is the repository URL (https:// or ssh://, or scp-like SSH).
	URL string
	// Ref is a branch, tag, or commit SHA. Empty defaults to DefaultRef.
	Ref string
	// Path is the subpath within the repo whose files are returned. Empty means
	// the repository root.
	Path string
}

func (r Reference) withDefaults() Reference {
	if strings.TrimSpace(r.Ref) == "" {
		r.Ref = DefaultRef
	}
	return r
}

// Result is a successfully fetched tree.
type Result struct {
	// Commit is the resolved commit SHA (plain git SHA, SPEC §11.1).
	Commit string
	// Dir is the absolute path to the requested subpath on disk.
	Dir string
	// WorktreeDir is the absolute path to the checkout root.
	WorktreeDir string
	// Cleanup removes the checkout. Callers MUST invoke it when done.
	Cleanup func() error
}

// Source fetches configuration content from git.
type Source interface {
	// Fetch resolves ref within the repository and returns the requested
	// subpath plus the resolved commit. cred may be nil for anonymous access.
	Fetch(ctx context.Context, ref Reference, cred *Credential) (*Result, error)
}

// Options configures a Client.
type Options struct {
	// AllowList restricts which source URLs may be fetched (R-AUTH.3). Empty
	// permits any host; operators are advised to set one. Entries are hosts
	// (matching subdomains) or URL prefixes (containing "/").
	AllowList []string
	// AllowLocalTransport permits filesystem/file:// sources. Off by default;
	// intended for test fixtures and never for production.
	AllowLocalTransport bool
	// Resolver enables DNS-based SSRF guarding of hostnames. When nil, only
	// IP-literal hosts are guarded.
	Resolver Resolver
}

// Client is the default Source implementation.
type Client struct {
	allowList           []string
	allowLocalTransport bool
	resolver            Resolver
}

// NewClient returns a Client configured with opts.
func NewClient(opts Options) *Client {
	return &Client{
		allowList:           opts.AllowList,
		allowLocalTransport: opts.AllowLocalTransport,
		resolver:            opts.Resolver,
	}
}

var _ Source = (*Client)(nil)

// Fetch implements Source.
func (c *Client) Fetch(ctx context.Context, ref Reference, cred *Credential) (*Result, error) {
	ref = ref.withDefaults()

	p, err := parseURL(ref.URL, c.allowLocalTransport)
	if err != nil {
		return nil, err
	}
	if err := c.validate(ctx, p); err != nil {
		return nil, err
	}
	subPath, err := sanitizePath(ref.Path)
	if err != nil {
		return nil, err
	}

	auth, cleanupAuth, err := cred.authMethod(p.kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cleanupAuth() }()

	refName, isCommit, err := c.resolveRef(ctx, p, auth, cred.tlsInsecure(), ref.Ref)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "kohen-git-*")
	if err != nil {
		return nil, wrapError(ReasonFetchFailed, err, "creating work directory")
	}
	cleanup := func() error { return os.RemoveAll(dir) }

	commit, err := c.checkout(ctx, dir, p, auth, cred.tlsInsecure(), ref.Ref, refName, isCommit)
	if err != nil {
		_ = cleanup()
		return nil, err
	}

	subDir := filepath.Join(dir, filepath.FromSlash(subPath))
	if !within(dir, subDir) {
		_ = cleanup()
		return nil, newError(ReasonPathNotFound, "resolved subpath escapes the repository")
	}
	if _, statErr := os.Stat(subDir); statErr != nil {
		_ = cleanup()
		if os.IsNotExist(statErr) {
			return nil, newError(ReasonPathNotFound,
				"path "+subPath+" was not found at commit "+commit)
		}
		return nil, wrapError(ReasonFetchFailed, statErr, "accessing subpath %q", subPath)
	}

	// Defense in depth (SPEC R7.5 / TM6): ensure the subpath does not escape the
	// worktree through a symlinked directory component committed in the repo.
	// go-git's checkout tends to neutralize escaping symlinks, but we make the
	// containment guarantee our own rather than relying on library internals.
	if err := verifyContained(dir, subDir); err != nil {
		_ = cleanup()
		return nil, err
	}

	return &Result{Commit: commit, Dir: subDir, WorktreeDir: dir, Cleanup: cleanup}, nil
}

// resolveRef lists remote refs and classifies ref as a branch, a tag, or a
// commit. It returns the reference name to clone (empty for a commit) and
// whether the ref is a commit.
func (c *Client) resolveRef(ctx context.Context, p parsedURL, auth transport.AuthMethod, insecureTLS bool, ref string) (plumbing.ReferenceName, bool, error) {
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{p.raw},
	})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: auth, InsecureSkipTLS: insecureTLS})
	if err != nil {
		return "", false, classifyTransportError(err, "listing remote refs")
	}

	branch := plumbing.NewBranchReferenceName(ref)
	tag := plumbing.NewTagReferenceName(ref)
	haveBranch, haveTag := false, false
	for _, r := range refs {
		switch r.Name() {
		case branch:
			haveBranch = true
		case tag:
			haveTag = true
		}
	}
	if haveBranch {
		return branch, false, nil
	}
	if haveTag {
		return tag, false, nil
	}
	if looksLikeHash(ref) {
		return "", true, nil
	}
	return "", false, newError(ReasonFetchFailed, "ref "+ref+" was not found (no matching branch, tag, or commit)")
}

// checkout clones dir at the resolved ref and returns the concrete commit SHA.
// Branch/tag clones are shallow (SPEC T2); commit refs require a full clone plus
// a checkout, since not all servers support shallow fetch-by-commit.
func (c *Client) checkout(ctx context.Context, dir string, p parsedURL, auth transport.AuthMethod, insecureTLS bool, ref string, refName plumbing.ReferenceName, isCommit bool) (string, error) {
	if isCommit {
		repo, err := gogit.PlainCloneContext(ctx, dir, false, &gogit.CloneOptions{
			URL:             p.raw,
			Auth:            auth,
			InsecureSkipTLS: insecureTLS,
			Tags:            gogit.AllTags,
		})
		if err != nil {
			return "", classifyTransportError(err, "cloning repository")
		}
		hash, err := repo.ResolveRevision(plumbing.Revision(ref))
		if err != nil {
			return "", wrapError(ReasonFetchFailed, err, "resolving commit %q", ref)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return "", wrapError(ReasonFetchFailed, err, "opening worktree")
		}
		if err := wt.Checkout(&gogit.CheckoutOptions{Hash: *hash}); err != nil {
			return "", wrapError(ReasonFetchFailed, err, "checking out commit %q", ref)
		}
		return hash.String(), nil
	}

	opts := &gogit.CloneOptions{
		URL:             p.raw,
		Auth:            auth,
		ReferenceName:   refName,
		SingleBranch:    true,
		InsecureSkipTLS: insecureTLS,
	}
	// Shallow fetch is an optimization that the local transport does not
	// support; only enable it for real remotes.
	if p.kind != transportLocal {
		opts.Depth = 1
	}
	repo, err := gogit.PlainCloneContext(ctx, dir, false, opts)
	if err != nil {
		return "", classifyTransportError(err, "cloning repository")
	}
	head, err := repo.Head()
	if err != nil {
		return "", wrapError(ReasonFetchFailed, err, "resolving HEAD")
	}
	return head.Hash().String(), nil
}

// classifyTransportError maps go-git transport errors to the SPEC condition
// reasons, distinguishing authentication failures from generic fetch failures.
func classifyTransportError(err error, action string) *Error {
	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) {
		return wrapError(ReasonAuthFailed, err, "%s", action)
	}
	return wrapError(ReasonFetchFailed, err, "%s", action)
}

// looksLikeHash reports whether s is a plausible git object hash (7-40 hex).
func looksLikeHash(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, ch := range s {
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f', ch >= 'A' && ch <= 'F':
		default:
			return false
		}
	}
	return true
}

// sanitizePath normalizes a repo subpath and rejects traversal outside the
// repository root (SPEC R7.5). The repo root is represented as ".".
func sanitizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." || p == "/" {
		return ".", nil
	}
	clean := path.Clean("/" + filepath.ToSlash(p))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		return ".", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", newError(ReasonPathNotFound, "path "+p+" escapes the repository")
	}
	return clean, nil
}

// verifyContained resolves all symlinks in both paths and confirms the subpath
// still lies within the worktree. It fails closed on any resolution error.
func verifyContained(worktree, subDir string) error {
	resolvedRoot, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return wrapError(ReasonFetchFailed, err, "resolving worktree")
	}
	resolvedSub, err := filepath.EvalSymlinks(subDir)
	if err != nil {
		return wrapError(ReasonPathNotFound, err, "resolving subpath")
	}
	if !within(resolvedRoot, resolvedSub) {
		return newError(ReasonPathNotFound,
			"subpath resolves outside the repository (symlink escape rejected)")
	}
	return nil
}

// within reports whether target is base or lies beneath it.
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
