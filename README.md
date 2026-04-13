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
shasync help s3                 # using an S3 remote (incl. credentials)
shasync help gcs                # using a GCS remote (incl. credentials)
shasync help ssh                # syncing two machines over SSH with rsync
shasync help ignore             # the .blobsignore file
```

## How it works

Everything lives under `.blobs/` in the project root:

```
.blobs/
  HEAD                 # current manifest SHA
  config.json          # remote URL, if configured
  objects/ab/cdef...   # file blob (sharded by first 2 chars of SHA)
  manifests/ab/cdef... # manifest JSON (same sharding)
```

Files are ingested with a copy-on-write reflink when the filesystem
supports it (APFS on macOS, Btrfs/XFS on Linux), so commit and checkout
consume almost no extra disk space. On filesystems without reflink
support the tool falls back to a regular copy.

## License

MIT — see [LICENSE](LICENSE).
