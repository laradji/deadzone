// Command deadzone is the single CLI entry point for the deadzone
// toolchain. It dispatches a leading positional verb to one of four
// subcommand runners living alongside this file:
//
//	deadzone server        run the MCP stdio server against a deadzone.db
//	deadzone scrape        index libraries_sources.yaml into ./artifacts
//	deadzone consolidate   merge ./artifacts/*.db into a single deadzone.db
//	deadzone packs {upload|download|list}
//	                       manage the per-lib artifacts on the rolling
//	                       GitHub Release
//
// Top-level `-version` short-circuits before any embedder/DB load so
// the smoke test in release.yml can call it on a stock runner with no
// model cache and no DB on disk. Per-subcommand `-version` is not
// supported — the single top-level banner is enough for the smoke test
// and keeps each subcommand's flag surface focused on its own knobs.
//
// Routing is the same `os.Args[1]` switch + `flag.NewFlagSet` per sub
// pattern that cmd/packs/main.go already used before this refactor —
// see issue #98 for the consolidation rationale (one tarball, one
// binary, the duplicated CGO+ORT payload shipped once instead of four
// times).
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/laradji/deadzone/internal/buildinfo"
)

// Build-time values overridden by `-ldflags -X main.version=…` at
// release build time (see justfile's build-release recipe). One set
// for the whole binary — subcommands that previously owned their own
// copy now share these via the package-level symbols.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `deadzone — self-hosted MCP server for semantic doc search.

Usage:
  deadzone <subcommand> [flags]

Subcommands:
  server        run the MCP stdio server against a deadzone.db
  scrape        index libraries from libraries_sources.yaml into ./artifacts
  consolidate   merge ./artifacts/*.db into a single deadzone.db
  packs         upload/download/list per-lib artifacts on the rolling release

Run "deadzone <subcommand> -h" for subcommand flags.
`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "deadzone:", err)
		os.Exit(1)
	}
}

// dispatch is the top-level router. It owns the handling of the zero-
// arg help path, the global `-version`, and the unknown-subcommand
// case; anything else is forwarded to the matching runX function with
// the remaining argv tail so each subcommand owns its own flag.FlagSet.
func dispatch(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	switch args[0] {
	case "-version", "--version", "version":
		// Short-circuits before any embedder/DB load — same fast path
		// the old per-binary -version flags had, now collapsed to one.
		fmt.Println(buildinfo.Format("deadzone", version, commit, date))
		return nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	}

	sub, rest := args[0], args[1:]
	switch sub {
	case "server":
		return runServer(rest)
	case "scrape":
		return runScrape(rest)
	case "consolidate":
		return runConsolidate(rest)
	case "packs":
		return runPacks(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n\n", sub)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
		return errors.New("unreachable")
	}
}
