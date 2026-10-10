use std::io;

/// Errors returned by shasync-core.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("io: {0}")]
    Io(#[from] io::Error),

    #[error("not inside a shasync project (no .blobs/ found)")]
    NotARepo,

    #[error(".blobs/ already exists")]
    AlreadyInitialized,

    /// Same wording as the Go CLI's `errNoKey`.
    #[error("no encryption key — shasync always encrypts, so every repo needs one (derive it from a passphrase, or generate a random key)")]
    NoKey,

    #[error("invalid key: {0}")]
    InvalidKey(String),

    #[error("crypto: {0}")]
    Crypto(String),

    /// An object downloaded from the remote did not carry the SHAS header.
    #[error("{0} is not encrypted — shasync only accepts encrypted remotes")]
    NotEncrypted(String),

    #[error("sha mismatch: got {got}, expected {want}")]
    ShaMismatch { got: String, want: String },

    #[error("manifest {sha}: {msg}")]
    Manifest { sha: String, msg: String },

    #[error("json: {0}")]
    Json(#[from] serde_json::Error),

    #[error("compression: {0}")]
    Compression(String),

    #[error("remote: object not found: {0}")]
    NotFound(String),

    #[error("remote: {0}")]
    Remote(String),

    #[error("remote HEAD malformed: expected 64 hex chars, got {0:?}")]
    MalformedRemoteHead(String),

    #[error("remote salt has wrong length: {0} bytes")]
    BadSalt(usize),

    /// A file changed on disk while a sync was in flight, so applying the
    /// remote version would have overwritten it. Nothing was lost; the next
    /// sync commits and merges it. (Go: `errLocalEditsDuringSync`.)
    #[error("local files changed during sync; they'll be synced next cycle")]
    LocalEditsDuringSync,

    #[error("uncommitted changes — commit or revert before merging a divergent remote")]
    UncommittedChanges,

    #[error("no common ancestor between local ({local}) and remote ({remote}) — histories are unrelated")]
    NoCommonAncestor { local: String, remote: String },

    /// Push refused: the remote is strictly ahead of local (pull first).
    #[error("remote has moved (remote={remote}, local={local}) — pull first")]
    RemoteAhead { remote: String, local: String },

    #[error("{0}")]
    Other(String),
}

pub type Result<T> = std::result::Result<T, Error>;
