# Multi-process locking on `deadzone.db` (issue #172)

## TL;DR

`tursogo` v0.5.3 takes an OS-level (fcntl) file lock on the first I/O
against a database file, which by default excludes a second process
opening the same file. The Rust core ships **two escape hatches** that
skip the lock — one is reachable from the Go driver as it stands today.
deadzone uses it: `cmd/deadzone/server.go` sets the
`LIMBO_DISABLE_FILE_LOCK=1` env var before `db.OpenReader`, restoring
the multi-process contract claimed by #131. A defense-in-depth
`db.ErrReaderBusy` sentinel still wraps the raw tursogo lock error so
any future regression surfaces with a human-readable message.

## Reproducer (pre-fix)

One-line shell repro against any consolidated `deadzone.db`:

```sh
deadzone server --db ./deadzone.db & sleep 2 && deadzone server --db ./deadzone.db
```

Pre-fix the second invocation failed with:

```
deadzone: open db: open db reader ./deadzone.db: set query_only:
turso: error: Locking error: Failed locking file. File is locked by another process
```

The failure happened on `PRAGMA query_only = 1` — the first real query
that forces tursogo to actually open a connection and acquire the
file lock.

## What was tried, and what works

### DSN flags (`?key=value`)

`tursogo`'s `parseDSN` (`driver_db.go:622`) accepts `experimental`,
`async`, `vfs`, `encryption_*`, `_busy_timeout`. **Notably absent vs
libSQL / mattn-sqlite3**: `mode=ro`, `immutable=1`, `nolock=1`,
`cache=shared`. None of the supported flags disable the OS-level
lock for a file-backed database.

### `?experimental=multiprocess_wal`

Turso (the engine) gained a real cross-process WAL coordinator in
`core/storage/shared_wal_coordination.rs` — a `.tshm` shared memory
file plus byte-range locks gating writer / checkpointer / reader
slots. The matching `multiprocess_wal` experimental feature is
parsed by `sdk-kit/src/rsapi.rs` and flips `OpenFlags::NoLock`.

**Caveat — not in v0.5.3.** A `git grep "multiprocess_wal"` against
the `bindings/go/v0.5.3` tag returns 0 hits in `sdk-kit/src/rsapi.rs`.
The feature landed in master between v0.5.3 (2026-04-02) and
v0.6.0-pre.X. Empirical test on v0.6.0-pre.25 with both processes
passing `?experimental=multiprocess_wal`: `.tshm` is created, but
the second-process open still fails with the same lock error —
likely a still-baking ABI / binding wiring gap. Not a path to ship
on today.

### `LIMBO_DISABLE_FILE_LOCK` env var (chosen)

`core/io/common.rs` defines:

```rust
pub const ENV_DISABLE_FILE_LOCK: &str = "LIMBO_DISABLE_FILE_LOCK";
```

Every IO backend (`unix.rs`, `io_uring.rs`, `windows.rs`,
`win_iocp.rs`) honors it:

```rust
if std::env::var(common::ENV_DISABLE_FILE_LOCK).is_err()
    && !flags.contains(OpenFlags::ReadOnly)
{
    unix_file.lock_file(true)?;
}
```

So setting `LIMBO_DISABLE_FILE_LOCK=1` in the environment makes
`open_file` skip `lock_file` entirely. Empirically verified against
tursogo v0.5.3:

- Without the env var: process B's `PRAGMA query_only = 1` returns
  `turso: error: Locking error: …`.
- With `LIMBO_DISABLE_FILE_LOCK=1` in both processes: B opens the
  same file, runs `SELECT count(*) FROM t`, gets `3`. Both processes
  remain alive.

This is the path deadzone uses today.

### `OpenFlags::ReadOnly` (Rust-side equivalent)

The same `unix.rs` snippet shows that passing `OpenFlags::ReadOnly`
**also** skips `lock_file`. The `tursodb --readonly` CLI flag
exercises this. But tursogo's Go binding does NOT expose
`OpenFlags::ReadOnly` — neither the DSN nor `TursoDatabaseConfig`
carry it. The env var is the only Go-reachable lever.

## Decision: env-var bypass + ErrReaderBusy fallback

`cmd/deadzone/server.go` calls `os.Setenv("LIMBO_DISABLE_FILE_LOCK",
"1")` before `db.OpenReader`. Process-scoped: only the server
processes opt out of the lock. Mutator subcommands (`consolidate`,
`scrape`, `dbrelease`) live in their own processes and never set
the var, so concurrent writers are still serialised by fcntl as
before — only the read path is widened.

`db.ErrReaderBusy` and the substring detection in
`db.isTursoLockError` stay in place as defense in depth: if the env
var is somehow stripped (sandbox, env scrubber, future tursogo bump
renaming the var), the lock failure surfaces with a clear human
message instead of the raw tursogo string. The
`TestOpenReader_MultiProcess/FallbackErrReaderBusyWithoutEnvVar`
sub-test pins this branch.

## Risks and re-evaluate-when

- **The env var is internal.** `LIMBO_DISABLE_FILE_LOCK` lives in
  `core/io/common.rs` with no public-docs commitment. A rename to
  `TURSO_DISABLE_FILE_LOCK` is plausible mid-development. Mitigation:
  pin tursogo in `go.mod` (`v0.5.3`) and re-validate the var name on
  every bump. The `FallbackErrReaderBusyWithoutEnvVar` sub-test
  catches a regression because the env var name is the only knob —
  if a rename silently disables it, the unit test still asserts the
  fallback path gives a clear error.

- **Shared `.tshm` coordinator may eventually be the right answer.**
  When `multiprocess_wal` ships in a stable tursogo Go release with
  working `.tshm` semantics, switch to `?experimental=multiprocess_wal`
  in the DSN and drop the env var. That path supports concurrent
  *writers* too (via the single-writer slot in `.tshm`), not just
  readers — bigger upside for any future "consolidate while serving"
  workflow.

- **Lock bypass weakens the protection against a stray writer in the
  same process.** Not relevant here: `runServer` only calls
  `db.OpenReader` and pins every connection to `PRAGMA query_only`,
  so the server cannot accidentally write. `cmd/deadzone/consolidate.go`
  et al. live in separate processes that do NOT set the env var.

## Out of scope

- Forking tursogo or swapping it for `mattn/go-sqlite3`.
- Daemon-of-servers architecture multiplexing multiple MCP clients
  through a single in-memory `deadzone server`.
- NFS / CIFS / GFS2 / Lustre validation — `multiprocess_wal` docs
  call those out as unsafe; the env-var bypass inherits the same
  caveats. deadzone targets local filesystems only.

## Test commands

- Reproduce pre-fix: `LIMBO_DISABLE_FILE_LOCK= deadzone server &
  sleep 2 && LIMBO_DISABLE_FILE_LOCK= deadzone server` (force-empty
  env to defeat the bypass).
- Verify post-fix: `deadzone server & sleep 2 && deadzone server` —
  both processes should run.
- `mise exec -- go test ./internal/db/... -run TestOpenReader_MultiProcess -v`
- `just test`
