package git

import (
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// defaultHTTPSUsername is used for HTTPS token auth when no username is given.
// Most providers ignore the username for personal-access-token auth, but
// go-git's BasicAuth requires a non-empty value.
const defaultHTTPSUsername = "kohen"

// Credential holds git authentication material, matching the documented
// Secret schema in SPEC R7.8. The zero value means anonymous access.
type Credential struct {
	// Username is the HTTPS username (optional; defaults to "kohen" for tokens).
	Username string
	// Password is the HTTPS password or token.
	Password string

	// PrivateKey is a PEM-encoded SSH private key.
	PrivateKey []byte
	// Passphrase decrypts PrivateKey, if it is encrypted.
	Passphrase string
	// KnownHosts is an OpenSSH known_hosts file used for host-key verification.
	KnownHosts []byte

	// InsecureSkipTLSVerify disables TLS certificate verification for HTTPS.
	// This is an explicit, logged opt-in (SPEC R7.8) and defaults to off.
	InsecureSkipTLSVerify bool
	// InsecureIgnoreHostKey disables SSH host-key verification. Explicit,
	// logged opt-in (SPEC R7.8); defaults to off.
	InsecureIgnoreHostKey bool
}

// noopCleanup is returned when a credential allocates no temporary resources.
func noopCleanup() error { return nil }

// authMethod builds the go-git AuthMethod for the given transport. It returns a
// cleanup function that MUST be called after the fetch completes (it removes any
// temporary known_hosts file). A nil credential yields anonymous access.
func (c *Credential) authMethod(kind transportKind) (transport.AuthMethod, func() error, error) {
	if c == nil {
		return nil, noopCleanup, nil
	}
	switch kind {
	case transportHTTPS:
		if c.Password == "" {
			return nil, noopCleanup, nil
		}
		username := c.Username
		if username == "" {
			username = defaultHTTPSUsername
		}
		return &githttp.BasicAuth{Username: username, Password: c.Password}, noopCleanup, nil

	case transportSSH:
		if len(c.PrivateKey) == 0 {
			return nil, noopCleanup, nil
		}
		signer, err := gitssh.NewPublicKeys("git", c.PrivateKey, c.Passphrase)
		if err != nil {
			return nil, noopCleanup, wrapError(ReasonAuthFailed, err, "parsing ssh private key")
		}
		cleanup, err := applyHostKeyPolicy(signer, c)
		if err != nil {
			return nil, noopCleanup, err
		}
		return signer, cleanup, nil

	default:
		return nil, noopCleanup, nil
	}
}

// applyHostKeyPolicy configures SSH host-key verification on signer. Verification
// is ON by default and requires a known_hosts file; disabling it is an explicit
// opt-in.
func applyHostKeyPolicy(signer *gitssh.PublicKeys, c *Credential) (func() error, error) {
	if c.InsecureIgnoreHostKey {
		//nolint:gosec // Explicit, documented opt-in per SPEC R7.8.
		signer.HostKeyCallback = cryptossh.InsecureIgnoreHostKey()
		return noopCleanup, nil
	}
	if len(c.KnownHosts) == 0 {
		return noopCleanup, newError(ReasonAuthFailed,
			"ssh host-key verification is on by default but no known_hosts was provided (set known_hosts or opt in to insecure)")
	}
	f, err := os.CreateTemp("", "kohen-known-hosts-*")
	if err != nil {
		return noopCleanup, wrapError(ReasonAuthFailed, err, "creating known_hosts file")
	}
	cleanup := func() error { return os.Remove(f.Name()) }
	if _, err := f.Write(c.KnownHosts); err != nil {
		_ = f.Close()
		_ = cleanup()
		return noopCleanup, wrapError(ReasonAuthFailed, err, "writing known_hosts file")
	}
	if err := f.Close(); err != nil {
		_ = cleanup()
		return noopCleanup, wrapError(ReasonAuthFailed, err, "closing known_hosts file")
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		_ = cleanup()
		return noopCleanup, wrapError(ReasonAuthFailed, err, "loading known_hosts")
	}
	signer.HostKeyCallback = cb
	return cleanup, nil
}

// tlsInsecure reports whether HTTPS TLS verification should be skipped.
func (c *Credential) tlsInsecure() bool {
	return c != nil && c.InsecureSkipTLSVerify
}
