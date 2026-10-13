# How shasync works, step by step

A walkthrough of what shasync stores and what each operation does, showing
at every step **what is on your machine** and **what is in the bucket**.
`ARCHITECTURE.md` explains the design decisions behind it; this file follows
one folder through its life.

The running example: a notes folder synced between a Mac and a phone through
`s3://my-bucket/notes/`.

---

## 1. The pieces

| Thing | What it is | Named by |
|---|---|---|
| **Blob** | The contents of one file, byte for byte. Two files with the same bytes are one blob. | SHA-256 of the contents |
| **Manifest** | One snapshot of the whole folder: a list of every path, the blob it holds, its size, time and permissions, plus the previous snapshot's SHA (its *parent*). Like a git commit. | SHA-256 of the manifest's JSON |
| **HEAD** | A pointer: the SHA of the newest manifest. There are two: your machine's (`.blobs/HEAD`) and the bucket's (`HEAD`). Syncing is mostly about getting these to agree. | — |
| **Key** | The 32-byte AES-256 key that encrypts everything in the bucket. Derived from your passphrase. | — |
| **Salt** | 16 random bytes stored in the bucket, mixed into the passphrase so the same passphrase gives the same key on every device. Not secret. | — |
| **Push-log** | Bucket files listing which blobs and manifests each push uploaded. A shortcut so a push needn't ask the bucket about every file. | upload time + manifest SHA |
| **Client ID** | A random name for each device (like `aed23d4d`), used in snapshot messages and conflict-copy names. | — |

Everything except the two HEADs is **immutable**. A blob or manifest is
written once under its SHA and never changed. Nothing in the bucket is ever
deleted by sync. The only thing that changes is which manifest HEAD points to.

---

## 2. Where everything lives

A file's contents can be in three places. This document always uses these
names:

- **Notes folder**: your files, as you edit them (`notes/todo.md`). On each device.
- **Local store**: shasync's private copies on that device, in `notes/.blobs/`.
  One copy per version of each file, plus every snapshot.
- **Bucket**: the encrypted copies on S3, shared by all devices.

```
ON YOUR MACHINE                              IN THE BUCKET  (s3://my-bucket/notes/)
───────────────                              ─────────────
notes/                                       HEAD            plaintext: newest manifest SHA
  todo.md          ← your files              salt            plaintext: 16 random bytes
  ideas/plan.md                              blobs/<sha>     encrypted file contents
  .blobs/          ← shasync's own data      manifests/<sha> encrypted snapshots
    config.json      remote URL, client ID   push-log/<time>-<sha>
    key              the encryption key        plaintext: SHAs one push uploaded
    HEAD             newest local manifest
    objects/ab/cdef…       blobs, plain
    manifests/ab/cdef….json  manifests, plain JSON
    push-log-cache   what this machine knows
                     the bucket holds
```

| | On your machine | In the bucket |
|---|---|---|
| Blobs | `objects/<2 chars>/<rest of sha>`, the plain file, read-only. Can be cloned (reflinked) straight into the notes folder. | `blobs/<sha>`: zstd-compressed, then AES-256-GCM encrypted. |
| Manifests | `manifests/<2 chars>/<rest>.json`, plain JSON. | `manifests/<sha>`: compressed and encrypted, so the bucket never shows your file names. |
| HEAD | `.blobs/HEAD` | `HEAD`, plaintext. It's just a SHA, already visible as an object name. |
| Key | `.blobs/key` (mode 600). Never uploaded. | Never stored. Only the salt is. |
| Which blobs | Usually only some: the ones this machine created, plus the ones it downloaded to check out a snapshot. `gc` can delete local copies of old ones. | All of them, from every device, forever. |
| Which manifests | The full history (they're small). | The full history. |

So **the bucket is the complete archive** and **each local store holds what
that device needs**: every manifest, and the blobs for what's in its notes
folder now, plus whatever older versions it happened to keep.

---

## 3. Setting up

**First device** (`shasync init`, `remote set`, `key set-passphrase`; in
REditMark, the Sync setup form):

1. Create `.blobs/` with a `config.json` holding the remote URL and a new client ID.
2. Look for `salt` in the bucket. None yet, so make 16 random bytes and upload them.
3. Key = Argon2id(passphrase, salt). This takes about a second on purpose, to
   slow down anyone guessing passphrases. Save it to `.blobs/key`.

| On the Mac | In the bucket |
|---|---|
| `.blobs/config.json`, `.blobs/key` | `salt` |

**Every other device** does the same, but finds the existing `salt`, so the
same passphrase gives the same key. Nothing secret is copied between devices.
A QR pairing only carries the bucket address and S3 access key; you still
type the passphrase.

---

## 4. Snapshotting (commit)

You edit `todo.md` and REditMark saves it. Later, a sync begins with a snapshot:

1. **Walk the notes folder**, skipping `.blobs/`, `.blobsignore` patterns and editor
   litter (`.DS_Store`, swap files, temp files from atomic saves).
2. **For each file**, compare its size and modified time with the entry in the
   current HEAD manifest:
   - **Same, and the local store has its copy:** reuse that entry. The file
     isn't even read, which is why snapshotting a big unchanged folder takes
     milliseconds.
   - **Different, or the local store's copy is missing:** hash the file and
     store a copy in the local store (`objects/`) under that hash, unless one
     is already there. This is also how a lost local copy of a current file
     comes back.
3. **Build a manifest** with every path and its blob, and the old HEAD as parent:

   ```json
   {
     "version": 1,
     "parent_sha": "c2227d9d…",
     "created_at": 1791693412439,
     "message": "sync from aed23d4d",
     "files": {
       "todo.md":       { "sha": "5d13b82b…", "size": 1157, "updated_at": 1791693400000, "mode": 420 },
       "ideas/plan.md": { "sha": "0e889be7…", "size": 221,  "updated_at": 1775000000000, "mode": 420 }
     }
   }
   ```

4. If nothing changed since HEAD, stop. Otherwise write the manifest to
   `manifests/`, named by its SHA, and point `.blobs/HEAD` at it.

| On the Mac | In the bucket |
|---|---|
| New blob for `todo.md`, new manifest, `HEAD` moved | Nothing yet. A snapshot never touches the network. |

Every snapshot is a complete listing of the notes folder, not a diff. A manifest
costs one line per file (for about 1,200 notes, roughly 250 KB, or 76 KB
compressed), but the blobs it points to are shared with every other snapshot.

---

## 5. Pushing (the Mac uploads)

The bucket's HEAD is the Mac's previous snapshot, so the Mac is simply ahead
(a *fast-forward*):

1. **Read the push-log** to learn what the bucket already has. To save time,
   the Mac keeps `push-log-cache`: everything it has seen so far, plus a
   cursor saying which push-log entries it has already read. So it downloads
   only entries newer than the cursor.
2. **Upload blobs** the push-log doesn't list. Each is read from the local
   store (`objects/`), compressed, encrypted, and uploaded to `blobs/<sha>`.
   If the local store's copy is gone (deleted by `gc`), ask the bucket:
   - **The bucket has it:** nothing to do.
   - **The bucket lacks it too, and the new snapshot uses it:** the file is
     still in your notes folder, but it changed (or its copy vanished) after
     this snapshot was taken. Stop and retry. The next sync's snapshot re-reads
     the file from your notes folder into the local store, then uploads it.
   - **The bucket lacks it too, and only older snapshots use it:** that old
     version is gone from everywhere and nothing can bring it back. Skip it,
     report it (REditMark shows it in the Sync panel), and carry on. Your
     current notes don't need it.
3. **Upload manifests** the push-log doesn't list, oldest first. That way
   anyone reading the bucket never finds a manifest whose parent is missing.
4. **Check what the new HEAD needs.** Every blob in the new snapshot and every
   manifest in its history must be in the bucket: uploaded just now, needed
   by the HEAD being replaced (so already checked), or confirmed by asking
   the bucket. Usually there is nothing to ask about.
5. **Move the bucket's HEAD** to the new manifest. This single write is the
   moment the snapshot becomes visible to other devices.
6. **Add a push-log entry** listing what this push uploaded, and update
   `push-log-cache`.

| On the Mac | In the bucket |
|---|---|
| `push-log-cache` updated | New `blobs/…`, `manifests/…`, `push-log/…` entry; `HEAD` = Mac's HEAD |

If the push stops halfway (no network, laptop closes), whatever was uploaded
just sits unreferenced until the next push, and HEAD still points at a
complete snapshot. Steps 4–5 are why HEAD never points at a snapshot that
can't be downloaded.

---

## 6. Pulling (the phone catches up)

The phone already has a snapshot, but an older one. (A phone with no
snapshot yet does a first sync instead, §6.1.)

1. **Read the bucket's HEAD.** It differs from the phone's.
2. **Download the manifest chain:** the new HEAD, then its parent, and so on,
   until reaching manifests the phone already has. Each one is decrypted and
   its SHA checked.
3. **Decide:** the phone's HEAD is an ancestor of the bucket's, so this is a
   fast-forward.
4. **Download the blobs the new HEAD uses** that the phone doesn't have,
   smallest first. Only the current snapshot's, not the whole history's.
5. **Apply it carefully:** change only paths that differ between the phone's
   snapshot and the new one. Before replacing or removing a file, check that
   it still holds exactly what the phone last snapshotted. If you edited it
   meanwhile, stop. The next cycle snapshots your edit and merges it (§8).
   Files the phone didn't know about are never deleted.
6. Point the phone's `.blobs/HEAD` at the new manifest.

| On the phone | In the bucket |
|---|---|
| New manifests, the new snapshot's blobs, files updated, `HEAD` = bucket's | Unchanged. A pull never writes to the bucket. |

### 6.1 A device's first sync

A new phone (no `.blobs/HEAD` yet) starts from the bucket's **newest
snapshot only**:

1. **Note which snapshot it's downloading** in `.blobs/clone-target`, so an
   interrupted first sync resumes with the same one, along with which of its
   paths already hold a different file.
2. **Download that one manifest**, not the history ("1,224 files listed").
   Older manifests are never needed by sync; later syncs download only
   manifests newer than this one.
3. **Download its blobs smallest first** ("Downloading 312 of 1,224"). Notes
   are small, so they arrive in seconds and PDFs come last. Each file is
   written into the notes folder as soon as its blob arrives, unless
   something is already at that path.
4. **Finish:** every path gets the snapshot's version, with two exceptions.
   A note you edited while files were still arriving keeps your edit, which
   the next snapshot records on top. A file that was already at a path before
   the first sync began (a note made on the phone before pairing; step 1
   records these) is renamed to a conflict copy next to it. Files the
   snapshot doesn't have are left alone.
5. **Point `.blobs/HEAD` at the snapshot** and delete `clone-target`. The rest of
   the cycle (§7) then snapshots the phone's own notes on top and pushes them.

Before this, a first sync snapshotted the phone's notes first. That gave the
phone a history unrelated to the bucket's, and sync refused to merge them.

---

## 7. One sync cycle

REditMark and `shasync watch` run one combined cycle:

```
snapshot the notes folder (§4)
read the bucket's HEAD
├─ no HEAD in bucket            → push everything (§5)
├─ same as local HEAD           → up to date, done
├─ bucket's is behind ours      → push (§5)
├─ ours is behind the bucket's  → pull (§6)
└─ both moved since they agreed → merge (§8), then push the merge
```

When REditMark runs it:

- **After edits settle:** 60 seconds after your last save, or at most 5
  minutes after the first unsynced save while you keep typing. This avoids
  a snapshot every few seconds.
- **Right away** when you switch notes, close a window, switch away from
  the app, or quit.
- **Every 30 seconds:** one small request comparing the bucket's HEAD with
  yours. If another device has synced, sync now.
- **Every 5 minutes:** a full cycle regardless.

---

## 8. Both devices edited (merge)

The Mac and phone both snapshotted since they last agreed:

```
            ┌── M1 ── M2      (Mac)
  ── A ─────┤
            └── P1            (phone, already pushed: bucket HEAD = P1)
```

1. Find the **common ancestor** A by walking both parent chains.
2. Download the blobs the phone's side added since A.
3. Compare each path in A, M2 (local) and P1 (remote):
   - same on both sides → keep it;
   - only one side changed it → take that change;
   - both changed it differently → **conflict**: the phone's version keeps
     the name, and the Mac's version is saved next to it as
     `todo (modified on 2026-10-10 211904 by aed23d4d).md`.
4. Write the merge snapshot with **P1 as its parent**, apply it to the notes folder
   (§6, step 5), and push it.

```
  ── A ── P1 ── merge          (the bucket's history stays a straight line)
```

Nothing is lost: both versions of a conflicted file are in the merged
snapshot, and M1 and M2 still exist as manifests, although HEAD's chain no
longer passes through them.

---

## 9. Freeing local space (`gc`)

`shasync gc` (CLI only) deletes local copies of old blobs:

1. Keep every blob the current HEAD uses.
2. **List the bucket's `blobs/`.**
3. Delete the local copies of the other blobs that the listing shows in the
   bucket. Anything the bucket lacks stays.

| On the Mac | In the bucket |
|---|---|
| Old blob copies removed from `objects/` | Unchanged (gc never deletes from the bucket) |

Old versions remain recoverable from the bucket.

---

## 10. The push-log is only a hint

The push-log exists to save work: without it, a push would have to ask the
bucket about every blob in its history. It can be **incomplete**:

- blobs uploaded before the push-log existed aren't in it;
- a push that stops between moving HEAD and writing its entry leaves that
  entry out;
- devices with different clocks can make one skip another's entries;
- `push-log-cache` is cleared when the remote changes, which forgets
  anything only the cache remembered.

It can also be **wrong the other way** if objects are removed from the bucket
by hand or a copy between buckets is partial.

So nothing that could cause damage relies on it:

| Operation | Could go wrong if it trusted the push-log | What it does instead |
|---|---|---|
| `gc` | Delete the only copy of a blob the bucket lacks | Lists the bucket itself (§9) |
| Push, uploading a blob missing from the local store | Fail with "No such file or directory" (the October 2026 bug) | Asks the bucket; skips it if present, retries if it's a current file, reports it if it's an old version gone everywhere (§5, step 2) |
| Push, moving HEAD | Point other devices at a snapshot they can't download | Checks everything the new HEAD needs first (§5, step 4) |

An out-of-date push-log now costs only extra uploads or checks, and each push
adds what it uploaded or confirmed, so the log fills in over time.

---

## 11. What is guaranteed

1. **Nothing is deleted from the bucket by sync.** Blobs and manifests are
   only added; only `HEAD` changes.
2. **The bucket's HEAD always points at a snapshot that can be downloaded
   whole:** its manifest chain and every blob it uses are in the bucket.
3. **A local blob is deleted only when a listing of the bucket shows it there.**
4. **Syncing never overwrites an edit it hasn't snapshotted** and never
   deletes a file it didn't know about (§6, step 5).
5. **Conflicts keep both versions** (§8).
6. **Everything in the bucket except `HEAD`, `salt` and the push-log is
   encrypted**, and those three hold only hashes, random bytes and times.

Known gap: two devices moving the bucket's HEAD at the same instant — the
later write wins. The earlier device still has its snapshot locally, sees on
its next check (within 30 seconds in REditMark) that the bucket's HEAD isn't
its own, and merges. So the change is delayed, not lost. S3's conditional
writes could close this window.
