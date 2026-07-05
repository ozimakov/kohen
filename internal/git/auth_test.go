package git

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestAuthMethodNilCredential(t *testing.T) {
	var c *Credential
	auth, cleanup, err := c.authMethod(transportHTTPS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if auth != nil {
		t.Fatalf("expected anonymous (nil) auth, got %#v", auth)
	}
}

func TestAuthMethodHTTPS(t *testing.T) {
	t.Run("token with default username", func(t *testing.T) {
		auth, _, err := (&Credential{Password: "tok"}).authMethod(transportHTTPS)
		if err != nil {
			t.Fatal(err)
		}
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("expected BasicAuth, got %T", auth)
		}
		if ba.Username != defaultHTTPSUsername || ba.Password != "tok" {
			t.Fatalf("got %s/%s", ba.Username, ba.Password)
		}
	})

	t.Run("explicit username", func(t *testing.T) {
		auth, _, err := (&Credential{Username: "u", Password: "p"}).authMethod(transportHTTPS)
		if err != nil {
			t.Fatal(err)
		}
		ba := auth.(*githttp.BasicAuth)
		if ba.Username != "u" {
			t.Fatalf("username = %s", ba.Username)
		}
	})

	t.Run("no password is anonymous", func(t *testing.T) {
		auth, _, err := (&Credential{Username: "u"}).authMethod(transportHTTPS)
		if err != nil {
			t.Fatal(err)
		}
		if auth != nil {
			t.Fatalf("expected anonymous, got %#v", auth)
		}
	})
}

func testRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestAuthMethodSSH(t *testing.T) {
	keyPEM := testRSAKeyPEM(t)

	t.Run("no private key is anonymous", func(t *testing.T) {
		auth, _, err := (&Credential{}).authMethod(transportSSH)
		if err != nil {
			t.Fatal(err)
		}
		if auth != nil {
			t.Fatalf("expected anonymous, got %#v", auth)
		}
	})

	t.Run("host key verification required by default", func(t *testing.T) {
		_, _, err := (&Credential{PrivateKey: keyPEM}).authMethod(transportSSH)
		if err == nil {
			t.Fatal("expected error without known_hosts")
		}
		if r, _ := ReasonOf(err); r != ReasonAuthFailed {
			t.Fatalf("reason = %v, want AuthFailed", r)
		}
	})

	t.Run("insecure ignore host key", func(t *testing.T) {
		auth, cleanup, err := (&Credential{PrivateKey: keyPEM, InsecureIgnoreHostKey: true}).authMethod(transportSSH)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cleanup() }()
		if auth == nil {
			t.Fatal("expected auth method")
		}
	})

	t.Run("invalid key fails", func(t *testing.T) {
		_, _, err := (&Credential{PrivateKey: []byte("not a key")}).authMethod(transportSSH)
		if r, _ := ReasonOf(err); r != ReasonAuthFailed {
			t.Fatalf("reason = %v, want AuthFailed (err %v)", r, err)
		}
	})
}
