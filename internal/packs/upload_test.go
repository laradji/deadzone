package packs_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/laradji/deadzone/internal/packs"
	_ "turso.tech/database/tursogo"
)

// fakeReleaser captures every method call so tests can assert exactly
// which uploads happened. errOn lets a test inject a failure on the Nth
// Upload call (1-indexed) to exercise the error path.
type fakeReleaser struct {
	mu          sync.Mutex
	ensureCalls []ensureCall
	uploadCalls []uploadCall
	errOnUpload int // 0 = never, N = fail the Nth Upload call
	ensureErr   error
}

type ensureCall struct{ Repo, Tag string }
type uploadCall struct{ Repo, Tag, File string }

func (f *fakeReleaser) EnsureRelease(ctx context.Context, repo, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls = append(f.ensureCalls, ensureCall{repo, tag})
	return f.ensureErr
}

func (f *fakeReleaser) Upload(ctx context.Context, repo, tag, file string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCalls = append(f.uploadCalls, uploadCall{repo, tag, file})
	if f.errOnUpload != 0 && len(f.uploadCalls) == f.errOnUpload {
		return errors.New("fake upload failure")
	}
	return nil
}

// fakeArtifact builds a real .db file with just the meta table populated
// so db.ReadArtifactMeta returns the supplied identity. Body is the
// payload of the file (used to differentiate sha256s across tests).
// The file goes into dir with the given basename.
func fakeArtifact(t *testing.T, dir, basename, libID, body string) string {
	t.Helper()
	path := filepath.Join(dir, basename)

	// First create the meta table via a real turso connection so the
	// file is structurally identical to a scraper-produced artifact.
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	rows := []struct{ k, v string }{
		{"lib_id", libID},
		{"embedder_kind", "hugot"},
		{"model_version", "sentence-transformers/all-MiniLM-L6-v2"},
	}
	for _, r := range rows {
		if _, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, r.k, r.v); err != nil {
			t.Fatalf("insert meta: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Append a body so each test can produce a different sha256
	// without rebuilding the whole sqlite file. The trailing bytes
	// don't change the meta-table query result because turso reads
	// the header pages, but they DO change the file's sha256.
	if body != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open append: %v", err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatalf("append body: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close append: %v", err)
		}
	}

	return path
}

// seedManifest writes a placeholder manifest with no packs to artifactsDir,
// returning the manifest path.
func seedManifest(t *testing.T, artifactsDir string) string {
	t.Helper()
	path := filepath.Join(artifactsDir, "manifest.yaml")
	if err := os.WriteFile(path, []byte("release_tag: packs\nrepo: laradji/deadzone\npacks: []\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestUpload_FreshArtifactIsUploaded(t *testing.T) {
	dir := t.TempDir()
	manifestPath := seedManifest(t, dir)
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-1")

	rel := &fakeReleaser{}
	summary, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Releaser:     rel,
	})
	if err != nil {
		t.Fatalf("RunUpload: %v", err)
	}
	if summary.Uploaded != 1 || summary.Skipped != 0 || summary.Preserved != 0 {
		t.Errorf("summary = %+v, want Uploaded:1", summary)
	}
	if len(rel.ensureCalls) != 1 {
		t.Errorf("EnsureRelease called %d times, want 1", len(rel.ensureCalls))
	}
	if len(rel.uploadCalls) != 1 {
		t.Errorf("Upload called %d times, want 1", len(rel.uploadCalls))
	}

	// Manifest should now have the new entry.
	m, err := packs.Load(manifestPath)
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}
	if len(m.Packs) != 1 {
		t.Fatalf("len(Packs) = %d, want 1", len(m.Packs))
	}
	if m.Packs[0].LibID != "/x/y" {
		t.Errorf("LibID = %q", m.Packs[0].LibID)
	}
	if m.Packs[0].Asset != "x_y.db" {
		t.Errorf("Asset = %q", m.Packs[0].Asset)
	}
	if len(m.Packs[0].SHA256) != 64 {
		t.Errorf("SHA256 length = %d, want 64", len(m.Packs[0].SHA256))
	}
	if m.Packs[0].ScrapedWithEmbedder != "hugot" {
		t.Errorf("ScrapedWithEmbedder = %q", m.Packs[0].ScrapedWithEmbedder)
	}
}

func TestUpload_SecondRunIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	manifestPath := seedManifest(t, dir)
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-1")

	// First run primes the manifest.
	rel1 := &fakeReleaser{}
	if _, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Releaser:     rel1,
	}); err != nil {
		t.Fatalf("first RunUpload: %v", err)
	}

	// Capture the manifest bytes after the first run so we can verify
	// the second run leaves them byte-identical.
	firstBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// Second run with a fresh fake — should make zero gh calls.
	rel2 := &fakeReleaser{}
	summary, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir,
		ManifestPath: manifestPath,
		Repo:         "laradji/deadzone-test",
		Releaser:     rel2,
	})
	if err != nil {
		t.Fatalf("second RunUpload: %v", err)
	}
	if summary.Uploaded != 0 || summary.Skipped != 1 {
		t.Errorf("summary = %+v, want Uploaded:0 Skipped:1", summary)
	}
	if len(rel2.ensureCalls) != 0 {
		t.Errorf("EnsureRelease called %d times on no-op run, want 0", len(rel2.ensureCalls))
	}
	if len(rel2.uploadCalls) != 0 {
		t.Errorf("Upload called %d times on no-op run, want 0", len(rel2.uploadCalls))
	}

	secondBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Errorf("manifest bytes changed on idempotent run\nfirst:\n%s\nsecond:\n%s", firstBytes, secondBytes)
	}
}

func TestUpload_ChangedArtifactIsReuploaded(t *testing.T) {
	dir := t.TempDir()
	manifestPath := seedManifest(t, dir)
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-v1")

	rel1 := &fakeReleaser{}
	if _, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir, ManifestPath: manifestPath, Repo: "laradji/deadzone-test", Releaser: rel1,
	}); err != nil {
		t.Fatalf("first RunUpload: %v", err)
	}

	// Rebuild the same lib_id with a different body → different sha256.
	if err := os.Remove(filepath.Join(dir, "x_y.db")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-v2-much-longer-payload")

	rel2 := &fakeReleaser{}
	summary, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir, ManifestPath: manifestPath, Repo: "laradji/deadzone-test", Releaser: rel2,
	})
	if err != nil {
		t.Fatalf("second RunUpload: %v", err)
	}
	if summary.Uploaded != 1 || summary.Skipped != 0 {
		t.Errorf("summary = %+v, want Uploaded:1 Skipped:0", summary)
	}
	if len(rel2.uploadCalls) != 1 {
		t.Errorf("Upload called %d times, want 1", len(rel2.uploadCalls))
	}
}

func TestUpload_PreservesUnseenManifestEntries(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed manifest with TWO entries but only stage ONE artifact
	// on disk. The "ghost" entry must survive the upload run.
	manifestYAML := `release_tag: packs
repo: laradji/deadzone
packs:
  - lib_id: /ghost/lib
    asset: ghost_lib.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000001
    size: 100
    indexed_at: 2026-04-01T00:00:00Z
  - lib_id: /x/y
    asset: x_y.db
    sha256: 0000000000000000000000000000000000000000000000000000000000000002
    size: 100
    indexed_at: 2026-04-01T00:00:00Z
`
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Only /x/y is on disk. Different sha256 → should re-upload.
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-fresh")

	rel := &fakeReleaser{}
	summary, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir, ManifestPath: manifestPath, Repo: "laradji/deadzone-test", Releaser: rel,
	})
	if err != nil {
		t.Fatalf("RunUpload: %v", err)
	}
	if summary.Uploaded != 1 || summary.Preserved != 1 {
		t.Errorf("summary = %+v, want Uploaded:1 Preserved:1", summary)
	}

	m, err := packs.Load(manifestPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Packs) != 2 {
		t.Errorf("len(Packs) = %d, want 2 (ghost should still be present)", len(m.Packs))
	}
	if _, _, ok := m.Find("/ghost/lib"); !ok {
		t.Error("ghost entry was dropped from the manifest")
	}
}

func TestUpload_FailureLeavesManifestUntouched(t *testing.T) {
	dir := t.TempDir()
	manifestPath := seedManifest(t, dir)
	fakeArtifact(t, dir, "x_y.db", "/x/y", "body-1")

	originalBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	rel := &fakeReleaser{errOnUpload: 1}
	_, err = packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir, ManifestPath: manifestPath, Repo: "laradji/deadzone-test", Releaser: rel,
	})
	if err == nil {
		t.Fatal("expected error from failing fake upload, got nil")
	}

	finalBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(originalBytes) != string(finalBytes) {
		t.Errorf("manifest was rewritten despite upload failure\nbefore:\n%s\nafter:\n%s", originalBytes, finalBytes)
	}
}

func TestUpload_RequiresReleaser(t *testing.T) {
	_, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: "anywhere", ManifestPath: "any.yaml", Repo: "x/y",
	})
	if err == nil {
		t.Fatal("expected error for missing Releaser, got nil")
	}
}

func TestUpload_RequiresRepo(t *testing.T) {
	dir := t.TempDir()
	manifestPath := seedManifest(t, dir)
	_, err := packs.RunUpload(context.Background(), packs.UploadOptions{
		ArtifactsDir: dir, ManifestPath: manifestPath, Releaser: &fakeReleaser{},
	})
	if err == nil {
		t.Fatal("expected error for missing Repo, got nil")
	}
}

// compile-time guard: fakeReleaser must satisfy the Releaser interface.
var _ packs.Releaser = (*fakeReleaser)(nil)
