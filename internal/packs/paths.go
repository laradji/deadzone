package packs

// paths.go owns the folder-per-lib layout introduced by #101.
//
// Every lib lives in its own subdirectory of the top-level artifacts
// directory, named after a deterministic slug derived from the lib_id:
//
//	artifacts/
//	  modelcontextprotocol_go-sdk/
//	    artifact.db
//	    artifact.db-wal      (transient, present only during a scrape)
//	    artifact.db-shm      (transient)
//	    state.yaml
//	  facebook_react_v18/
//	    artifact.db
//	    state.yaml
//	  manifest.yaml
//
// Release-asset names can't contain "/", so the folder shape is
// strictly a local-on-disk layout. Per-artifact distribution is
// paused (#101 §E); when it returns it will tarball each folder.

import (
	"path/filepath"
	"strings"
)

// Slug derives the on-disk subdirectory name for a (lib_id, version)
// slot. The leading "/" is stripped from lib_id, the remaining slashes
// become underscores, and the version (when non-empty) is appended
// after another "_":
//
//	(/modelcontextprotocol/go-sdk, "")    → modelcontextprotocol_go-sdk
//	(/facebook/react,              v18)   → facebook_react_v18
//	(/hashicorp/terraform,         v1.14) → hashicorp_terraform_v1.14
//
// The mapping is deterministic and 1:1 with (lib_id, version), so an
// operator listing artifacts/ can recover every slot by inspection.
// Hyphens and dots are preserved; empty version produces the
// single-version legacy form (no trailing underscore). See #113.
func Slug(libID, version string) string {
	trimmed := strings.TrimPrefix(libID, "/")
	slug := strings.ReplaceAll(trimmed, "/", "_")
	if version == "" {
		return slug
	}
	return slug + "_" + version
}

// ArtifactDir returns <artifactsDir>/<slug>/ — the per-(lib, version)
// folder that holds artifact.db (+ WAL/SHM during runs) and
// state.yaml.
func ArtifactDir(artifactsDir, libID, version string) string {
	return filepath.Join(artifactsDir, Slug(libID, version))
}

// ArtifactDBPath returns <artifactsDir>/<slug>/artifact.db — the
// canonical per-(lib, version) database path the scraper writes and
// the consolidator reads.
func ArtifactDBPath(artifactsDir, libID, version string) string {
	return filepath.Join(ArtifactDir(artifactsDir, libID, version), "artifact.db")
}

// StatePath returns <artifactsDir>/<slug>/state.yaml — the
// per-(lib, version) sidecar carrying content metadata (embedder
// identity, schema version, scrape dates, counts). See state.go for
// the StateFile shape and lifecycle.
func StatePath(artifactsDir, libID, version string) string {
	return filepath.Join(ArtifactDir(artifactsDir, libID, version), "state.yaml")
}
