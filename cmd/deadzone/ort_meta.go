package main

// ort-meta is a hidden CI-plumbing subcommand introduced in #196 to keep
// the OCI image build (release.yml's docker job + the docker-build
// justfile recipe) reading the pinned onnxruntime URL/SHA256/library
// filename from the same source of truth Bootstrap uses
// (internal/ort.pinnedReleases). Without this command the docker job
// would have to hardcode a parallel copy of those constants in YAML,
// and a future ORT bump that updated only internal/ort would silently
// publish images linking against the wrong .so version.
//
// Hidden: true keeps it out of `--help` because it is not a user-facing
// verb — it is plumbing that the build pipeline shells out to. The JSON
// shape is the contract; treat changes to it as breaking the pipeline.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/laradji/deadzone/internal/ort"
)

// ortMetaPlatform is one entry in the JSON output. Field tags use the
// snake_case shape jq one-liners read most cleanly.
type ortMetaPlatform struct {
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	LibName string `json:"lib_name"`
}

type ortMetaOutput struct {
	Version   string            `json:"version"`
	Platforms []ortMetaPlatform `json:"platforms"`
}

var ortMetaCmd = &cobra.Command{
	Use:    "ort-meta",
	Short:  "Print pinned onnxruntime release metadata as JSON (CI plumbing)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runOrtMeta(cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(ortMetaCmd)
}

func runOrtMeta(w io.Writer) error {
	out := ortMetaOutput{Version: ort.Version}
	for _, plat := range ort.SupportedPlatforms() {
		// Only the linux arches are stageable into the OCI image;
		// emitting darwin/windows would tempt callers to bake them too
		// and surface confusing "no such platform" errors when they
		// try. Filter here, not at call site.
		if !strings.HasPrefix(plat, "linux/") {
			continue
		}
		parts := strings.SplitN(plat, "/", 2)
		url, sum, lib, ok := ort.PinnedRelease(parts[0], parts[1])
		if !ok {
			// SupportedPlatforms returned this key, so this branch is
			// unreachable barring an internal contract violation.
			return fmt.Errorf("ort-meta: SupportedPlatforms listed %q but PinnedRelease returned ok=false", plat)
		}
		out.Platforms = append(out.Platforms, ortMetaPlatform{
			GOOS:    parts[0],
			GOARCH:  parts[1],
			URL:     url,
			SHA256:  sum,
			LibName: lib,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
