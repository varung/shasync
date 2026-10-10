//! Three-way auto-merge of diverged chains (Go: `autoMerge`, `conflictCopyPath`).

use std::collections::{BTreeMap, BTreeSet};

use chrono::{Local, TimeZone};

use crate::error::{Error, Result};
use crate::manifest::{Manifest, ManifestFile};
use crate::repo::{same_file, short_sha, Repo};

/// Builds the merge of `local_sha` into `remote_sha` relative to `anc_sha`.
/// The parent is the remote tip. Per path:
/// - same on both sides → remote's entry;
/// - local unchanged since the ancestor → remote's;
/// - remote unchanged → local's;
/// - otherwise a conflict: remote keeps the path (or stays deleted) and the
///   local version is added as `"<stem> (modified on <stamp> by <client>)<ext>"`.
///
/// `now_ms` is unix milliseconds; the stamp uses the local time zone, as Go.
/// Returns the manifest and the sorted conflicted paths.
pub fn auto_merge(
    repo: &Repo,
    client_id: &str,
    now_ms: i64,
    anc_sha: &str,
    remote_sha: &str,
    local_sha: &str,
) -> Result<(Manifest, Vec<String>)> {
    let anc = repo.read_manifest(anc_sha)?;
    let rem = repo.read_manifest(remote_sha)?;
    let loc = repo.read_manifest(local_sha)?;
    let stamp = Local
        .timestamp_millis_opt(now_ms)
        .single()
        .ok_or_else(|| Error::Other("bad merge time".into()))?
        .format("%Y-%m-%d %H%M%S")
        .to_string();

    let paths: BTreeSet<&String> = anc.files.keys().chain(rem.files.keys()).chain(loc.files.keys()).collect();
    let mut out: BTreeMap<String, ManifestFile> = BTreeMap::new();
    let mut conflicts = Vec::new();
    for p in paths {
        let (a, r, l) = (anc.files.get(p), rem.files.get(p), loc.files.get(p));
        if same_file(r, l) || same_file(a, l) {
            if let Some(r) = r {
                out.insert(p.clone(), r.clone());
            }
        } else if same_file(a, r) {
            if let Some(l) = l {
                out.insert(p.clone(), l.clone());
            }
        } else {
            conflicts.push(p.clone());
            if let Some(r) = r {
                out.insert(p.clone(), r.clone());
            }
            if let Some(l) = l {
                out.insert(conflict_copy_path(p, client_id, &stamp), l.clone());
            }
        }
    }
    conflicts.sort();

    let mut msg = format!("auto-merge {} into {}", short_sha(local_sha), short_sha(remote_sha));
    if !conflicts.is_empty() {
        let suf = if conflicts.len() == 1 { "" } else { "s" };
        msg = format!("{msg} ({} conflict{suf})", conflicts.len());
    }
    Ok((Manifest::new(remote_sha, now_ms, msg, out), conflicts))
}

/// `"notes/todo.md"` → `"notes/todo (modified on <stamp> by <client>).md"`.
pub fn conflict_copy_path(p: &str, client_id: &str, stamp: &str) -> String {
    let (dir, base) = match p.rfind('/') {
        Some(i) => (&p[..=i], &p[i + 1..]),
        None => ("", p),
    };
    let (stem, ext) = match base.rfind('.') {
        Some(dot) if dot > 0 => (&base[..dot], &base[dot..]),
        _ => (base, ""),
    };
    format!("{dir}{stem} (modified on {stamp} by {client_id}){ext}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn conflict_copy_names() {
        let s = "2026-04-20 101530";
        assert_eq!(conflict_copy_path("notes/todo.md", "a1b2c3d4", s), "notes/todo (modified on 2026-04-20 101530 by a1b2c3d4).md");
        assert_eq!(conflict_copy_path("README", "x", s), "README (modified on 2026-04-20 101530 by x)");
        assert_eq!(conflict_copy_path(".bashrc", "x", s), ".bashrc (modified on 2026-04-20 101530 by x)");
        assert_eq!(conflict_copy_path("a/b.tar.gz", "x", s), "a/b.tar (modified on 2026-04-20 101530 by x).gz");
    }
}
