//! Pull / push / sync against the in-memory and local-directory remotes,
//! including the Go syncsafe_test.go scenarios (edits made during a sync are
//! never overwritten, files created during a sync are never deleted).

mod common;

use std::fs;
use std::path::Path;
use std::sync::Arc;

use common::{get, put};
use shasync_core::remote::{LocalDirRemote, MemoryRemote, Remote};
use shasync_core::{Error, Key, Repo, SyncAction, Syncer};

fn key() -> Key {
    Key::from_hex("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f").unwrap()
}

struct Machine {
    _dir: tempfile::TempDir,
    syncer: Syncer,
}

impl Machine {
    fn new(remote: Arc<dyn Remote>) -> Machine {
        let dir = tempfile::tempdir().unwrap();
        let repo = Repo::init(dir.path()).unwrap();
        let syncer = Syncer::new(repo, remote, key()).unwrap();
        Machine { _dir: dir, syncer }
    }
    fn root(&self) -> &Path {
        self.syncer.repo().root()
    }
    fn put(&self, rel: &str, data: &str) {
        put(self.root(), rel, data.as_bytes());
    }
    fn get(&self, rel: &str) -> Option<String> {
        get(self.root(), rel)
    }
    /// One watch cycle; an edit during the cycle may stop it early.
    async fn sync(&self) -> Option<SyncAction> {
        match self.syncer.sync().await {
            Ok(r) => Some(r.action),
            Err(Error::LocalEditsDuringSync) => None,
            Err(e) => panic!("sync failed: {e}"),
        }
    }
    fn conflict_copies(&self, stem: &str) -> Vec<String> {
        let mut v: Vec<String> = fs::read_dir(self.root())
            .unwrap()
            .map(|e| e.unwrap().file_name().into_string().unwrap())
            .filter(|n| n.starts_with(&format!("{stem} (modified on ")))
            .collect();
        v.sort();
        v
    }
}

/// B creates the repo and syncs; A is a fresh clone that syncs it down.
async fn two_machines(remote: Arc<dyn Remote>, files: &[(&str, &str)]) -> (Machine, Machine) {
    let (a, b) = (Machine::new(remote.clone()), Machine::new(remote));
    for (n, d) in files {
        b.put(n, d);
    }
    assert!(matches!(b.sync().await, Some(SyncAction::Pushed { .. })));
    assert!(matches!(a.sync().await, Some(SyncAction::FastForwarded { .. })));
    (a, b)
}

#[tokio::test(flavor = "multi_thread")]
async fn edits_made_during_sync_are_kept() {
    let mem = MemoryRemote::new();
    let (a, b) = two_machines(Arc::new(mem.clone()), &[("shared.md", "v1\n"), ("draft.md", "draft v1\n")]).await;
    b.put("shared.md", "v2 from B\n");
    b.sync().await;

    let root = a.root().to_path_buf();
    mem.on_next_blob_download(move || {
        put(&root, "draft.md", b"draft v2, typed on A during sync\n");
        put(&root, "brand-new.md", b"created on A during sync\n");
    });
    a.sync().await;
    assert_eq!(a.get("shared.md").unwrap(), "v2 from B\n", "B's change not applied on A");
    assert_eq!(a.get("draft.md").unwrap(), "draft v2, typed on A during sync\n", "edit overwritten");
    assert_eq!(a.get("brand-new.md").unwrap(), "created on A during sync\n", "new file deleted");

    a.sync().await;
    b.sync().await;
    assert_eq!(b.get("draft.md").unwrap(), "draft v2, typed on A during sync\n");
    assert_eq!(b.get("brand-new.md").unwrap(), "created on A during sync\n");
}

#[tokio::test(flavor = "multi_thread")]
async fn same_file_edited_during_sync_becomes_conflict_copy() {
    let mem = MemoryRemote::new();
    let (a, b) = two_machines(Arc::new(mem.clone()), &[("note.md", "v1\n")]).await;
    b.put("note.md", "v2 from B\n");
    b.sync().await;

    let root = a.root().to_path_buf();
    mem.on_next_blob_download(move || put(&root, "note.md", b"v2 typed on A during sync\n"));
    assert_eq!(a.sync().await, None, "cycle should stop with LocalEditsDuringSync");
    assert_eq!(a.get("note.md").unwrap(), "v2 typed on A during sync\n", "edit overwritten mid-sync");

    let Some(SyncAction::Merged { conflicts, pushed: true, .. }) = a.sync().await else { panic!("expected a merge") };
    assert_eq!(conflicts, vec!["note.md"]);
    assert_eq!(a.get("note.md").unwrap(), "v2 from B\n");
    let copies = a.conflict_copies("note");
    assert_eq!(copies.len(), 1);
    assert_eq!(a.get(&copies[0]).unwrap(), "v2 typed on A during sync\n");

    b.sync().await;
    assert_eq!(b.get(&copies[0]).unwrap(), "v2 typed on A during sync\n");
}

#[tokio::test(flavor = "multi_thread")]
async fn pull_never_clobbers_and_delete_spares_edited_file() {
    let mem = MemoryRemote::new();
    let (a, b) = two_machines(Arc::new(mem.clone()), &[("keep.md", "k\n"), ("gone.md", "g\n"), ("x.md", "x1\n")]).await;
    // B deletes gone.md and changes x.md; A edits gone.md while pulling.
    fs::remove_file(b.root().join("gone.md")).unwrap();
    b.put("x.md", "x2 longer\n");
    b.sync().await;
    let root = a.root().to_path_buf();
    mem.on_next_blob_download(move || put(&root, "gone.md", b"edited on A mid-pull\n"));
    assert!(matches!(a.syncer.pull().await, Err(Error::LocalEditsDuringSync)));
    assert_eq!(a.get("gone.md").unwrap(), "edited on A mid-pull\n");
    // The next pull commits the edit and merges: A's edit survives B's delete.
    let rep = a.syncer.pull().await.unwrap();
    assert!(matches!(rep.action, SyncAction::Merged { pushed: false, .. }), "{rep:?}");
    assert_eq!(a.get("x.md").unwrap(), "x2 longer\n");
    assert_eq!(a.get("gone.md"), None, "remote delete keeps the path deleted");
    let copies = a.conflict_copies("gone");
    assert_eq!(copies.len(), 1);
    assert_eq!(a.get(&copies[0]).unwrap(), "edited on A mid-pull\n");
}

#[tokio::test(flavor = "multi_thread")]
async fn fork_merge_disjoint_and_conflicting() {
    let mem: Arc<dyn Remote> = Arc::new(MemoryRemote::new());
    let (a, b) = two_machines(mem, &[("both.md", "base\n"), ("same.md", "base\n")]).await;
    a.put("both.md", "from A\n");
    a.put("only-a.md", "a\n");
    a.put("same.md", "identical\n");
    a.syncer.repo().commit("A offline").unwrap();
    b.put("both.md", "from B\n");
    b.put("only-b.md", "b\n");
    b.put("same.md", "identical\n");
    fs::create_dir_all(b.root().join("dir")).unwrap();
    b.put("dir/new.txt", "nested\n");
    assert!(matches!(b.syncer.push().await.unwrap().action, SyncAction::Pushed { .. }));

    // A is behind and diverged: push merges (as Go's sync does) and uploads.
    let rep = a.syncer.push().await.unwrap();
    let SyncAction::Merged { conflicts, pushed: true, .. } = rep.action else { panic!("{rep:?}") };
    assert_eq!(conflicts, vec!["both.md"]);
    assert_eq!(a.get("both.md").unwrap(), "from B\n");
    assert_eq!(a.get("only-a.md").unwrap(), "a\n");
    assert_eq!(a.get("only-b.md").unwrap(), "b\n");
    assert_eq!(a.get("same.md").unwrap(), "identical\n");
    assert_eq!(a.get("dir/new.txt").unwrap(), "nested\n");
    let copy = &a.conflict_copies("both")[0];
    assert_eq!(a.get(copy).unwrap(), "from A\n");
    let merge_head = a.syncer.repo().head().unwrap().unwrap();
    let m = a.syncer.repo().read_manifest(&merge_head).unwrap();
    assert!(m.message.starts_with("auto-merge ") && m.message.ends_with("(1 conflict)"), "{}", m.message);

    // B fast-forwards to the merge.
    assert!(matches!(b.syncer.pull().await.unwrap().action, SyncAction::FastForwarded { .. }));
    assert_eq!(b.get(copy).unwrap(), "from A\n");
    assert_eq!(b.syncer.repo().head().unwrap().unwrap(), merge_head);
    assert!(matches!(b.sync().await, Some(SyncAction::UpToDate { .. })));
}

#[tokio::test(flavor = "multi_thread")]
async fn push_refuses_when_remote_strictly_ahead() {
    let mem: Arc<dyn Remote> = Arc::new(MemoryRemote::new());
    let (a, b) = two_machines(mem, &[("f.md", "1\n")]).await;
    b.put("f.md", "22\n");
    b.sync().await;
    assert!(matches!(a.syncer.push().await, Err(Error::RemoteAhead { .. })));
    assert!(matches!(a.syncer.pull().await.unwrap().action, SyncAction::FastForwarded { .. }));
    assert_eq!(a.get("f.md").unwrap(), "22\n");
    assert!(matches!(a.syncer.pull().await.unwrap().action, SyncAction::UpToDate { .. }));
    // Local ahead: pull says so, push uploads.
    a.put("f.md", "333\n");
    a.syncer.repo().commit("ahead").unwrap();
    assert!(matches!(a.syncer.pull().await.unwrap().action, SyncAction::LocalAhead { .. }));
    assert!(matches!(a.syncer.push().await.unwrap().action, SyncAction::Pushed { .. }));
}

#[tokio::test(flavor = "multi_thread")]
async fn everything_uploaded_is_encrypted_and_plaintext_is_refused() {
    let mem = MemoryRemote::new();
    let (a, b) = two_machines(Arc::new(mem.clone()), &[("secret.txt", "TOP SECRET CONTENT\n"), ("dir/x.md", "x\n")]).await;
    for (k, v) in mem.objects() {
        if k.starts_with("blobs/") || k.starts_with("manifests/") {
            assert_eq!(&v[..5], b"SHAS2", "{k}");
            assert!(!String::from_utf8_lossy(&v).contains("TOP SECRET") && !String::from_utf8_lossy(&v).contains("secret.txt"));
        } else {
            assert!(k == "HEAD" || k.starts_with("push-log/"), "{k}");
        }
    }
    // Replace a blob B is about to fetch with its plaintext: A must refuse it.
    b.put("new.txt", "fresh\n");
    b.sync().await;
    let sha = shasync_core::sha256_hex(b"fresh\n");
    mem.insert_raw(&format!("blobs/{sha}"), b"fresh\n".to_vec());
    let err = a.syncer.pull().await.unwrap_err();
    assert!(matches!(err, Error::NotEncrypted(_)), "{err}");
    assert_eq!(a.get("new.txt"), None);
}

#[tokio::test(flavor = "multi_thread")]
async fn wrong_key_cannot_read() {
    let mem: Arc<dyn Remote> = Arc::new(MemoryRemote::new());
    let b = Machine::new(mem.clone());
    b.put("f.md", "1\n");
    b.sync().await;
    let dir = tempfile::tempdir().unwrap();
    let other = Syncer::new(Repo::init(dir.path()).unwrap(), mem, Key::generate().unwrap()).unwrap();
    assert!(matches!(other.pull().await, Err(Error::Crypto(_))));
}

#[tokio::test(flavor = "multi_thread")]
async fn default_ignores_and_blobsignore_stay_local() {
    let mem: Arc<dyn Remote> = Arc::new(MemoryRemote::new());
    let (a, b) = (Machine::new(mem.clone()), Machine::new(mem));
    for n in ["note.md", ".note.md.swp", "note.md~", "4913", ".DS_Store", ".#note.md", ".tmp-123", "#auto#", "sub/._x", "logs/a.log"] {
        b.put(n, "x");
    }
    b.put(".blobsignore", "logs/\n");
    b.sync().await;
    a.sync().await;
    let files = a.syncer.repo().scan().unwrap();
    assert_eq!(files, vec![".blobsignore", "note.md"]);
}

#[tokio::test(flavor = "multi_thread")]
async fn local_dir_remote_round_trip_and_push_log_dedup() {
    let bucket = tempfile::tempdir().unwrap();
    let remote: Arc<dyn Remote> = Arc::new(LocalDirRemote::new(bucket.path()));
    let (a, b) = two_machines(remote.clone(), &[("a.txt", "same bytes\n"), ("b.txt", "same bytes\n")]).await;
    assert_eq!(a.get("b.txt").unwrap(), "same bytes\n");
    assert_eq!(remote.list("blobs/").await.unwrap().len(), 1, "identical files upload once");

    // A second push of already-uploaded content uploads no blobs (push-log).
    b.put("c.txt", "same bytes\n");
    let rep = b.syncer.push().await.unwrap();
    let SyncAction::Pushed { new_blobs, new_manifests, .. } = rep.action else { panic!("{rep:?}") };
    assert_eq!((new_blobs, new_manifests), (0, 1));
    assert!(fs::read_to_string(b.root().join(".blobs/push-log-cache")).unwrap().starts_with("push-log/"));
    assert_eq!(remote.list("push-log/").await.unwrap().len(), 2);

    // Remote HEAD is plaintext "<sha>\n".
    let head = remote.get("HEAD").await.unwrap();
    assert_eq!(String::from_utf8(head).unwrap(), format!("{}\n", b.syncer.repo().head().unwrap().unwrap()));
}

#[tokio::test(flavor = "multi_thread")]
async fn passphrase_salt_shared_through_remote() {
    let mem: Arc<dyn Remote> = Arc::new(MemoryRemote::new());
    let (d1, d2) = (tempfile::tempdir().unwrap(), tempfile::tempdir().unwrap());
    let (r1, r2) = (Repo::init(d1.path()).unwrap(), Repo::init(d2.path()).unwrap());
    let k1 = shasync_core::sync::set_passphrase(&r1, mem.as_ref(), b"pass phrase").await.unwrap();
    let k2 = shasync_core::sync::set_passphrase(&r2, mem.as_ref(), b"pass phrase").await.unwrap();
    assert_eq!(k1, k2);
    assert_eq!(r2.require_key().unwrap(), k1);
    let k3 = shasync_core::sync::derive_key_from_remote(mem.as_ref(), b"other").await.unwrap();
    assert_ne!(k1, k3);
    assert!(shasync_core::sync::set_passphrase(&r1, mem.as_ref(), b"pass phrase").await.is_err(), "key exists");
}

#[test]
fn checkout_and_status_roundtrip() {
    let dir = tempfile::tempdir().unwrap();
    let repo = Repo::init(dir.path()).unwrap();
    put(dir.path(), "a/b/c.txt", b"one\n");
    put(dir.path(), "top.txt", b"top\n");
    let c1 = repo.commit("one").unwrap().head.unwrap();
    assert!(!repo.commit("again").unwrap().committed, "clean tree commits nothing");
    put(dir.path(), "top.txt", b"two!!\n");
    fs::remove_file(dir.path().join("a/b/c.txt")).unwrap();
    assert!(matches!(repo.checkout(&c1, false), Err(Error::UncommittedChanges)));
    let c2 = repo.commit("two").unwrap().head.unwrap();
    repo.checkout(&c1, false).unwrap();
    assert_eq!(get(dir.path(), "a/b/c.txt").unwrap(), "one\n");
    assert_eq!(get(dir.path(), "top.txt").unwrap(), "top\n");
    assert!(repo.status().unwrap().is_clean());
    repo.checkout(&c2, false).unwrap();
    assert!(!dir.path().join("a").exists(), "empty dirs pruned");
    let log = repo.log(None, 10).unwrap();
    assert_eq!(log.iter().map(|e| e.manifest.message.as_str()).collect::<Vec<_>>(), vec!["two", "one"]);
    assert_eq!(log[0].removed, vec!["a/b/c.txt"]);
    assert_eq!(log[0].modified, vec!["top.txt"]);
}

/// A remote whose push-log predates some blobs (they were uploaded before
/// the push-log existed) and a repo whose `gc` removed its copies of them:
/// pushing must not try to re-upload them from local disk.
#[tokio::test(flavor = "multi_thread")]
async fn push_skips_gced_blobs_the_push_log_never_listed() {
    let old = MemoryRemote::new();
    let a = Machine::new(Arc::new(old.clone()));
    a.put("note.md", "v1\n");
    a.sync().await;
    let v1 = a.syncer.repo().read_manifest(&a.syncer.repo().head().unwrap().unwrap()).unwrap().files["note.md"].sha.clone();
    a.put("note.md", "v2, longer\n");
    a.sync().await;

    // The same objects without the push-log entries that list them.
    let mem = MemoryRemote::new();
    for (k, v) in old.objects() {
        if !k.starts_with("push-log/") {
            mem.insert_raw(&k, v);
        }
    }
    mem.insert_raw("push-log/00000000000000000001-unrelated", b"\n".to_vec());
    let root = a.root().to_path_buf();
    fs::remove_file(a.syncer.repo().object_path(&v1)).unwrap();
    let _ = fs::remove_file(root.join(".blobs/push-log-cache"));

    let syncer = Syncer::new(Repo::open_exact(&root).unwrap(), Arc::new(mem.clone()), key()).unwrap();
    put(&root, "note.md", b"v3, longer still\n");
    let rep = syncer.sync().await.unwrap();
    assert!(matches!(rep.action, SyncAction::Pushed { .. }), "{rep:?}");

    // A blob gone from both sides is an error, not a silent skip.
    let mem2 = MemoryRemote::new();
    for (k, v) in mem.objects() {
        if !k.starts_with("push-log/") && !k.ends_with(&v1) {
            mem2.insert_raw(&k, v);
        }
    }
    mem2.insert_raw("push-log/00000000000000000001-unrelated", b"\n".to_vec());
    let _ = fs::remove_file(root.join(".blobs/push-log-cache"));
    let syncer = Syncer::new(Repo::open_exact(&root).unwrap(), Arc::new(mem2), key()).unwrap();
    put(&root, "note.md", b"v4\n");
    let err = syncer.sync().await.unwrap_err().to_string();
    assert!(err.contains(&v1) && err.contains("neither here nor on the remote"), "{err}");
}

/// A push-log that lists a blob the remote doesn't have (deleted by hand,
/// a half-copied bucket) must not let HEAD point at a snapshot nobody can
/// download: the push checks and uploads it before moving HEAD.
#[tokio::test(flavor = "multi_thread")]
async fn push_never_trusts_the_push_log_for_what_head_needs() {
    let old = MemoryRemote::new();
    let a = Machine::new(Arc::new(old.clone()));
    a.put("note.md", "v1\n");
    a.sync().await;
    let blob = a.syncer.repo().read_manifest(&a.syncer.repo().head().unwrap().unwrap()).unwrap().files["note.md"].sha.clone();

    // Same push-log, but the blob and HEAD are gone.
    let mem = MemoryRemote::new();
    for (k, v) in old.objects() {
        if k != format!("blobs/{blob}") && k != "HEAD" {
            mem.insert_raw(&k, v);
        }
    }
    let root = a.root().to_path_buf();
    let syncer = Syncer::new(Repo::open_exact(&root).unwrap(), Arc::new(mem.clone()), key()).unwrap();
    put(&root, "other.md", b"new file\n");
    let rep = syncer.sync().await.unwrap();
    assert!(matches!(rep.action, SyncAction::Pushed { .. }), "{rep:?}");
    assert!(mem.objects().contains_key(&format!("blobs/{blob}")), "HEAD points at a snapshot whose blob isn't on the remote");

    // A fresh clone can sync it.
    let b = Machine::new(Arc::new(mem.clone()));
    assert!(matches!(b.sync().await, Some(SyncAction::FastForwarded { .. })));
    assert_eq!(b.get("note.md").unwrap(), "v1\n");
}
