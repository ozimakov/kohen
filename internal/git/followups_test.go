package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestFetchSymlinkDirEscapeRejected commits a symlink pointing outside the repo
// and asserts the fetched subpath cannot escape the worktree (SPEC R7.5 / TM6).
func TestFetchSymlinkDirEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "leak.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeFixtureFile(t, dir, "real.txt", "hi")
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("evil symlink", &gogit.CommitOptions{Author: sig()}); err != nil {
		t.Fatal(err)
	}

	// Whether go-git materializes or neutralizes the symlink, we must never
	// return a Dir from which the outside secret is reachable.
	res, err := localClient().Fetch(context.Background(), Reference{URL: dir, Ref: "main", Path: "evil/sub"}, nil)
	if err != nil {
		if r, _ := ReasonOf(err); r != ReasonPathNotFound {
			t.Fatalf("expected PathNotFound, got %v (err %v)", r, err)
		}
		return
	}
	t.Cleanup(func() { _ = res.Cleanup() })
	if _, err := os.Stat(filepath.Join(res.Dir, "leak.txt")); err == nil {
		t.Fatalf("ESCAPE: leak.txt reachable via %s", res.Dir)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in       string
		mustNot  string
		contains string
	}{
		{in: "https://user:s3cr3t@github.com/acme/repo.git", mustNot: "s3cr3t", contains: "redacted"},
		{in: "https://github.com/acme/repo.git", contains: "github.com"},
	}
	for _, tc := range tests {
		got := redact(tc.in)
		if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
			t.Fatalf("redact(%q) = %q still contains secret", tc.in, got)
		}
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("redact(%q) = %q, want contains %q", tc.in, got, tc.contains)
		}
	}
}

func TestAllowListDenialRedactsCredentials(t *testing.T) {
	// An allow-list denial message includes the URL; it must be redacted.
	c := NewClient(Options{AllowList: []string{"github.com"}})
	p, err := parseURL("https://user:topsecret@evil.example/x.git", false)
	if err != nil {
		t.Fatal(err)
	}
	err = c.validate(context.Background(), p)
	if err == nil {
		t.Fatal("expected allow-list denial")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("allow-list error leaked credentials: %v", err)
	}
}
