//! Encryption, key derivation and the remote object codec, byte-compatible
//! with the Go implementation (`crypto.go`, `compress.go`, `uploadFile`,
//! `downloadAndStore`).
//!
//! Wire format v2 ("SHAS2"):
//! ```text
//! [0..5)   magic "SHAS2"
//! [5..9)   chunk_size     (u32 BE; 65536 when written by shasync)
//! [9..17)  plaintext_size (u64 BE)
//! [17..25) nonce_prefix   (8 random bytes)
//! [25..)   chunk i = AES-256-GCM(pt[i*N..(i+1)*N], nonce = prefix || u32_BE(i))
//! ```
//! The legacy whole-file "SHAS1" format (`"SHAS1" || nonce(12) || GCM(pt)`)
//! is accepted on read only.

use std::fmt;

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Nonce};

use crate::error::{Error, Result};
use crate::sha256_hex;

pub const KEY_LENGTH: usize = 32;
pub const SALT_LENGTH: usize = 16;
pub const CHUNK_SIZE: usize = 65536;
pub const MAGIC_V2: &[u8; 5] = b"SHAS2";
pub const MAGIC_V1: &[u8; 5] = b"SHAS1";
pub const HEADER_V2_LEN: usize = 25;
const NONCE_PREFIX_LEN: usize = 8;
const TAG_SIZE: usize = 16;
const V1_HEADER_LEN: usize = 17;

/// Argon2id parameters — must match Go's `argon2.IDKey(pass, salt, 3, 64*1024, 4, 32)`.
pub const ARGON_TIME: u32 = 3;
pub const ARGON_MEMORY_KIB: u32 = 64 * 1024;
pub const ARGON_THREADS: u32 = 4;

const ZSTD_MAGIC: [u8; 4] = [0x28, 0xB5, 0x2F, 0xFD];

/// A 32-byte AES-256 repository key.
#[derive(Clone, PartialEq, Eq)]
pub struct Key([u8; KEY_LENGTH]);

impl fmt::Debug for Key {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Key(<redacted>)")
    }
}

impl Key {
    pub fn from_bytes(b: &[u8]) -> Result<Key> {
        let arr: [u8; KEY_LENGTH] = b
            .try_into()
            .map_err(|_| Error::InvalidKey(format!("key must decode to {KEY_LENGTH} bytes (got {})", b.len())))?;
        Ok(Key(arr))
    }

    /// Parses a hex key (surrounding whitespace ignored), as in `.blobs/key`
    /// or `SHASYNC_KEY`.
    pub fn from_hex(s: &str) -> Result<Key> {
        let b = hex::decode(s.trim()).map_err(|e| Error::InvalidKey(format!("key is not valid hex: {e}")))?;
        Key::from_bytes(&b)
    }

    /// 32 random bytes (Go: `shasync key gen`).
    pub fn generate() -> Result<Key> {
        let mut b = [0u8; KEY_LENGTH];
        random_bytes(&mut b)?;
        Ok(Key(b))
    }

    /// Argon2id(passphrase, salt, t=3, m=64 MiB, p=4) → 32 bytes, as Go's
    /// `deriveKeyFromPassphrase`. The salt is the 16 raw bytes stored at
    /// `<prefix>/salt` on the remote.
    pub fn derive_from_passphrase(passphrase: &[u8], salt: &[u8]) -> Result<Key> {
        use argon2::{Algorithm, Argon2, Params, Version};
        let params = Params::new(ARGON_MEMORY_KIB, ARGON_TIME, ARGON_THREADS, Some(KEY_LENGTH))
            .map_err(|e| Error::Crypto(format!("argon2 params: {e}")))?;
        let a = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let mut out = [0u8; KEY_LENGTH];
        a.hash_password_into(passphrase, salt, &mut out).map_err(|e| Error::Crypto(format!("argon2: {e}")))?;
        Ok(Key(out))
    }

    pub fn as_bytes(&self) -> &[u8; KEY_LENGTH] {
        &self.0
    }

    /// Lowercase hex, as written to `.blobs/key` (without the trailing newline).
    pub fn to_hex(&self) -> String {
        hex::encode(self.0)
    }

    fn cipher(&self) -> Aes256Gcm {
        Aes256Gcm::new_from_slice(&self.0).expect("32-byte key")
    }
}

pub(crate) fn random_bytes(buf: &mut [u8]) -> Result<()> {
    getrandom::getrandom(buf).map_err(|e| Error::Crypto(format!("random: {e}")))
}

/// A fresh 16-byte Argon2id salt.
pub fn generate_salt() -> Result<[u8; SALT_LENGTH]> {
    let mut s = [0u8; SALT_LENGTH];
    random_bytes(&mut s)?;
    Ok(s)
}

/// True if `data` starts with a SHAS1 or SHAS2 header.
pub fn is_encrypted(data: &[u8]) -> bool {
    data.len() >= 5 && (&data[..5] == MAGIC_V2 || &data[..5] == MAGIC_V1)
}

/// Encrypts `plaintext` in the SHAS2 format with a random nonce prefix.
pub fn encrypt(key: &Key, plaintext: &[u8]) -> Result<Vec<u8>> {
    let mut prefix = [0u8; NONCE_PREFIX_LEN];
    random_bytes(&mut prefix)?;
    encrypt_with_nonce_prefix(key, plaintext, prefix)
}

/// SHAS2 encryption with a caller-chosen nonce prefix (deterministic output;
/// for tests only — reusing a prefix with the same key breaks GCM).
pub fn encrypt_with_nonce_prefix(key: &Key, plaintext: &[u8], prefix: [u8; NONCE_PREFIX_LEN]) -> Result<Vec<u8>> {
    let cipher = key.cipher();
    let num_chunks = plaintext.len().div_ceil(CHUNK_SIZE);
    let mut out = Vec::with_capacity(HEADER_V2_LEN + plaintext.len() + num_chunks * TAG_SIZE);
    out.extend_from_slice(MAGIC_V2);
    out.extend_from_slice(&(CHUNK_SIZE as u32).to_be_bytes());
    out.extend_from_slice(&(plaintext.len() as u64).to_be_bytes());
    out.extend_from_slice(&prefix);
    let mut nonce = [0u8; 12];
    nonce[..NONCE_PREFIX_LEN].copy_from_slice(&prefix);
    for (i, chunk) in plaintext.chunks(CHUNK_SIZE).enumerate() {
        nonce[NONCE_PREFIX_LEN..].copy_from_slice(&(i as u32).to_be_bytes());
        let ct = cipher
            .encrypt(Nonce::from_slice(&nonce), chunk)
            .map_err(|_| Error::Crypto("encrypt failed".into()))?;
        out.extend_from_slice(&ct);
    }
    Ok(out)
}

/// Decrypts a SHAS2 (or legacy SHAS1) object. Anything else is refused.
pub fn decrypt(key: &Key, framed: &[u8]) -> Result<Vec<u8>> {
    if framed.len() < 5 {
        return Err(Error::Crypto("ciphertext too short to contain magic".into()));
    }
    match &framed[..5] {
        m if m == MAGIC_V2 => decrypt_v2(key, framed),
        m if m == MAGIC_V1 => decrypt_v1(key, framed),
        _ => Err(Error::Crypto("unrecognized wire format (wrong key, unencrypted, or future version?)".into())),
    }
}

fn decrypt_v2(key: &Key, framed: &[u8]) -> Result<Vec<u8>> {
    if framed.len() < HEADER_V2_LEN {
        return Err(Error::Crypto("v2 ciphertext too short".into()));
    }
    let chunk_size = u32::from_be_bytes(framed[5..9].try_into().unwrap()) as u64;
    let pt_size = u64::from_be_bytes(framed[9..17].try_into().unwrap());
    let prefix = &framed[17..25];
    let body = &framed[HEADER_V2_LEN..];
    if chunk_size == 0 {
        return Err(Error::Crypto("v2: invalid chunk_size 0".into()));
    }
    let num_chunks = if pt_size > 0 { pt_size.div_ceil(chunk_size) } else { 0 };
    let expected = pt_size
        .checked_add(num_chunks.saturating_mul(TAG_SIZE as u64))
        .ok_or_else(|| Error::Crypto("v2: size overflow".into()))?;
    if body.len() as u64 != expected {
        return Err(Error::Crypto(format!(
            "v2: ciphertext body length mismatch (got {}, expected {expected})",
            body.len()
        )));
    }
    let cipher = key.cipher();
    let mut pt = Vec::with_capacity(pt_size as usize);
    let mut nonce = [0u8; 12];
    nonce[..NONCE_PREFIX_LEN].copy_from_slice(prefix);
    let mut pos = 0usize;
    for i in 0..num_chunks {
        nonce[NONCE_PREFIX_LEN..].copy_from_slice(&(i as u32).to_be_bytes());
        let ct_len = if i == num_chunks - 1 { pt_size - i * chunk_size } else { chunk_size } as usize + TAG_SIZE;
        let chunk = &body[pos..pos + ct_len];
        pos += ct_len;
        let p = cipher
            .decrypt(Nonce::from_slice(&nonce), chunk)
            .map_err(|_| Error::Crypto(format!("decrypt chunk {i}: message authentication failed")))?;
        pt.extend_from_slice(&p);
    }
    Ok(pt)
}

fn decrypt_v1(key: &Key, framed: &[u8]) -> Result<Vec<u8>> {
    if framed.len() < V1_HEADER_LEN + TAG_SIZE {
        return Err(Error::Crypto("v1 ciphertext too short".into()));
    }
    key.cipher()
        .decrypt(Nonce::from_slice(&framed[5..17]), &framed[V1_HEADER_LEN..])
        .map_err(|_| Error::Crypto("v1 decrypt: message authentication failed".into()))
}

pub fn is_zstd(data: &[u8]) -> bool {
    data.len() >= 4 && data[..4] == ZSTD_MAGIC
}

/// zstd at the default level (Go uses klauspost `SpeedDefault` ≈ level 3).
pub fn compress(data: &[u8]) -> Result<Vec<u8>> {
    zstd::bulk::compress(data, 3).map_err(|e| Error::Compression(e.to_string()))
}

pub fn decompress(data: &[u8]) -> Result<Vec<u8>> {
    zstd::stream::decode_all(data).map_err(|e| Error::Compression(e.to_string()))
}

/// Encodes a local object for upload exactly as Go's `uploadFile`:
/// zstd-compress, then SHAS2-encrypt.
pub fn encode_object(key: &Key, plaintext: &[u8]) -> Result<Vec<u8>> {
    let out = encrypt(key, &compress(plaintext)?)?;
    debug_assert!(is_encrypted(&out));
    Ok(out)
}

/// Decodes a downloaded object exactly as Go's `downloadAndStore`: refuse
/// anything without a SHAS header, decrypt, decompress if the plaintext
/// starts with the zstd magic, then verify `sha256 == expected_sha`.
/// `name` is used in error messages (e.g. the remote key).
pub fn decode_object(key: &Key, data: &[u8], expected_sha: &str, name: &str) -> Result<Vec<u8>> {
    let pt = open_object(key, data, name)?;
    let got = sha256_hex(&pt);
    if got != expected_sha {
        return Err(Error::ShaMismatch { got, want: expected_sha.to_string() });
    }
    Ok(pt)
}

/// A downloaded object's plaintext, unverified: refuse plaintext, decrypt,
/// decompress. (A manifest stored as a delta only hashes to its SHA once
/// rebuilt; see `delta`.)
pub fn open_object(key: &Key, data: &[u8], name: &str) -> Result<Vec<u8>> {
    if !is_encrypted(data) {
        return Err(Error::NotEncrypted(name.to_string()));
    }
    let mut pt = decrypt(key, data)?;
    if is_zstd(&pt) {
        pt = decompress(&pt)?;
    }
    Ok(pt)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_all_sizes() {
        let key = Key::generate().unwrap();
        for n in [0usize, 1, 15, 16, 17, CHUNK_SIZE - 1, CHUNK_SIZE, CHUNK_SIZE + 1, 3 * CHUNK_SIZE + 7] {
            let pt: Vec<u8> = (0..n).map(|i| (i * 31 % 251) as u8).collect();
            let ct = encrypt(&key, &pt).unwrap();
            assert_eq!(ct.len(), HEADER_V2_LEN + n + n.div_ceil(CHUNK_SIZE) * TAG_SIZE);
            assert_eq!(decrypt(&key, &ct).unwrap(), pt);
        }
    }

    #[test]
    fn tamper_and_wrong_key_fail() {
        let key = Key::generate().unwrap();
        let mut ct = encrypt(&key, b"hello").unwrap();
        assert!(decrypt(&Key::generate().unwrap(), &ct).is_err());
        let last = ct.len() - 1;
        ct[last] ^= 1;
        assert!(decrypt(&key, &ct).is_err());
    }

    #[test]
    fn plaintext_refused() {
        let key = Key::generate().unwrap();
        let data = b"just some plaintext";
        let sha = sha256_hex(data);
        assert!(matches!(decode_object(&key, data, &sha, "blobs/x"), Err(Error::NotEncrypted(_))));
        let enc = encode_object(&key, data).unwrap();
        assert_eq!(decode_object(&key, &enc, &sha, "blobs/x").unwrap(), data);
        assert!(decode_object(&key, &enc, &"0".repeat(64), "blobs/x").is_err());
    }
}
