// Package version owns the single source of truth for the build version.
package version

import (
	_ "embed"
	"strings"
)

//go:embed version.txt
var versionFile string

// Version is injected by the build script via
//
//	go build -ldflags "-X model-router/internal/version.Version=0.0.1-rc"
//
// and left empty for plain `go run` / `go build` runs, which fall back to
// version.txt so the version has exactly one source of truth.
var Version string

// Get returns the injected build version, or the embedded version.txt value.
func Get() string {
	if v := strings.TrimSpace(Version); v != "" {
		return v
	}
	return strings.TrimSpace(versionFile)
}
