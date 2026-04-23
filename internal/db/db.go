// Package db manages the local turso database that backs deadzone's
// vector-based semantic search. Documents are stored as TEXT alongside an
// F32_BLOB embedding column whose width is set at Open time from the
// embedder's reported Dim, and queries are ranked by vector_distance_cos
// against the query's embedding.
//
// The package is intentionally embedder-agnostic: it does not import
// internal/embed, and the embedder's identity travels through the Meta
// struct supplied by the caller. The meta table records this identity in
// the database itself so a binary opening an existing file can detect
// (and refuse) a mismatch with the embedder it was asked to use.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "turso.tech/database/tursogo"
)

// ErrEmbedderMismatch is returned by Open when the meta stored in an
// existing database disagrees with the Meta the caller passed in. Callers
// should treat this as fatal: there is no safe way to mix vectors produced
// by different embedders in the same docs table. Wrap with errors.Is to
// detect.
var ErrEmbedderMismatch = errors.New("embedder metadata mismatch")

// ErrSchemaMismatch is returned by Open when the database's stored schema
// version does not match CurrentSchemaVersion. Callers should treat this
// as fatal: the current code cannot read a database produced by a build
// with a different table layout. Use a fresh database file (drop and
// re-scrape) until an in-place migration is implemented. Wrap with
// errors.Is to detect.
var ErrSchemaMismatch = errors.New("database schema version mismatch")

// ErrArtifactLibIDMissing is returned by OpenArtifact when the caller
// passes libID == "" (i.e. asks the artifact to identify itself) but
// the on-disk meta table has no lib_id key. Wrap with errors.Is to
// detect — the consolidate path treats it as a structural problem with
// the artifact file itself, not a transient I/O error.
var ErrArtifactLibIDMissing = errors.New("artifact has no lib_id in meta")

// ErrArtifactLibIDMismatch is returned by OpenArtifact when both the
// stored and the requested lib_id are non-empty but disagree. Catches
// the failure mode where an artifact gets renamed on disk so its
// filename and recorded lib_id no longer match, which would otherwise
// silently merge rows under the wrong key.
var ErrArtifactLibIDMismatch = errors.New("artifact lib_id mismatch")

// CurrentSchemaVersion is the on-disk schema version written by this
// build. Bump whenever the table layout changes in a non-backwards-
// compatible way (e.g. a new required table like libs). Stored in the
// meta table at first Open and cross-checked on every subsequent open
// against this constant; a mismatch surfaces as ErrSchemaMismatch.
const CurrentSchemaVersion = 4

// Meta describes the embedder a database was created with. It is written
// to the meta table the first time a fresh DB is opened and cross-checked
// on every subsequent open.
//
// Equality is by value: every field must match exactly for a reopen to be
// accepted. Bumping ModelVersion in the embedder is therefore the standard
// way to invalidate previously-indexed databases.
type Meta struct {
	EmbedderKind string
	EmbeddingDim int
	ModelVersion string
}

// DB wraps *sql.DB with the Meta the database was opened with so that
// Insert and SearchByEmbedding can validate vector lengths without
// re-reading the meta table on every call. *sql.DB is embedded so callers
// can still use QueryRow, Exec, Close, etc. directly on a *DB.
//
// ArtifactLibID / ArtifactVersion are populated only when the database
// was opened via OpenArtifact. Together they form the canonical
// (lib_id, version) slot this artifact carries (read from the meta
// table at open time). ArtifactVersion is "" for single-version libs,
// matching the on-wire canonical form. The main consolidated database
// always leaves both empty — the libs table is the source of truth
// there.
type DB struct {
	*sql.DB
	Meta            Meta
	ArtifactLibID   string
	ArtifactVersion string
}

// Doc represents a documentation snippet stored in the docs table.
//
// Version is the canonical form for single-version libs: empty string,
// not NULL. Multi-version libs pass the version tag as recorded in the
// registry (e.g. "v1.14"). LibID always carries the base lib_id — the
// "/base/version" concat that earlier builds emitted is gone.
type Doc struct {
	LibID   string
	Version string
	Title   string
	Content string
}

// Open opens (or creates) a local turso database at path and ensures the
// schema is in place. The meta argument describes the embedder the caller
// intends to use:
//
//   - On a fresh database, the meta is persisted and the docs table is
//     created with an F32_BLOB column whose width matches meta.EmbeddingDim.
//   - On an existing database, the stored meta must equal the supplied
//     meta — otherwise ErrEmbedderMismatch is returned, wrapped with the
//     conflicting values so the user knows what to do (typically: rebuild
//     the database with a fresh file).
//
// tursogo's DSN is a bare path — the "file:" prefix used by libSQL is NOT
// stripped and would create a file literally named "file:<path>".
func Open(path string, meta Meta) (*DB, error) {
	if err := validateMeta(meta); err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	raw, err := sql.Open("turso", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// tursogo is BETA; serialize connections defensively to avoid any
	// potential driver-level races. Harmless for the MCP server's
	// one-query-at-a-time workload.
	// TODO: revisit once tursogo reaches a stable release.
	raw.SetMaxOpenConns(1)

	// The meta table is created unconditionally on every Open so even a
	// freshly-created file is queryable for its embedder identity.
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create meta table: %w", err)
	}

	stored, storedSchemaVersion, hasMeta, err := readMeta(raw)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("read meta: %w", err)
	}

	if hasMeta {
		if err := checkStoredMeta(stored, storedSchemaVersion, meta); err != nil {
			raw.Close()
			return nil, err
		}
	} else {
		if err := writeMeta(raw, meta); err != nil {
			raw.Close()
			return nil, fmt.Errorf("write meta: %w", err)
		}
	}

	// The docs table's vector width is determined by meta.EmbeddingDim.
	// CREATE TABLE IF NOT EXISTS is safe across reopens because the
	// mismatch check above has already guaranteed the dim hasn't changed
	// since the table was first created.
	docsSchema := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS docs (
		id INTEGER PRIMARY KEY,
		lib_id TEXT NOT NULL,
		version TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding F32_BLOB(%d) NOT NULL
	)`, meta.EmbeddingDim)
	if _, err := raw.Exec(docsSchema); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create docs table: %w", err)
	}
	if _, err := raw.Exec(`CREATE INDEX IF NOT EXISTS idx_docs_lib_id ON docs(lib_id)`); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create lib_id index: %w", err)
	}

	// libs is a per-lib catalog table that backs search_libraries:
	// one row per indexed library, holding an embedding of the lib_id
	// text plus the corpus size for the lib. Vector width matches the
	// docs table for the same reason — both columns have to be openable
	// by the same Embedder, which the meta cross-check above guarantees.
	libsSchema := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS libs (
		lib_id    TEXT NOT NULL,
		version   TEXT NOT NULL DEFAULT '',
		doc_count INTEGER NOT NULL DEFAULT 0,
		embedding F32_BLOB(%d) NOT NULL,
		PRIMARY KEY (lib_id, version)
	)`, meta.EmbeddingDim)
	if _, err := raw.Exec(libsSchema); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create libs table: %w", err)
	}
	if _, err := raw.Exec(`CREATE INDEX IF NOT EXISTS libs_doc_count_idx ON libs(doc_count DESC)`); err != nil {
		raw.Close()
		return nil, fmt.Errorf("create libs_doc_count_idx: %w", err)
	}

	return &DB{DB: raw, Meta: meta}, nil
}

// ErrReaderNotInitialized is returned by OpenReader when the file
// exists but has no meta rows — i.e. a mutator has never opened it to
// lay down the schema. Readers must not bootstrap DBs themselves (that
// is exclusively the mutator path's job), so the right fix is to run a
// mutator command (consolidate / dbrelease / scrape) first. Wrap with
// errors.Is to detect.
var ErrReaderNotInitialized = errors.New("database was never initialized by a mutator")

// OpenReader opens an existing deadzone database in read-only mode. It
// is the entry point used by `deadzone server` and any other caller
// that issues only SELECTs. Unlike Open it does NOT run any DDL and
// does NOT accept writes: a PRAGMA query_only = 1 is set on the
// connection immediately after open so any subsequent INSERT / UPDATE /
// DELETE / CREATE TABLE returns a SQLite "attempt to write a readonly
// database" error.
//
// Motivation: Open's unconditional CREATE TABLE IF NOT EXISTS meta
// takes a write-intent lock on every boot, which serializes concurrent
// MCP server processes against the same deadzone.db file. OpenReader
// skips all schema DDL so N reader processes can coexist without
// SQLITE_BUSY, while preserving the usual schema + embedder meta
// cross-check semantics.
//
// Contract vs Open:
//
//   - Same validateMeta / schema_version / embedder meta checks.
//     ErrSchemaMismatch and ErrEmbedderMismatch surface exactly as they
//     do from Open, so callers can errors.Is them identically.
//   - The file MUST exist. If it does not, os.ErrNotExist is returned
//     wrapped with the path — readers never fabricate empty stubs.
//   - The file MUST have been initialized by a mutator: if the meta
//     table is absent or empty, ErrReaderNotInitialized is returned.
//   - Writes fail fast via query_only and (as a second line of defence)
//     the caller-facing *DB has no helper that attempts mutations.
func OpenReader(path string, meta Meta) (*DB, error) {
	if err := validateMeta(meta); err != nil {
		return nil, fmt.Errorf("open db reader: %w", err)
	}

	// Stat first so a missing file surfaces as os.ErrNotExist rather
	// than tursogo quietly creating a new empty DB. The mutator path
	// (Open) intentionally creates-if-missing; the reader path never
	// does.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open db reader %s: %w", path, err)
	}

	raw, err := sql.Open("turso", path)
	if err != nil {
		return nil, fmt.Errorf("open db reader: %w", err)
	}

	// Same tursogo-beta defensive serialization as Open. We also pin
	// the idle pool to one connection with unlimited lifetime so the
	// single connection we set PRAGMA query_only on below is the exact
	// connection every subsequent query reuses. Without that pin, the
	// pool could close and re-open an unpragma'd connection under load,
	// silently dropping the read-only contract.
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	raw.SetConnMaxLifetime(0)
	raw.SetConnMaxIdleTime(0)

	// query_only turns the connection into a strict SELECT-only view of
	// the database at the SQLite layer. It is intentionally set BEFORE
	// any readMeta so even the meta-validation path cannot accidentally
	// issue a write. The tursogo DSN is a bare path and does not
	// support a ?mode=ro query string (documented at db.go:117), so
	// this pragma is the portable way to enforce the read-only contract.
	if _, err := raw.Exec(`PRAGMA query_only = 1`); err != nil {
		raw.Close()
		return nil, fmt.Errorf("open db reader %s: set query_only: %w", path, err)
	}

	// Probe sqlite_master before readMeta so a DB file that exists but
	// has no meta table at all (e.g. a stray `touch deadzone.db`, or a
	// half-failed bootstrap) surfaces as ErrReaderNotInitialized rather
	// than as a raw "no such table: meta" driver error. Without this
	// probe the caller can't tell a "please run a mutator first"
	// situation from an honest I/O failure via errors.Is.
	var metaTableCount int
	if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&metaTableCount); err != nil {
		raw.Close()
		return nil, fmt.Errorf("open db reader %s: probe meta: %w", path, err)
	}
	if metaTableCount == 0 {
		raw.Close()
		return nil, fmt.Errorf("%w: %s: run `deadzone consolidate` or `deadzone dbrelease` to populate it",
			ErrReaderNotInitialized, path)
	}

	stored, storedSchemaVersion, hasMeta, err := readMeta(raw)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("open db reader %s: read meta: %w", path, err)
	}
	if !hasMeta {
		raw.Close()
		return nil, fmt.Errorf("%w: %s: run `deadzone consolidate` or `deadzone dbrelease` to populate it",
			ErrReaderNotInitialized, path)
	}

	if err := checkStoredMeta(stored, storedSchemaVersion, meta); err != nil {
		raw.Close()
		return nil, err
	}

	return &DB{DB: raw, Meta: stored}, nil
}

// OpenArtifact opens (or creates) a per-(lib_id, version) artifact
// database. An artifact carries a single (lib_id, version) pair
// recorded in its meta table; the recorded values are the source of
// truth for which library slot the artifact's docs and libs rows
// belong to.
//
// (libID, version) semantics:
//
//   - libID != "" — the caller knows which (lib_id, version) slot this
//     artifact represents (e.g. the scraper). On a fresh file the pair
//     is written. On an existing file both values must match the
//     stored ones, otherwise ErrArtifactLibIDMismatch is returned.
//     Version may be "" for single-version libs — that is the
//     canonical form.
//
//   - libID == "" — the caller is reading an existing artifact and
//     wants to discover its (lib_id, version) (e.g. consolidate).
//     Version must also be "" in this mode; it is populated from the
//     stored meta. The file must already exist; if it doesn't, an
//     os.ErrNotExist-wrapped error is returned without creating a
//     stub. If the file exists but has no lib_id stored,
//     ErrArtifactLibIDMissing is returned.
//
// Embedder meta and schema version validation are inherited from Open
// — an artifact built with a different embedder than the caller's
// surfaces as ErrEmbedderMismatch, exactly like the main DB.
func OpenArtifact(path string, meta Meta, libID, version string) (*DB, error) {
	// Refuse to fabricate a stub file when the caller is in
	// "read existing artifact" mode. Lets the consolidate path
	// distinguish "no such file" from "file exists but is malformed"
	// without inspecting the resulting *DB.
	if libID == "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("open artifact %s: %w", path, err)
		}
	}

	d, err := Open(path, meta)
	if err != nil {
		return nil, err
	}

	stored, hasStored, err := readArtifactLibID(d.DB)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("open artifact %s: read lib_id: %w", path, err)
	}
	storedVersion, _, err := readArtifactVersion(d.DB)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("open artifact %s: read version: %w", path, err)
	}

	switch {
	case libID != "" && hasStored:
		if stored != libID {
			d.Close()
			return nil, fmt.Errorf("%w: stored lib_id=%q requested=%q (file=%s)",
				ErrArtifactLibIDMismatch, stored, libID, path)
		}
		if storedVersion != version {
			d.Close()
			return nil, fmt.Errorf("%w: stored version=%q requested=%q (file=%s, lib_id=%s)",
				ErrArtifactLibIDMismatch, storedVersion, version, path, libID)
		}
	case libID != "" && !hasStored:
		if err := writeArtifactLibID(d.DB, libID); err != nil {
			d.Close()
			return nil, fmt.Errorf("open artifact %s: write lib_id: %w", path, err)
		}
		if err := writeArtifactVersion(d.DB, version); err != nil {
			d.Close()
			return nil, fmt.Errorf("open artifact %s: write version: %w", path, err)
		}
		stored = libID
		storedVersion = version
	case libID == "" && !hasStored:
		d.Close()
		return nil, fmt.Errorf("%w: %s", ErrArtifactLibIDMissing, path)
	}

	d.ArtifactLibID = stored
	d.ArtifactVersion = storedVersion
	return d, nil
}

// readArtifactLibID returns the lib_id meta value if present. The
// boolean is false (with no error) when the row is simply absent — the
// "main DB, no artifact identity" case — so callers can distinguish
// "missing" from "I/O failure" without an errors.Is dance.
func readArtifactLibID(raw *sql.DB) (string, bool, error) {
	var v string
	err := raw.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKeyLibID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// writeArtifactLibID inserts the lib_id meta row. Caller is responsible
// for guaranteeing the row does not already exist (OpenArtifact only
// calls this on the !hasStored branch); a UNIQUE-constraint failure
// here means the readArtifactLibID call above raced with a writer,
// which the single-connection scraper rules out by construction.
func writeArtifactLibID(raw *sql.DB, libID string) error {
	_, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaKeyLibID, libID)
	return err
}

// readArtifactVersion returns the version meta value if present. A
// missing row returns ("", false, nil) — older artifacts that
// predate #113 never wrote this key and we want them to surface as
// ErrSchemaMismatch at Open, not here.
func readArtifactVersion(raw *sql.DB) (string, bool, error) {
	var v string
	err := raw.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKeyVersion).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// writeArtifactVersion inserts the version meta row. Like
// writeArtifactLibID, the caller guarantees the row does not already
// exist. Empty string is the canonical single-version form and is
// written verbatim so round-tripping the value preserves the
// distinction between "single-version lib" and "never wrote version".
func writeArtifactVersion(raw *sql.DB, version string) error {
	_, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, metaKeyVersion, version)
	return err
}

// Insert stores a Doc along with its precomputed embedding. The embedding
// must have exactly db.Meta.EmbeddingDim components — the dimension travels
// with the *DB rather than being a package-level constant so a single
// binary can support multiple embedder kinds.
func Insert(db *DB, doc Doc, embedding []float32) error {
	if len(embedding) != db.Meta.EmbeddingDim {
		return fmt.Errorf("insert doc: embedding length %d, want %d", len(embedding), db.Meta.EmbeddingDim)
	}
	_, err := db.Exec(
		`INSERT INTO docs(lib_id, version, title, content, embedding) VALUES (?, ?, ?, ?, vector(?))`,
		doc.LibID, doc.Version, doc.Title, doc.Content, formatVector(embedding),
	)
	if err != nil {
		return fmt.Errorf("insert doc: %w", err)
	}
	return nil
}

// SearchByEmbedding returns the top-k Docs ranked by cosine distance to
// queryVec (lower = more similar). k defaults to 10 if <= 0. The query
// vector must have db.Meta.EmbeddingDim components.
//
// The (libID, version) filter is three-valued:
//
//   - libID == ""                        — no filter (version is ignored).
//   - libID != "" and version == ""      — match every indexed version of
//     the lib (WHERE lib_id = ?). The single-version case is the same query
//     since those rows carry version = "".
//   - libID != "" and version != ""      — pin to a specific version
//     (WHERE lib_id = ? AND version = ?).
//
// Passing version without libID is a usage error at the MCP tool layer;
// this function does not enforce it so unit tests stay small.
func SearchByEmbedding(db *DB, queryVec []float32, libID, version string, k int) ([]Doc, error) {
	if len(queryVec) != db.Meta.EmbeddingDim {
		return nil, fmt.Errorf("search: query vector length %d, want %d", len(queryVec), db.Meta.EmbeddingDim)
	}
	if k <= 0 {
		k = 10
	}

	q := formatVector(queryVec)

	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case libID == "":
		rows, err = db.Query(
			`SELECT lib_id, version, title, content
			 FROM docs
			 ORDER BY vector_distance_cos(embedding, vector(?)) ASC
			 LIMIT ?`,
			q, k,
		)
	case version == "":
		rows, err = db.Query(
			`SELECT lib_id, version, title, content
			 FROM docs
			 WHERE lib_id = ?
			 ORDER BY vector_distance_cos(embedding, vector(?)) ASC
			 LIMIT ?`,
			libID, q, k,
		)
	default:
		rows, err = db.Query(
			`SELECT lib_id, version, title, content
			 FROM docs
			 WHERE lib_id = ? AND version = ?
			 ORDER BY vector_distance_cos(embedding, vector(?)) ASC
			 LIMIT ?`,
			libID, version, q, k,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []Doc
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.LibID, &d.Version, &d.Title, &d.Content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// LibEmbedder is the minimal subset of an embedder that UpsertLibIfNew
// needs. Defining a local interface keeps the db package free of any
// import on internal/embed (matching the package-level rule the docs
// table already follows) while still letting tests pass a counting
// wrapper that asserts the embed call is actually skipped on the
// idempotent re-upsert path.
//
// EmbedDocument (not EmbedQuery) is the right method here: a lib_id row
// is indexed content, retrieved against user queries via
// SearchLibsByEmbedding. Retrieval-trained models use different prefixes
// for the two sides and mixing them up silently degrades match quality.
type LibEmbedder interface {
	EmbedDocument(text string) ([]float32, error)
}

// LibInfo is one row of the libs table as returned by SearchLibsByEmbedding
// and TopLibsByDocCount. Distance is the raw cosine distance from the
// query vector (lower is better, 0 is identical, 2 is maximally far);
// callers convert it to a 1 - dist match score before serializing to
// the wire so the LLM sees a monotonically-good number. The doc-count
// path returns Distance = 0 (no query was issued), and the search path
// fills it in from vector_distance_cos.
type LibInfo struct {
	LibID    string
	Version  string
	DocCount int
	Distance float32
}

// UpsertLibIfNew inserts a row into the libs table for (libID, version)
// iff one doesn't already exist. The embedding is computed from the
// lib_id text alone with "/" and "-" turned into spaces so the encoder
// sees something resembling natural language
// ("/hashicorp/terraform-provider-aws" → "hashicorp terraform provider
// aws"). Version is intentionally NOT mixed into the embed text:
// multiple versions of the same lib should rank identically against a
// free-text library-name query, so the embedding stays keyed on the
// base identity while the row's (lib_id, version) primary key keeps
// the versions distinct on disk.
//
// Re-running this function for an existing (lib_id, version) pair is a
// fast no-op that does NOT call EmbedDocument — the issue's "at most
// one Embed call per (lib, version) per database" guarantee is
// enforced here and verified by tests that count the call against a
// wrapping LibEmbedder.
//
// Version is the canonical empty string for single-version libs; the
// primary key is on (lib_id, version) so the same base lib with two
// versions cleanly produces two rows.
func UpsertLibIfNew(d *DB, libID, version string, e LibEmbedder) error {
	if libID == "" {
		return errors.New("upsert lib: libID must not be empty")
	}
	var existing int
	if err := d.QueryRow(`SELECT count(*) FROM libs WHERE lib_id = ? AND version = ?`, libID, version).Scan(&existing); err != nil {
		return fmt.Errorf("upsert lib %q version %q: check exists: %w", libID, version, err)
	}
	if existing > 0 {
		return nil
	}
	vec, err := e.EmbedDocument(normalizeLibIDText(libID))
	if err != nil {
		return fmt.Errorf("upsert lib %q version %q: embed: %w", libID, version, err)
	}
	if len(vec) != d.Meta.EmbeddingDim {
		return fmt.Errorf("upsert lib %q version %q: embedding length %d, want %d", libID, version, len(vec), d.Meta.EmbeddingDim)
	}
	if _, err := d.Exec(
		`INSERT INTO libs (lib_id, version, doc_count, embedding) VALUES (?, ?, 0, vector(?))`,
		libID, version, formatVector(vec),
	); err != nil {
		return fmt.Errorf("upsert lib %q version %q: insert: %w", libID, version, err)
	}
	return nil
}

// UpdateLibCount sets the doc_count for an existing libs row keyed on
// (libID, version). The scraper calls this once per lib at the end of
// a run with the actual number of docs that were inserted, so
// search_libraries can surface "how well-indexed is this lib" without
// recounting on every query. Updating a row that does not exist is
// silently a no-op (zero rows affected) — the scraper is responsible
// for calling UpsertLibIfNew first.
func UpdateLibCount(d *DB, libID, version string, count int) error {
	if libID == "" {
		return errors.New("update lib count: libID must not be empty")
	}
	if count < 0 {
		return fmt.Errorf("update lib count: count must be >= 0, got %d", count)
	}
	if _, err := d.Exec(`UPDATE libs SET doc_count = ? WHERE lib_id = ? AND version = ?`, count, libID, version); err != nil {
		return fmt.Errorf("update lib count %q version %q: %w", libID, version, err)
	}
	return nil
}

// SearchLibsByEmbedding returns the top-`limit` libs ranked by cosine
// distance to queryVec, breaking ties by doc_count desc so a
// well-indexed lib outranks a barely-touched one when both score
// equally on semantic match. The query vector must have
// db.Meta.EmbeddingDim components — the same constraint as
// SearchByEmbedding, for the same reason (cross-embedder vectors are
// nonsense).
func SearchLibsByEmbedding(d *DB, queryVec []float32, limit int) ([]LibInfo, error) {
	if len(queryVec) != d.Meta.EmbeddingDim {
		return nil, fmt.Errorf("search libs: query vector length %d, want %d", len(queryVec), d.Meta.EmbeddingDim)
	}
	if limit <= 0 {
		limit = 10
	}
	// Tie-break on lib_id then version so the same base lib's versions
	// return in a deterministic order when they score identically
	// (which they always do — the embedding doesn't include version).
	rows, err := d.Query(
		`SELECT lib_id, version, doc_count, vector_distance_cos(embedding, vector(?)) AS dist
		 FROM libs
		 ORDER BY dist ASC, doc_count DESC, lib_id ASC, version ASC
		 LIMIT ?`,
		formatVector(queryVec), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search libs: %w", err)
	}
	defer rows.Close()

	results := make([]LibInfo, 0, limit)
	for rows.Next() {
		var (
			info LibInfo
			// turso returns vector_distance_cos as a REAL; scan into a
			// float64 then narrow to float32 to match LibInfo.Distance
			// (database/sql's Scan doesn't bind directly to *float32).
			dist float64
		)
		if err := rows.Scan(&info.LibID, &info.Version, &info.DocCount, &dist); err != nil {
			return nil, fmt.Errorf("search libs: scan: %w", err)
		}
		info.Distance = float32(dist)
		results = append(results, info)
	}
	return results, rows.Err()
}

// TopLibsByDocCount returns the top-`limit` libs ranked by doc_count
// descending. This is the cheap "no query" path that powers the empty-
// name branch of search_libraries: an LLM exploring an unfamiliar
// corpus gets a useful "what's even in here" answer without paying for
// an embedder call. Distance is left at 0 (the row's match_score in
// the wire format ends up at 1.0).
func TopLibsByDocCount(d *DB, limit int) ([]LibInfo, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.Query(
		`SELECT lib_id, version, doc_count
		 FROM libs
		 ORDER BY doc_count DESC, lib_id ASC, version ASC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("top libs: %w", err)
	}
	defer rows.Close()

	results := make([]LibInfo, 0, limit)
	for rows.Next() {
		var info LibInfo
		if err := rows.Scan(&info.LibID, &info.Version, &info.DocCount); err != nil {
			return nil, fmt.Errorf("top libs: scan: %w", err)
		}
		results = append(results, info)
	}
	return results, rows.Err()
}

// normalizeLibIDText turns a path-like lib_id into a string the encoder
// can embed as natural language: "/" and "-" become spaces, surrounding
// whitespace is trimmed. The transformation is intentionally trivial
// and lossy — the embedder's job is to project semantic content, not
// to roundtrip the lib_id. Centroid-of-doc-embeddings was rejected in
// the issue because it has no recompute-on-partial-rescrape problem
// and the lib_id text alone gives the encoder enough signal to handle
// queries like "terraform aws" → "/hashicorp/terraform-provider-aws".
func normalizeLibIDText(libID string) string {
	return strings.TrimSpace(strings.NewReplacer("/", " ", "-", " ").Replace(libID))
}

// Meta table key names. Defined as constants to keep the read/write paths
// in sync and to give callers a single place to look for "what does the
// meta table actually contain".
const (
	metaKeyEmbedderKind  = "embedder_kind"
	metaKeyEmbeddingDim  = "embedding_dim"
	metaKeyModelVersion  = "model_version"
	metaKeySchemaVersion = "schema_version"
	// metaKeyLibID is written by OpenArtifact and absent from the main
	// consolidated database. The reader (readMeta) intentionally
	// ignores any meta keys it does not recognize, so adding this key
	// is backwards-compatible with the existing schema version.
	metaKeyLibID = "lib_id"
	// metaKeyVersion is the per-artifact version tag. Same lifecycle
	// as metaKeyLibID: written once at OpenArtifact time, absent from
	// the main consolidated DB. Empty string is the canonical
	// single-version form and is persisted as such.
	metaKeyVersion = "version"
)

// checkStoredMeta validates that the on-disk schema version and
// embedder identity match what the caller is bringing. Shared by Open
// and OpenReader so the reader/mutator split cannot drift on error
// messages, sentinels, or the ordering of the two checks.
//
// Schema version is checked before embedder meta on purpose: an old
// DB whose embedder still matches but whose schema predates a required
// table (e.g. pre-libs #55) must surface as a schema problem rather
// than as a spurious embedder mismatch.
func checkStoredMeta(storedMeta Meta, storedSchemaVersion int, wantMeta Meta) error {
	if storedSchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("%w: stored=%d current=%d; use a fresh database file and re-scrape until an in-place migration is implemented",
			ErrSchemaMismatch, storedSchemaVersion, CurrentSchemaVersion)
	}
	if storedMeta != wantMeta {
		return fmt.Errorf("%w: stored=%+v requested=%+v; use a fresh database file or rebuild with the matching embedder",
			ErrEmbedderMismatch, storedMeta, wantMeta)
	}
	return nil
}

func validateMeta(m Meta) error {
	if m.EmbedderKind == "" {
		return errors.New("meta.EmbedderKind must not be empty")
	}
	if m.EmbeddingDim <= 0 {
		return fmt.Errorf("meta.EmbeddingDim must be > 0, got %d", m.EmbeddingDim)
	}
	if m.ModelVersion == "" {
		return errors.New("meta.ModelVersion must not be empty")
	}
	return nil
}

// readMeta returns the stored Meta, the stored schema version (0 if the
// schema_version key is absent — i.e. a pre-libs database), and a boolean
// indicating whether any meta rows were found. A partially-populated meta
// table (some required keys present, others missing) is treated as a
// corrupt database and returns an error rather than silently filling the
// gaps from the caller.
func readMeta(raw *sql.DB) (Meta, int, bool, error) {
	rows, err := raw.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return Meta{}, 0, false, err
	}
	defer rows.Close()

	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Meta{}, 0, false, err
		}
		values[k] = v
	}
	if err := rows.Err(); err != nil {
		return Meta{}, 0, false, err
	}

	if len(values) == 0 {
		return Meta{}, 0, false, nil
	}

	kind, hasKind := values[metaKeyEmbedderKind]
	dimStr, hasDim := values[metaKeyEmbeddingDim]
	version, hasVersion := values[metaKeyModelVersion]
	if !hasKind || !hasDim || !hasVersion {
		return Meta{}, 0, false, fmt.Errorf("meta table has unexpected keys %v; expected %s, %s, %s",
			keysOf(values), metaKeyEmbedderKind, metaKeyEmbeddingDim, metaKeyModelVersion)
	}

	dim, err := strconv.Atoi(dimStr)
	if err != nil {
		return Meta{}, 0, false, fmt.Errorf("parse %s=%q: %w", metaKeyEmbeddingDim, dimStr, err)
	}

	// schema_version is intentionally optional at the read layer:
	// pre-libs databases never wrote this key, and we want them to
	// surface as ErrSchemaMismatch in Open rather than as a corrupt-
	// meta error here. Missing key → 0, which never matches any future
	// CurrentSchemaVersion.
	schemaVersion := 0
	if s, ok := values[metaKeySchemaVersion]; ok {
		parsed, err := strconv.Atoi(s)
		if err != nil {
			return Meta{}, 0, false, fmt.Errorf("parse %s=%q: %w", metaKeySchemaVersion, s, err)
		}
		schemaVersion = parsed
	}

	return Meta{
		EmbedderKind: kind,
		EmbeddingDim: dim,
		ModelVersion: version,
	}, schemaVersion, true, nil
}

func writeMeta(raw *sql.DB, m Meta) error {
	rows := []struct {
		key, value string
	}{
		{metaKeyEmbedderKind, m.EmbedderKind},
		{metaKeyEmbeddingDim, strconv.Itoa(m.EmbeddingDim)},
		{metaKeyModelVersion, m.ModelVersion},
		{metaKeySchemaVersion, strconv.Itoa(CurrentSchemaVersion)},
	}
	for _, r := range rows {
		if _, err := raw.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, r.key, r.value); err != nil {
			return fmt.Errorf("write %s: %w", r.key, err)
		}
	}
	return nil
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// formatVector encodes a []float32 as a JSON array literal understood by
// turso's vector() constructor: "[0.1,0.2,0.3]". Safe to pass as a bound
// TEXT parameter to `vector(?)` — verified by TestFormatVector_Roundtrip.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
