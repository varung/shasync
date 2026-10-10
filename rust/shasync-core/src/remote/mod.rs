//! Remote blob stores. Keys are slash-separated and relative to the repo's
//! remote prefix: `HEAD`, `salt`, `blobs/<sha>`, `manifests/<sha>`,
//! `push-log/<ts>-<sha>`.

mod local;
mod memory;
mod s3;

pub use local::LocalDirRemote;
pub use memory::MemoryRemote;
pub use s3::{S3Config, S3Remote};

use async_trait::async_trait;

use crate::error::Result;

/// An abstract blob store (Go: `Remote`).
#[async_trait]
pub trait Remote: Send + Sync {
    async fn exists(&self, key: &str) -> Result<bool>;
    /// Fetches an object; a missing key is [`crate::Error::NotFound`].
    async fn get(&self, key: &str) -> Result<Vec<u8>>;
    async fn put(&self, key: &str, data: Vec<u8>) -> Result<()>;
    /// Keys under `prefix`, relative to the remote root (the prefix itself
    /// is kept: `list("blobs/")` → `["blobs/<sha>", …]`), sorted.
    async fn list(&self, prefix: &str) -> Result<Vec<String>>;
}

pub fn remote_blob_key(sha: &str) -> String {
    format!("blobs/{sha}")
}

pub fn remote_manifest_key(sha: &str) -> String {
    format!("manifests/{sha}")
}

pub const REMOTE_HEAD_KEY: &str = "HEAD";
pub const REMOTE_SALT_KEY: &str = "salt";
pub const PUSH_LOG_PREFIX: &str = "push-log/";

/// Splits `s3://bucket/some/prefix` into ("s3", "bucket", "some/prefix").
pub fn parse_remote_url(url: &str) -> Option<(&str, String, String)> {
    let (scheme, rest) = url.split_once("://")?;
    let rest = rest.trim_matches('/');
    let (bucket, prefix) = match rest.split_once('/') {
        Some((b, p)) => (b.to_string(), p.to_string()),
        None => (rest.to_string(), String::new()),
    };
    Some((scheme, bucket, prefix))
}
