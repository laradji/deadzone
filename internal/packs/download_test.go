// DISABLED — see issue #101. Per-artifact download flow is paused.
// Each Test* function below is stubbed with t.Skip so the package
// compiles against the new manifest schema. The pre-#101 test bodies
// referenced Manifest.Packs and the writeManifestWithPacks helper
// (removed as part of the schema rewrite); reviving them is a follow-up
// to the distribution-pipeline revival.

package packs_test

import "testing"

func TestDownload_FreshClone(t *testing.T)                           { t.Skip(disabledSkip) }
func TestDownload_AlreadyPresentSkipsHTTP(t *testing.T)              { t.Skip(disabledSkip) }
func TestDownload_TamperedLocalIsRedownloaded(t *testing.T)          { t.Skip(disabledSkip) }
func TestDownload_TamperedServerHardAborts(t *testing.T)             { t.Skip(disabledSkip) }
func TestDownload_TamperedServerDoesNotOverwriteGoodLocal(t *testing.T) {
	t.Skip(disabledSkip)
}
func TestDownload_LibFilterMatchesVersionedChildren(t *testing.T) { t.Skip(disabledSkip) }
func TestDownload_LibFilterExactMatch(t *testing.T)               { t.Skip(disabledSkip) }
func TestDownload_LibFilterNoMatchErrors(t *testing.T)            { t.Skip(disabledSkip) }
func TestDownload_MissingAssetOnServerErrors(t *testing.T)        { t.Skip(disabledSkip) }
func TestDownload_FetchesState(t *testing.T)                      { t.Skip(disabledSkip) }
func TestDownload_StateMissingOnServerIsWarning(t *testing.T)     { t.Skip(disabledSkip) }
func TestDownload_RequiresFetcher(t *testing.T)                   { t.Skip(disabledSkip) }
func TestDownload_RequiresRepo(t *testing.T)                      { t.Skip(disabledSkip) }
