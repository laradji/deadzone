// DISABLED — see issue #101. Per-artifact list flow is paused. Each
// Test* function is stubbed with t.Skip; pre-#101 bodies exercised
// Manifest.Packs which is no longer part of the schema.

package packs_test

import "testing"

func TestList_PrintsHeaderAndRows(t *testing.T)     { t.Skip(disabledSkip) }
func TestList_ShowsStateColumns(t *testing.T)       { t.Skip(disabledSkip) }
func TestList_MissingStateShowsEmDash(t *testing.T) { t.Skip(disabledSkip) }
func TestList_EmptyManifest(t *testing.T)           { t.Skip(disabledSkip) }
