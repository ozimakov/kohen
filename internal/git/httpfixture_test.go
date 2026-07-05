package git

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitHTTPBackend locates git's smart-HTTP CGI, skipping the test when git or the
// backend is unavailable so the suite stays portable.
func gitHTTPBackend(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed; skipping smart-HTTP fixture test")
	}
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not found at %s: %v", backend, err)
	}
	return backend
}

// httpGitServer is a smart-HTTP git server backed by a bare clone of a seeded
// fixture repo, served over TLS. When user/pass are non-empty it requires HTTP
// basic auth. It satisfies the S1.1 Definition of Done's "Tier 2 against the
// git-server fixture" for the network transport.
type httpGitServer struct {
	url    string // https://host:port/repo.git
	first  string
	second string
}

func newHTTPGitServer(t *testing.T, user, pass string) httpGitServer {
	t.Helper()
	backend := gitHTTPBackend(t)
	fx := newFixture(t) // seeded non-bare repo with commits + tag v1

	projectRoot := t.TempDir()
	bare := filepath.Join(projectRoot, "repo.git")
	if out, err := exec.Command("git", "clone", "--bare", fx.dir, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}

	handler := &cgi.Handler{
		Path: backend,
		Root: "/",
		Dir:  projectRoot,
		Env: []string{
			"GIT_PROJECT_ROOT=" + projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH=" + os.Getenv("PATH"),
		},
	}

	var h http.Handler = handler
	if user != "" || pass != "" {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != user || p != pass {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			handler.ServeHTTP(w, r)
		})
	}

	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return httpGitServer{url: srv.URL + "/repo.git", first: fx.first, second: fx.second}
}

// httpClient returns a Client that permits the loopback fixture host.
func httpClient(allowList ...string) *Client {
	return NewClient(Options{AllowLoopback: true, AllowList: allowList})
}

// insecureCred trusts the fixture's self-signed TLS cert (test only).
func insecureCred(user, pass string) *Credential {
	return &Credential{Username: user, Password: pass, InsecureSkipTLSVerify: true}
}

func TestHTTPFetchHappyPath(t *testing.T) {
	srv := newHTTPGitServer(t, "", "")
	res, err := httpClient().Fetch(context.Background(),
		Reference{URL: srv.url, Ref: "main", Path: "services/app/prod"}, insecureCred("", ""))
	if err != nil {
		t.Fatalf("Fetch over https: %v", err)
	}
	t.Cleanup(func() { _ = res.Cleanup() })
	if res.Commit != srv.second {
		t.Errorf("commit = %s, want %s", res.Commit, srv.second)
	}
	if got := readFile(t, res.Dir, "app.yaml"); got != "version: 2\n" {
		t.Errorf("app.yaml = %q", got)
	}
}

func TestHTTPFetchTagResolves(t *testing.T) {
	srv := newHTTPGitServer(t, "", "")
	res, err := httpClient().Fetch(context.Background(),
		Reference{URL: srv.url, Ref: "v1", Path: "services/app/prod"}, insecureCred("", ""))
	if err != nil {
		t.Fatalf("Fetch tag over https: %v", err)
	}
	t.Cleanup(func() { _ = res.Cleanup() })
	if res.Commit != srv.first {
		t.Errorf("tag commit = %s, want %s", res.Commit, srv.first)
	}
}

func TestHTTPFetchAuthSuccess(t *testing.T) {
	srv := newHTTPGitServer(t, "alice", "s3cr3t-token")
	res, err := httpClient().Fetch(context.Background(),
		Reference{URL: srv.url, Ref: "main"}, insecureCred("alice", "s3cr3t-token"))
	if err != nil {
		t.Fatalf("authenticated Fetch: %v", err)
	}
	t.Cleanup(func() { _ = res.Cleanup() })
	if res.Commit != srv.second {
		t.Errorf("commit = %s", res.Commit)
	}
}

func TestHTTPFetchAuthFailure(t *testing.T) {
	srv := newHTTPGitServer(t, "alice", "s3cr3t-token")
	_, err := httpClient().Fetch(context.Background(),
		Reference{URL: srv.url, Ref: "main"}, insecureCred("alice", "wrong-password"))
	if r, _ := ReasonOf(err); r != ReasonAuthFailed {
		t.Fatalf("reason = %v, want AuthFailed (err %v)", r, err)
	}
}

func TestHTTPFetchUnreachableHost(t *testing.T) {
	// A closed loopback port: the connection is refused → FetchFailed.
	_, err := httpClient().Fetch(context.Background(),
		Reference{URL: "https://127.0.0.1:1/repo.git", Ref: "main"}, insecureCred("", ""))
	if r, _ := ReasonOf(err); r != ReasonFetchFailed {
		t.Fatalf("reason = %v, want FetchFailed (err %v)", r, err)
	}
}

func TestHTTPFetchDisallowedByAllowList(t *testing.T) {
	srv := newHTTPGitServer(t, "", "")
	// Allow-list that does not include the loopback fixture host: validate() must
	// reject before any network access (exercised through Fetch).
	_, err := httpClient("github.com").Fetch(context.Background(),
		Reference{URL: srv.url, Ref: "main"}, insecureCred("", ""))
	if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
		t.Fatalf("reason = %v, want SourceNotAllowed (err %v)", r, err)
	}
}

func TestHTTPFetchLoopbackBlockedByDefault(t *testing.T) {
	srv := newHTTPGitServer(t, "", "")
	// Without AllowLoopback, the SSRF guard blocks the loopback fixture host.
	c := NewClient(Options{})
	_, err := c.Fetch(context.Background(), Reference{URL: srv.url, Ref: "main"}, insecureCred("", ""))
	if r, _ := ReasonOf(err); r != ReasonSourceNotAllowed {
		t.Fatalf("reason = %v, want SourceNotAllowed for loopback (err %v)", r, err)
	}
}
