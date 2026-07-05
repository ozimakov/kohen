// Command gitserver is a tiny in-cluster smart-HTTP git server used only by the
// U1 end-to-end suite. It serves a seeded repository over HTTPS (self-signed,
// generated at startup) via git-http-backend, and exposes a small admin endpoint
// to commit config changes so the e2e can exercise rollout-on-change.
//
// It is NOT part of the operator and is never shipped; it lives under test/ and
// is built into a throwaway image loaded into kind.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	root := flag.String("root", "/srv/git", "git project root")
	repo := flag.String("repo", "config", "repository name under root (served as <name>/.git)")
	flag.Parse()

	repoDir := filepath.Join(*root, *repo)
	if err := seedRepo(repoDir); err != nil {
		log.Fatalf("seeding repo: %v", err)
	}

	backend, err := gitHTTPBackend()
	if err != nil {
		log.Fatalf("locating git-http-backend: %v", err)
	}

	cgiHandler := &cgi.Handler{
		Path: backend,
		Root: "/",
		Dir:  *root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + *root,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH=" + os.Getenv("PATH"),
		},
	}

	mux := http.NewServeMux()
	// Admin: commit a config change. POST /admin/commit?path=app.yaml with the
	// new file content as the body; returns the new commit SHA.
	mux.HandleFunc("/admin/commit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		rel := r.URL.Query().Get("path")
		if rel == "" || strings.Contains(rel, "..") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sha, err := commitFile(repoDir, rel, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, sha)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("/", cgiHandler)

	cert, err := selfSignedCert()
	if err != nil {
		log.Fatalf("generating cert: %v", err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("gitserver serving %s over HTTPS on %s (repo path /%s/.git)", repoDir, *addr, *repo)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func gitHTTPBackend() (string, error) {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", err
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		return "", err
	}
	return backend, nil
}

func sig() *object.Signature {
	return &object.Signature{Name: "kohen-e2e", Email: "e2e@kohen.dev", When: time.Now()}
}

// seedRepo initializes repoDir with an initial commit on branch main if it does
// not already exist.
func seedRepo(repoDir string) error {
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		return nil // already seeded
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}
	r, err := gogit.PlainInit(repoDir, false)
	if err != nil {
		return err
	}
	if err := r.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "svc"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repoDir, "svc", "app.yaml"), []byte("greeting: hello-v1\n"), 0o644); err != nil {
		return err
	}
	wt, err := r.Worktree()
	if err != nil {
		return err
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return err
	}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig()})
	return err
}

// commitFile writes rel with content and commits it, returning the new SHA.
func commitFile(repoDir, rel string, content []byte) (string, error) {
	r, err := gogit.PlainOpen(repoDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(repoDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return "", err
	}
	wt, err := r.Worktree()
	if err != nil {
		return "", err
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return "", err
	}
	h, err := wt.Commit("update "+rel, &gogit.CommitOptions{Author: sig()})
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kohen-e2e-gitserver"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"*", "localhost", "gitserver"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Wildcard DNSNames like "*" don't cover the service FQDN; the operator
	// connects with InsecureSkipTLSVerify in e2e, so the SAN is only a
	// formality. Build the tls.Certificate directly from DER + key.
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
