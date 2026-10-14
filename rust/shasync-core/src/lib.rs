//! # shasync-core
//!
//! A library port of shasync: content-addressed folder
//! snapshots synced through a plain blob store, always end-to-end encrypted.
//! Repositories, manifests and remote objects are byte-compatible with the Go
//! CLI, so the two can work on the same `.blobs/` folder and the same bucket.
//!
//! ```no_run
//! # async fn demo() -> shasync_core::Result<()> {
//! use std::sync::Arc;
//! use shasync_core::{Repo, Syncer, remote::{S3Config, S3Remote}};
//!
//! let repo = Repo::open("/path/to/folder")?;
//! let key = repo.require_key()?;
//! let cfg = repo.read_config()?;
//! let s3 = S3Remote::new(S3Config::from_url(&cfg.remote, "us-east-1", "AKIA…", "secret")?)?;
//! let syncer = Syncer::new(repo, Arc::new(s3), key)?;
//! let report = syncer.sync().await?;
//! println!("{report:?}");
//! # Ok(()) }
//! ```

pub mod crypto;
pub mod delta;
pub mod error;
mod gojson;
pub mod ignore;
pub mod manifest;
pub mod merge;
pub mod remote;
pub mod repo;
pub mod sync;

pub use crypto::Key;
pub use error::{Error, Result};
pub use manifest::{Manifest, ManifestFile};
pub use remote::Remote;
pub use repo::{CommitResult, Config, LogEntry, Repo, Status};
pub use sync::{Progress, ProgressFn, SyncAction, SyncReport, Syncer};

/// Lowercase hex SHA-256 of `data`.
pub fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    hex::encode(Sha256::digest(data))
}
