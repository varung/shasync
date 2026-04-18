package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

const mainHelp = `shasync — content-addressed folder snapshots

A tiny alternative to git for the "keep a folder in sync across N machines"
use case. Each snapshot is a manifest (a map of path -> SHA-256) plus a pile
of content-addressed blobs. No server is required to use it; you can sync
via S3/GCS or plain rsync over SSH.

USAGE
  shasync <command> [flags...]
  shasync help <topic>

COMMANDS
  init                          initialize .blobs/ in the current directory
  commit [-m <msg>]             snapshot the working tree, print manifest SHA
  checkout <sha> [--force]      reflink files from manifest <sha> into the dir
  status                        show changes vs HEAD
  log [-n <count>]              walk the manifest parent chain
  head                          print the HEAD manifest SHA
  remote set <url>              configure remote (gs://... or s3://...)
  remote show                   print the configured remote
  push [<sha>]                  upload manifest + blobs to remote (default HEAD)
  pull <sha>                    download manifest + blobs from remote
  key gen                       generate a repo encryption key at .blobs/key
  key show                      print the configured encryption key (or "none")
  help <topic>                  show detailed help on a topic

HELP TOPICS
  getting-started   quickstart: init, commit, checkout, log
  s3                using an S3 remote (including credentials)
  gcs               using a Google Cloud Storage remote (including credentials)
  ssh               syncing two machines over SSH with rsync
  ignore            the .blobsignore file
  encryption        encrypting blobs and manifests on the remote

For a short command reference, run:  shasync --help
For a deep dive on a topic, run:     shasync help <topic>
`

const gettingStartedHelp = `GETTING STARTED

Create a snapshot of the current folder, modify some files, and snapshot
again. Every snapshot keeps a full copy of every file (deduplicated by
content), so you can jump back and forth without losing anything.

    $ cd my-project
    $ shasync init
    initialized .blobs/ in /home/you/my-project

    $ echo "hello" > notes.txt
    $ shasync commit -m "first snapshot"
    3f2a1c...  (1 files)

    $ echo "hello world" > notes.txt
    $ shasync status
    HEAD 3f2a1c...
      M  notes.txt

    $ shasync commit -m "tweak"
    8b0e44...  (1 files)

    $ shasync log -n 5
    8b0e44...  1 files  tweak
    3f2a1c...  1 files  first snapshot

    $ shasync checkout 3f2a1c...
    checked out 3f2a1c...  (1 files)
    $ cat notes.txt
    hello

HOW IT WORKS

Everything lives under .blobs/ in the project root:

  .blobs/
    HEAD                 # current manifest SHA
    config.json          # remote URL, if configured
    objects/ab/cdef...   # file blob (sharded by first 2 chars of SHA)
    manifests/ab/cdef... # manifest JSON (same sharding)

Files are ingested with a copy-on-write reflink when the filesystem
supports it (APFS on macOS, Btrfs/XFS on Linux), so commit and checkout
consume almost no extra disk space. On filesystems without reflink
support the tool falls back to a regular copy.

SAFETY

  * checkout refuses to run if the working tree has uncommitted changes.
    Pass --force to discard them.
  * Object files are written read-only (0444). An "accidental edit" of
    a tracked file inside .blobs/objects/ will fail with EACCES.
  * Manifests are immutable; their SHA is derived from their bytes.

IGNORING FILES

Create a .blobsignore at the project root. See:  shasync help ignore
`

const s3Help = `S3 REMOTE

SET UP

    shasync remote set s3://<bucket>/<prefix>

Example:
    shasync remote set s3://my-bucket/projects/site-a
    shasync push

The bucket must already exist. The prefix is optional and acts as a
namespace within the bucket (so one bucket can hold many shasync projects).

LAYOUT IN S3

    s3://<bucket>/<prefix>/manifests/<sha>
    s3://<bucket>/<prefix>/blobs/<sha>

CREDENTIALS

shasync uses the standard AWS SDK credential chain — in order of precedence:

  1. Environment variables:
       AWS_ACCESS_KEY_ID
       AWS_SECRET_ACCESS_KEY
       AWS_SESSION_TOKEN         (optional, for temporary creds)
       AWS_REGION                (required; e.g. us-east-1)
  2. Shared credentials file: ~/.aws/credentials
     Shared config file:      ~/.aws/config
     Select a profile with:   AWS_PROFILE=my-profile
  3. IAM role on an EC2 instance, ECS task, or Lambda function
     (picked up automatically, no config needed).
  4. Workload identity when running on EKS with an associated role.

Quickest setup on a laptop:
    aws configure                       # writes ~/.aws/credentials
    export AWS_REGION=us-east-1
    shasync remote set s3://my-bucket/my-project
    shasync push

REQUIRED IAM PERMISSIONS

At minimum the caller needs:
    s3:PutObject, s3:GetObject, s3:HeadObject
on arn:aws:s3:::<bucket>/<prefix>/*

Listing (s3:ListBucket) is not required.

EMULATORS AND S3-COMPATIBLE STORES

To point at MinIO, LocalStack, or any other S3-compatible endpoint:

    export SHASYNC_S3_ENDPOINT=http://localhost:9000
    export SHASYNC_S3_FORCE_PATH_STYLE=1
    export AWS_ACCESS_KEY_ID=minioadmin
    export AWS_SECRET_ACCESS_KEY=minioadmin
    export AWS_REGION=us-east-1
    shasync remote set s3://my-bucket/my-project
`

const gcsHelp = `GCS REMOTE

SET UP

    shasync remote set gs://<bucket>/<prefix>

Example:
    shasync remote set gs://my-bucket/projects/site-a
    shasync push

The bucket must already exist. The prefix is optional.

LAYOUT IN GCS

    gs://<bucket>/<prefix>/manifests/<sha>
    gs://<bucket>/<prefix>/blobs/<sha>

CREDENTIALS

shasync uses Google's Application Default Credentials (ADC). In order of
precedence:

  1. GOOGLE_APPLICATION_CREDENTIALS pointing at a service-account JSON key:
         export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json
  2. User credentials from:
         gcloud auth application-default login
     (written to ~/.config/gcloud/application_default_credentials.json)
  3. The metadata server on GCE / GKE / Cloud Run / Cloud Functions —
     automatic when running inside Google Cloud.

Quickest setup on a laptop:
    gcloud auth application-default login
    shasync remote set gs://my-bucket/my-project
    shasync push

REQUIRED IAM PERMISSIONS

At minimum the caller needs:
    storage.objects.create
    storage.objects.get
The role "roles/storage.objectAdmin" on the bucket is a simple choice.
Listing is not required.

EMULATOR

To point at fake-gcs-server or Google's own emulator:

    export STORAGE_EMULATOR_HOST=http://localhost:4443
    shasync remote set gs://my-bucket/my-project

(shasync automatically forces reads through the JSON API when this env var
is set, because fake-gcs-server host-gates its XML endpoints.)
`

const sshHelp = `SYNCING TWO MACHINES OVER SSH

shasync has no built-in SSH remote, but the .blobs/ directory is designed
so that rsync is a perfectly safe and efficient transport. This section
explains the recipe and why it cannot lose data.

THE RECIPE

On machine A (the source), push everything into machine B's .blobs/:

    rsync -a --ignore-existing .blobs/ B:/path/to/project/.blobs/

Then on B, update HEAD (or just pick a SHA) and check it out:

    ssh B 'cd /path/to/project && shasync checkout <sha>'

That is the entire protocol. One command to ship data, one command to
materialize it.

WHY IT CANNOT LOSE DATA

Every file under .blobs/objects/ and .blobs/manifests/ is named by the
SHA-256 of its contents. Two consequences follow:

  * Identical name implies identical content (barring SHA collision,
    which we treat as impossible).
  * Nothing is ever mutated — once written, a blob or manifest is
    immutable. Objects are even chmod'd to 0444.

So "rsync with --ignore-existing" means:

    if the destination has a file with this name, skip it;
    otherwise copy it over.

Skipping is safe: the file is byte-identical to what we would have
written. Copying is safe: it is a brand-new SHA the destination has
never seen. There is no path where rsync overwrites good data with bad.

Because rsync is purely additive on the blob/manifest tree, every past
version on both machines survives every sync. Running the command in
reverse (B -> A) is equally safe and merges histories: A ends up with
every version B has, B ends up with every version A has.

HEAD AND CONFIG

Two files inside .blobs/ are mutable: HEAD and config.json. With
--ignore-existing they are left alone on the destination if they exist
(each machine keeps its own HEAD and remote config). On a first-time
sync where B has no .blobs/ yet, rsync copies them too, bootstrapping
B as a full clone of A.

If you explicitly want to push your HEAD to B, follow up with:

    scp .blobs/HEAD B:/path/to/project/.blobs/HEAD

BIDIRECTIONAL SYNC

    # A -> B
    rsync -a --ignore-existing .blobs/ B:/path/to/project/.blobs/
    # B -> A
    rsync -a --ignore-existing B:/path/to/project/.blobs/ .blobs/

After both runs, every manifest and every blob known to either side is
present on both sides. HEADs remain independent; pick a SHA and
checkout when you want to converge the working trees.

DIVERGENT HISTORY

If both machines commit independently, their HEAD chains diverge.
shasync has no merge command today, so you resolve this by picking one
side's SHA to check out. The parent_sha field recorded in each
manifest leaves room to add a proper merge later without changing
the on-disk format.
`

const ignoreHelp = `IGNORE RULES

Create a .blobsignore at the project root to exclude paths from scans.

SYNTAX

One pattern per line. Blank lines and lines starting with # are ignored.
Each pattern is interpreted with Go's path.Match (shell-glob) semantics
and is matched against every path suffix, so a pattern like:

    node_modules

matches both "node_modules" at the root and "a/b/node_modules/" nested
anywhere. A trailing "/" marks a pattern as directory-only.

EXAMPLES

    # build artifacts
    dist
    build/
    *.log

    # editor junk
    .DS_Store
    *.swp

    # secrets
    .env
    .env.*

The .blobs/ directory itself is always ignored automatically.
`

const encryptionHelp = `ENCRYPTION

shasync can encrypt blobs and manifests client-side before uploading them
to the remote. Objects in the bucket become opaque AES-256-GCM ciphertext;
anyone who gains read access to the bucket (e.g. accidental public-read)
cannot recover your files without the key.

Threat model: protects against unintentional bucket exposure. It is NOT
designed to resist an attacker who has both the ciphertext AND the key
(any single-key scheme has this property).

SETUP

    cd my-project
    shasync init
    shasync key gen
    shasync remote set s3://my-bucket/my-project
    shasync push

"key gen" writes .blobs/key (chmod 600) — a 32-byte AES-256 key encoded
as hex. The key is NEVER sent to the remote.

To sync a second machine, copy .blobs/key out-of-band (scp, password
manager, etc) into the same path on the other machine, then pull as normal.

ENV OVERRIDE

Set SHASYNC_KEY to a hex-encoded 32-byte key to override the file:

    export SHASYNC_KEY=$(shasync key show)   # copy-paste into ~/.zshrc on machine B

CHANGING KEYS

There is no in-place rotation. If .blobs/key is lost, already-pushed
ciphertext cannot be decrypted. To rotate, start a fresh prefix
(shasync remote set s3://bucket/new-prefix), shasync key gen, and push.

WIRE FORMAT

Each uploaded object is:
    magic(5)="SHAS1" || nonce(12, random) || AES-256-GCM(plaintext) || tag(16)

The remote key name is still blobs/<sha-of-plaintext> or
manifests/<sha-of-plaintext>, so dedup across clients still works.

MIXED MODE

A single remote prefix should be either all-encrypted or all-plaintext.
Pulling an encrypted object without a key (or vice versa) errors out.
`

var helpTopics = map[string]string{
	"getting-started": gettingStartedHelp,
	"quickstart":      gettingStartedHelp,
	"s3":              s3Help,
	"gcs":             gcsHelp,
	"ssh":             sshHelp,
	"rsync":           sshHelp,
	"ignore":          ignoreHelp,
	"blobsignore":     ignoreHelp,
	"encryption":      encryptionHelp,
	"encrypt":         encryptionHelp,
	"key":             encryptionHelp,
}

func cmdHelp(args []string) error {
	if len(args) == 0 {
		fmt.Print(mainHelp)
		return nil
	}
	topic := strings.ToLower(args[0])
	body, ok := helpTopics[topic]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown help topic: %s\n\navailable topics:\n", topic)
		names := make([]string, 0, len(helpTopics))
		seen := make(map[string]bool)
		for k, v := range helpTopics {
			if !seen[v] {
				seen[v] = true
				names = append(names, k)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(os.Stderr, "  %s\n", n)
		}
		return fmt.Errorf("no such topic")
	}
	fmt.Print(body)
	return nil
}
