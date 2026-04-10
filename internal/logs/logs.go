// Package logs centralises slog wiring for deadzone's binaries so the
// MCP server and scraper produce consistent JSON-on-stderr output.
//
// Both cmd/server and cmd/scraper call New at startup with os.Stderr.
// Server stdout is reserved for the MCP JSON-RPC channel — anything
// written there that isn't a valid JSON-RPC message disconnects the
// client — so logs MUST go to stderr. The scraper writes to stderr too
// for consistency, even though it's a one-shot CLI without that
// constraint.
package logs

import (
	"io"
	"log/slog"
)

// New returns a JSON slog.Logger writing to w.
//
// When verbose is true the minimum level is Debug, which both unlocks
// per-doc traces in the scraper (slog.Debug) and lets binaries opt into
// extra fields gated on the same flag. Otherwise the level is Info.
func New(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
