package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// fixture is a seeded local git repository with two commits on branch "main"
// and a tag "v1" at the first commit.
type fixture struct {
	dir    string
	first  string // commit SHA of the tagged first commit
	second string // commit SHA of HEAD (main)
}

func sig() *object.Signature {
	return &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}
}

func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	// Use "main" as the default branch to match SPEC defaults.
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	writeFixtureFile(t, dir, "README.md", "root file")
	writeFixtureFile(t, dir, "services/app/prod/app.yaml", "version: 1\n")
	writeFixtureFile(t, dir, "services/app/prod/logging.conf", "level=info\n")
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	h1, err := wt.Commit("first", &gogit.CommitOptions{Author: sig()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1", h1, &gogit.CreateTagOptions{Message: "release v1", Tagger: sig()}); err != nil {
		t.Fatal(err)
	}

	writeFixtureFile(t, dir, "services/app/prod/app.yaml", "version: 2\n")
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	h2, err := wt.Commit("second", &gogit.CommitOptions{Author: sig()})
	if err != nil {
		t.Fatal(err)
	}

	return fixture{dir: dir, first: h1.String(), second: h2.String()}
}

func localClient() *Client {
	return NewClient(Options{AllowLocalTransport: true})
}

func mustFetch(t *testing.T, ref Reference) *Result {
	t.Helper()
	res, err := localClient().Fetch(context.Background(), ref, nil)
	if err != nil {
		t.Fatalf("Fetch(%+v): %v", ref, err)
	}
	t.Cleanup(func() { _ = res.Cleanup() })
	return res
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFetchBranch(t *testing.T) {
	fx := newFixture(t)
	res := mustFetch(t, Reference{URL: fx.dir, Ref: "main", Path: "services/app/prod"})
	if res.Commit != fx.second {
		t.Fatalf("commit = %s, want %s", res.Commit, fx.second)
	}
	if got := readFile(t, res.Dir, "app.yaml"); got != "version: 2\n" {
		t.Fatalf("app.yaml = %q", got)
	}
}

func TestFetchDefaultRef(t *testing.T) {
	fx := newFixture(t)
	// Empty ref defaults to "main".
	res := mustFetch(t, Reference{URL: fx.dir, Path: "services/app/prod"})
	if res.Commit != fx.second {
		t.Fatalf("commit = %s, want %s", res.Commit, fx.second)
	}
}

func TestFetchTag(t *testing.T) {
	fx := newFixture(t)
	res := mustFetch(t, Reference{URL: fx.dir, Ref: "v1", Path: "services/app/prod"})
	if res.Commit != fx.first {
		t.Fatalf("commit = %s, want %s (tag should pin first commit)", res.Commit, fx.first)
	}
	if got := readFile(t, res.Dir, "app.yaml"); got != "version: 1\n" {
		t.Fatalf("app.yaml = %q, want version 1", got)
	}
}

func TestFetchCommitFull(t *testing.T) {
	fx := newFixture(t)
	res := mustFetch(t, Reference{URL: fx.dir, Ref: fx.first, Path: "services/app/prod"})
	if res.Commit != fx.first {
		t.Fatalf("commit = %s, want %s", res.Commit, fx.first)
	}
	if got := readFile(t, res.Dir, "app.yaml"); got != "version: 1\n" {
		t.Fatalf("app.yaml = %q", got)
	}
}

func TestFetchCommitShort(t *testing.T) {
	fx := newFixture(t)
	res := mustFetch(t, Reference{URL: fx.dir, Ref: fx.first[:10], Path: "services/app/prod"})
	if res.Commit != fx.first {
		t.Fatalf("commit = %s, want %s", res.Commit, fx.first)
	}
}

func TestFetchRepoRoot(t *testing.T) {
	fx := newFixture(t)
	res := mustFetch(t, Reference{URL: fx.dir, Ref: "main"})
	if res.Dir != res.WorktreeDir {
		t.Fatalf("root fetch Dir %q != WorktreeDir %q", res.Dir, res.WorktreeDir)
	}
	if got := readFile(t, res.Dir, "README.md"); got != "root file" {
		t.Fatalf("README.md = %q", got)
	}
}

func TestFetchPathNotFound(t *testing.T) {
	fx := newFixture(t)
	_, err := localClient().Fetch(context.Background(), Reference{URL: fx.dir, Ref: "main", Path: "does/not/exist"}, nil)
	if r, _ := ReasonOf(err); r != ReasonPathNotFound {
		t.Fatalf("reason = %v, want PathNotFound (err %v)", r, err)
	}
}

func TestFetchRefNotFound(t *testing.T) {
	fx := newFixture(t)
	_, err := localClient().Fetch(context.Background(), Reference{URL: fx.dir, Ref: "nonexistent-branch", Path: "."}, nil)
	if r, _ := ReasonOf(err); r != ReasonFetchFailed {
		t.Fatalf("reason = %v, want FetchFailed (err %v)", r, err)
	}
}

func TestFetchDisallowedURLFailsClosed(t *testing.T) {
	// Without AllowLocalTransport, a bare path has no permitted scheme.
	c := NewClient(Options{})
	fx := newFixture(t)
	_, err := c.Fetch(context.Background(), Reference{URL: fx.dir, Ref: "main"}, nil)
	if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
		t.Fatalf("reason = %v, want SourceNotAllowed (err %v)", r, err)
	}
}

func TestFetchPathTraversalRejected(t *testing.T) {
	fx := newFixture(t)
	_, err := localClient().Fetch(context.Background(), Reference{URL: fx.dir, Ref: "main", Path: "../../etc"}, nil)
	if r, _ := ReasonOf(err); r != ReasonPathNotFound {
		t.Fatalf("reason = %v, want PathNotFound for traversal (err %v)", r, err)
	}
}

func TestFetchCleanupRemovesWorktree(t *testing.T) {
	fx := newFixture(t)
	res, err := localClient().Fetch(context.Background(), Reference{URL: fx.dir, Ref: "main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.WorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("expected worktree removed, stat err = %v", err)
	}
}
