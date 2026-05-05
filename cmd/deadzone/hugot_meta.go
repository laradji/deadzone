package main

// hugot-meta is a hidden CI-plumbing subcommand introduced in #207 to
// keep the OCI image build (scrape-pack.yml's docker job) reading the
// pinned hugot model URL/SHA256/destination from the same source of
// truth NewHugot ultimately reads at runtime — internal/embed's
// pinnedModelFiles + ModelRevision constants. Without this command the
// docker job would have to hardcode a parallel copy of those constants
// in YAML, and a future model bump that updated only internal/embed
// would silently publish images linking against the wrong-version
// weights or, worse, ship an image whose baked tokenizer.json no longer
// matched what hugot's NewPipeline expected.
//
// Mirrors ort-meta's design (see ort_meta.go) — same hidden-flag
// rationale, same "JSON shape is the contract" philosophy. The two
// commands together cover both halves of the OCI bake (libonnxruntime
// + model weights).
//
// Hidden: true keeps it out of `--help` because it is not a user-facing
// verb — it is plumbing that the build pipeline shells out to.

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/laradji/deadzone/internal/embed"
)

// hugotMetaOutput is the JSON shape scrape-pack.yml's docker job parses
// with jq. Field names use the snake_case shape jq one-liners read most
// cleanly. Adding fields is non-breaking; renaming or removing fields
// breaks the docker job and must coincide with a workflow YAML update.
type hugotMetaOutput struct {
	ModelID     string                  `json:"model_id"`
	Revision    string                  `json:"revision"`
	DestDirname string                  `json:"dest_dirname"`
	Files       []embed.PinnedModelFile `json:"files"`
}

var hugotMetaCmd = &cobra.Command{
	Use:    "hugot-meta",
	Short:  "Print pinned hugot model metadata as JSON (CI plumbing)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHugotMeta(cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(hugotMetaCmd)
}

func runHugotMeta(w io.Writer) error {
	modelID, revision, destDirname, files := embed.PinnedModel()
	out := hugotMetaOutput{
		ModelID:     modelID,
		Revision:    revision,
		DestDirname: destDirname,
		Files:       files,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
