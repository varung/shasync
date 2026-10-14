//! Keyframe + delta manifests on the remote.
//!
//! Locally every manifest is stored full. On the remote, `manifests/<sha>`
//! is either the full manifest (a *keyframe*) or a *delta*: the changes from
//! one keyframe, never from another delta, so reading one costs at most one
//! extra download. A delta is used while it stays small next to a full
//! listing (see [`choose`]); then the next snapshot goes up full and becomes
//! the new keyframe, like keyframes in video.
//!
//! A delta's plaintext is compact JSON starting with `{"delta":1,` and
//! holding `base` (a keyframe's SHA), the snapshot's `parent_sha`,
//! `created_at` and `message`, `set` (path -> file, for paths new or changed
//! since the base) and `del` (paths gone since the base). Rebuilding gives
//! the canonical manifest bytes, whose SHA must be the one asked for, so a
//! bad delta is an error, never a wrong snapshot. The Go CLI does the same.

use std::collections::{BTreeMap, HashSet};
use std::fs;
use std::io::Write;

use serde::Deserialize;
use serde_json::{json, Value};

use crate::crypto::compress;
use crate::error::{Error, Result};
use crate::manifest::{Manifest, ManifestFile, MANIFEST_SCHEMA_VERSION};
use crate::repo::Repo;
use crate::sha256_hex;

/// Manifests known to be stored full on the remote, one SHA per line.
pub const KEYFRAMES_FILE: &str = "keyframes";

const DELTA_PREFIX: &[u8] = b"{\"delta\":";
/// Below this (compressed) a full listing is cheap enough to always send.
const MIN_FULL: usize = 16 * 1024;
/// A delta goes up while it's under a tenth of the full listing (compressed),
/// and never over this.
const MAX_DELTA: usize = 64 * 1024;
/// How far back to look for a keyframe.
const MAX_WALK: usize = 1000;

pub fn is_delta(plaintext: &[u8]) -> bool {
    plaintext.starts_with(DELTA_PREFIX)
}

#[derive(Deserialize)]
struct RawDelta {
    delta: i64,
    base: String,
    #[serde(default)]
    parent_sha: String,
    #[serde(default)]
    created_at: i64,
    #[serde(default)]
    message: String,
    #[serde(default)]
    set: BTreeMap<String, RawFile>,
    #[serde(default)]
    del: Vec<String>,
}

#[derive(Deserialize)]
struct RawFile {
    sha: String,
    #[serde(default)]
    size: i64,
    #[serde(default)]
    updated_at: i64,
    #[serde(default)]
    mode: u32,
}

/// A parsed delta: rebuild it with [`Delta::apply`] once its base is local.
pub struct Delta(RawDelta);

impl Delta {
    pub fn parse(plaintext: &[u8], sha: &str) -> Result<Delta> {
        let raw: RawDelta = serde_json::from_slice(plaintext)
            .map_err(|e| Error::Manifest { sha: sha.to_string(), msg: format!("delta: {e}") })?;
        if raw.delta != 1 {
            return Err(Error::Manifest { sha: sha.to_string(), msg: format!("unsupported delta version {}", raw.delta) });
        }
        Ok(Delta(raw))
    }

    pub fn base(&self) -> &str {
        &self.0.base
    }

    /// The full manifest's canonical bytes, checked against `sha`.
    pub fn apply(&self, base: &Manifest, sha: &str) -> Result<Vec<u8>> {
        let mut files = base.files.clone();
        for p in &self.0.del {
            files.remove(p);
        }
        for (p, f) in &self.0.set {
            files.insert(p.clone(), ManifestFile { sha: f.sha.clone(), size: f.size, updated_at: f.updated_at, mode: f.mode });
        }
        let m = Manifest {
            version: MANIFEST_SCHEMA_VERSION,
            parent_sha: self.0.parent_sha.clone(),
            created_at: self.0.created_at,
            message: self.0.message.clone(),
            files,
        };
        let bytes = m.to_canonical_bytes();
        let got = sha256_hex(&bytes);
        if got != sha {
            return Err(Error::ShaMismatch { got, want: sha.to_string() });
        }
        Ok(bytes)
    }
}

/// `m` as changes from keyframe `base` (sha `base_sha`).
pub fn make(base_sha: &str, base: &Manifest, m: &Manifest) -> Vec<u8> {
    let mut set = serde_json::Map::new();
    for (p, f) in &m.files {
        if base.files.get(p) != Some(f) {
            let mut e = serde_json::Map::new();
            e.insert("sha".into(), json!(f.sha));
            e.insert("size".into(), json!(f.size));
            if f.updated_at != 0 { e.insert("updated_at".into(), json!(f.updated_at)); }
            if f.mode != 0 { e.insert("mode".into(), json!(f.mode)); }
            set.insert(p.clone(), Value::Object(e));
        }
    }
    let del: Vec<&String> = base.files.keys().filter(|p| !m.files.contains_key(*p)).collect();
    // Written by hand so it starts with {"delta":1, (serde_json sorts keys).
    let s = |v: &str| serde_json::to_string(v).unwrap_or_default();
    format!(
        "{{\"delta\":1,\"base\":{},\"parent_sha\":{},\"created_at\":{},\"message\":{},\"set\":{},\"del\":{}}}",
        s(base_sha), s(&m.parent_sha), m.created_at, s(&m.message),
        Value::Object(set), serde_json::to_string(&del).unwrap_or_else(|_| "[]".into()),
    )
    .into_bytes()
}

impl Repo {
    fn keyframes_path(&self) -> std::path::PathBuf {
        self.blobs_dir().join(KEYFRAMES_FILE)
    }

    /// Manifests known to be full on the remote.
    pub fn keyframes(&self) -> HashSet<String> {
        fs::read_to_string(self.keyframes_path())
            .map(|t| t.lines().map(str::trim).filter(|l| l.len() == 64).map(String::from).collect())
            .unwrap_or_default()
    }

    /// Remembers that `sha` is stored full on the remote.
    pub fn add_keyframe(&self, sha: &str) -> Result<()> {
        if self.keyframes().contains(sha) {
            return Ok(());
        }
        let mut f = fs::OpenOptions::new().create(true).append(true).open(self.keyframes_path())?;
        writeln!(f, "{sha}")?;
        Ok(())
    }
}

/// What to upload for local manifest `sha`: its plaintext and whether
/// that's the full manifest (a new keyframe) or a delta.
pub fn choose(repo: &Repo, sha: &str) -> Result<(Vec<u8>, bool)> {
    let full = std::fs::read(repo.manifest_path(sha))?;
    let m = Manifest::parse(&full, sha)?;
    let keyframes = repo.keyframes();
    let mut base = None;
    let mut cur = m.parent_sha.clone();
    for _ in 0..MAX_WALK {
        if cur.is_empty() || !repo.has_manifest(&cur) { break; }
        if keyframes.contains(&cur) { base = Some(cur); break; }
        cur = repo.read_manifest(&cur)?.parent_sha;
    }
    let Some(base_sha) = base else { return Ok((full, true)) };
    let full_c = compress(&full)?.len();
    if full_c < MIN_FULL { return Ok((full, true)); }
    let base_m = repo.read_manifest(&base_sha)?;
    let delta = make(&base_sha, &base_m, &m);
    if compress(&delta)?.len() >= (full_c / 10).min(MAX_DELTA) { return Ok((full, true)); }
    // Never upload a delta that doesn't rebuild this exact snapshot.
    match Delta::parse(&delta, sha).and_then(|d| d.apply(&base_m, sha)) {
        Ok(_) => Ok((delta, false)),
        Err(_) => Ok((full, true)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn big(n: usize) -> BTreeMap<String, ManifestFile> {
        (0..n).map(|i| (format!("notes/folder {}/note {i}.md", i % 37),
            ManifestFile { sha: sha256_hex(format!("{i}").as_bytes()), size: i as i64 * 7, updated_at: 1_700_000_000_000 + i as i64, mode: 420 })).collect()
    }

    #[test]
    fn delta_rebuilds_the_exact_manifest() {
        let base = Manifest::new("", 1, "base", big(50));
        let (base_sha, _) = base.encode();
        let mut files = base.files.clone();
        files.remove("notes/folder 1/note 1.md");
        files.insert("new \"quoted\" ünïcode.md".into(), ManifestFile { sha: "ab".repeat(32), size: 3, updated_at: 0, mode: 0 });
        files.get_mut("notes/folder 2/note 2.md").unwrap().updated_at = 5;
        let m = Manifest::new(base_sha.clone(), 2, "sync from x", files);
        let (sha, _) = m.encode();
        let d = make(&base_sha, &base, &m);
        assert!(is_delta(&d));
        let back = Delta::parse(&d, &sha).unwrap();
        assert_eq!(back.base(), base_sha);
        assert_eq!(back.apply(&base, &sha).unwrap(), m.to_canonical_bytes());
        // A delta against the wrong base fails its check.
        let other = Manifest::new("", 9, "other", big(3));
        assert!(back.apply(&other, &sha).is_err());
    }
}
