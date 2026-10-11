//! Talking to a remote: HEAD pointer, salt, object transfer, the push-log
//! dedup protocol, and the pull / push / sync flows. Mirrors `sync.go`,
//! `watch.go` (`syncOnce`, `fetchAndCheckout`) and the transfer half of
//! `commands.go` (`uploadManifestChainAndSetHead`, `doMerge`, …).

use std::collections::{BTreeSet, HashSet};
use std::fs;
use std::sync::Arc;

use futures::stream::{self, StreamExt, TryStreamExt};

use crate::crypto::{decode_object, encode_object, is_encrypted, Key, SALT_LENGTH};
use crate::error::{Error, Result};
use crate::merge::auto_merge;
use crate::remote::{remote_blob_key, remote_manifest_key, Remote, PUSH_LOG_PREFIX, REMOTE_HEAD_KEY, REMOTE_SALT_KEY};
use crate::repo::{now_unix_ms, CommitResult, Repo};

/// Parallel transfers, as in Go.
const WORKERS: usize = 16;

// ----- HEAD and salt ---------------------------------------------------------------

/// The SHA at `<prefix>/HEAD`, or `None` if there is none yet.
pub async fn read_remote_head(r: &dyn Remote) -> Result<Option<String>> {
    if !r.exists(REMOTE_HEAD_KEY).await? {
        return Ok(None);
    }
    let b = r.get(REMOTE_HEAD_KEY).await?;
    let sha = String::from_utf8_lossy(&b).trim().to_string();
    if sha.len() != 64 {
        return Err(Error::MalformedRemoteHead(sha));
    }
    Ok(Some(sha))
}

/// Writes `sha + "\n"` (plaintext) to `<prefix>/HEAD`.
pub async fn write_remote_head(r: &dyn Remote, sha: &str) -> Result<()> {
    r.put(REMOTE_HEAD_KEY, format!("{sha}\n").into_bytes()).await
}

/// The 16-byte Argon2id salt at `<prefix>/salt`, if present.
pub async fn fetch_salt(r: &dyn Remote) -> Result<Option<[u8; SALT_LENGTH]>> {
    if !r.exists(REMOTE_SALT_KEY).await? {
        return Ok(None);
    }
    let b = r.get(REMOTE_SALT_KEY).await?;
    let salt: [u8; SALT_LENGTH] = b.as_slice().try_into().map_err(|_| Error::BadSalt(b.len()))?;
    Ok(Some(salt))
}

/// Fetches the salt, creating and uploading a fresh one if none exists
/// (Go: `fetchOrCreateSalt`). Returns (salt, created).
pub async fn fetch_or_create_salt(r: &dyn Remote) -> Result<([u8; SALT_LENGTH], bool)> {
    if let Some(s) = fetch_salt(r).await? {
        return Ok((s, false));
    }
    let s = crate::crypto::generate_salt()?;
    r.put(REMOTE_SALT_KEY, s.to_vec()).await?;
    Ok((s, true))
}

/// `shasync key set-passphrase`: fetch (or create) the remote salt, derive
/// the key with Argon2id and save it to `.blobs/key`. Refuses to replace an
/// existing key file.
pub async fn set_passphrase(repo: &Repo, r: &dyn Remote, passphrase: &[u8]) -> Result<Key> {
    if passphrase.is_empty() {
        return Err(Error::InvalidKey("empty passphrase".into()));
    }
    if repo.key_path().exists() {
        return Err(Error::Other(format!("{} already exists", repo.key_path().display())));
    }
    let (salt, _) = fetch_or_create_salt(r).await?;
    let pass = passphrase.to_vec();
    let key = tokio::task::spawn_blocking(move || Key::derive_from_passphrase(&pass, &salt)).await.map_err(join_err)??;
    repo.save_key(&key, false)?;
    Ok(key)
}

/// Derives the key from a passphrase and the remote's existing salt (does not
/// create a salt or write anything).
pub async fn derive_key_from_remote(r: &dyn Remote, passphrase: &[u8]) -> Result<Key> {
    let salt = fetch_salt(r).await?.ok_or_else(|| Error::Other("remote has no salt (no passphrase set up yet)".into()))?;
    let pass = passphrase.to_vec();
    tokio::task::spawn_blocking(move || Key::derive_from_passphrase(&pass, &salt)).await.map_err(join_err)?
}

fn join_err(e: tokio::task::JoinError) -> Error {
    Error::Other(format!("background task failed: {e}"))
}

// ----- outcomes ----------------------------------------------------------------------

/// What a pull / push / sync did.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SyncAction {
    /// No commits anywhere (or, for pull, no remote HEAD yet).
    Nothing,
    /// Local HEAD == remote HEAD.
    UpToDate { head: String },
    /// Pull only: local is strictly ahead of the remote.
    LocalAhead { local: String, remote: String },
    /// Uploaded `head` and moved the remote HEAD to it.
    Pushed { head: String, new_blobs: usize, new_manifests: usize },
    /// Applied the remote tip to the working tree.
    FastForwarded { to: String },
    /// Chains had diverged: merged, applied to the working tree, and (for
    /// push/sync) uploaded.
    Merged { merge: String, conflicts: Vec<String>, pushed: bool },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SyncReport {
    /// The implicit commit made at the start, if the tree was dirty.
    pub committed: Option<String>,
    pub action: SyncAction,
}

// ----- Syncer --------------------------------------------------------------------------

/// Everything needed to sync one repo with one remote.
///
/// All operations need a tokio runtime (CPU/disk-heavy steps run on
/// `spawn_blocking`).
#[derive(Clone)]
pub struct Syncer {
    repo: Repo,
    remote: Arc<dyn Remote>,
    key: Key,
    client_id: String,
}

impl Syncer {
    /// Uses (and if needed, creates) the repo's client ID.
    pub fn new(repo: Repo, remote: Arc<dyn Remote>, key: Key) -> Result<Syncer> {
        let client_id = repo.ensure_client_id()?;
        Ok(Syncer { repo, remote, key, client_id })
    }

    pub fn repo(&self) -> &Repo {
        &self.repo
    }

    pub fn remote(&self) -> &dyn Remote {
        self.remote.as_ref()
    }

    async fn blocking<T: Send + 'static>(&self, f: impl FnOnce(Repo) -> Result<T> + Send + 'static) -> Result<T> {
        let repo = self.repo.clone();
        tokio::task::spawn_blocking(move || f(repo)).await.map_err(join_err)?
    }

    async fn commit_if_dirty(&self) -> Result<CommitResult> {
        let id = self.client_id.clone();
        self.blocking(move |r| r.commit_if_dirty(&id)).await
    }

    /// Commit + pull: brings remote changes into the working tree without
    /// ever overwriting a file edited during the sync. Diverged chains are
    /// merged locally (not uploaded; the next push does that).
    pub async fn pull(&self) -> Result<SyncReport> {
        let c = self.commit_if_dirty().await?;
        let committed = c.committed.then(|| c.head.clone().unwrap());
        let report = |action| Ok(SyncReport { committed: committed.clone(), action });
        let Some(remote) = read_remote_head(self.remote()).await? else { return report(SyncAction::Nothing) };
        let local = self.repo.head_str()?;
        if local == remote {
            return report(SyncAction::UpToDate { head: remote });
        }
        self.fetch_manifest_chain(&remote).await?;
        if local.is_empty() || self.repo.is_ancestor(&local, &remote)? {
            self.fetch_and_apply(&remote).await?;
            return report(SyncAction::FastForwarded { to: remote });
        }
        if self.repo.is_ancestor(&remote, &local)? {
            return report(SyncAction::LocalAhead { local, remote });
        }
        let (merge, conflicts) = self.merge(&local, &remote).await?;
        self.fetch_and_apply(&merge).await?;
        report(SyncAction::Merged { merge, conflicts, pushed: false })
    }

    /// Commit + push. Fast-forwards the remote when it is empty or behind;
    /// when the chains diverged, merges (as Go's sync does), applies the merge
    /// to the working tree safely and uploads it. If the remote is strictly
    /// ahead, returns [`Error::RemoteAhead`] (pull first), like `shasync push`.
    pub async fn push(&self) -> Result<SyncReport> {
        let c = self.commit_if_dirty().await?;
        let committed = c.committed.then(|| c.head.clone().unwrap());
        let report = |action| Ok(SyncReport { committed: committed.clone(), action });
        let local = self.repo.head_str()?;
        if local.is_empty() {
            return report(SyncAction::Nothing);
        }
        let remote = read_remote_head(self.remote()).await?.unwrap_or_default();
        if remote == local {
            return report(SyncAction::UpToDate { head: local });
        }
        if remote.is_empty() || self.repo.is_ancestor(&remote, &local)? {
            return report(self.upload_chain_and_set_head(&local).await?);
        }
        self.fetch_manifest_chain(&remote).await?;
        if self.repo.is_ancestor(&local, &remote)? {
            return Err(Error::RemoteAhead { remote, local });
        }
        let (merge, conflicts) = self.merge(&local, &remote).await?;
        self.fetch_and_apply(&merge).await?;
        self.upload_chain_and_set_head(&merge).await?;
        report(SyncAction::Merged { merge, conflicts, pushed: true })
    }

    /// One full watch cycle (Go: `syncOnce`): commit, then fast-forward
    /// whichever side is behind, or merge a fork and push the merge.
    ///
    /// A fresh clone with no commits simply fast-forwards to the remote (as
    /// Go's `syncOnce` does).
    pub async fn sync(&self) -> Result<SyncReport> {
        let c = self.commit_if_dirty().await?;
        let committed = c.committed.then(|| c.head.clone().unwrap());
        let report = |action| Ok(SyncReport { committed: committed.clone(), action });
        let Some(remote) = read_remote_head(self.remote()).await? else {
            return match c.head {
                None => report(SyncAction::Nothing),
                Some(sha) => report(self.upload_chain_and_set_head(&sha).await?),
            };
        };
        let local = self.repo.head_str()?;
        if local == remote {
            return report(SyncAction::UpToDate { head: local });
        }
        self.fetch_manifest_chain(&remote).await?;
        if !local.is_empty() && self.repo.is_ancestor(&remote, &local)? {
            return report(self.upload_chain_and_set_head(&local).await?);
        }
        if local.is_empty() || self.repo.is_ancestor(&local, &remote)? {
            self.fetch_and_apply(&remote).await?;
            return report(SyncAction::FastForwarded { to: remote });
        }
        let (merge, conflicts) = self.merge(&local, &remote).await?;
        self.fetch_and_apply(&merge).await?;
        self.upload_chain_and_set_head(&merge).await?;
        report(SyncAction::Merged { merge, conflicts, pushed: true })
    }

    // ----- building blocks -----------------------------------------------------------

    /// Downloads manifest `tip` and its ancestors that aren't local (Go:
    /// `fetchManifestChain`). Doesn't touch the working tree or HEAD.
    pub async fn fetch_manifest_chain(&self, tip: &str) -> Result<()> {
        let mut cur = tip.to_string();
        while !cur.is_empty() {
            if !self.repo.has_manifest(&cur) {
                self.download_manifest(&cur).await?;
            }
            cur = self.repo.read_manifest(&cur)?.parent_sha;
        }
        Ok(())
    }

    /// Downloads every blob `manifest_sha` references that isn't local.
    /// Returns how many were downloaded.
    pub async fn fetch_blobs_for(&self, manifest_sha: &str) -> Result<usize> {
        if !self.repo.has_manifest(manifest_sha) {
            self.download_manifest(manifest_sha).await?;
        }
        let m = self.repo.read_manifest(manifest_sha)?;
        let need: BTreeSet<String> =
            m.files.values().filter(|f| !self.repo.has_object(&f.sha)).map(|f| f.sha.clone()).collect();
        self.download_blobs(need).await
    }

    /// Go's `fetchAndCheckout`: download `target`'s blobs, then move the
    /// working tree from local HEAD to `target` with
    /// [`Repo::apply_manifest_safely`]. Never overwrites edits.
    pub async fn fetch_and_apply(&self, target: &str) -> Result<()> {
        self.fetch_blobs_for(target).await?;
        let target = target.to_string();
        self.blocking(move |r| {
            let from = r.head()?;
            r.apply_manifest_safely(from.as_deref(), &target)
        })
        .await
    }

    async fn download_blobs(&self, shas: impl IntoIterator<Item = String>) -> Result<usize> {
        let shas: Vec<String> = shas.into_iter().collect();
        let n = shas.len();
        stream::iter(shas.into_iter().map(|sha| async move {
            let dst = self.repo.object_path(&sha);
            self.download_to(&remote_blob_key(&sha), dst, sha).await
        }))
        .buffer_unordered(WORKERS)
        .try_collect::<Vec<()>>()
        .await?;
        Ok(n)
    }

    async fn download_manifest(&self, sha: &str) -> Result<()> {
        self.download_to(&remote_manifest_key(sha), self.repo.manifest_path(sha), sha.to_string()).await
    }

    /// Go's `downloadAndStore`: fetch, refuse plaintext, decrypt, decompress,
    /// verify the SHA, write read-only.
    async fn download_to(&self, remote_key: &str, dst: std::path::PathBuf, sha: String) -> Result<()> {
        let data = self.remote.get(remote_key).await?;
        let key = self.key.clone();
        let name = remote_key.to_string();
        let repo = self.repo.clone();
        tokio::task::spawn_blocking(move || {
            let pt = decode_object(&key, &data, &sha, &name)?;
            repo.store_verified(&dst, &pt)
        })
        .await
        .map_err(join_err)?
    }

    /// Go's `uploadFile`: read, zstd, encrypt (mandatory), upload.
    /// Uploads one blob. The push-log doesn't list blobs uploaded before it
    /// existed, and `gc` deletes local copies of uploaded blobs, so a blob
    /// missing here is fine as long as the remote has it.
    async fn upload_blob(&self, sha: &str) -> Result<()> {
        let key = remote_blob_key(sha);
        if !self.repo.has_object(sha) {
            if self.remote.exists(&key).await? {
                return Ok(());
            }
            return Err(Error::Other(format!("blob {sha} is in this repo's history but neither here nor on the remote")));
        }
        self.upload_file(self.repo.object_path(sha), key).await
    }

    async fn upload_file(&self, local: std::path::PathBuf, remote_key: String) -> Result<()> {
        let key = self.key.clone();
        let data = tokio::task::spawn_blocking(move || -> Result<Vec<u8>> {
            let plain = fs::read(&local)?;
            encode_object(&key, &plain)
        })
        .await
        .map_err(join_err)??;
        if !is_encrypted(&data) {
            return Err(Error::Other(format!("refusing to upload {remote_key}: encryption produced no SHAS header")));
        }
        self.remote.put(&remote_key, data).await
    }

    /// Go's `doMerge`: requires a clean tree, finds the common ancestor,
    /// fetches remote-side blobs, builds and stores the merge manifest.
    /// Doesn't touch the working tree or HEAD.
    async fn merge(&self, local: &str, remote: &str) -> Result<(String, Vec<String>)> {
        if !self.blocking(|r| r.status()).await?.is_clean() {
            // The tree was committed moments ago, so this means a save landed
            // mid-sync; the next cycle commits and merges it.
            return Err(Error::LocalEditsDuringSync);
        }
        let anc = self.repo.find_common_ancestor(local, remote)?.ok_or_else(|| Error::NoCommonAncestor {
            local: crate::repo::short_sha(local).to_string(),
            remote: crate::repo::short_sha(remote).to_string(),
        })?;
        // Blobs referenced on the remote side since the ancestor.
        let mut need = BTreeSet::new();
        let mut cur = remote.to_string();
        while !cur.is_empty() && cur != anc {
            let m = self.repo.read_manifest(&cur)?;
            need.extend(m.files.values().filter(|f| !self.repo.has_object(&f.sha)).map(|f| f.sha.clone()));
            cur = m.parent_sha;
        }
        self.download_blobs(need).await?;
        let (m, conflicts) = auto_merge(&self.repo, &self.client_id, now_unix_ms(), &anc, remote, local)?;
        let sha = self.repo.write_manifest(&m)?;
        Ok((sha, conflicts))
    }

    /// Go's `uploadManifestChainAndSetHead`: upload the blobs and manifests of
    /// `sha`'s local chain that the push-log says the remote lacks (manifests
    /// oldest-first), set remote HEAD, then append a push-log entry.
    pub async fn upload_chain_and_set_head(&self, sha: &str) -> Result<SyncAction> {
        let mut chain = vec![sha.to_string()];
        let tip = self.repo.read_manifest(sha)?;
        let mut blobs: BTreeSet<String> = tip.files.values().map(|f| f.sha.clone()).collect();
        let mut p = tip.parent_sha;
        while !p.is_empty() && self.repo.has_manifest(&p) {
            let pm = self.repo.read_manifest(&p)?;
            blobs.extend(pm.files.values().map(|f| f.sha.clone()));
            chain.push(p);
            p = pm.parent_sha;
        }

        let mut cache = PushLogCache::read(&self.repo);
        let (mut known, new_cursor) = self.fetch_known_remote_shas(&cache).await?;

        let novel_blobs: Vec<String> = blobs.into_iter().filter(|b| !known.contains(b)).collect();
        stream::iter(novel_blobs.iter().map(|b| self.upload_blob(b)))
            .buffer_unordered(WORKERS)
            .try_collect::<Vec<()>>()
            .await?;

        let novel_manifests: Vec<String> = chain.iter().rev().filter(|m| !known.contains(*m)).cloned().collect();
        for m in &novel_manifests {
            self.upload_file(self.repo.manifest_path(m), remote_manifest_key(m)).await?;
        }

        write_remote_head(self.remote(), sha).await?;

        let all_novel: Vec<String> = novel_blobs.iter().chain(novel_manifests.iter()).cloned().collect();
        if !all_novel.is_empty() {
            let entry = all_novel.join("\n") + "\n";
            let key = format!("{PUSH_LOG_PREFIX}{:020}-{sha}", now_unix_ms());
            self.remote.put(&key, entry.into_bytes()).await?;
            known.extend(all_novel);
            cache.cursor = key;
            cache.known = known;
            let _ = cache.write(&self.repo);
        } else if new_cursor != cache.cursor {
            cache.cursor = new_cursor;
            cache.known = known;
            let _ = cache.write(&self.repo);
        }
        Ok(SyncAction::Pushed { head: sha.to_string(), new_blobs: novel_blobs.len(), new_manifests: novel_manifests.len() })
    }

    /// Go's `fetchKnownRemoteSHAs`: the SHAs listed in push-log entries newer
    /// than the cache cursor, merged into the cached set. A remote with no
    /// push-log and no cache falls back to listing `blobs/` and `manifests/`.
    async fn fetch_known_remote_shas(&self, cache: &PushLogCache) -> Result<(HashSet<String>, String)> {
        let entries = self.remote.list(PUSH_LOG_PREFIX).await?;
        if entries.is_empty() && cache.cursor.is_empty() && cache.known.is_empty() {
            let mut known = HashSet::new();
            for prefix in ["blobs/", "manifests/"] {
                for k in self.remote.list(prefix).await? {
                    known.insert(k.trim_start_matches(prefix).to_string());
                }
            }
            return Ok((known, String::new()));
        }
        let mut to_fetch: Vec<String> =
            entries.into_iter().filter(|e| cache.cursor.is_empty() || *e > cache.cursor).collect();
        to_fetch.sort();
        let lists: Vec<Vec<u8>> = stream::iter(to_fetch.iter().map(|k| self.remote.get(k)))
            .buffered(WORKERS)
            .try_collect()
            .await?;
        let mut known = cache.known.clone();
        for data in lists {
            for l in String::from_utf8_lossy(&data).split('\n') {
                if !l.is_empty() {
                    known.insert(l.to_string());
                }
            }
        }
        let cursor = to_fetch.last().cloned().unwrap_or_else(|| cache.cursor.clone());
        Ok((known, cursor))
    }
}

/// `.blobs/push-log-cache`: first line the cursor, then one known SHA per
/// line (same file and format as Go, so both can share a repo).
struct PushLogCache {
    cursor: String,
    known: HashSet<String>,
}

impl PushLogCache {
    fn read(repo: &Repo) -> PushLogCache {
        let mut c = PushLogCache { cursor: String::new(), known: HashSet::new() };
        let Ok(s) = fs::read_to_string(repo.push_log_cache_path()) else { return c };
        let mut lines = s.trim_end_matches('\n').split('\n');
        c.cursor = lines.next().unwrap_or_default().to_string();
        c.known = lines.filter(|l| !l.is_empty()).map(str::to_string).collect();
        c
    }

    fn write(&self, repo: &Repo) -> Result<()> {
        let mut shas: Vec<&String> = self.known.iter().collect();
        shas.sort();
        let mut s = format!("{}\n", self.cursor);
        for sha in shas {
            s.push_str(sha);
            s.push('\n');
        }
        crate::repo::write_file_atomic(&repo.push_log_cache_path(), s.as_bytes(), 0o644)
    }
}
