package main

// Tests for the `deadzone cache-signals` subcommand.
//
// The command is a thin glue layer between db.CurrentSchemaVersion and
// embed.HugotSignature(); these tests assert it (a) emits valid JSON
// (b) round-trips both fields with the expected types and (c) the
// values match the underlying constants exactly. The point is to catch
// drift between the Go source-of-truth and what CI workflows actually
// see on stdout.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	"github.com/laradji/deadzone/internal/embed"
)

func TestWriteCacheSignals_OutputShape(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCacheSignals(&buf); err != nil {
		t.Fatalf("writeCacheSignals: %v", err)
	}
	out := buf.Bytes()

	// Must be a single line of valid JSON ending in newline. Bash
	// consumers in the workflow pipe through `jq -r '.field'`, which
	// tolerates the trailing newline but not an extra one.
	if !bytes.HasSuffix(out, []byte("\n")) {
		t.Errorf("output should end with a newline; got %q", out)
	}
	if got := bytes.Count(out, []byte("\n")); got != 1 {
		t.Errorf("output should be a single line; got %d newlines in %q", got, out)
	}

	var got struct {
		SchemaVersion     int    `json:"schema_version"`
		EmbedderSignature string `json:"embedder_signature"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if got.SchemaVersion != db.CurrentSchemaVersion {
		t.Errorf("schema_version = %d, want %d (db.CurrentSchemaVersion)", got.SchemaVersion, db.CurrentSchemaVersion)
	}
	wantSig := embed.HugotSignature()
	if got.EmbedderSignature != wantSig {
		t.Errorf("embedder_signature = %q, want %q (embed.HugotSignature)", got.EmbedderSignature, wantSig)
	}
}
