# shasync

Content-addressed folder snapshots — a tiny alternative to git for the
"keep a folder in sync across N machines" use case. Each snapshot is a
manifest (a map of path → SHA-256) plus a pile of content-addressed
blobs. No server is required: sync via S3/GCS, or via plain rsync over
SSH.

## Install

```bash
go install github.com/varung/shasync@latest
```

Or build from a checkout:

```bash
git clone git@github.com:varung/shasync.git
cd shasync
go install .
```

## Quickstart

```bash
mkdir my-project && cd my-project
shasync init
echo "hello" > notes.txt
shasync commit -m "first snapshot"
shasync log
```

## Documentation

All docs are accessible from the binary itself:

```bash
shasync --help                  # command reference
shasync help getting-started    # quickstart walkthrough
shasync help architecture       # full system overview: layout, commands, data flow
shasync help encryption         # client-side encryption + passphrase setup
shasync help s3                 # using an S3 remote (incl. credentials)
shasync help gcs                # using a GCS remote (incl. credentials)
shasync help ssh                # syncing two machines over SSH with rsync
shasync help ignore             # the .blobsignore file
```

## How it works

### Local layout

```
<repo>/
  <your files ...>
  .blobsignore                       # optional, gitignore-style patterns
  .blobs/
    HEAD                             # current manifest SHA
    config.json                      # { "remote": "s3://bucket/prefix" }
    key                              # hex AES-256 key (chmod 600; gitignored)
    objects/<sha[:2]>/<sha[2:]>      # one file per blob, immutable
    manifests/<sha[:2]>/<sha[2:]>.json
```

Files are ingested with a copy-on-write reflink when the filesystem
supports it (APFS on macOS, Btrfs/XFS on Linux), so commit and checkout
consume almost no extra disk space.

### Remote layout

```
<bucket>/<prefix>/
  salt                # 16 random bytes; present only if a passphrase is in use
  blobs/<sha>         # one object per blob (encrypted if a key is configured)
  manifests/<sha>     # one object per manifest (same)
```

The remote object key is always the SHA-256 of the plaintext, so two
clients uploading the same file dedup automatically. One bucket can host
many shasync projects — each lives under its own `<prefix>`.

### Encryption (optional)

If a key is configured (`shasync key gen` or `shasync key set-passphrase`),
all blobs and manifests are AES-256-GCM encrypted before upload and
decrypted + SHA-verified on download. The key is never sent to the
remote. Wire format is chunked (64 KiB plaintext chunks), so a
range-aware reader can fetch just the chunks covering a plaintext byte
range without downloading the whole blob — useful for large-file
scenarios and browser clients.

Two ways to configure a key:

- **Random key** — `shasync key gen` writes 32 random bytes to
  `.blobs/key`. Copy that file out-of-band to every other machine.
- **Passphrase** — `shasync key set-passphrase` prompts for a passphrase,
  derives a key via Argon2id (salt stored in the bucket at
  `<prefix>/salt`, never secret), and caches the derived key at
  `.blobs/key`. Run the same command with the same passphrase on each
  machine — no file transfer required.

Threat model is accidental bucket exposure: protects against a misconfigured
public-read bucket, not against an attacker who has compromised the key.
See `shasync help encryption` for details.

## License

MIT — see [LICENSE](LICENSE).
