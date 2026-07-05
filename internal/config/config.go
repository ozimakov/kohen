// Package config defines the operator-scoped configuration (SPEC §12, §16):
// security allow-lists and fail-safe timing that an operator/platform team sets
// at install time, distinct from per-ConfigSync tenant fields.
package config

import (
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// DefaultMaxDegradedDuration bounds how long a ConfigSync may serve last-known-
// good secrets before hard-failing (SPEC R8.11); also used as a general
// fail-safe ceiling.
const DefaultMaxDegradedDuration = 15 * time.Minute

// Config is the operator-scoped configuration.
type Config struct {
	// SourceAllowList restricts git source hosts/prefixes (SPEC R-AUTH.3). Empty
	// means "no operator restriction" (per-tenant validation still applies).
	SourceAllowList []string `json:"sourceAllowList,omitempty"`

	// SecretStoreAllowList restricts permissible ExternalSecret secret stores
	// by name (SPEC R-AUTH.4). Empty means no store restriction. Enforced by
	// the apply-if-present manifest guard rails (internal/manifest.Guard).
	SecretStoreAllowList []string `json:"secretStoreAllowList,omitempty"`

	// MaxDegradedDuration bounds degraded (last-known-good) serving (R8.11).
	MaxDegradedDuration metav1.Duration `json:"maxDegradedDuration,omitempty"`

	// AllowInsecureGitTLS permits skipping TLS verification for git (test/dev
	// only, off by default; SPEC R-AUTH.7).
	AllowInsecureGitTLS bool `json:"allowInsecureGitTLS,omitempty"`
}

// Default returns a Config with documented defaults applied.
func Default() *Config {
	return &Config{
		MaxDegradedDuration: metav1.Duration{Duration: DefaultMaxDegradedDuration},
	}
}

// Load reads a YAML/JSON config file and applies defaults. An empty path returns
// Default().
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading operator config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing operator config %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.MaxDegradedDuration.Duration <= 0 {
		c.MaxDegradedDuration = metav1.Duration{Duration: DefaultMaxDegradedDuration}
	}
}

func (c *Config) validate() error {
	if c.MaxDegradedDuration.Duration <= 0 {
		return fmt.Errorf("maxDegradedDuration must be positive")
	}
	return nil
}
