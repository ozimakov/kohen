// Package version holds build-time identity for the operator binary.
// Values are overwritten via -ldflags at image build time.
package version

// Version is the SemVer product version (e.g. "1.0.0"), or "dev" for local builds.
var Version = "dev"

// Commit is the short git SHA embedded at build time, or "none".
var Commit = "none"
