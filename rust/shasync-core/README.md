# shasync-core

A Rust library port of shasync's core, so an app (e.g. Tauri on desktop and
iOS) can read and write shasync repositories in-process. It is
byte-compatible with the Go CLI: both can share the same `.blobs/` folder and
the same bucket. It is library-only, with no CLI.

## API overview

| Item | What it does |
|---|---|
| `Repo::open(path)` / `open_exact` / `init` | Find `.blobs/` (walking up), or create one with a client ID |
| `Repo::read_config` / `write_config` / `set_remote` / `ensure_client_id` | `.blobs/config.json` (`set_remote` also clears the push-log cache) |
| `Repo::load_key` / `require_key` / `save_key` | `.blobs/key` (hex, mode 0600) |
| `Repo::scan` / `ignore_patterns` | Working-tree walk with the default ignores plus `.blobsignore` |
| `Repo::commit` / `commit_at` / `commit_if_dirty` | Snapshot. Same manifest bytes and SHA as Go for the same tree |
| `Repo::status` / `diff_working_tree` / `log` | Changes vs HEAD (size+mtime, then hash); walk history |
| `Repo::checkout(sha, force)` | Materialize a snapshot (like `shasync checkout`) |
| `Repo::apply_manifest_safely(from, to)` / `disk_matches` | Apply a snapshot without ever clobbering edits |
| `Repo::read_manifest` / `write_manifest` / `read_object` / `write_object` | The content-addressed store |
| `Manifest`, `ManifestFile`, `Manifest::to_canonical_bytes` / `encode` / `parse` | Schema v1, with Go's canonical JSON and strict reader |
| `Key::{from_hex, generate, derive_from_passphrase}` | Raw key, or Argon2id (t=3, m=64 MiB, p=4, 32 bytes) |
| `crypto::{encrypt, decrypt, encode_object, decode_object, is_encrypted}` | SHAS2 (SHAS1 read-only); object codec = zstd, then SHAS2. Plaintext is refused |
| `merge::{auto_merge, conflict_copy_path}` | Three-way merge with `(modified on … by …)` copies |
| `Remote` trait (`exists/get/put/list`, async) | `S3Remote` (`S3Config`), `MemoryRemote`, `LocalDirRemote` |
| `sync::{read_remote_head, write_remote_head, fetch_salt, fetch_or_create_salt, set_passphrase, derive_key_from_remote}` | Remote pointer and salt |
| `Syncer::new(repo, Arc<dyn Remote>, key)` | `pull()`, `push()`, `sync()`, `fetch_manifest_chain`, `fetch_blobs_for`, `fetch_and_apply`, `upload_chain_and_set_head` → `SyncReport { committed, action: SyncAction }` |

`Syncer` needs a tokio runtime, because hashing, crypto and file IO run on
`spawn_blocking`. `Error::LocalEditsDuringSync` means "a file was saved
mid-sync; nothing was lost, so retry", as in Go's watch loop.

### Flows

- **`sync()`** is Go's `syncOnce` (one `watch` cycle). It commits a dirty
  tree, then either uploads (when the remote is empty or behind),
  fast-forwards the working tree safely (when local is behind), or, if the
  two have forked, merges, applies the merge safely and uploads it.
- **`pull()`** commits, then fast-forwards, merges (locally only), or
  reports `LocalAhead`.
- **`push()`** commits, then uploads when it can fast-forward. If the
  remote is strictly ahead it returns `Error::RemoteAhead`, like
  `shasync push`. If the histories have forked it merges and uploads, like
  `sync()`.
- Working-tree updates always go through `apply_manifest_safely`. Only
  paths that differ are touched, each only if it still holds its
  snapshotted content, and files created during the sync are never
  deleted.

## Compatibility guarantees (tested against the Go code)

- **Manifests:** the same canonical JSON as Go's `json.MarshalIndent` +
  `"\n"`, including Go's HTML escaping and U+2028/U+2029 escapes, field
  order, `omitempty` and byte-sorted paths. So the same tree gives the
  same manifest SHA. The reader is as strict as Go's (unknown fields,
  `version != 1` and non-empty `chunks` are rejected).
- **Objects:** SHAS2 with 64 KiB chunks and an 8-byte nonce prefix plus a
  big-endian u32 counter. Uploads are zstd + SHAS2. Downloads must be
  SHAS-framed; they are decrypted, decompressed only if they start with
  the zstd magic, and SHA-verified.
- **Keys:** `.blobs/key` is hex + `"\n"`. Passphrase keys use Argon2id
  v1.3 with Go's parameters and the 16-byte salt at `<prefix>/salt`.
- **Scan:** the same default ignores, `.blobsignore` parsing and a port
  of Go's `path.Match`. Symlinks are skipped. The size+mtime fast path
  reuses the parent entry exactly as Go does.
- **Remote layout:** `HEAD`, `salt`, `blobs/<sha>`, `manifests/<sha>`,
  plus the `push-log/` dedup protocol and `.blobs/push-log-cache`, in
  the same format, so Go and Rust can alternate on one repo.

Known differences (none of them affect the format):

- `load_key` reads only `.blobs/key`. It ignores `SHASYNC_KEY`.
- `sync()` on a fresh clone with no commits fast-forwards; Go's
  `syncOnce` fails there.
- Names that aren't valid UTF-8 are skipped. Go would commit them under
  a U+FFFD-mangled name.
- Ingest re-hashes the copied bytes, so a file edited mid-commit is
  never stored under the wrong SHA.
- S3 credentials are passed in explicitly. Nothing is read from
  `~/.aws`. There is no bucket-privacy check, and the region isn't
  auto-detected.

## S3

`S3Remote` is built on `rusty-s3` (SigV4 presigned requests) and `reqwest`
with rustls/ring. There is no OpenSSL and no AWS SDK. It builds for
`aarch64-apple-ios`. Path-style addressing is the default, as in Go. Set
`S3Config.endpoint` for R2/MinIO. Each PUT carries a signed
`x-amz-checksum-crc32`.

## Running the tests

```sh
cd rust
cargo test                       # unit + local sync + Go compat tests
cargo build --target aarch64-apple-ios
```

`tests/compat_go.rs` needs Go. It builds the shasync CLI and a test-only
helper (`rust/compat/_gohelper/compat_helper_test.go`, compiled into the
package's test binary with `go test -c -overlay`, so the Go sources stay
untouched and `go test ./...` never sees it). It then checks:

- Go→Rust and Rust→Go decryption (raw SHAS2 and the full object codec)
- passphrase derivation
- canonical manifest bytes for awkward paths and messages
- ignore matching (Go's `path.Match` edge cases)
- identical commits of the same tree, including on a Rust parent
- Rust opening, statusing and committing a Go-CLI repo, then `shasync log/status/checkout` on the result
- `auto_merge` producing the same SHA as Go
- a full Go CLI ⇄ Rust sync, with forks in both directions, through an in-memory S3 (gofakes3) on 127.0.0.1

The Go CLI runs with a scratch `HOME` and nonexistent AWS config and
credential files. No real bucket is ever contacted.
