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

// Slug derives the on-disk subdirectory name for a lib_id. The leading
// "/" is stripped and the remaining slashes become underscores:
//
//	/modelcontextprotocol/go-sdk → modelcontextprotocol_go-sdk
//	/facebook/react/v18          → facebook_react_v18
//
// The mapping is deterministic and 1:1 with the lib_id, so an operator
// listing artifacts/ can recover every lib by inspection. Hyphens and
// dots are preserved.
func Slug(libID string) string {
	trimmed := strings.TrimPrefix(libID, "/")
	return strings.ReplaceAll(trimmed, "/", "_")
}

// ArtifactDir returns <artifactsDir>/<slug>/ — the per-lib folder that
// holds artifact.db (+ WAL/SHM during runs) and state.yaml.
func ArtifactDir(artifactsDir, libID string) string {
	return filepath.Join(artifactsDir, Slug(libID))
}

// ArtifactDBPath returns <artifactsDir>/<slug>/artifact.db — the
// canonical per-lib database path the scraper writes and the
// consolidator reads.
func ArtifactDBPath(artifactsDir, libID string) string {
	return filepath.Join(ArtifactDir(artifactsDir, libID), "artifact.db")
}

// StatePath returns <artifactsDir>/<slug>/state.yaml — the per-lib
// sidecar carrying content metadata (embedder identity, schema
// version, scrape dates, counts). See state.go for the StateFile
// shape and lifecycle.
func StatePath(artifactsDir, libID string) string {
	return filepath.Join(ArtifactDir(artifactsDir, libID), "state.yaml")
}
