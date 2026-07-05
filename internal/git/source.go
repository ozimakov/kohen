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
	"time"

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
	// AllowLoopback permits source hosts that resolve to loopback addresses.
	// Off by default (loopback is an SSRF vector); intended only for tests that
	// run a git server on 127.0.0.1.
	AllowLoopback bool
	// Resolver enables DNS-based SSRF guarding of hostnames. When nil, only
	// IP-literal hosts are guarded.
	Resolver Resolver
	// CacheTTL enables the repo+commit fetch cache (T10) with the given idle
	// TTL. Zero disables caching (every Fetch clones into a fresh temp dir).
	CacheTTL time.Duration
	// CacheMaxEntries bounds the number of cached worktrees; zero uses the
	// default.
	CacheMaxEntries int
}

// Client is the default Source implementation.
type Client struct {
	allowList           []string
	allowLocalTransport bool
	allowLoopback       bool
	resolver            Resolver
	cache               *fetchCache
}

// NewClient returns a Client configured with opts. It also installs the
// process-wide safe HTTPS transport so redirects are guarded (R-AUTH.7).
func NewClient(opts Options) *Client {
	installSafeHTTPTransport(opts.Resolver)
	c := &Client{
		allowList:           opts.AllowList,
		allowLocalTransport: opts.AllowLocalTransport,
		allowLoopback:       opts.AllowLoopback,
		resolver:            opts.Resolver,
	}
	if opts.CacheTTL > 0 {
		c.cache = newFetchCache(opts.CacheTTL, opts.CacheMaxEntries)
	}
	return c
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

	rr, err := c.resolveRef(ctx, p, auth, cred.tlsInsecure(), ref.Ref)
	if err != nil {
		return nil, err
	}

	worktreeDir, commit, cleanup, err := c.obtainWorktree(ctx, p, auth, cred.tlsInsecure(), ref.Ref, rr)
	if err != nil {
		return nil, err
	}

	subDir := filepath.Join(worktreeDir, filepath.FromSlash(subPath))
	if !within(worktreeDir, subDir) {
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
	if err := verifyContained(worktreeDir, subDir); err != nil {
		_ = cleanup()
		return nil, err
	}

	return &Result{Commit: commit, Dir: subDir, WorktreeDir: worktreeDir, Cleanup: cleanup}, nil
}

// obtainWorktree returns a checkout for the resolved ref, reusing a cached
// worktree keyed by repo+commit when caching is enabled and the commit is known
// ahead of cloning (T10). It returns the worktree dir, the resolved commit, and
// a cleanup that either releases the cache reference or removes the temp dir.
func (c *Client) obtainWorktree(ctx context.Context, p parsedURL, auth transport.AuthMethod, insecureTLS bool, ref string, rr resolvedRef) (string, string, func() error, error) {
	if c.cache != nil && rr.hash != "" && p.kind != transportLocal {
		key := p.raw + "@" + rr.hash
		entry, err := c.cache.acquire(ctx, key, func(dir string) (string, error) {
			return c.checkout(ctx, dir, p, auth, insecureTLS, ref, rr)
		})
		if err != nil {
			return "", "", nil, err
		}
		return entry.dir, entry.commit, func() error { c.cache.release(entry); return nil }, nil
	}

	dir, err := os.MkdirTemp("", "kohen-git-*")
	if err != nil {
		return "", "", nil, wrapError(ReasonFetchFailed, err, "creating work directory")
	}
	commit, err := c.checkout(ctx, dir, p, auth, insecureTLS, ref, rr)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	return dir, commit, func() error { return os.RemoveAll(dir) }, nil
}

// resolvedRef is the outcome of resolving a Reference's ref against the remote.
type resolvedRef struct {
	name     plumbing.ReferenceName // branch/tag ref to clone; empty for a commit
	isCommit bool
	// hash is the concrete commit SHA when known ahead of cloning (branch/tag
	// from the ref listing, or a full 40-char commit ref). Empty for a short
	// commit SHA, which can only be expanded by cloning. Used as the fetch
	// dedup key (T10).
	hash string
}

// resolveRef lists remote refs and classifies ref as a branch, a tag, or a
// commit, capturing the concrete commit SHA when it is known cheaply.
func (c *Client) resolveRef(ctx context.Context, p parsedURL, auth transport.AuthMethod, insecureTLS bool, ref string) (resolvedRef, error) {
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{p.raw},
	})
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: auth, InsecureSkipTLS: insecureTLS})
	if err != nil {
		return resolvedRef{}, classifyTransportError(err, "listing remote refs")
	}

	branch := plumbing.NewBranchReferenceName(ref)
	tag := plumbing.NewTagReferenceName(ref)
	var branchHash, tagHash string
	for _, r := range refs {
		switch r.Name() {
		case branch:
			branchHash = r.Hash().String()
		case tag:
			tagHash = r.Hash().String()
		}
	}
	if branchHash != "" {
		return resolvedRef{name: branch, hash: branchHash}, nil
	}
	if tagHash != "" {
		// Annotated tags point at a tag object; the concrete commit is resolved
		// after checkout, so only use the hash as a dedup key for lightweight
		// tags (verified post-clone). Keep it simple: treat tag hash as the key.
		return resolvedRef{name: tag, hash: tagHash}, nil
	}
	if looksLikeHash(ref) {
		full := ""
		if len(ref) == 40 {
			full = strings.ToLower(ref)
		}
		return resolvedRef{isCommit: true, hash: full}, nil
	}
	return resolvedRef{}, newError(ReasonFetchFailed, "ref "+ref+" was not found (no matching branch, tag, or commit)")
}

// checkout clones dir at the resolved ref and returns the concrete commit SHA.
// Branch/tag clones are shallow (SPEC T2); commit refs require a full clone plus
// a checkout, since not all servers support shallow fetch-by-commit.
func (c *Client) checkout(ctx context.Context, dir string, p parsedURL, auth transport.AuthMethod, insecureTLS bool, ref string, rr resolvedRef) (string, error) {
	if rr.isCommit {
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
		ReferenceName:   rr.name,
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
