//! The local `.blobs/` store and the working tree: open/init, config, key,
//! HEAD, objects, manifests, scan, commit, status, checkout and the safe
//! apply used by sync. Mirrors `store.go`, `scan.go`, `commands.go`,
//! `syncsafe.go` and the local half of `sync.go`.

use std::collections::{BTreeMap, BTreeSet, HashSet};
use std::fs::{self, File};
use std::io::{self, Read};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::crypto::Key;
use crate::error::{Error, Result};
use crate::ignore::{match_ignore, read_ignore_patterns};
use crate::manifest::{diff_files, Manifest, ManifestFile};

pub const BLOBS_DIR: &str = ".blobs";
const OBJECTS_DIR: &str = "objects";
const MANIFESTS_DIR: &str = "manifests";
const HEAD_FILE: &str = "HEAD";
const CONFIG_FILE: &str = "config.json";
const KEY_FILE: &str = "key";
const PUSH_LOG_CACHE_FILE: &str = "push-log-cache";
const CLONE_TARGET_FILE: &str = "clone-target";

/// `.blobs/config.json`. Unknown fields are ignored on read (as in Go).
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Config {
    /// `s3://bucket/prefix` or `gs://bucket/prefix`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub remote: String,
    /// 8 hex chars identifying this clone; labels conflict copies.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub client_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub s3_endpoint: String,
    /// Name of an AWS credentials profile (used by the Go CLI; informational here).
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub s3_profile: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub s3_virtual_hosted_style: bool,
}

/// Result of [`Repo::commit`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CommitResult {
    /// The HEAD after the call (`None` if the repo has no commits and the
    /// tree is empty).
    pub head: Option<String>,
    /// True if a new manifest was written.
    pub committed: bool,
}

/// Working-tree changes relative to a manifest (each list sorted).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Status {
    pub head: Option<String>,
    pub added: Vec<String>,
    pub modified: Vec<String>,
    pub removed: Vec<String>,
}

impl Status {
    pub fn is_clean(&self) -> bool {
        self.added.is_empty() && self.modified.is_empty() && self.removed.is_empty()
    }
}

/// One entry of [`Repo::log`].
#[derive(Debug, Clone)]
pub struct LogEntry {
    pub sha: String,
    pub manifest: Manifest,
    pub added: Vec<String>,
    pub modified: Vec<String>,
    pub removed: Vec<String>,
}

/// A handle to a shasync repository: a folder containing `.blobs/`.
#[derive(Debug, Clone)]
pub struct Repo {
    root: PathBuf,
}

impl Repo {
    // ----- open / init -----------------------------------------------------

    /// Finds the repository containing `path` by walking up to the first
    /// directory that has a `.blobs/` subdirectory (Go: `findStore`).
    pub fn open(path: impl AsRef<Path>) -> Result<Repo> {
        let mut dir = fs::canonicalize(path.as_ref())?;
        loop {
            if dir.join(BLOBS_DIR).is_dir() {
                return Ok(Repo { root: dir });
            }
            if !dir.pop() {
                return Err(Error::NotARepo);
            }
        }
    }

    /// Opens `root` itself without searching parents.
    pub fn open_exact(root: impl Into<PathBuf>) -> Result<Repo> {
        let root = root.into();
        if !root.join(BLOBS_DIR).is_dir() {
            return Err(Error::NotARepo);
        }
        Ok(Repo { root })
    }

    /// Creates `.blobs/` (objects, manifests) in `root` and assigns a client
    /// ID, like `shasync init`.
    pub fn init(root: impl Into<PathBuf>) -> Result<Repo> {
        let root = root.into();
        let bp = root.join(BLOBS_DIR);
        if bp.exists() {
            return Err(Error::AlreadyInitialized);
        }
        for d in [bp.clone(), bp.join(OBJECTS_DIR), bp.join(MANIFESTS_DIR)] {
            fs::create_dir_all(&d)?;
        }
        let repo = Repo { root };
        repo.ensure_client_id()?;
        Ok(repo)
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn blobs_dir(&self) -> PathBuf {
        self.root.join(BLOBS_DIR)
    }

    // ----- config ------------------------------------------------------------

    pub fn read_config(&self) -> Result<Config> {
        match fs::read(self.blobs_dir().join(CONFIG_FILE)) {
            Ok(b) => Ok(serde_json::from_slice(&b)?),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(Config::default()),
            Err(e) => Err(e.into()),
        }
    }

    pub fn write_config(&self, c: &Config) -> Result<()> {
        let mut b = serde_json::to_vec_pretty(c)?;
        b.push(b'\n');
        write_file_atomic(&self.blobs_dir().join(CONFIG_FILE), &b, 0o644)
    }

    /// Returns the client ID, generating and persisting one if missing.
    pub fn ensure_client_id(&self) -> Result<String> {
        let mut c = self.read_config()?;
        if !c.client_id.is_empty() {
            return Ok(c.client_id);
        }
        let mut b = [0u8; 4];
        crate::crypto::random_bytes(&mut b)?;
        c.client_id = hex::encode(b);
        self.write_config(&c)?;
        Ok(c.client_id)
    }

    /// Sets the remote URL (and S3 endpoint / addressing style). Also clears
    /// the push-log cache, which describes the *previous* remote's contents.
    pub fn set_remote(&self, url: &str, s3_endpoint: Option<&str>, virtual_hosted_style: bool) -> Result<()> {
        let mut c = self.read_config()?;
        c.remote = url.to_string();
        c.s3_endpoint = s3_endpoint.unwrap_or_default().to_string();
        c.s3_virtual_hosted_style = virtual_hosted_style;
        self.write_config(&c)?;
        match fs::remove_file(self.push_log_cache_path()) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e.into()),
            _ => Ok(()),
        }
    }

    // ----- key -----------------------------------------------------------------

    pub fn key_path(&self) -> PathBuf {
        self.blobs_dir().join(KEY_FILE)
    }

    /// Reads `.blobs/key` (hex). `Ok(None)` if there is no key file. Unlike
    /// the Go CLI this does not consult `SHASYNC_KEY`; use
    /// [`Key::from_hex`] on it yourself if you want that.
    pub fn load_key(&self) -> Result<Option<Key>> {
        match fs::read_to_string(self.key_path()) {
            Ok(s) => Ok(Some(Key::from_hex(&s)?)),
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    /// Like [`Repo::load_key`] but a missing key is [`Error::NoKey`].
    pub fn require_key(&self) -> Result<Key> {
        self.load_key()?.ok_or(Error::NoKey)
    }

    /// Writes `.blobs/key` as `hex + "\n"`, mode 0600. Refuses to replace an
    /// existing key unless `overwrite` (existing blobs would stop decrypting).
    pub fn save_key(&self, key: &Key, overwrite: bool) -> Result<()> {
        if !overwrite && self.key_path().exists() {
            return Err(Error::Other(format!("{} already exists", self.key_path().display())));
        }
        write_file_atomic(&self.key_path(), format!("{}\n", key.to_hex()).as_bytes(), 0o600)
    }

    // ----- HEAD ----------------------------------------------------------------

    /// Local HEAD, or `None` before the first commit.
    pub fn head(&self) -> Result<Option<String>> {
        match fs::read_to_string(self.blobs_dir().join(HEAD_FILE)) {
            Ok(s) => {
                let s = s.trim();
                Ok(if s.is_empty() { None } else { Some(s.to_string()) })
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    pub fn write_head(&self, sha: &str) -> Result<()> {
        write_file_atomic(&self.blobs_dir().join(HEAD_FILE), format!("{sha}\n").as_bytes(), 0o644)
    }

    pub(crate) fn head_str(&self) -> Result<String> {
        Ok(self.head()?.unwrap_or_default())
    }

    // ----- objects ---------------------------------------------------------------

    pub fn object_path(&self, sha: &str) -> PathBuf {
        self.blobs_dir().join(OBJECTS_DIR).join(&sha[..2]).join(&sha[2..])
    }

    pub fn has_object(&self, sha: &str) -> bool {
        sha.len() > 2 && self.object_path(sha).exists()
    }

    /// Plaintext bytes of a blob in the local store.
    pub fn read_object(&self, sha: &str) -> Result<Vec<u8>> {
        Ok(fs::read(self.object_path(sha))?)
    }

    /// Stores `data` as an object (if missing) and returns its SHA.
    pub fn write_object(&self, data: &[u8]) -> Result<String> {
        let sha = crate::sha256_hex(data);
        self.store_verified(&self.object_path(&sha), data)?;
        Ok(sha)
    }

    pub(crate) fn store_verified(&self, dst: &Path, data: &[u8]) -> Result<()> {
        if dst.exists() {
            return Ok(());
        }
        fs::create_dir_all(dst.parent().unwrap())?;
        write_file_atomic(dst, data, 0o444)
    }

    /// Hashes `src` and copies (reflinks where possible) it into the object
    /// store. Returns (sha, size) of the bytes actually stored: the copy is
    /// re-hashed, so a file modified mid-ingest can never be stored under the
    /// wrong SHA.
    fn ingest_file(&self, src: &Path) -> Result<(String, i64)> {
        let (sha, size) = hash_file(src)?;
        if self.has_object(&sha) {
            return Ok((sha, size));
        }
        let objects = self.blobs_dir().join(OBJECTS_DIR);
        fs::create_dir_all(&objects)?;
        let tmp = objects.join(tmp_name());
        if let Err(e) = reflink_copy::reflink_or_copy(src, &tmp) {
            let _ = fs::remove_file(&tmp);
            return Err(e.into());
        }
        let (sha, size) = match hash_file(&tmp) {
            Ok(v) => v,
            Err(e) => {
                let _ = fs::remove_file(&tmp);
                return Err(e);
            }
        };
        let dst = self.object_path(&sha);
        if dst.exists() {
            let _ = fs::remove_file(&tmp);
            return Ok((sha, size));
        }
        let res = (|| -> Result<()> {
            fs::create_dir_all(dst.parent().unwrap())?;
            fs::set_permissions(&tmp, fs::Permissions::from_mode(0o444))?;
            fs::rename(&tmp, &dst)?;
            Ok(())
        })();
        if res.is_err() {
            let _ = fs::remove_file(&tmp);
        }
        res.map(|_| (sha, size))
    }

    // ----- manifests ---------------------------------------------------------------

    pub fn manifest_path(&self, sha: &str) -> PathBuf {
        self.blobs_dir().join(MANIFESTS_DIR).join(&sha[..2]).join(format!("{}.json", &sha[2..]))
    }

    pub fn has_manifest(&self, sha: &str) -> bool {
        sha.len() > 2 && self.manifest_path(sha).exists()
    }

    pub fn read_manifest(&self, sha: &str) -> Result<Manifest> {
        if sha.len() <= 2 {
            return Err(Error::Manifest { sha: sha.into(), msg: "invalid sha".into() });
        }
        let b = fs::read(self.manifest_path(sha))?;
        Manifest::parse(&b, sha)
    }

    /// Canonicalizes, hashes and stores `m` (if new). Returns its SHA.
    pub fn write_manifest(&self, m: &Manifest) -> Result<String> {
        let (sha, bytes) = m.encode();
        self.store_verified(&self.manifest_path(&sha), &bytes)?;
        Ok(sha)
    }

    // ----- scan ------------------------------------------------------------------

    /// Ignore patterns in effect: defaults + `.blobsignore`.
    pub fn ignore_patterns(&self) -> Result<Vec<String>> {
        Ok(read_ignore_patterns(&self.root)?)
    }

    /// Relative, forward-slash paths of all tracked files in the working
    /// tree, sorted. Skips `.blobs/`, symlinks and ignored paths (Go:
    /// `walkWorkingDir`). Names that aren't valid UTF-8 are skipped.
    pub fn scan(&self) -> Result<Vec<String>> {
        let patterns = self.ignore_patterns()?;
        let mut out = Vec::new();
        self.walk(&self.root, "", &patterns, &mut out)?;
        Ok(out)
    }

    fn walk(&self, dir: &Path, rel_dir: &str, patterns: &[String], out: &mut Vec<String>) -> Result<()> {
        let mut entries: Vec<_> = fs::read_dir(dir)?.collect::<io::Result<_>>()?;
        entries.sort_by_key(|e| e.file_name());
        for e in entries {
            let Some(name) = e.file_name().to_str().map(str::to_owned) else { continue };
            let rel = if rel_dir.is_empty() { name } else { format!("{rel_dir}/{name}") };
            let ft = e.file_type()?;
            if ft.is_dir() {
                if rel == BLOBS_DIR || match_ignore(patterns, &format!("{rel}/")) {
                    continue;
                }
                self.walk(&e.path(), &rel, patterns, out)?;
                continue;
            }
            if ft.is_symlink() || match_ignore(patterns, &rel) {
                continue;
            }
            out.push(rel);
        }
        Ok(())
    }

    fn abs(&self, rel: &str) -> PathBuf {
        self.root.join(rel)
    }

    // ----- commit ------------------------------------------------------------------

    /// Snapshots the working tree (Go: `snapshotWorkingTree`). If the files
    /// (path, sha, size, mode) equal HEAD's, nothing is written. A fresh repo
    /// with an empty tree commits nothing.
    pub fn commit(&self, message: &str) -> Result<CommitResult> {
        self.commit_at(message, now_unix_ms())
    }

    /// [`Repo::commit`] with an explicit `created_at` (unix ms).
    pub fn commit_at(&self, message: &str, created_at: i64) -> Result<CommitResult> {
        let parent = self.head_str()?;
        let parent_files = if parent.is_empty() { BTreeMap::new() } else { self.read_manifest(&parent)?.files };
        let files = self.build_files(&parent_files)?;
        if parent.is_empty() && files.is_empty() {
            return Ok(CommitResult { head: None, committed: false });
        }
        if !parent.is_empty() && files_equal(&files, &parent_files) {
            return Ok(CommitResult { head: Some(parent), committed: false });
        }
        let m = Manifest::new(parent, created_at, message, files);
        let sha = self.write_manifest(&m)?;
        self.write_head(&sha)?;
        Ok(CommitResult { head: Some(sha), committed: true })
    }

    /// The commit that push/pull/sync make implicitly: message
    /// `"sync from <client_id>"` (Go: `ensureTreeCommitted`).
    pub fn commit_if_dirty(&self, client_id: &str) -> Result<CommitResult> {
        let msg = if client_id.is_empty() { "sync".to_string() } else { format!("sync from {client_id}") };
        self.commit(&msg)
    }

    /// Builds the `files` map for the current tree. Files whose size and
    /// mtime match the parent entry (and whose blob is present) reuse the
    /// parent entry verbatim, including its mode, exactly as Go does.
    fn build_files(&self, parent_files: &BTreeMap<String, ManifestFile>) -> Result<BTreeMap<String, ManifestFile>> {
        let paths = self.scan()?;
        let next = AtomicUsize::new(0);
        let files = Mutex::new(BTreeMap::new());
        let first_err: Mutex<Option<Error>> = Mutex::new(None);
        let workers = 8.min(paths.len().max(1));
        std::thread::scope(|sc| {
            for _ in 0..workers {
                sc.spawn(|| loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= paths.len() || first_err.lock().unwrap().is_some() {
                        return;
                    }
                    let rel = &paths[i];
                    match self.file_entry(rel, parent_files.get(rel)) {
                        Ok(Some(f)) => {
                            files.lock().unwrap().insert(rel.clone(), f);
                        }
                        Ok(None) => {}
                        Err(e) => {
                            first_err.lock().unwrap().get_or_insert(e);
                            return;
                        }
                    }
                });
            }
        });
        if let Some(e) = first_err.into_inner().unwrap() {
            return Err(e);
        }
        Ok(files.into_inner().unwrap())
    }

    /// `Ok(None)` if the file vanished between scan and stat.
    fn file_entry(&self, rel: &str, prev: Option<&ManifestFile>) -> Result<Option<ManifestFile>> {
        let abs = self.abs(rel);
        let st = match fs::metadata(&abs) {
            Ok(st) => st,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        let updated_at = mtime_ms(&st);
        let mode = st.permissions().mode() & 0o777;
        if let Some(prev) = prev {
            if prev.size == st.len() as i64 && prev.updated_at == updated_at && self.has_object(&prev.sha) {
                return Ok(Some(prev.clone()));
            }
        }
        let (sha, size) = match self.ingest_file(&abs) {
            Ok(v) => v,
            Err(Error::Io(e)) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e),
        };
        Ok(Some(ManifestFile { sha, size, updated_at, mode }))
    }

    // ----- status --------------------------------------------------------------------

    /// Working tree vs local HEAD (Go: `shasync status`). Size+mtime is the
    /// fast check; differing files are hashed before being called modified.
    pub fn status(&self) -> Result<Status> {
        let head = self.head()?;
        let files = match &head {
            Some(h) => self.read_manifest(h)?.files,
            None => BTreeMap::new(),
        };
        let mut st = self.diff_working_tree(&files)?;
        st.head = head;
        Ok(st)
    }

    /// Working tree vs an arbitrary file map (Go: `diffWorkingTree`).
    pub fn diff_working_tree(&self, base: &BTreeMap<String, ManifestFile>) -> Result<Status> {
        let paths = self.scan()?;
        let mut st = Status::default();
        let seen: HashSet<&str> = paths.iter().map(String::as_str).collect();
        for rel in &paths {
            let Some(prev) = base.get(rel) else {
                st.added.push(rel.clone());
                continue;
            };
            let md = fs::metadata(self.abs(rel))?;
            if prev.size != md.len() as i64 || prev.updated_at != mtime_ms(&md) {
                let (sha, _) = hash_file(&self.abs(rel))?;
                if sha != prev.sha {
                    st.modified.push(rel.clone());
                }
            }
        }
        for p in base.keys() {
            if !seen.contains(p.as_str()) {
                st.removed.push(p.clone());
            }
        }
        Ok(st)
    }

    // ----- log -------------------------------------------------------------------------

    /// Walks the parent chain from `start` (default: HEAD), up to `limit`
    /// entries, stopping at the first manifest not present locally.
    pub fn log(&self, start: Option<&str>, limit: usize) -> Result<Vec<LogEntry>> {
        let mut out = Vec::new();
        let mut cur = match start {
            Some(s) => s.to_string(),
            None => self.head_str()?,
        };
        while !cur.is_empty() && out.len() < limit && self.has_manifest(&cur) {
            let m = self.read_manifest(&cur)?;
            let parent_files = if !m.parent_sha.is_empty() && self.has_manifest(&m.parent_sha) {
                self.read_manifest(&m.parent_sha)?.files
            } else {
                BTreeMap::new()
            };
            let (added, modified, removed) = diff_files(&parent_files, &m.files);
            let next = m.parent_sha.clone();
            out.push(LogEntry { sha: cur, manifest: m, added, modified, removed });
            cur = next;
        }
        Ok(out)
    }

    // ----- checkout / safe apply ----------------------------------------------------------

    /// Materializes `target` into the working tree and sets HEAD (Go:
    /// `checkoutTarget`). Unless `force`, refuses when the tree has
    /// uncommitted changes. Deletes tracked-but-unwanted files.
    pub fn checkout(&self, target: &str, force: bool) -> Result<()> {
        if !self.has_manifest(target) {
            return Err(Error::Other(format!("manifest {target} not found locally")));
        }
        let tm = self.read_manifest(target)?;
        if !force && !self.status()?.is_clean() {
            return Err(Error::UncommittedChanges);
        }
        if let Some(missing) = tm.files.values().find(|f| !self.has_object(&f.sha)) {
            return Err(Error::Other(format!("blob {} missing locally", missing.sha)));
        }
        for p in self.scan()? {
            if !tm.files.contains_key(&p) {
                let abs = self.abs(&p);
                let _ = fs::set_permissions(&abs, fs::Permissions::from_mode(0o644));
                remove_if_exists(&abs)?;
            }
        }
        for (p, f) in &tm.files {
            self.materialize(p, f)?;
        }
        prune_empty_dirs(&self.root);
        self.write_head(target)
    }

    fn materialize(&self, rel: &str, f: &ManifestFile) -> Result<()> {
        let abs = self.abs(rel);
        reflink_checkout(&self.object_path(&f.sha), &abs)?;
        let mode = if f.mode != 0 { f.mode } else { 0o644 };
        let _ = fs::set_permissions(&abs, fs::Permissions::from_mode(mode));
        if f.updated_at > 0 {
            let t = UNIX_EPOCH + Duration::from_millis(f.updated_at as u64);
            if let Ok(file) = File::open(&abs) {
                let _ = file.set_times(fs::FileTimes::new().set_accessed(t).set_modified(t));
            }
        }
        Ok(())
    }

    /// Moves the working tree from manifest `from` (what the tree held when
    /// the sync snapshotted it; `None`/empty = nothing) to `target`, touching
    /// only paths that differ between the two, and only if each path still
    /// holds exactly its `from` content (or is still absent). Files not in
    /// `from` are never deleted. If a path was edited, stops with
    /// [`Error::LocalEditsDuringSync`] and leaves HEAD alone (Go:
    /// `applyManifestSafely`).
    pub fn apply_manifest_safely(&self, from: Option<&str>, target: &str) -> Result<()> {
        let from_files = match from {
            Some(f) if !f.is_empty() => self.read_manifest(f)?.files,
            _ => BTreeMap::new(),
        };
        let tm = self.read_manifest(target)?;
        let paths: BTreeSet<&String> = from_files.keys().chain(tm.files.keys()).collect();
        for p in paths {
            let was = from_files.get(p);
            let want = tm.files.get(p);
            if same_file(was, want) {
                continue;
            }
            if let Some(w) = want {
                if !self.has_object(&w.sha) {
                    return Err(Error::Other(format!("blob {} missing locally for {p}", short_sha(&w.sha))));
                }
            }
            if !self.disk_matches(p, was)? {
                return Err(Error::LocalEditsDuringSync);
            }
            match want {
                None => {
                    let abs = self.abs(p);
                    let _ = fs::set_permissions(&abs, fs::Permissions::from_mode(0o644));
                    remove_if_exists(&abs)?;
                }
                Some(w) => self.materialize(p, w)?,
            }
        }
        prune_empty_dirs(&self.root);
        self.write_head(target)
    }

    /// Whether the working-tree file at `rel` still holds `f`'s content
    /// (`None` = should not exist). Same size and mtime is trusted; otherwise
    /// the file is hashed (Go: `diskMatches`).
    pub fn disk_matches(&self, rel: &str, f: Option<&ManifestFile>) -> Result<bool> {
        let abs = self.abs(rel);
        let st = match fs::symlink_metadata(&abs) {
            Ok(st) => st,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(f.is_none()),
            Err(e) => return Err(e.into()),
        };
        let Some(f) = f else { return Ok(false) };
        if st.is_dir() || st.len() as i64 != f.size {
            return Ok(false);
        }
        if mtime_ms(&st) == f.updated_at {
            return Ok(true);
        }
        Ok(hash_file(&abs)?.0 == f.sha)
    }

    // ----- ancestry ------------------------------------------------------------------------

    /// Whether `ancestor` is in `descendant`'s parent chain (inclusive),
    /// walking only local manifests; an incomplete chain answers `false`.
    pub fn is_ancestor(&self, ancestor: &str, descendant: &str) -> Result<bool> {
        if ancestor.is_empty() || descendant.is_empty() {
            return Ok(false);
        }
        let mut cur = descendant.to_string();
        while !cur.is_empty() {
            if cur == ancestor {
                return Ok(true);
            }
            if !self.has_manifest(&cur) {
                return Ok(false);
            }
            cur = self.read_manifest(&cur)?.parent_sha;
        }
        Ok(false)
    }

    fn ancestors(&self, sha: &str) -> Result<HashSet<String>> {
        let mut out = HashSet::new();
        let mut cur = sha.to_string();
        while !cur.is_empty() {
            out.insert(cur.clone());
            if !self.has_manifest(&cur) {
                break;
            }
            cur = self.read_manifest(&cur)?.parent_sha;
        }
        Ok(out)
    }

    /// Most recent shared ancestor of `a` and `b` (Go: `findCommonAncestor`).
    pub fn find_common_ancestor(&self, a: &str, b: &str) -> Result<Option<String>> {
        let anc_a = self.ancestors(a)?;
        let mut cur = b.to_string();
        while !cur.is_empty() {
            if anc_a.contains(&cur) {
                return Ok(Some(cur));
            }
            if !self.has_manifest(&cur) {
                return Ok(None);
            }
            cur = self.read_manifest(&cur)?.parent_sha;
        }
        Ok(None)
    }

    // ----- push-log cache -------------------------------------------------------------------

    /// `.blobs/clone-target`: the snapshot a first sync is downloading, so
    /// an interrupted one resumes with the same snapshot.
    pub(crate) fn clone_target_path(&self) -> PathBuf {
        self.blobs_dir().join(CLONE_TARGET_FILE)
    }

    /// During a first sync: writes `f` to `rel` if nothing is there yet, so
    /// files appear as they download. Anything already there is left for
    /// [`Repo::finish_clone`].
    pub(crate) fn place_if_absent(&self, rel: &str, f: &ManifestFile) -> Result<()> {
        match fs::symlink_metadata(self.abs(rel)) {
            Err(e) if e.kind() == io::ErrorKind::NotFound => self.materialize(rel, f),
            _ => Ok(()),
        }
    }

    /// Ends a first sync: every path of `target` gets target's content. A
    /// different file already at a path (a note made before syncing, or
    /// edited while files were arriving) is kept as a conflict copy next to
    /// it, as a merge would. Files `target` doesn't have are left alone (the
    /// next snapshot adds them). Then HEAD = `target`. Returns the copies.
    pub(crate) fn finish_clone(&self, target: &str, client_id: &str, now_ms: i64) -> Result<Vec<String>> {
        let tm = self.read_manifest(target)?;
        let stamp = crate::merge::conflict_stamp(now_ms)?;
        let mut copies = Vec::new();
        for (p, f) in &tm.files {
            if self.disk_matches(p, Some(f))? {
                continue;
            }
            if !self.has_object(&f.sha) {
                return Err(Error::Other(format!("blob {} missing locally for {p}", short_sha(&f.sha))));
            }
            let abs = self.abs(p);
            if let Ok(st) = fs::symlink_metadata(&abs) {
                if st.is_dir() {
                    return Err(Error::Other(format!("{p} is a folder here but a file in the snapshot being downloaded")));
                }
                let mut copy = crate::merge::conflict_copy_path(p, client_id, &stamp);
                let mut n = 2;
                while self.abs(&copy).exists() {
                    copy = crate::merge::conflict_copy_path(p, client_id, &format!("{stamp} {n}"));
                    n += 1;
                }
                fs::rename(&abs, self.abs(&copy))?;
                copies.push(copy);
            }
            self.materialize(p, f)?;
        }
        self.write_head(target)?;
        let _ = fs::remove_file(self.clone_target_path());
        Ok(copies)
    }

    pub(crate) fn push_log_cache_path(&self) -> PathBuf {
        self.blobs_dir().join(PUSH_LOG_CACHE_FILE)
    }
}

// ----- helpers ---------------------------------------------------------------------------------

/// Go's `sameFile`: both absent, or both present with the same SHA.
pub(crate) fn same_file(a: Option<&ManifestFile>, b: Option<&ManifestFile>) -> bool {
    match (a, b) {
        (None, None) => true,
        (Some(a), Some(b)) => a.sha == b.sha,
        _ => false,
    }
}

/// Go's `manifestFilesEqual`: same paths with the same sha, size and mode.
fn files_equal(a: &BTreeMap<String, ManifestFile>, b: &BTreeMap<String, ManifestFile>) -> bool {
    a.len() == b.len()
        && a.iter().all(|(k, av)| b.get(k).is_some_and(|bv| av.sha == bv.sha && av.size == bv.size && av.mode == bv.mode))
}

pub fn short_sha(s: &str) -> &str {
    if s.len() >= 12 {
        &s[..12]
    } else {
        s
    }
}

pub fn now_unix_ms() -> i64 {
    system_time_ms(SystemTime::now())
}

fn system_time_ms(t: SystemTime) -> i64 {
    let ns: i128 = match t.duration_since(UNIX_EPOCH) {
        Ok(d) => d.as_nanos() as i128,
        Err(e) => -(e.duration().as_nanos() as i128),
    };
    ns.div_euclid(1_000_000) as i64
}

/// Modification time in unix ms, floored like Go's `Time.UnixMilli`.
pub(crate) fn mtime_ms(md: &fs::Metadata) -> i64 {
    md.modified().map(system_time_ms).unwrap_or(0)
}

/// Streaming SHA-256 of a file: (hex sha, size).
pub fn hash_file(path: &Path) -> Result<(String, i64)> {
    let mut f = File::open(path)?;
    let mut h = Sha256::new();
    let mut buf = vec![0u8; 256 * 1024];
    let mut n: i64 = 0;
    loop {
        let k = f.read(&mut buf)?;
        if k == 0 {
            break;
        }
        h.update(&buf[..k]);
        n += k as i64;
    }
    Ok((hex::encode(h.finalize()), n))
}

fn tmp_name() -> String {
    let mut b = [0u8; 8];
    let _ = crate::crypto::random_bytes(&mut b);
    format!(".tmp-{}", hex::encode(b))
}

/// Write to a temp file in the same directory, chmod, then rename.
pub(crate) fn write_file_atomic(path: &Path, data: &[u8], mode: u32) -> Result<()> {
    let dir = path.parent().ok_or_else(|| Error::Other("path has no parent".into()))?;
    let tmp = dir.join(tmp_name());
    let res = (|| -> io::Result<()> {
        fs::write(&tmp, data)?;
        fs::set_permissions(&tmp, fs::Permissions::from_mode(mode))?;
        fs::rename(&tmp, path)
    })();
    if res.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    Ok(res?)
}

fn remove_if_exists(p: &Path) -> Result<()> {
    match fs::remove_file(p) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e.into()),
        _ => Ok(()),
    }
}

/// Go's `reflinkCheckout`: replace whatever is at `work` with a clone (or
/// copy) of the object.
fn reflink_checkout(object: &Path, work: &Path) -> Result<()> {
    if let Some(parent) = work.parent() {
        fs::create_dir_all(parent)?;
    }
    if let Ok(st) = fs::symlink_metadata(work) {
        let _ = fs::set_permissions(work, fs::Permissions::from_mode(0o644));
        if st.is_dir() {
            fs::remove_dir_all(work)?;
        } else {
            fs::remove_file(work)?;
        }
    }
    reflink_copy::reflink_or_copy(object, work)?;
    Ok(())
}

/// Removes empty directories under `root` (bottom-up), never `root` itself
/// or anything under `.blobs/` (Go: `pruneEmptyDirs`).
fn prune_empty_dirs(root: &Path) {
    fn visit(dir: &Path) -> bool {
        let Ok(rd) = fs::read_dir(dir) else { return false };
        let mut empty = true;
        for e in rd.flatten() {
            let is_dir = e.file_type().map(|t| t.is_dir()).unwrap_or(false);
            if is_dir && visit(&e.path()) && fs::remove_dir(e.path()).is_ok() {
                continue;
            }
            empty = false;
        }
        empty
    }
    let Ok(rd) = fs::read_dir(root) else { return };
    for e in rd.flatten() {
        if e.file_name() == BLOBS_DIR || !e.file_type().map(|t| t.is_dir()).unwrap_or(false) {
            continue;
        }
        if visit(&e.path()) {
            let _ = fs::remove_dir(e.path());
        }
    }
}
