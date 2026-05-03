package main

// cache-signals is a CI-side hook for the per-lib artifact cache key
// in scrape-pack.yml and cache-keepalive.yml. It prints a single-line
// JSON object containing two semantic invalidation signals:
//
//   - schema_version    : db.CurrentSchemaVersion. Bumping it means
//                         existing artifacts are at an incompatible
//                         schema and must be rescraped.
//   - embedder_signature: embed.HugotSignature(). Hash over the five
//                         hugot.go constants that determine the
//                         vector space — model identity, quantization
//                         variant, retrieval prefixes. A constant edit
//                         here means cached vectors live in a
//                         different space than what the binary
//                         produces and must be rescraped.
//
// Both are extracted directly from the Go side rather than re-derived
// in bash so the cache key has a single source of truth that the
// compiler audits at build time. See #193.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
)

var cacheSignalsCmd = &cobra.Command{
	Use:   "cache-signals",
	Short: "Print cache-key signals (schema_version, embedder_signature) as JSON for CI workflows",
	Long: `Emits a single-line JSON object on stdout:

  {"schema_version":N,"embedder_signature":"sha256hex"}

scrape-pack.yml and cache-keepalive.yml consume this output to feed the
per-lib artifact cache key, so any schema bump or embedder constant
change invalidates the cache fleet in lockstep with the binary's
guarantees.

No flags. No model load. Sub-millisecond.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return writeCacheSignals(os.Stdout)
	},
}

// writeCacheSignals serialises the two signals to w as a single line
// of JSON. Split out from RunE so tests can pass a bytes.Buffer
// without trampling os.Stdout.
func writeCacheSignals(w io.Writer) error {
	sig, err := embed.Signature(embed.KindHugot)
	if err != nil {
		return fmt.Errorf("compute embedder signature: %w", err)
	}
	out := struct {
		SchemaVersion     int    `json:"schema_version"`
		EmbedderSignature string `json:"embedder_signature"`
	}{
		SchemaVersion:     db.CurrentSchemaVersion,
		EmbedderSignature: sig,
	}
	// json.Encoder appends a single trailing newline, which the
	// `jq -r` consumers in the workflows expect.
	if err := json.NewEncoder(w).Encode(out); err != nil {
		return fmt.Errorf("encode cache signals: %w", err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(cacheSignalsCmd)
}
