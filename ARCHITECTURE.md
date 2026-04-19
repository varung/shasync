# shasync architecture

A content-addressed folder snapshot tool with optional client-side
encryption. Roughly: "a tiny git for syncing a folder across machines,
designed to also be reachable from a browser."

This document explains how the pieces fit together, why the design
choices were made, and which decisions are load-bearing for future
features (chiefly: in-browser access).

---

## 1. Goals and non-goals

**Goals**

- Sync a directory of files across N machines via a plain blob store
  (S3 / GCS). No server to run.
- Preserve full history (git-like parent chain of snapshots).
- Content-addressed deduplication: identical bytes in two files, two
  commits, or two clients upload exactly once.
- Optional end-to-end encryption such that a leaked bucket is still
  unreadable. Threat model: accidental bucket exposure — not a
  compromised key.
- Wire format that a browser can consume with just `fetch()` + the
  WebCrypto API — including partial reads of large files.
- Zero required server for the CLI path; friendly to a serverless
  browser path (Firebase Auth → Storage Rules → GCS).

**Non-goals**

- Merge / branching / collaborative editing. Snapshots are linear.
- Multi-writer conflict resolution — last push wins on the pointer
  (`HEAD` is purely local; the remote has no mutable pointer).
- Fine-grained access control per file. Authorization happens at the
  bucket / prefix layer.
- Streaming encryption for multi-TB files. The wire format supports
  it, but the current implementation buffers one blob at a time.

---

## 2. Overall shape

```
┌─────────────────────────┐                   ┌────────────────────────┐
│  working tree           │                   │  remote blob store     │
│  /home/me/proj/...      │                   │  s3://bucket/prefix/   │
│    foo.txt  bar.jpg     │                   │    salt                │
│  .blobs/                │                   │    blobs/<sha>         │
│    HEAD                 │   push   ───►     │    manifests/<sha>     │
│    config.json          │   pull   ◄───     │                        │
│    key        (opt)     │                   │                        │
│    objects/<sha>        │                   │                        │
│    manifests/<sha>.json │                   │                        │
└─────────────────────────┘                   └────────────────────────┘
```

The three layers of state:

1. **Working tree** — the user's actual files, editable.
2. **Local store** at `.blobs/` — immutable content-addressed
   artifacts plus a pointer (`HEAD`) and some config.
3. **Remote** — the same artifacts uploaded to a bucket, optionally
   encrypted.

Content addressing is the spine: every blob is named by `sha256(
plaintext )`, every manifest is named by `sha256( manifest_json )`.
Nothing is ever rewritten; garbage collection is by prefix deletion.

---

## 3. Local layout

```
<repo>/
  <user files…>
  .blobsignore                  gitignore-style patterns, optional
  .blobs/
    HEAD                        "<64 hex chars>\n" — the current manifest SHA
    config.json                 { "remote": "s3://bucket/prefix" }
    key                         hex-encoded 32-byte AES-256 key (chmod 600; optional)
    objects/<ab>/<cdef...>      file blobs, sharded by first 2 hex chars
    manifests/<ab>/<cdef...>.json
```

Notes

- The `.blobs/` directory is always excluded from the working-tree
  scan, so it never gets committed into itself.
- `.blobs/key` is a secret. On machines where you've run
  `shasync key set-passphrase`, this file is the Argon2id-derived key
  and can be regenerated from the passphrase any time.
- `objects/` files are written read-only (`0444`) after ingest so
  accidental edits fail loudly. On filesystems that support it
  (Btrfs/XFS/APFS) `checkout` uses `ioctl(FICLONE)` / `clonefile(2)`
  to reflink from `objects/` into the working tree, so snapshots are
  near-zero disk cost.

---

## 4. Remote layout

```
<bucket>/<prefix>/
  salt                          16 random bytes; present iff the repo uses
                                a passphrase-derived key
  blobs/<sha>                   one object per blob
  manifests/<sha>               one object per manifest
```

- A single bucket can host many shasync projects. Each one picks its
  own `<prefix>`, and their namespaces don't overlap.
- Dedup is per-prefix, not bucket-wide. Identical files in two
  projects upload twice unless they share a prefix.
- The remote object key is always the SHA of the plaintext — even
  when the stored bytes are ciphertext. This preserves dedup across
  clients: two machines encrypting the same plaintext try to write
  under the same key, so the second one is a no-op via the `Exists`
  check.
- There is no `HEAD` on the remote. The latest snapshot is whatever
  SHA you happen to pull. Discovery ("what was the newest commit?")
  is an out-of-band concern — a future add-on could write a
  `branches/main` pointer, but the core design is content-addressed
  and pointerless.
- There is no listing step in the hot path. Every upload and every
  download targets a known key computed from a SHA, so `ListObjects`
  permissions are never needed.

---

## 5. Manifests

A manifest is canonical JSON (sorted keys, indented). Schema:

```json
{
  "version": 1,
  "parent_sha": "<64 hex chars, or empty for the first commit>",
  "created_at": 1713427200123,
  "message": "commit message",
  "files": {
    "relative/path/to/file.txt": {
      "sha": "<blob sha256>",
      "size": 12345,
      "updated_at": 1713427199000,
      "mode": 420
    }
  }
}
```

The manifest itself is hashed and stored as `manifests/<sha>`. That SHA
is both the commit identity and the pull argument. Walking the
`parent_sha` chain gives you the full history.

Manifests are intentionally minimal: a map from path to blob SHA plus a
bit of POSIX metadata. No xattrs, no symlinks, no ACLs. Anything the
CLI can't portably reproduce isn't stored, so a browser client can
also fully produce or consume them.

---

## 6. Data flow — commit

```
walk working tree
  → for each file:
      if (path, size, mtime) matches parent manifest:
        reuse parent's blob SHA (no hashing)
      else:
        hash file → ingest into objects/<sha> (reflink or copy)
  → build new manifest
  → canonicalize + sha256 it
  → write manifests/<sha>
  → update HEAD
```

The fast path (unchanged file) avoids even reading the file's
contents. The slow path (changed file) is a single SHA-256 pass plus
an `ioctl(FICLONE)` — so `commit` on an unchanged 10 GB tree is
milliseconds.

---

## 7. Data flow — push

```
resolve target manifest SHA (default: HEAD)
collect:
  - all ancestor manifests the remote doesn't have
  - all blobs they reference
for each blob in parallel (16 workers):
  HEAD-check remote. If present, skip.
  Read local object. If a key is configured, encrypt. Upload.
for each manifest (oldest-first):
  HEAD-check. If present, skip.
  Read, encrypt, upload.
```

Manifests are uploaded last so a remote observer never sees a
manifest whose blobs haven't landed. This matters when another client
is polling for new commits: the "commit is visible" transition is
manifest-atomic.

---

## 8. Data flow — pull

```
if manifests/<target> not local:
  download it (decrypt + verify sha)
walk the parent chain:
  for each ancestor manifest not local:
    best-effort download
collect all blob SHAs referenced
for each missing blob in parallel (16 workers):
  download
  if key configured: decrypt + verify sha before writing locally
  else:              stream directly to objects/<sha>
```

Post-pull, the local `objects/` directory has everything needed to
`checkout <target>`. `checkout` is offline — it only reads from the
local store.

---

## 9. Encryption

### 9.1 What gets encrypted

Both **blobs** (file contents) and **manifests** (path maps). If
manifests were plaintext, a bucket reader would see your full
directory layout — usually more informative than the file contents.

### 9.2 Key material

Two ways to configure a repo:

- **Random key** — `shasync key gen` writes 32 random bytes (hex-
  encoded) to `.blobs/key`. You copy that file out-of-band to every
  other machine.
- **Passphrase** — `shasync key set-passphrase` prompts for a
  passphrase, fetches (or creates) a 16-byte random salt stored at
  `<prefix>/salt` in the remote, runs
  `Argon2id(passphrase, salt, time=3, memory=64 MiB, threads=4, 32 bytes)`,
  and caches the derived key at `.blobs/key`. Same command + same
  passphrase on any other machine yields the same derived key — no
  file transfer.

The salt is not secret. Storing it in the bucket is the only way two
machines independently derive the same key from a shared passphrase.
An attacker with bucket read access can grab the salt and run offline
guesses; Argon2id slows them to ~100 ms per guess, but a weak
passphrase is still crackable. Use 4+ diceware words.

### 9.3 Wire format (SHAS2)

Every encrypted object on the remote looks like:

```
offset  size  field
0       5     magic "SHAS2"
5       4     chunk_size (uint32 big-endian; default 65536)
9       8     plaintext_size (uint64 BE)
17      8     nonce_prefix (8 random bytes)
25      …     concatenated chunks
```

Chunk `i` is `AES-256-GCM(plaintext[i*N:(i+1)*N], nonce = nonce_prefix || uint32_BE(i))`.

Each encrypted chunk on the wire is `plaintext_chunk_len + 16` bytes
(the +16 is the GCM auth tag). Because chunks have a fixed plaintext
size (except the last), a range-aware reader can compute exactly
which ciphertext byte range it needs for any plaintext window:

```
chunks_needed    = range(pt_lo / N, (pt_hi - 1) / N + 1)
ct_offset_of(i)  = 25 + i * (N + 16)
ct_len_of(i)     = N + 16                                # non-final
                   (plaintext_size - i*N) + 16           # final chunk
```

HTTP Range GET those bytes, AES-GCM-decrypt each chunk independently,
slice to the requested window. This is the mechanism a browser
viewer would use to open a 500 MB video without downloading all of
it.

### 9.4 Legacy format (SHAS1)

An earlier version of shasync used a whole-file format
(`"SHAS1" || nonce(12) || AES-GCM(plaintext)`). SHAS1 is accepted on
read for backwards compatibility; writes are always SHAS2.

### 9.5 Threat model

Protects against:

- A bucket that accidentally goes public-read.
- A bucket operator (or adversarial cloud insider) who can enumerate
  stored bytes.
- Network observers between client and bucket.

Does NOT protect against:

- An attacker who has both the ciphertext and the key. Any single-
  shared-key scheme has this property; a compromise of `.blobs/key`
  exposes everything it ever encrypted, retroactively.
- An attacker with bucket write access. They can DoS (scramble the
  salt, overwrite blobs) but can't read. Use bucket ACLs as the
  second line of defense.
- Side channels: ciphertext sizes leak file sizes; upload timing
  leaks commit patterns.

### 9.6 Mixed mode

A given prefix should be either all-encrypted or all-plaintext.
Pulling an encrypted object without a key — or a plaintext object
with a key — errors out with a clear message rather than silently
producing garbage.

---

## 10. Browser-friendliness

The CLI is the primary client today, but design decisions were
chosen to keep a browser port viable:

- **Content-addressed, immutable objects** — perfect CDN behaviour,
  trivially safe to cache forever.
- **Manifest as path→SHA map** — the entire "directory listing" is
  one GET, parseable into a virtual filesystem.
- **Chunked encryption (SHAS2)** — HTTP Range reads work, so a
  browser can open individual large files on demand without
  downloading everything.
- **WebCrypto-native primitives only** — AES-256-GCM and SHA-256 are
  built in; Argon2id requires a WASM library (~100 KB).
- **No listing in the hot path** — every GET/PUT targets a known
  key. A browser client only needs per-object read, not bucket-wide
  list.

### 10.1 Browser authorization

The CLI authenticates to the bucket with AWS/GCP credentials. A
browser shouldn't ever see those. Two realistic auth shapes:

1. **Firebase Auth + Firebase Storage rules** (recommended). The
   underlying bucket is GCS; the CLI pushes via standard ADC, the
   browser reads via Firebase Auth JWTs and Storage Rules gate
   access per-user. No backend to run.
2. **Pre-signed URLs from a tiny backend.** The backend signs
   per-request PUT/GET URLs; the browser holds no credentials at
   all. Works for either S3 or GCS.

Either path gives you defense-in-depth: bucket auth stops random
strangers from even seeing the ciphertext (first line), client-side
encryption stops anyone who does see it from reading plaintext
(second line).

### 10.2 CORS

Regardless of auth, the bucket needs CORS rules permitting the
browser origin. This is one-time bucket configuration, not
something shasync manages.

---

## 11. Command surface

```
shasync init                     create .blobs/ in the current directory
shasync commit [-m <msg>]        snapshot the working tree
shasync checkout <sha> [--force] materialise a snapshot into the working tree
shasync status                   show changes vs HEAD (size+mtime fast check)
shasync log [-n <count>]         walk the manifest parent chain
shasync head                     print the HEAD manifest SHA

shasync remote set <url>         configure remote (s3://... or gs://...)
shasync remote show              print the configured remote
shasync push [<sha>]             upload manifest chain + blobs to remote
shasync pull <sha>               download manifest chain + blobs from remote

shasync key gen                  random 32-byte AES-256 key at .blobs/key
shasync key set-passphrase       Argon2id-derived key from a memorable passphrase
shasync key show                 print the currently loaded key (hex)

shasync help <topic>             architecture | encryption | s3 | gcs | …
```

---

## 12. Open design questions

These are deliberately unresolved; the current shape makes each
addition possible without breaking the wire format.

- **Streaming encryption for multi-GB files.** The SHAS2 format is
  chunk-independent and streaming-friendly; the implementation
  currently buffers. Adding a streaming encoder/decoder is
  transparent.
- **Key rotation.** No in-place rotation. Rotation today means
  "start a new prefix, re-push, delete the old prefix." A future
  design could store per-blob key-wrapping metadata in the
  manifest; deliberately deferred.
- **A `refs/` concept.** Right now the only mutable pointer lives
  locally in `.blobs/HEAD`. Publishing "the latest commit" to the
  remote would help multi-user flows and browser viewers; exact
  semantics (atomic swap? last-writer-wins?) are TBD.
- **Full browser port.** The design is ready for it. The missing
  pieces are a static page, an auth hookup (Firebase Auth is the
  obvious choice), and a WASM Argon2id binding.

---

## 13. File tour

| File                  | Purpose                                               |
|-----------------------|-------------------------------------------------------|
| `main.go`             | Command dispatch                                      |
| `commands.go`         | All `cmd*` functions: init, commit, checkout, push, pull, remote, key |
| `store.go`            | `.blobs/` layout, HEAD, config, object + manifest IO  |
| `manifest.go`         | Manifest struct + schema version                      |
| `scan.go`             | Working-tree walk + `.blobsignore` matching          |
| `crypto.go`           | SHAS1 (read) + SHAS2 (read/write); Argon2id; salt IO |
| `remote.go`           | `Remote` interface; URL parsing; parallel helper      |
| `remote_s3.go`        | S3 implementation                                     |
| `remote_gcs.go`       | GCS implementation                                    |
| `reflink_{linux,darwin,other}.go` | Platform reflink + copy fallback          |
| `help.go`             | Embedded help topics (`shasync help …`)               |
| `e2e_test.go`         | End-to-end tests against S3 and GCS emulators         |

