// Package buildinfo formats the runtime version banner shared by all
// four deadzone binaries. Each cmd/*/main.go owns its own package-level
// version/commit/date vars (they have to live in each main package so
// `-ldflags -X main.version=…` can target them) and calls Format to
// render the banner for the -version flag and startup logs.
package buildinfo

import "fmt"

// Format renders the standard "<name> <version> (<commit>, built
// <date>)" banner, e.g.:
//
//	deadzone-server v0.1.0 (abc1234, built 2026-04-12T12:34:56Z)
//
// The three value strings come from ldflags injection at release build
// time (see justfile's build-release recipe) or from their default
// "dev" / "unknown" fallbacks when the binary was built without -X.
func Format(name, version, commit, date string) string {
	return fmt.Sprintf("%s %s (%s, built %s)", name, version, commit, date)
}
