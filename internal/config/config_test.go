package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ozimakov/kohen/internal/config"
)

func TestLoadEmptyPathReturnsDefaults(t *testing.T) {
	c, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxDegradedDuration.Duration != config.DefaultMaxDegradedDuration {
		t.Fatalf("MaxDegradedDuration = %v, want default", c.MaxDegradedDuration.Duration)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	body := "sourceAllowList:\n  - github.com/acme\nmaxDegradedDuration: 5m\nallowInsecureGitTLS: true\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SourceAllowList) != 1 || c.SourceAllowList[0] != "github.com/acme" {
		t.Fatalf("SourceAllowList = %v", c.SourceAllowList)
	}
	if c.MaxDegradedDuration.Duration != 5*time.Minute {
		t.Fatalf("MaxDegradedDuration = %v, want 5m", c.MaxDegradedDuration.Duration)
	}
	if !c.AllowInsecureGitTLS {
		t.Fatal("AllowInsecureGitTLS not parsed")
	}
}

func TestLoadAppliesDefaultForZeroDuration(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte("sourceAllowList: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxDegradedDuration.Duration != config.DefaultMaxDegradedDuration {
		t.Fatalf("expected default MaxDegradedDuration, got %v", c.MaxDegradedDuration.Duration)
	}
}
