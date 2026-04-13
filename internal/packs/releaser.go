package packs

// releaser.go owns the Releaser interface and its production gh-CLI
// implementation. Split out from upload.go as of #101 so the interface
// stays live for `deadzone dbrelease` (which shells `gh release upload
// deadzone.db deadzone.db.sha256`) while the rest of upload.go —
// per-artifact upload flow — is disabled.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Releaser is the surface `dbrelease` (and, historically, the
// per-artifact `packs upload` flow, now disabled — see #101) uses to
// talk to GitHub Releases. It exists as an interface so tests can
// substitute a fake Releaser without spinning up real `gh`. The
// production implementation is GHReleaser below.
type Releaser interface {
	// EnsureRelease guarantees that a release with tag exists for the
	// given owner/repo. It is safe (and cheap) to call multiple times.
	EnsureRelease(ctx context.Context, repo, tag string) error

	// Upload uploads a single file as a release asset, clobbering any
	// existing asset with the same name. The asset name on the release
	// is filepath.Base(file).
	Upload(ctx context.Context, repo, tag, file string) error
}

// GHReleaser is the production Releaser, implemented by shelling out to
// the `gh` CLI. It is intentionally thin: every method is a single
// exec.Cmd with stderr captured for diagnostics.
//
// `gh` is already on every contributor's PATH per #22 and handles auth
// (2FA, token refresh, org scopes) transparently — reimplementing this
// in pure Go would be a worse `gh`.
type GHReleaser struct {
	// ensured tracks (repo, tag) pairs we've already verified during
	// this run, so a multi-file upload only calls `gh release view`
	// once. Reset at the start of each cmd invocation by virtue of
	// being a per-instance map.
	ensured map[string]bool
}

// NewGHReleaser returns a fresh production releaser. Always pass to
// callers via opts.Releaser; never reuse across processes.
func NewGHReleaser() *GHReleaser {
	return &GHReleaser{ensured: map[string]bool{}}
}

// EnsureRelease checks via `gh release view` whether a release exists
// for the tag. If it does not, the method returns a clear error
// pointing the operator at the tag-push step — `dbrelease` assumes
// CI's release.yml already created the release object when the tag
// was pushed (see #101 §F).
func (g *GHReleaser) EnsureRelease(ctx context.Context, repo, tag string) error {
	cacheKey := repo + "\x00" + tag
	if g.ensured[cacheKey] {
		return nil
	}
	view := exec.CommandContext(ctx, "gh", "release", "view", tag, "--repo", repo)
	var stderr bytes.Buffer
	view.Stderr = &stderr
	if err := view.Run(); err == nil {
		g.ensured[cacheKey] = true
		return nil
	} else if isReleaseNotFound(stderr.String()) {
		return fmt.Errorf("release %s/%s does not exist yet — push the tag first so CI's release.yml creates it", repo, tag)
	} else {
		return fmt.Errorf("gh release view %s/%s: %w (%s)", repo, tag, err, strings.TrimSpace(stderr.String()))
	}
}

// Upload runs `gh release upload <tag> <file> --clobber --repo <repo>`.
// --clobber lets the same asset name be re-uploaded over an older copy.
func (g *GHReleaser) Upload(ctx context.Context, repo, tag, file string) error {
	cmd := exec.CommandContext(ctx, "gh", "release", "upload", tag, file,
		"--clobber",
		"--repo", repo,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh release upload %s/%s %s: %w (%s)", repo, tag, file, err, strings.TrimSpace(stderr.String()))
	}
	slog.Info("packs.dbrelease.uploaded", "repo", repo, "tag", tag, "file", file)
	return nil
}

// isReleaseNotFound recognises the stderr message gh emits when a
// release tag isn't on the repo yet. The exact wording is "release not
// found" (lowercase) as of gh 2.x; we lowercase the comparison so a
// future capitalization tweak doesn't silently break.
func isReleaseNotFound(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "release not found")
}
