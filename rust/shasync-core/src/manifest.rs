//! Manifest (snapshot) schema v1 and its canonical encoding.
//!
//! The canonical bytes are exactly what Go's `json.MarshalIndent(m, "", "  ")`
//! produces for the Go `Manifest` struct, plus one trailing `'\n'`. The
//! manifest's identity is `sha256(canonical bytes)`.

use std::collections::BTreeMap;

use serde::Deserialize;

use crate::error::{Error, Result};
use crate::gojson::{newline_indent, write_string};
use crate::sha256_hex;

/// The only manifest schema version this crate reads or writes.
pub const MANIFEST_SCHEMA_VERSION: i64 = 1;

/// One file entry: blob SHA plus a little POSIX metadata.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ManifestFile {
    /// SHA-256 (lowercase hex) of the file's plaintext bytes.
    pub sha: String,
    pub size: i64,
    /// Modification time in unix milliseconds; 0 = unknown (omitted in JSON).
    pub updated_at: i64,
    /// Unix permission bits (e.g. 0o644 = 420); 0 = default (omitted in JSON).
    pub mode: u32,
}

/// A snapshot: path (forward slashes, relative to the repo root) → file.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Manifest {
    pub version: i64,
    /// Parent manifest SHA, or empty for a root commit.
    pub parent_sha: String,
    /// Unix milliseconds.
    pub created_at: i64,
    pub message: String,
    /// Sorted byte-lexicographically, as Go sorts map keys.
    pub files: BTreeMap<String, ManifestFile>,
}

impl Manifest {
    pub fn new(parent_sha: impl Into<String>, created_at: i64, message: impl Into<String>, files: BTreeMap<String, ManifestFile>) -> Self {
        Manifest { version: MANIFEST_SCHEMA_VERSION, parent_sha: parent_sha.into(), created_at, message: message.into(), files }
    }

    /// Canonical bytes, identical to the Go implementation's.
    pub fn to_canonical_bytes(&self) -> Vec<u8> {
        let mut out = Vec::with_capacity(128 + self.files.len() * 160);
        out.push(b'{');
        newline_indent(&mut out, 1);
        out.extend_from_slice(b"\"version\": ");
        out.extend_from_slice(self.version.to_string().as_bytes());
        if !self.parent_sha.is_empty() {
            out.push(b',');
            newline_indent(&mut out, 1);
            out.extend_from_slice(b"\"parent_sha\": ");
            write_string(&mut out, &self.parent_sha);
        }
        out.push(b',');
        newline_indent(&mut out, 1);
        out.extend_from_slice(b"\"created_at\": ");
        out.extend_from_slice(self.created_at.to_string().as_bytes());
        if !self.message.is_empty() {
            out.push(b',');
            newline_indent(&mut out, 1);
            out.extend_from_slice(b"\"message\": ");
            write_string(&mut out, &self.message);
        }
        if !self.files.is_empty() {
            out.push(b',');
            newline_indent(&mut out, 1);
            out.extend_from_slice(b"\"files\": {");
            let mut first = true;
            for (path, f) in &self.files {
                if !first {
                    out.push(b',');
                }
                first = false;
                newline_indent(&mut out, 2);
                write_string(&mut out, path);
                out.extend_from_slice(b": {");
                newline_indent(&mut out, 3);
                out.extend_from_slice(b"\"sha\": ");
                write_string(&mut out, &f.sha);
                out.push(b',');
                newline_indent(&mut out, 3);
                out.extend_from_slice(b"\"size\": ");
                out.extend_from_slice(f.size.to_string().as_bytes());
                if f.updated_at != 0 {
                    out.push(b',');
                    newline_indent(&mut out, 3);
                    out.extend_from_slice(b"\"updated_at\": ");
                    out.extend_from_slice(f.updated_at.to_string().as_bytes());
                }
                if f.mode != 0 {
                    out.push(b',');
                    newline_indent(&mut out, 3);
                    out.extend_from_slice(b"\"mode\": ");
                    out.extend_from_slice(f.mode.to_string().as_bytes());
                }
                newline_indent(&mut out, 2);
                out.push(b'}');
            }
            newline_indent(&mut out, 1);
            out.push(b'}');
        }
        newline_indent(&mut out, 0);
        out.push(b'}');
        out.push(b'\n');
        out
    }

    /// Canonical bytes and their SHA-256 (the manifest's identity).
    pub fn encode(&self) -> (String, Vec<u8>) {
        let b = self.to_canonical_bytes();
        (sha256_hex(&b), b)
    }

    /// Parses manifest bytes with the same strictness as Go's
    /// `parseManifest`: unknown fields are rejected, the version must be 1,
    /// and the reserved `chunks` field must be empty. `sha` is only used in
    /// error messages.
    pub fn parse(bytes: &[u8], sha: &str) -> Result<Manifest> {
        let raw: RawManifest = serde_json::from_slice(bytes)
            .map_err(|e| Error::Manifest { sha: sha.to_string(), msg: format!("parse: {e}") })?;
        let version = raw.version.unwrap_or(0);
        if version != MANIFEST_SCHEMA_VERSION {
            return Err(Error::Manifest {
                sha: sha.to_string(),
                msg: format!("unsupported schema version {version} (this build supports {MANIFEST_SCHEMA_VERSION})"),
            });
        }
        if raw.chunks.as_ref().is_some_and(|c| !c.is_empty()) {
            return Err(Error::Manifest {
                sha: sha.to_string(),
                msg: "chunks field populated but schema is v1 (reserved for future use)".into(),
            });
        }
        let mut files = BTreeMap::new();
        for (p, f) in raw.files.unwrap_or_default() {
            let f = f.ok_or_else(|| Error::Manifest { sha: sha.to_string(), msg: format!("null entry for {p}") })?;
            files.insert(
                p,
                ManifestFile {
                    sha: f.sha.unwrap_or_default(),
                    size: f.size.unwrap_or(0),
                    updated_at: f.updated_at.unwrap_or(0),
                    mode: f.mode.unwrap_or(0),
                },
            );
        }
        Ok(Manifest {
            version,
            parent_sha: raw.parent_sha.unwrap_or_default(),
            created_at: raw.created_at.unwrap_or(0),
            message: raw.message.unwrap_or_default(),
            files,
        })
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawManifest {
    version: Option<i64>,
    parent_sha: Option<String>,
    created_at: Option<i64>,
    message: Option<String>,
    files: Option<BTreeMap<String, Option<RawFile>>>,
    chunks: Option<Vec<String>>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawFile {
    sha: Option<String>,
    size: Option<i64>,
    updated_at: Option<i64>,
    mode: Option<u32>,
}

/// Paths added / modified / removed going from `parent` to `cur` (each sorted).
/// Mirrors Go's `diffManifestFiles`.
pub fn diff_files(
    parent: &BTreeMap<String, ManifestFile>,
    cur: &BTreeMap<String, ManifestFile>,
) -> (Vec<String>, Vec<String>, Vec<String>) {
    let mut added = Vec::new();
    let mut modified = Vec::new();
    let mut removed = Vec::new();
    for (p, f) in cur {
        match parent.get(p) {
            None => added.push(p.clone()),
            Some(pf) if pf.sha != f.sha => modified.push(p.clone()),
            _ => {}
        }
    }
    for p in parent.keys() {
        if !cur.contains_key(p) {
            removed.push(p.clone());
        }
    }
    (added, modified, removed)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_layout_matches_go_marshal_indent() {
        let mut files = BTreeMap::new();
        files.insert("b/second.txt".into(), ManifestFile { sha: "bb".into(), size: 2, updated_at: 2, mode: 0 });
        files.insert("a/first.txt".into(), ManifestFile { sha: "aa".into(), size: 1, updated_at: 1, mode: 420 });
        files.insert("zeta.bin".into(), ManifestFile { sha: "zz".into(), size: 3, updated_at: 0, mode: 0 });
        let m = Manifest::new("abc", 1712419200000, "m", files);
        let want = "{\n  \"version\": 1,\n  \"parent_sha\": \"abc\",\n  \"created_at\": 1712419200000,\n  \"message\": \"m\",\n  \"files\": {\n    \"a/first.txt\": {\n      \"sha\": \"aa\",\n      \"size\": 1,\n      \"updated_at\": 1,\n      \"mode\": 420\n    },\n    \"b/second.txt\": {\n      \"sha\": \"bb\",\n      \"size\": 2,\n      \"updated_at\": 2\n    },\n    \"zeta.bin\": {\n      \"sha\": \"zz\",\n      \"size\": 3\n    }\n  }\n}\n";
        assert_eq!(String::from_utf8(m.to_canonical_bytes()).unwrap(), want);
        let back = Manifest::parse(want.as_bytes(), "t").unwrap();
        assert_eq!(back, m);
    }

    #[test]
    fn empty_manifest_omits_optional_fields() {
        let m = Manifest::new("", 0, "", BTreeMap::new());
        assert_eq!(String::from_utf8(m.to_canonical_bytes()).unwrap(), "{\n  \"version\": 1,\n  \"created_at\": 0\n}\n");
    }

    #[test]
    fn strict_reader() {
        assert!(Manifest::parse(br#"{"version":1,"created_at":0,"files":{},"surprise_field":"x"}"#, "t").is_err());
        assert!(Manifest::parse(br#"{"version":2,"created_at":0,"chunks":["a"]}"#, "t").is_err());
        assert!(Manifest::parse(br#"{"version":1,"created_at":0,"chunks":["a","b"]}"#, "t").is_err());
        assert!(Manifest::parse(br#"{"version":1,"created_at":0,"files":{"a":{"sha":"x","size":1,"bogus":1}}}"#, "t").is_err());
        assert!(Manifest::parse(br#"{"version":1,"created_at":0,"files":null,"chunks":[]}"#, "t").is_ok());
    }
}
