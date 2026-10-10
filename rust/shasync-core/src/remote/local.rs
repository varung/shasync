use std::io;
use std::path::{Path, PathBuf};

use async_trait::async_trait;

use super::Remote;
use crate::error::{Error, Result};

/// A remote backed by a local directory: key `a/b` is the file `<root>/a/b`.
/// Useful for tests and for syncing via a mounted/shared folder.
#[derive(Debug, Clone)]
pub struct LocalDirRemote {
    root: PathBuf,
}

impl LocalDirRemote {
    pub fn new(root: impl Into<PathBuf>) -> Self {
        LocalDirRemote { root: root.into() }
    }

    fn path(&self, key: &str) -> Result<PathBuf> {
        if key.is_empty() || key.split('/').any(|s| s.is_empty() || s == "." || s == "..") {
            return Err(Error::Remote(format!("invalid key {key:?}")));
        }
        Ok(self.root.join(key))
    }
}

fn collect(dir: &Path, rel: &str, out: &mut Vec<String>) -> io::Result<()> {
    let rd = match std::fs::read_dir(dir) {
        Ok(rd) => rd,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    for e in rd {
        let e = e?;
        let Some(name) = e.file_name().to_str().map(str::to_owned) else { continue };
        if name.starts_with(".tmp-") {
            continue;
        }
        let k = if rel.is_empty() { name } else { format!("{rel}/{name}") };
        if e.file_type()?.is_dir() {
            collect(&e.path(), &k, out)?;
        } else {
            out.push(k);
        }
    }
    Ok(())
}

#[async_trait]
impl Remote for LocalDirRemote {
    async fn exists(&self, key: &str) -> Result<bool> {
        Ok(self.path(key)?.is_file())
    }

    async fn get(&self, key: &str) -> Result<Vec<u8>> {
        match std::fs::read(self.path(key)?) {
            Err(e) if e.kind() == io::ErrorKind::NotFound => Err(Error::NotFound(key.to_string())),
            r => Ok(r?),
        }
    }

    async fn put(&self, key: &str, data: Vec<u8>) -> Result<()> {
        let p = self.path(key)?;
        let dir = p.parent().unwrap().to_path_buf();
        std::fs::create_dir_all(&dir)?;
        let mut rnd = [0u8; 8];
        crate::crypto::random_bytes(&mut rnd)?;
        let tmp = dir.join(format!(".tmp-{}", hex::encode(rnd)));
        std::fs::write(&tmp, data)?;
        std::fs::rename(&tmp, &p)?;
        Ok(())
    }

    async fn list(&self, prefix: &str) -> Result<Vec<String>> {
        let mut out = Vec::new();
        collect(&self.root, "", &mut out)?;
        out.retain(|k| k.starts_with(prefix));
        out.sort();
        Ok(out)
    }
}
