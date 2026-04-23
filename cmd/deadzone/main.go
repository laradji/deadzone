// Command deadzone is the single CLI entry point for the deadzone
// toolchain. It dispatches a leading verb to one of five subcommand
// runners living alongside this file:
//
//	deadzone server        run the MCP stdio server against a deadzone.db
//	deadzone scrape        index libraries_sources.yaml into ./artifacts
//	deadzone consolidate   merge ./artifacts/<slug>/artifact.db into a single deadzone.db
//	deadzone fetch-db      download/refresh the cached deadzone.db from the latest GH Release
//	deadzone dbrelease     upload ./deadzone.db to a tagged GitHub Release
//
// Top-level `--version` short-circuits before any embedder/DB load so
// the smoke test in release.yml can call it on a stock runner with no
// model cache and no DB on disk. Per-subcommand `--version` is not
// supported — the single top-level banner is enough for the smoke test
// and keeps each subcommand's flag surface focused on its own knobs.
//
// Routing is a `spf13/cobra` rootCmd with one child `*cobra.Command` per
// sibling file, wired in that file's init(). See issue #134 for the
// migration rationale: sub-subcommand ergonomics and built-in shell
// completion outgrew the stdlib `flag` + `os.Args` switch.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/laradji/deadzone/internal/buildinfo"
)

// Build-time values overridden by `-ldflags -X main.version=…` at
// release build time (see justfile's build-release recipe). One set
// for the whole binary — subcommands that previously owned their own
// copy share these via the package-level symbols.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// rootCmd is the top-level cobra command. Child commands are registered
// in sibling files' init() so adding a subcommand is a one-file change.
// SilenceUsage + SilenceErrors let main() own the error formatting so
// a RunE failure prints "deadzone: <err>" instead of cobra dumping the
// usage block on every runtime error.
var rootCmd = &cobra.Command{
	Use:   "deadzone",
	Short: "Self-hosted MCP server for semantic doc search",
	Long: `deadzone — self-hosted MCP server for semantic doc search.

Scrape library documentation into per-lib artifacts, consolidate them
into a single deadzone.db, then serve semantic search over stdio MCP.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// versionCmd mirrors `deadzone --version` as a positional subcommand so
// the banner is reachable via both `deadzone --version` / `-v` and
// `deadzone version` without relying on cobra's default "<use> version
// <version>" template format.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the deadzone version banner",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println(rootCmd.Version)
		return nil
	},
}

func main() {
	// rootCmd.Version is set in main() rather than at package-init so
	// the build-time ldflag-injected vars are guaranteed observed.
	// Setting it here also means `deadzone --version` and the implicit
	// `-v` short flag cobra wires up both short-circuit before any RunE
	// runs — matching the old dispatch's fast -version path.
	rootCmd.Version = buildinfo.Format("deadzone", version, commit, date)
	// Override cobra's default "<use> version <version>\n" template so
	// the output matches the old stdlib-flag banner exactly — the
	// release.yml smoke tests grep on this string shape.
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.AddCommand(versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "deadzone:", err)
		os.Exit(1)
	}
}
