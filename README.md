# shasync

**Open-source Dropbox that runs on your own cloud storage.**

shasync keeps a folder in sync across all your machines using S3 or GCS
as the backend. No proprietary server, no monthly fee, no storage limits
beyond what your bucket allows. It's a single binary with no dependencies.

- **Dropbox-style auto-sync** — `shasync watch` monitors your files and
  syncs changes in the background, including auto-merging edits from
  other machines
- **Works with any cloud** — bring your own S3 bucket (AWS, Cloudflare R2,
  MinIO, etc.) or GCS bucket. One bucket can host many projects.
- **End-to-end encrypted** — optional AES-256-GCM encryption. Your cloud
  provider never sees plaintext. Key never leaves your machines.
- **Handles large files natively** — no git-lfs needed. Copy-on-write
  reflinks (APFS, Btrfs, XFS) mean a 5GB video takes no extra disk space
  in the local object store. Commit and checkout are near-instant for
  unchanged files.
- **Full version history** — every snapshot is a content-addressed manifest
  with a parent pointer. Browse, diff, and restore old versions.
- **Conflict resolution that never blocks you** — when two machines edit the
  same file, both versions are kept. The remote version stays at the
  original path; your local version is saved as a timestamped sibling
  (like Dropbox does). No merge conflicts to resolve manually. Ever.

## Why not just use...

| | shasync | Dropbox/Drive | git | git-lfs | rsync |
|---|---|---|---|---|---|
| Self-hosted on your cloud | yes | no | sort of | sort of | yes |
| Auto-sync / watch mode | yes | yes | no | no | no |
| Large binary files | yes (COW) | yes | no | yes | yes |
| Version history | yes | limited | yes | yes | no |
| E2E encryption | yes | no | no | no | no |
| Offline conflict merge | automatic | automatic | manual | manual | last-write-wins |
| Single binary, no server | yes | no | yes | yes | yes |

## Install

```bash
go install github.com/varung/shasync@latest
```

Or build from source:

```bash
git clone git@github.com:varung/shasync.git
cd shasync && go install .
```

## Quickstart

```bash
# Machine A: create a project
mkdir my-project && cd my-project
shasync init
shasync remote set s3://my-bucket/my-project

# Add files and push
echo "hello" > notes.txt
shasync push                    # auto-commits dirty files, uploads to S3

# Machine B: clone it
mkdir my-project && cd my-project
shasync init
shasync remote set s3://my-bucket/my-project
shasync pull                    # downloads and materializes files
```

## Watch mode (auto-sync)

The killer feature. Run `shasync watch` and forget about it — changes
are committed and synced automatically:

```bash
shasync watch                   # default: 2s debounce, 30s remote poll
shasync watch --debounce 5s --poll 1m
```

Every time a file changes:
1. Wait for the debounce quiet period
2. Commit the working tree
3. Pull from remote (auto-merge if another machine pushed)
4. Push to remote

It also polls the remote on an interval to pick up changes from other
machines, even when no local files changed.

## Multi-machine sync

The manual workflow (if you prefer explicit control):

```bash
shasync pull        # fetch + merge + checkout
(edit files)
shasync push        # auto-commit + upload
```

If two machines push independently, the second machine's `push` will
fail with "remote has moved." Run `pull` — it auto-merges:

- Files changed on only one side: taken as-is
- Files changed on both sides to the same content: no conflict
- Files changed on both sides differently: remote version kept at
  original path, local version saved as
  `<name> (modified on <date> by <client-id>).<ext>`

No manual conflict resolution. No blocked pushes. No lost work.

## Encryption

```bash
shasync key set-passphrase      # Argon2id derivation; same passphrase on each machine
# or
shasync key gen                 # random key; copy .blobs/key to other machines
```

When a key is configured, all blobs and manifests are AES-256-GCM
encrypted before upload and verified on download. The key never touches
the remote. Wire format is chunked (64 KiB) to support range reads.

## Compression

All uploads are zstd-compressed before encryption. Downloads auto-detect
the format via magic bytes, so old uncompressed blobs from before this
feature was added are read transparently. zstd's incompressible-data
fast path means there's no meaningful penalty for binary files.

## Copy-on-write

On APFS (macOS) and Btrfs/XFS (Linux), shasync uses filesystem reflinks
so the working tree and the object store share physical disk extents. A
2GB file appears in both your working directory and `.blobs/objects/` but
occupies disk space only once. This is why shasync doesn't need a
separate "large file" mode — the same code path handles a 1KB config
file and a 10GB dataset equally well.

Verify it works on your filesystem: `shasync test-cow`

## How it works

### Local layout

```
<repo>/
  <your files>
  .blobsignore                       # optional, gitignore-style
  .blobs/
    HEAD                             # current manifest SHA
    config.json                      # { "remote": "s3://..." }
    key                              # hex AES-256 key (optional)
    objects/<sha[:2]>/<sha[2:]>      # immutable content-addressed blobs
    manifests/<sha[:2]>/<sha[2:]>.json
```

### Remote layout

```
<bucket>/<prefix>/
  HEAD              # mutable pointer to current tip manifest
  salt              # Argon2id salt (only if passphrase in use)
  blobs/<sha>       # one object per blob (compressed, optionally encrypted)
  manifests/<sha>   # one object per manifest (same)
```

Remote keys are the SHA-256 of the plaintext, so identical files dedup
across clients and commits automatically.

## Documentation

All docs are built into the binary:

```bash
shasync help getting-started    # quickstart
shasync help workflow           # multi-machine sync + auto-merge
shasync help watch              # watch mode
shasync help compression        # zstd compression details
shasync help encryption         # E2E encryption + passphrase setup
shasync help architecture       # full system design
shasync help s3                 # S3 remote setup
shasync help gcs                # GCS remote setup
shasync help ignore             # .blobsignore patterns
```

Or see [ARCHITECTURE.md](ARCHITECTURE.md) for the full system design.

## License

MIT — see [LICENSE](LICENSE).
