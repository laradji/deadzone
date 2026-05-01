package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDBRelease_TagFlagRequired pins the cobra MarkFlagRequired wiring
// for `dbrelease --tag` so a future flag rename can't silently drop the
// guard. See #161.
func TestDBRelease_TagFlagRequired(t *testing.T) {
	// Cobra's required-flag check looks at pflag.Changed, not the
	// bound variable's value, so reset both: the global default may
	// still be "" but a sibling test could have set Changed=true.
	tagFlag := dbreleaseCmd.Flags().Lookup("tag")
	if tagFlag == nil {
		t.Fatal("dbrelease cmd missing --tag flag")
	}
	tagFlag.Changed = false
	dbreleaseTag = ""

	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	// Empty slice (not nil) — nil makes cobra fall back to os.Args[1:],
	// which under `go test` carries flags like -test.v and breaks the
	// parser before it ever checks required flags.
	rootCmd.SetArgs([]string{"dbrelease"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatalf("expected required-flag error when --tag is missing; got nil (output: %s)", buf.String())
	}
	const want = `required flag(s) "tag" not set`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}
