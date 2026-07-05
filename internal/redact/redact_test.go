package redact_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/ozimakov/kohen/internal/redact"
)

func TestRedactorString(t *testing.T) {
	r := redact.New()
	r.Add("s3cr3t-token-value")
	got := r.String("using token s3cr3t-token-value now")
	if strings.Contains(got, "s3cr3t-token-value") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, redact.Placeholder) {
		t.Fatalf("placeholder missing: %q", got)
	}
}

func TestRedactorIgnoresShortValues(t *testing.T) {
	r := redact.New()
	r.Add("ab") // shorter than minSecretLen — must not redact
	if got := r.String("cabbage"); got != "cabbage" {
		t.Fatalf("short value redacted: %q", got)
	}
}

func TestRedactorMultilineKey(t *testing.T) {
	r := redact.New()
	key := "-----BEGIN KEY-----\nAAAABBBBCCCCDDDD\n-----END KEY-----"
	r.Add(key)
	if got := r.String("loaded AAAABBBBCCCCDDDD line"); strings.Contains(got, "AAAABBBBCCCCDDDD") {
		t.Fatalf("multiline secret line leaked: %q", got)
	}
}

func TestRedactingLogger(t *testing.T) {
	var buf strings.Builder
	base := funcr.New(func(prefix, args string) {
		buf.WriteString(args)
		buf.WriteString("\n")
	}, funcr.Options{})

	r := redact.New()
	r.Add("hunter2password")
	log := redact.NewLogger(base, r)

	log.Info("connecting", "url", "https://user:hunter2password@host/repo.git")
	log.Error(errors.New("auth failed for hunter2password"), "fetch")

	out := buf.String()
	if strings.Contains(out, "hunter2password") {
		t.Fatalf("secret leaked through logger: %q", out)
	}
	if !strings.Contains(out, redact.Placeholder) {
		t.Fatalf("expected redaction placeholder in %q", out)
	}
}

func TestRedactingLoggerWithValues(t *testing.T) {
	var buf strings.Builder
	base := funcr.New(func(prefix, args string) { buf.WriteString(args) }, funcr.Options{})
	r := redact.New()
	r.Add("topsecretvalue")
	log := redact.NewLogger(base, r).WithValues("token", "topsecretvalue").WithName("git")
	log.Info("ok")
	if strings.Contains(buf.String(), "topsecretvalue") {
		t.Fatalf("secret leaked through WithValues: %q", buf.String())
	}
}
