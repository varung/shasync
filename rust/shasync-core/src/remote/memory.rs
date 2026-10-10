use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;

use super::Remote;
use crate::error::{Error, Result};

type Hook = Box<dyn FnOnce() + Send>;

/// An in-memory bucket for tests. Cloning shares the same storage.
///
/// [`MemoryRemote::on_next_blob_download`] runs a closure once, at the next
/// `blobs/…` download — i.e. mid-sync, after the working tree was
/// snapshotted — to simulate a user saving files during a sync.
#[derive(Clone, Default)]
pub struct MemoryRemote {
    objs: Arc<Mutex<BTreeMap<String, Vec<u8>>>>,
    hook: Arc<Mutex<Option<Hook>>>,
}

impl MemoryRemote {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn on_next_blob_download(&self, f: impl FnOnce() + Send + 'static) {
        *self.hook.lock().unwrap() = Some(Box::new(f));
    }

    /// Snapshot of all stored objects.
    pub fn objects(&self) -> BTreeMap<String, Vec<u8>> {
        self.objs.lock().unwrap().clone()
    }

    pub fn insert_raw(&self, key: &str, data: Vec<u8>) {
        self.objs.lock().unwrap().insert(key.to_string(), data);
    }
}

#[async_trait]
impl Remote for MemoryRemote {
    async fn exists(&self, key: &str) -> Result<bool> {
        Ok(self.objs.lock().unwrap().contains_key(key))
    }

    async fn get(&self, key: &str) -> Result<Vec<u8>> {
        if key.starts_with("blobs/") {
            let hook = self.hook.lock().unwrap().take();
            if let Some(f) = hook {
                f();
            }
        }
        self.objs.lock().unwrap().get(key).cloned().ok_or_else(|| Error::NotFound(key.to_string()))
    }

    async fn put(&self, key: &str, data: Vec<u8>) -> Result<()> {
        self.objs.lock().unwrap().insert(key.to_string(), data);
        Ok(())
    }

    async fn list(&self, prefix: &str) -> Result<Vec<String>> {
        Ok(self.objs.lock().unwrap().keys().filter(|k| k.starts_with(prefix)).cloned().collect())
    }
}
