use std::time::Duration;

use async_trait::async_trait;
use reqwest::StatusCode;
use rusty_s3::actions::{ListObjectsV2, S3Action};
use rusty_s3::{Bucket, Credentials, UrlStyle};
use url::Url;

use super::{parse_remote_url, Remote};
use crate::error::{Error, Result};

const SIGN_TTL: Duration = Duration::from_secs(15 * 60);

/// Connection settings for an S3 (or S3-compatible: R2, MinIO, …) remote.
#[derive(Clone)]
pub struct S3Config {
    pub bucket: String,
    /// Key prefix inside the bucket (no leading/trailing slash needed).
    pub prefix: String,
    /// AWS region of the bucket (`"auto"` for Cloudflare R2).
    pub region: String,
    /// Custom endpoint (e.g. `https://<acct>.r2.cloudflarestorage.com`).
    /// Default: `https://s3.<region>.amazonaws.com`.
    pub endpoint: Option<String>,
    /// Virtual-hosted-style URLs. Default false = path-style, like Go.
    pub virtual_hosted_style: bool,
    pub access_key_id: String,
    pub secret_access_key: String,
    pub session_token: Option<String>,
}

impl std::fmt::Debug for S3Config {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("S3Config")
            .field("bucket", &self.bucket)
            .field("prefix", &self.prefix)
            .field("region", &self.region)
            .field("endpoint", &self.endpoint)
            .field("virtual_hosted_style", &self.virtual_hosted_style)
            .finish_non_exhaustive()
    }
}

impl S3Config {
    /// Settings for an `s3://bucket/prefix` URL (as stored in
    /// `.blobs/config.json`), with explicit credentials. Credentials are never
    /// read from disk by this crate.
    pub fn from_url(
        url: &str,
        region: impl Into<String>,
        access_key_id: impl Into<String>,
        secret_access_key: impl Into<String>,
    ) -> Result<S3Config> {
        let (scheme, bucket, prefix) =
            parse_remote_url(url).ok_or_else(|| Error::Remote(format!("unsupported remote url: {url}")))?;
        if scheme != "s3" {
            return Err(Error::Remote(format!("not an s3:// url: {url}")));
        }
        Ok(S3Config {
            bucket,
            prefix,
            region: region.into(),
            endpoint: None,
            virtual_hosted_style: false,
            access_key_id: access_key_id.into(),
            secret_access_key: secret_access_key.into(),
            session_token: None,
        })
    }
}

/// S3 remote built on `rusty-s3` (request signing) + `reqwest` (rustls).
pub struct S3Remote {
    bucket: Bucket,
    creds: Credentials,
    prefix: String,
    http: reqwest::Client,
}

impl S3Remote {
    pub fn new(cfg: S3Config) -> Result<S3Remote> {
        let endpoint = cfg.endpoint.clone().unwrap_or_else(|| format!("https://s3.{}.amazonaws.com", cfg.region));
        let endpoint: Url = endpoint.parse().map_err(|e| Error::Remote(format!("bad endpoint: {e}")))?;
        let style = if cfg.virtual_hosted_style { UrlStyle::VirtualHost } else { UrlStyle::Path };
        let bucket = Bucket::new(endpoint, style, cfg.bucket.clone(), cfg.region.clone())
            .map_err(|e| Error::Remote(format!("bucket: {e}")))?;
        let creds = match cfg.session_token {
            Some(t) => Credentials::new_with_token(cfg.access_key_id, cfg.secret_access_key, t),
            None => Credentials::new(cfg.access_key_id, cfg.secret_access_key),
        };
        let http = reqwest::Client::builder()
            .connect_timeout(Duration::from_secs(20))
            .build()
            .map_err(|e| Error::Remote(e.to_string()))?;
        Ok(S3Remote { bucket, creds, prefix: cfg.prefix.trim_matches('/').to_string(), http })
    }

    fn key(&self, k: &str) -> String {
        if self.prefix.is_empty() {
            k.to_string()
        } else {
            format!("{}/{}", self.prefix, k)
        }
    }

    async fn send(&self, req: reqwest::RequestBuilder) -> Result<reqwest::Response> {
        req.send().await.map_err(|e| Error::Remote(e.to_string()))
    }
}

async fn fail(what: &str, key: &str, resp: reqwest::Response) -> Error {
    let status = resp.status();
    let body = resp.text().await.unwrap_or_default();
    Error::Remote(format!("{what} {key}: HTTP {status}: {}", body.chars().take(300).collect::<String>()))
}

#[async_trait]
impl Remote for S3Remote {
    async fn exists(&self, key: &str) -> Result<bool> {
        let full = self.key(key);
        let url = self.bucket.head_object(Some(&self.creds), &full).sign(SIGN_TTL);
        let resp = self.send(self.http.head(url)).await?;
        match resp.status() {
            s if s.is_success() => Ok(true),
            StatusCode::NOT_FOUND => Ok(false),
            _ => Err(fail("HEAD", &full, resp).await),
        }
    }

    async fn get(&self, key: &str) -> Result<Vec<u8>> {
        let full = self.key(key);
        let url = self.bucket.get_object(Some(&self.creds), &full).sign(SIGN_TTL);
        let resp = self.send(self.http.get(url)).await?;
        match resp.status() {
            s if s.is_success() => Ok(resp.bytes().await.map_err(|e| Error::Remote(e.to_string()))?.to_vec()),
            StatusCode::NOT_FOUND => Err(Error::NotFound(key.to_string())),
            _ => Err(fail("GET", &full, resp).await),
        }
    }

    async fn put(&self, key: &str, data: Vec<u8>) -> Result<()> {
        let full = self.key(key);
        // Send (and sign) a CRC32 of the body: S3 verifies it on upload, and it
        // replaces any checksum stored with a previous version of the object.
        let crc = {
            use base64::Engine;
            base64::engine::general_purpose::STANDARD.encode(crc32fast::hash(&data).to_be_bytes())
        };
        let mut action = self.bucket.put_object(Some(&self.creds), &full);
        action.headers_mut().insert("x-amz-checksum-crc32", crc.clone());
        let url = action.sign(SIGN_TTL);
        let resp = self.send(self.http.put(url).header("x-amz-checksum-crc32", crc).body(data)).await?;
        if resp.status().is_success() {
            Ok(())
        } else {
            Err(fail("PUT", &full, resp).await)
        }
    }

    async fn list(&self, prefix: &str) -> Result<Vec<String>> {
        let full = self.key(prefix);
        let strip = if self.prefix.is_empty() { 0 } else { self.prefix.len() + 1 };
        let mut out = Vec::new();
        let mut token: Option<String> = None;
        loop {
            let mut action = ListObjectsV2::new(&self.bucket, Some(&self.creds));
            action.with_prefix(full.as_str());
            if let Some(t) = &token {
                action.with_continuation_token(t.as_str());
            }
            let url = action.sign(SIGN_TTL);
            let resp = self.send(self.http.get(url)).await?;
            if !resp.status().is_success() {
                return Err(fail("LIST", &full, resp).await);
            }
            let text = resp.text().await.map_err(|e| Error::Remote(e.to_string()))?;
            let parsed = ListObjectsV2::parse_response(&text).map_err(|e| Error::Remote(format!("list: parse: {e}")))?;
            for c in parsed.contents {
                out.push(if c.key.len() >= strip { c.key[strip..].to_string() } else { c.key });
            }
            match parsed.next_continuation_token {
                Some(t) if !t.is_empty() => token = Some(t),
                _ => break,
            }
        }
        out.sort();
        Ok(out)
    }
}
