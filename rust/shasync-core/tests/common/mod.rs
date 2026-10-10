//! Shared helpers for integration tests: building and driving the Go side.
#![allow(dead_code)]

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::sync::OnceLock;

/// The shasync Go module root (two levels above this crate).
pub fn go_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../..").canonicalize().unwrap()
}

pub struct GoTools {
    /// `go test -c` binary of the shasync package plus the compat helper.
    pub helper: PathBuf,
    /// `go build` of the shasync CLI.
    pub cli: PathBuf,
}

/// Builds the Go helper and CLI once per test process.
pub fn go_tools() -> &'static GoTools {
    static TOOLS: OnceLock<GoTools> = OnceLock::new();
    TOOLS.get_or_init(|| {
        let root = go_root();
        let out = Path::new(env!("CARGO_TARGET_TMPDIR")).join("go-compat");
        fs::create_dir_all(&out).unwrap();
        let overlay = out.join("overlay.json");
        let overlay_json = serde_json::json!({
            "Replace": {
                root.join("zz_compat_helper_test.go").to_str().unwrap():
                    root.join("rust/compat/_gohelper/compat_helper_test.go").to_str().unwrap()
            }
        });
        fs::write(&overlay, overlay_json.to_string()).unwrap();
        let helper = out.join("compat.test");
        let cli = out.join("shasync-go");
        let ok = Command::new("go")
            .current_dir(&root)
            .args(["test", "-c", &format!("-overlay={}", overlay.display()), "-o"])
            .arg(&helper)
            .arg(".")
            .status()
            .expect("`go` must be installed for the compat tests")
            .success();
        assert!(ok, "building the Go compat helper failed");
        let ok = Command::new("go").current_dir(&root).args(["build", "-o"]).arg(&cli).arg(".").status().unwrap().success();
        assert!(ok, "building the Go CLI failed");
        GoTools { helper, cli }
    })
}

/// Runs one helper mode with `COMPAT_*` env vars; returns the COMPAT_OUT bytes.
pub fn go_helper(mode: &str, env: &[(&str, &str)]) -> Vec<u8> {
    let tools = go_tools();
    let dir = tempfile::tempdir().unwrap();
    let out = dir.path().join("out");
    let res = Command::new(&tools.helper)
        .args(["-test.run", "^TestCompatHelper$"])
        .env("SHASYNC_COMPAT_MODE", mode)
        .env("COMPAT_OUT", &out)
        .envs(env.iter().copied())
        .output()
        .unwrap();
    assert!(res.status.success(), "go helper {mode} failed:\n{}\n{}", String::from_utf8_lossy(&res.stdout), String::from_utf8_lossy(&res.stderr));
    fs::read(&out).unwrap_or_default()
}

/// Runs `go_helper` with COMPAT_IN set to a temp file holding `input`.
pub fn go_helper_in(mode: &str, input: &[u8], env: &[(&str, &str)]) -> Vec<u8> {
    let dir = tempfile::tempdir().unwrap();
    let inp = dir.path().join("in");
    fs::write(&inp, input).unwrap();
    let mut env = env.to_vec();
    let p = inp.to_str().unwrap().to_string();
    env.push(("COMPAT_IN", &p));
    go_helper(mode, &env)
}

/// A sandboxed environment for the Go CLI: no real AWS config/credentials,
/// no ambient key, everything pointed at a local fake S3 (if given).
pub struct GoCli {
    pub home: tempfile::TempDir,
    pub env: BTreeMap<String, String>,
}

impl GoCli {
    pub fn new(s3_endpoint: Option<&str>) -> GoCli {
        let home = tempfile::tempdir().unwrap();
        let mut env = BTreeMap::new();
        env.insert("PATH".into(), std::env::var("PATH").unwrap_or_default());
        env.insert("HOME".into(), home.path().to_str().unwrap().into());
        env.insert("TZ".into(), std::env::var("TZ").unwrap_or_else(|_| "UTC".into()));
        let nowhere = home.path().join("no-such-aws-file");
        env.insert("AWS_CONFIG_FILE".into(), nowhere.to_str().unwrap().into());
        env.insert("AWS_SHARED_CREDENTIALS_FILE".into(), nowhere.to_str().unwrap().into());
        env.insert("AWS_EC2_METADATA_DISABLED".into(), "true".into());
        if let Some(ep) = s3_endpoint {
            env.insert("SHASYNC_S3_ENDPOINT".into(), ep.into());
            env.insert("SHASYNC_S3_FORCE_PATH_STYLE".into(), "1".into());
            env.insert("AWS_ACCESS_KEY_ID".into(), "test".into());
            env.insert("AWS_SECRET_ACCESS_KEY".into(), "test".into());
            env.insert("AWS_REGION".into(), "us-east-1".into());
        }
        GoCli { home, env }
    }

    pub fn run(&self, dir: &Path, args: &[&str], extra_env: &[(&str, &str)]) -> Output {
        Command::new(&go_tools().cli)
            .current_dir(dir)
            .args(args)
            .env_clear()
            .envs(&self.env)
            .envs(extra_env.iter().copied())
            .output()
            .unwrap()
    }

    /// Runs and asserts success; returns stdout.
    pub fn ok(&self, dir: &Path, args: &[&str], extra_env: &[(&str, &str)]) -> String {
        let o = self.run(dir, args, extra_env);
        assert!(
            o.status.success(),
            "shasync {args:?} failed:\nstdout: {}\nstderr: {}",
            String::from_utf8_lossy(&o.stdout),
            String::from_utf8_lossy(&o.stderr)
        );
        String::from_utf8(o.stdout).unwrap()
    }
}

/// Writes `data` at `root/rel`, creating parent directories.
pub fn put(root: &Path, rel: &str, data: &[u8]) {
    let p = root.join(rel);
    fs::create_dir_all(p.parent().unwrap()).unwrap();
    fs::write(p, data).unwrap();
}

pub fn get(root: &Path, rel: &str) -> Option<String> {
    fs::read_to_string(root.join(rel)).ok()
}
