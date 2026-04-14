package db_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/laradji/deadzone/internal/db"
	_ "turso.tech/database/tursogo"
)

// makeArtifact builds a fresh artifact file at
// <dir>/<slug>/artifact.db containing one libs row and the supplied
// docs. The slug matches the scraper's naming rule (leading "/"
// stripped, remaining "/" → "_", plus "_<version>" when version !=
// ""), so the test fixtures double as a regression check on the
// folder-per-(lib, version) layout introduced by #113. Returns the
// artifact's on-disk path. version is "" for single-version libs —
// the canonical form.
func makeArtifact(t *testing.T, dir, libID, version string, docs []db.Doc) string {
	t.Helper()
	libDir := filepath.Join(dir, artifactBasename(libID, version))
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", libDir, err)
	}
	path := filepath.Join(libDir, "artifact.db")

	a, err := db.OpenArtifact(path, metaFor(testEmbedder), libID, version)
	if err != nil {
		t.Fatalf("OpenArtifact %q version %q: %v", libID, version, err)
	}
	if err := db.UpsertLibIfNew(a, libID, version, testEmbedder); err != nil {
		a.Close()
		t.Fatalf("UpsertLibIfNew %q version %q: %v", libID, version, err)
	}
	for _, doc := range docs {
		doc.Version = version
		if err := db.Insert(a, doc, embedText(t, testEmbedder, doc)); err != nil {
			a.Close()
			t.Fatalf("Insert into artifact %q version %q: %v", libID, version, err)
		}
	}
	if err := db.UpdateLibCount(a, libID, version, len(docs)); err != nil {
		a.Close()
		t.Fatalf("UpdateLibCount %q version %q: %v", libID, version, err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close artifact %q version %q: %v", libID, version, err)
	}
	return path
}

// artifactBasename mirrors the scraper's slug derivation (see
// packs.Slug). Kept here in the test package (rather than imported)
// so the test catches drift if the packs rule changes — when both
// sides break together it's a deliberate refactor; when only one
// side breaks the diff is a red flag.
func artifactBasename(libID, version string) string {
	out := libID
	if len(out) > 0 && out[0] == '/' {
		out = out[1:]
	}
	b := []byte(out)
	for i, c := range b {
		if c == '/' {
			b[i] = '_'
		}
	}
	slug := string(b)
	if version == "" {
		return slug
	}
	return slug + "_" + version
}

func TestOpenArtifact_FreshWritesLibID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	a, err := db.OpenArtifact(path, metaFor(testEmbedder), "/foo/bar", "")
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}
	if a.ArtifactLibID != "/foo/bar" {
		t.Errorf("ArtifactLibID = %q, want %q", a.ArtifactLibID, "/foo/bar")
	}
	if a.ArtifactVersion != "" {
		t.Errorf("ArtifactVersion = %q, want empty", a.ArtifactVersion)
	}
	a.Close()

	// Reopen with the same (lib_id, version) should succeed.
	reopened, err := db.OpenArtifact(path, metaFor(testEmbedder), "/foo/bar", "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.ArtifactLibID != "/foo/bar" {
		t.Errorf("reopen ArtifactLibID = %q, want %q", reopened.ArtifactLibID, "/foo/bar")
	}
	reopened.Close()

	// Reopen with libID="" should also succeed and surface the
	// stored value — this is the consolidate code path.
	discovered, err := db.OpenArtifact(path, metaFor(testEmbedder), "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if discovered.ArtifactLibID != "/foo/bar" {
		t.Errorf("discovered ArtifactLibID = %q, want %q", discovered.ArtifactLibID, "/foo/bar")
	}
	discovered.Close()
}

// TestOpenArtifact_FreshWritesLibIDAndVersion pins the multi-version
// arm of the meta round-trip: both lib_id and version are persisted
// on first write, and the consolidate-mode reopen (libID == "")
// recovers both.
func TestOpenArtifact_FreshWritesLibIDAndVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh_v.db")
	a, err := db.OpenArtifact(path, metaFor(testEmbedder), "/hashicorp/terraform", "v1.14")
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}
	if a.ArtifactLibID != "/hashicorp/terraform" || a.ArtifactVersion != "v1.14" {
		t.Errorf("got (%q, %q), want (%q, %q)", a.ArtifactLibID, a.ArtifactVersion, "/hashicorp/terraform", "v1.14")
	}
	a.Close()

	discovered, err := db.OpenArtifact(path, metaFor(testEmbedder), "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if discovered.ArtifactLibID != "/hashicorp/terraform" || discovered.ArtifactVersion != "v1.14" {
		t.Errorf("discover got (%q, %q), want (%q, %q)", discovered.ArtifactLibID, discovered.ArtifactVersion, "/hashicorp/terraform", "v1.14")
	}
	discovered.Close()
}

func TestOpenArtifact_RejectsLibIDMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tampered.db")
	a, err := db.OpenArtifact(path, metaFor(testEmbedder), "/real/lib", "")
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}
	a.Close()

	_, err = db.OpenArtifact(path, metaFor(testEmbedder), "/wrong/lib", "")
	if err == nil {
		t.Fatal("expected ErrArtifactLibIDMismatch, got nil")
	}
	if !errors.Is(err, db.ErrArtifactLibIDMismatch) {
		t.Errorf("expected ErrArtifactLibIDMismatch, got %v", err)
	}
}

// TestOpenArtifact_RejectsVersionMismatch catches the failure mode
// where a (lib_id, v1.14) artifact gets misaddressed as (lib_id,
// v1.13) by a buggy caller — without this check the two versions
// would quietly collide in main.
func TestOpenArtifact_RejectsVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tampered_v.db")
	a, err := db.OpenArtifact(path, metaFor(testEmbedder), "/hashicorp/terraform", "v1.14")
	if err != nil {
		t.Fatalf("OpenArtifact: %v", err)
	}
	a.Close()

	_, err = db.OpenArtifact(path, metaFor(testEmbedder), "/hashicorp/terraform", "v1.13")
	if err == nil {
		t.Fatal("expected ErrArtifactLibIDMismatch (on version drift), got nil")
	}
	if !errors.Is(err, db.ErrArtifactLibIDMismatch) {
		t.Errorf("expected ErrArtifactLibIDMismatch, got %v", err)
	}
}

func TestOpenArtifact_DiscoverModeRequiresExistingFile(t *testing.T) {
	// libID="" against a non-existent file must NOT create a stub.
	path := filepath.Join(t.TempDir(), "ghost.db")
	_, err := db.OpenArtifact(path, metaFor(testEmbedder), "", "")
	if err == nil {
		t.Fatal("expected error opening missing artifact in discover mode, got nil")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("OpenArtifact created a stub file at %s; should have refused", path)
	}
}

func TestOpenArtifact_DiscoverModeOnMainDBFails(t *testing.T) {
	// A database opened via Open() (not OpenArtifact) has no lib_id
	// meta key. Asking OpenArtifact to discover its identity must
	// surface ErrArtifactLibIDMissing rather than silently returning
	// an empty lib_id that would later cause a DELETE WHERE lib_id=''
	// to wipe untagged rows in main.
	path := filepath.Join(t.TempDir(), "main.db")
	d, err := db.Open(path, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	d.Close()

	_, err = db.OpenArtifact(path, metaFor(testEmbedder), "", "")
	if err == nil {
		t.Fatal("expected ErrArtifactLibIDMissing, got nil")
	}
	if !errors.Is(err, db.ErrArtifactLibIDMissing) {
		t.Errorf("expected ErrArtifactLibIDMissing, got %v", err)
	}
}

func TestConsolidate_EmptyDirIsNoop(t *testing.T) {
	mainPath := filepath.Join(t.TempDir(), "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()

	emptyDir := t.TempDir()
	result, err := db.Consolidate(main, emptyDir)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Artifacts != 0 || result.DocsMerged != 0 || result.LibsMerged != 0 {
		t.Errorf("got %+v, want zero result", result)
	}

	var docCount, libCount int
	if err := main.QueryRow(`SELECT count(*) FROM docs`).Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if err := main.QueryRow(`SELECT count(*) FROM libs`).Scan(&libCount); err != nil {
		t.Fatalf("count libs: %v", err)
	}
	if docCount != 0 || libCount != 0 {
		t.Errorf("empty consolidate left rows behind: docs=%d libs=%d", docCount, libCount)
	}
}

func TestConsolidate_MergesMultipleArtifacts(t *testing.T) {
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")

	// Two artifacts, one doc each, distinct lib_ids. Each artifact
	// is its own .db file, isolated from the others — exactly what
	// the per-lib refactor promises.
	makeArtifact(t, artifactsDir, "/a/one", "", []db.Doc{
		{LibID: "/a/one", Title: "alpha", Content: "doc a"},
	})
	makeArtifact(t, artifactsDir, "/b/two", "", []db.Doc{
		{LibID: "/b/two", Title: "beta", Content: "doc b"},
	})

	mainPath := filepath.Join(tmp, "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()

	result, err := db.Consolidate(main, artifactsDir)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Artifacts != 2 || result.DocsMerged != 2 || result.LibsMerged != 2 {
		t.Errorf("got %+v, want {2,2,2}", result)
	}

	libIDs := readLibIDs(t, main)
	want := []string{"/a/one", "/b/two"}
	if len(libIDs) != len(want) {
		t.Fatalf("got libs %v, want %v", libIDs, want)
	}
	for i := range want {
		if libIDs[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, libIDs[i], want[i])
		}
	}
}

func TestConsolidate_ReplacesExistingLibInMain(t *testing.T) {
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")

	// First scrape: artifact with two docs.
	v1Path := makeArtifact(t, artifactsDir, "/x/y", "", []db.Doc{
		{LibID: "/x/y", Title: "v1 first", Content: "older content"},
		{LibID: "/x/y", Title: "v1 second", Content: "older content"},
	})

	mainPath := filepath.Join(tmp, "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()

	if _, err := db.Consolidate(main, artifactsDir); err != nil {
		t.Fatalf("first Consolidate: %v", err)
	}

	// Second scrape: rebuild the artifact with a single fresh doc.
	// This is exactly what `deadzone scrape -lib /x/y` does — the
	// scraper deletes the file and writes a new one.
	if err := os.Remove(v1Path); err != nil {
		t.Fatalf("remove v1 artifact: %v", err)
	}
	makeArtifact(t, artifactsDir, "/x/y", "", []db.Doc{
		{LibID: "/x/y", Title: "v2 only", Content: "newer content"},
	})

	if _, err := db.Consolidate(main, artifactsDir); err != nil {
		t.Fatalf("second Consolidate: %v", err)
	}

	var docCount int
	if err := main.QueryRow(`SELECT count(*) FROM docs WHERE lib_id = ?`, "/x/y").Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if docCount != 1 {
		t.Errorf("after replace: doc count = %d, want 1 (DELETE WHERE lib_id failed?)", docCount)
	}

	var title string
	if err := main.QueryRow(`SELECT title FROM docs WHERE lib_id = ?`, "/x/y").Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if title != "v2 only" {
		t.Errorf("title = %q, want %q", title, "v2 only")
	}
}

func TestConsolidate_EmbedderMismatchLeavesMainUnchanged(t *testing.T) {
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")

	// One healthy artifact + one tampered to look like it was built
	// with a different embedder. The healthy one is alphabetically
	// later so it would otherwise be merged second; if the merge
	// loop were not transactional, the first iteration would commit
	// before the second one detected the mismatch.
	makeArtifact(t, artifactsDir, "/aaa/healthy", "", []db.Doc{
		{LibID: "/aaa/healthy", Title: "ok", Content: "fine"},
	})
	tampered := makeArtifact(t, artifactsDir, "/zzz/bad", "", []db.Doc{
		{LibID: "/zzz/bad", Title: "bad", Content: "doomed"},
	})
	// Sneak through the driver to corrupt only the embedder_kind
	// row — same trick TestDB_RejectsPreLibsSchema uses to forge a
	// pre-libs database without rebuilding hugot from scratch.
	tamperEmbedderKind(t, tampered, "fake-embedder")

	// Seed main with one row so we can prove the rollback returned
	// it to its pre-call state instead of merging the healthy
	// artifact and aborting halfway.
	mainPath := filepath.Join(tmp, "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()
	seed := db.Doc{LibID: "/seed/lib", Title: "seed", Content: "seed content"}
	if err := db.Insert(main, seed, embedText(t, testEmbedder, seed)); err != nil {
		t.Fatalf("Insert seed: %v", err)
	}
	if err := db.UpsertLibIfNew(main, "/seed/lib", "", testEmbedder); err != nil {
		t.Fatalf("UpsertLibIfNew seed: %v", err)
	}

	_, err = db.Consolidate(main, artifactsDir)
	if err == nil {
		t.Fatal("expected ErrEmbedderMismatch, got nil")
	}
	if !errors.Is(err, db.ErrEmbedderMismatch) {
		t.Errorf("expected ErrEmbedderMismatch, got %v", err)
	}

	// Main must look exactly like it did pre-Consolidate: just the
	// seed row, neither artifact merged in.
	var docCount, libCount int
	if err := main.QueryRow(`SELECT count(*) FROM docs`).Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if err := main.QueryRow(`SELECT count(*) FROM libs`).Scan(&libCount); err != nil {
		t.Fatalf("count libs: %v", err)
	}
	if docCount != 1 {
		t.Errorf("docs = %d, want 1 (validation pass should run before any write)", docCount)
	}
	if libCount != 1 {
		t.Errorf("libs = %d, want 1", libCount)
	}
}

func TestConsolidate_SchemaMismatchLeavesMainUnchanged(t *testing.T) {
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")

	// Build a real artifact then drop the schema_version row to
	// fake a pre-libs (v0) artifact. Open() reads the missing key
	// as 0, which never matches CurrentSchemaVersion.
	path := makeArtifact(t, artifactsDir, "/old/lib", "", []db.Doc{
		{LibID: "/old/lib", Title: "x", Content: "x"},
	})
	dropSchemaVersion(t, path)

	mainPath := filepath.Join(tmp, "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()

	_, err = db.Consolidate(main, artifactsDir)
	if err == nil {
		t.Fatal("expected ErrSchemaMismatch, got nil")
	}
	if !errors.Is(err, db.ErrSchemaMismatch) {
		t.Errorf("expected ErrSchemaMismatch, got %v", err)
	}

	var docCount int
	if err := main.QueryRow(`SELECT count(*) FROM docs`).Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if docCount != 0 {
		t.Errorf("docs = %d, want 0 (main should be untouched)", docCount)
	}
}

// TestConsolidate_MergesMultipleVersionsOfSameLib is the load-bearing
// assertion behind #113's "artifacts keyed on (lib_id, version)"
// promise: two artifacts advertising the same lib_id but different
// versions must merge cleanly as two distinct (lib_id, version) rows
// in main, without either DELETE clobbering the other's data.
func TestConsolidate_MergesMultipleVersionsOfSameLib(t *testing.T) {
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")

	makeArtifact(t, artifactsDir, "/hashicorp/terraform", "v1.14", []db.Doc{
		{LibID: "/hashicorp/terraform", Title: "tf v1.14 intro", Content: "new stuff"},
	})
	makeArtifact(t, artifactsDir, "/hashicorp/terraform", "v1.13", []db.Doc{
		{LibID: "/hashicorp/terraform", Title: "tf v1.13 intro", Content: "older stuff"},
	})

	mainPath := filepath.Join(tmp, "main.db")
	main, err := db.Open(mainPath, metaFor(testEmbedder))
	if err != nil {
		t.Fatalf("Open main: %v", err)
	}
	defer main.Close()

	result, err := db.Consolidate(main, artifactsDir)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Artifacts != 2 || result.DocsMerged != 2 || result.LibsMerged != 2 {
		t.Errorf("got %+v, want {2,2,2}", result)
	}

	// Both libs rows present, same lib_id, different versions.
	rows, err := main.Query(`SELECT lib_id, version FROM libs ORDER BY version DESC`)
	if err != nil {
		t.Fatalf("select libs: %v", err)
	}
	defer rows.Close()
	type pair struct{ lib, ver string }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.lib, &p.ver); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	want := []pair{
		{"/hashicorp/terraform", "v1.14"},
		{"/hashicorp/terraform", "v1.13"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d libs rows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	// And both docs are preserved, not collapsed: the DELETE keyed on
	// (lib_id, version) must not have wiped the sibling version's doc.
	var docCount int
	if err := main.QueryRow(`SELECT count(*) FROM docs WHERE lib_id = ?`, "/hashicorp/terraform").Scan(&docCount); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if docCount != 2 {
		t.Errorf("doc count for terraform (both versions) = %d, want 2", docCount)
	}
}

// readLibIDs lists every lib_id in main's libs table in deterministic
// order. Used by the multi-artifact merge test to assert "every
// artifact's lib_id is now present in main".
func readLibIDs(t *testing.T, d *db.DB) []string {
	t.Helper()
	rows, err := d.Query(`SELECT lib_id FROM libs`)
	if err != nil {
		t.Fatalf("select lib_ids: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var libID string
		if err := rows.Scan(&libID); err != nil {
			t.Fatalf("scan lib_id: %v", err)
		}
		out = append(out, libID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate lib_ids: %v", err)
	}
	sort.Strings(out)
	return out
}

// tamperEmbedderKind rewrites the embedder_kind row in the meta table
// to simulate an artifact built with a different embedder. The artifact
// must already exist on disk and be closed.
func tamperEmbedderKind(t *testing.T, path, fakeKind string) {
	t.Helper()
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE meta SET value = ? WHERE key = ?`, fakeKind, "embedder_kind"); err != nil {
		t.Fatalf("tamper embedder_kind: %v", err)
	}
}

// dropSchemaVersion deletes the schema_version row from an artifact's
// meta table so it looks like a pre-libs (v0) database — Open will
// fail to read schema version and return ErrSchemaMismatch.
func dropSchemaVersion(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	if _, err := raw.Exec(`DELETE FROM meta WHERE key = ?`, "schema_version"); err != nil {
		t.Fatalf("delete schema_version: %v", err)
	}
}
