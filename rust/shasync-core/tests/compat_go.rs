//! Go ⇄ Rust interoperability. Builds the Go CLI and a test-only helper
//! (rust/compat/_gohelper, compiled in via `go test -overlay`) and checks
//! that both implementations read each other's bytes.
//!
//! Nothing here touches a real bucket: remote tests run against an
//! in-memory S3 (gofakes3) on 127.0.0.1, and the Go CLI runs with a scratch
//! HOME and AWS config/credential files pointed at nonexistent paths.

mod common;

use std::collections::BTreeMap;
use std::fs;
use std::io::{BufRead, BufReader};
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::sync::Arc;

use common::*;
use shasync_core::crypto::{self, Key};
use shasync_core::remote::{Remote, S3Config, S3Remote};
use shasync_core::{merge, sha256_hex, Manifest, ManifestFile, Repo, SyncAction, Syncer};

const KEY_HEX: &str = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f";

fn sample(n: usize, seed: u32) -> Vec<u8> {
    let mut x = seed.wrapping_mul(2654435761).wrapping_add(1);
    (0..n)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 17;
            x ^= x << 5;
            (x & 0xff) as u8
        })
        .collect()
}

const SIZES: &[usize] = &[0, 1, 15, 16, 65535, 65536, 65537, 3 * 65536 + 11, 300_000];

#[test]
fn go_encrypts_rust_decrypts() {
    let key = Key::from_hex(KEY_HEX).unwrap();
    for (i, &n) in SIZES.iter().enumerate() {
        let pt = sample(n, i as u32);
        let ct = go_helper_in("encrypt", &pt, &[("COMPAT_KEY", KEY_HEX)]);
        assert_eq!(&ct[..5], b"SHAS2");
        assert_eq!(crypto::decrypt(&key, &ct).unwrap(), pt, "size {n}");
        // Full object codec (zstd + encrypt), as uploadFile sends it.
        let obj = go_helper_in("encode", &pt, &[("COMPAT_KEY", KEY_HEX)]);
        assert_eq!(crypto::decode_object(&key, &obj, &sha256_hex(&pt), "blobs/x").unwrap(), pt, "size {n}");
    }
}

#[test]
fn rust_encrypts_go_decrypts() {
    let key = Key::from_hex(KEY_HEX).unwrap();
    for (i, &n) in SIZES.iter().enumerate() {
        let pt = sample(n, 100 + i as u32);
        let ct = crypto::encrypt(&key, &pt).unwrap();
        assert_eq!(go_helper_in("decrypt", &ct, &[("COMPAT_KEY", KEY_HEX)]), pt, "size {n}");
        let obj = crypto::encode_object(&key, &pt).unwrap();
        let sha = sha256_hex(&pt);
        assert_eq!(go_helper_in("decode", &obj, &[("COMPAT_KEY", KEY_HEX), ("COMPAT_SHA", &sha)]), pt, "size {n}");
    }
}

#[test]
fn plaintext_is_refused_by_both() {
    let key = Key::from_hex(KEY_HEX).unwrap();
    let pt = b"plaintext that someone put in the bucket".to_vec();
    assert!(crypto::decode_object(&key, &pt, &sha256_hex(&pt), "blobs/x").is_err());
    let out = Command::new(&go_tools().helper)
        .args(["-test.run", "^TestCompatHelper$"])
        .env("SHASYNC_COMPAT_MODE", "decode")
        .env("COMPAT_KEY", KEY_HEX)
        .env("COMPAT_SHA", sha256_hex(&pt))
        .env("COMPAT_IN", {
            let p = std::env::temp_dir().join(format!("shasync-plain-{}", std::process::id()));
            fs::write(&p, &pt).unwrap();
            p
        })
        .output()
        .unwrap();
    assert!(!out.status.success(), "Go accepted a plaintext object");
}

#[test]
fn passphrase_derivation_matches() {
    let cases: &[(&str, &str)] = &[
        ("correct horse battery staple", "000102030405060708090a0b0c0d0e0f"),
        ("pässwörd ☃", "ffeeddccbbaa99887766554433221100"),
        ("x", "8f1e2d3c4b5a69788796a5b4c3d2e1f0"),
    ];
    for (pass, salt_hex) in cases {
        let salt = hex::decode(salt_hex).unwrap();
        let rust = Key::derive_from_passphrase(pass.as_bytes(), &salt).unwrap().to_hex();
        let go = String::from_utf8(go_helper("derive", &[("COMPAT_PASS", pass), ("COMPAT_SALT", salt_hex)])).unwrap();
        assert_eq!(rust, go, "passphrase {pass:?}");
    }
}

#[test]
fn manifest_canonical_bytes_match_go() {
    let names = [
        "plain.txt",
        "dir/sub/file.bin",
        "amp & <angle> \"quote\" back\\slash.md",
        "日本語/é ü.txt",
        "line\u{2028}para\u{2029}.txt",
        "ctl\t\n\r\u{1}\u{1f}\u{7f}.txt",
        "emoji 🎉.png",
        "Z-upper",
        "a",
    ];
    let mut files = BTreeMap::new();
    for (i, n) in names.iter().enumerate() {
        files.insert(
            n.to_string(),
            ManifestFile {
                sha: sha256_hex(n.as_bytes()),
                size: i as i64 * 1000,
                updated_at: if i % 3 == 0 { 0 } else { 1_700_000_000_000 + i as i64 },
                mode: if i % 2 == 0 { 0 } else { 0o644 + i as u32 },
            },
        );
    }
    for (parent, msg) in [("", ""), ("ab".repeat(32).as_str(), "msg with <html> & \"quotes\"\nnewline ☃")] {
        let m = Manifest::new(parent, -5 + 1_712_419_200_000, msg, files.clone());
        let ours = m.to_canonical_bytes();
        let go = go_helper_in("canon", &ours, &[]);
        assert_eq!(String::from_utf8_lossy(&ours), String::from_utf8_lossy(&go));
        assert_eq!(Manifest::parse(&go, "x").unwrap(), m);
    }
    let empty = Manifest::new("", 0, "", BTreeMap::new()).to_canonical_bytes();
    assert_eq!(go_helper_in("canon", &empty, &[]), empty);
}

#[test]
fn ignore_matching_matches_go() {
    let patterns = [
        ".DS_Store", "._*", "*.swp", "*~", "4913", ".#*", "#*#", ".tmp-*", "node_modules", "node_modules/", "build/",
        "*.log", "docs/*.md", "a/**/b", "[abc]*.txt", "[^x]y", "[a-c]?", "\\*lit", "bad[", "[]x]", "*", "?", "日*",
        "a*b*c", "/abs",
    ];
    let paths = [
        ".DS_Store", "x/.DS_Store", "._foo", "note.md.swp", "note.md~", "4913", "a/4913/", ".#lock", "#auto#", ".tmp-1",
        "node_modules/", "x/node_modules/", "x/node_modules/y.js", "build/", "src/build/", "build.rs", "out.log",
        "x/y/out.log", "docs/a.md", "docs/sub/a.md", "a/x/b", "a/b", "bfile.txt", "dfile.txt", "zy", "xy", "b1",
        "*lit", "bad[", "]", "日本", "abc", "aXbYc", "abs", "", "dir/",
    ];
    let mut pairs = Vec::new();
    for p in patterns {
        for q in paths {
            pairs.push((p.to_string(), q.to_string()));
        }
    }
    let json = serde_json::to_vec(&pairs).unwrap();
    let go: Vec<bool> = serde_json::from_slice(&go_helper_in("match", &json, &[])).unwrap();
    let mut mismatches = Vec::new();
    for ((p, q), g) in pairs.iter().zip(go) {
        let r = shasync_core::ignore::match_ignore(std::slice::from_ref(p), q);
        if r != g {
            mismatches.push(format!("pattern {p:?} path {q:?}: go={g} rust={r}"));
        }
    }
    assert!(mismatches.is_empty(), "{}", mismatches.join("\n"));
}

/// A tree with awkward names, modes, an ignore file, ignored litter and a symlink.
fn build_tree(root: &Path) {
    put(root, "hello.txt", b"hello world\n");
    put(root, "sub/a.txt", b"alpha\n");
    put(root, "sub/deeper/b.bin", &sample(200_000, 7));
    put(root, "empty.txt", b"");
    put(root, "amp & <angle> \"q\".md", b"html-ish name\n");
    put(root, "日本語/é ü.txt", b"unicode\n");
    put(root, "line\u{2028}sep.txt", b"u2028\n");
    put(root, "tab\tname.txt", b"tab\n");
    put(root, "exec.sh", b"#!/bin/sh\necho hi\n");
    fs::set_permissions(root.join("exec.sh"), fs::Permissions::from_mode(0o755)).unwrap();
    put(root, "private.txt", b"secret-ish\n");
    fs::set_permissions(root.join("private.txt"), fs::Permissions::from_mode(0o600)).unwrap();
    put(root, ".blobsignore", b"# comment\nnode_modules/\n*.log\n\n  build  \n");
    put(root, "node_modules/pkg/index.js", b"ignored\n");
    put(root, "x/debug.log", b"ignored\n");
    put(root, "build/out.o", b"ignored\n");
    put(root, "keep.log.txt", b"kept\n");
    put(root, ".DS_Store", b"ignored\n");
    put(root, "note.md.swp", b"ignored\n");
    put(root, "nested/.DS_Store", b"ignored\n");
    std::os::unix::fs::symlink("hello.txt", root.join("link-to-hello")).unwrap();
}

fn manifest_bytes(root: &Path, sha: &str) -> Vec<u8> {
    fs::read(root.join(".blobs/manifests").join(&sha[..2]).join(format!("{}.json", &sha[2..]))).unwrap()
}

fn first_word(s: &str) -> String {
    s.split_whitespace().next().unwrap().to_string()
}

fn copy_dir(src: &Path, dst: &Path) {
    assert!(Command::new("cp").arg("-Rp").arg(src).arg(dst).status().unwrap().success());
}

fn rm_blobs(dir: &Path) {
    let _ = Command::new("chmod").arg("-R").arg("u+w").arg(dir).status();
    fs::remove_dir_all(dir).unwrap();
}

#[test]
fn same_tree_same_manifest_and_repo_interop() {
    let cli = GoCli::new(None);
    let tmp = tempfile::tempdir().unwrap();
    let root = tmp.path().join("tree");
    fs::create_dir(&root).unwrap();
    build_tree(&root);

    // 1. Go commits the tree.
    cli.ok(&root, &["init"], &[]);
    let go_sha = first_word(&cli.ok(&root, &["commit", "-m", "from go"], &[]));
    let go_bytes = manifest_bytes(&root, &go_sha);
    let go_m = Manifest::parse(&go_bytes, &go_sha).unwrap();
    assert!(go_m.files.contains_key("amp & <angle> \"q\".md"));
    assert!(!go_m.files.contains_key("link-to-hello"));
    assert!(!go_m.files.keys().any(|p| p.contains("node_modules") || p.ends_with(".log") || p.contains("DS_Store")));

    // 2. Rust commits the same tree in a fresh store: same bytes, same SHA, same objects.
    let go_blobs = tmp.path().join("go-blobs");
    fs::rename(root.join(".blobs"), &go_blobs).unwrap();
    let repo = Repo::init(&root).unwrap();
    let c = repo.commit_at("from go", go_m.created_at).unwrap();
    assert!(c.committed);
    assert_eq!(c.head.as_deref(), Some(go_sha.as_str()));
    assert_eq!(manifest_bytes(&root, &go_sha), go_bytes);
    for f in go_m.files.values() {
        assert_eq!(repo.read_object(&f.sha).unwrap(), fs::read(go_blobs.join("objects").join(&f.sha[..2]).join(&f.sha[2..])).unwrap());
    }
    rm_blobs(&root.join(".blobs"));
    fs::rename(&go_blobs, root.join(".blobs")).unwrap();

    // 3. Rust opens the Go repo.
    let repo = Repo::open(root.join("sub/deeper")).unwrap();
    assert_eq!(repo.root(), root.canonicalize().unwrap());
    assert_eq!(repo.head().unwrap().as_deref(), Some(go_sha.as_str()));
    assert_eq!(repo.read_config().unwrap().client_id.len(), 8);
    assert!(repo.status().unwrap().is_clean());
    let log = repo.log(None, 10).unwrap();
    assert_eq!(log.len(), 1);
    assert_eq!(log[0].manifest.message, "from go");

    // 4. Status agrees with Go's.
    put(&root, "hello.txt", b"hello again, longer\n");
    fs::remove_file(root.join("sub/a.txt")).unwrap();
    put(&root, "new dir/new.txt", b"new\n");
    let st = repo.status().unwrap();
    assert_eq!(st.added, vec!["new dir/new.txt"]);
    assert_eq!(st.modified, vec!["hello.txt"]);
    assert_eq!(st.removed, vec!["sub/a.txt"]);
    let go_status = cli.ok(&root, &["status"], &[]);
    for line in ["  A  new dir/new.txt", "  M  hello.txt", "  D  sub/a.txt"] {
        assert!(go_status.contains(line), "go status missing {line:?}:\n{go_status}");
    }

    // 5. Rust commits; Go sees it in log/status and agrees nothing changed.
    let rust_sha = repo.commit("from rust").unwrap().head.unwrap();
    assert!(cli.ok(&root, &["status"], &[]).contains("clean"));
    let go_log = cli.ok(&root, &["log"], &[]);
    assert!(go_log.contains(&format!("{rust_sha}  ")) && go_log.contains("from rust"), "{go_log}");
    assert!(go_log.contains("  A  new dir/new.txt") && go_log.contains("  D  sub/a.txt"), "{go_log}");
    assert!(cli.ok(&root, &["commit", "-m", "noop"], &[]).contains("nothing to commit"));
    assert_eq!(cli.ok(&root, &["head"], &[]).trim(), rust_sha);

    // 6. Second generation on top of a Rust parent (exercises the size+mtime
    //    fast path): Go and Rust again produce the identical commit.
    put(&root, "sub/deeper/b.bin", &sample(1000, 9));
    let backup = tmp.path().join("blobs-before");
    copy_dir(&root.join(".blobs"), &backup);
    let go2 = first_word(&cli.ok(&root, &["commit", "-m", "gen 2"], &[]));
    let go2_bytes = manifest_bytes(&root, &go2);
    let go2_m = Manifest::parse(&go2_bytes, &go2).unwrap();
    assert_eq!(go2_m.parent_sha, rust_sha);
    rm_blobs(&root.join(".blobs"));
    fs::rename(&backup, root.join(".blobs")).unwrap();
    let r2 = repo.commit_at("gen 2", go2_m.created_at).unwrap();
    assert_eq!(r2.head.as_deref(), Some(go2.as_str()));
    assert_eq!(manifest_bytes(&root, &go2), go2_bytes);

    // 7. Go can check out the Rust-written history.
    cli.ok(&root, &["checkout", &go_sha], &[]);
    assert_eq!(get(&root, "hello.txt").unwrap(), "hello world\n");
    assert_eq!(get(&root, "sub/a.txt").unwrap(), "alpha\n");
    assert!(repo.status().unwrap().is_clean());
}

#[test]
fn rust_reads_go_key_file() {
    let cli = GoCli::new(None);
    let tmp = tempfile::tempdir().unwrap();
    cli.ok(tmp.path(), &["init"], &[]);
    cli.ok(tmp.path(), &["key", "gen"], &[]);
    let repo = Repo::open(tmp.path()).unwrap();
    let key = repo.require_key().unwrap();
    assert_eq!(key.to_hex(), cli.ok(tmp.path(), &["key", "show"], &[]).trim());
    assert_eq!(fs::metadata(repo.key_path()).unwrap().permissions().mode() & 0o777, 0o600);
}

#[test]
fn auto_merge_matches_go() {
    let tmp = tempfile::tempdir().unwrap();
    let repo = Repo::init(tmp.path()).unwrap();
    let f = |s: &str, t: i64| ManifestFile { sha: sha256_hex(s.as_bytes()), size: s.len() as i64, updated_at: t, mode: 0o644 };
    let mk = |parent: &str, entries: &[(&str, &str)], t: i64| {
        let files = entries.iter().map(|(p, c)| (p.to_string(), f(c, t))).collect();
        repo.write_manifest(&Manifest::new(parent, t, "x", files)).unwrap()
    };
    let anc = mk("", &[("same", "s"), ("remote-only", "r0"), ("local-only", "l0"), ("both-same", "b0"), ("conflict.md", "c0"), ("ldel-rmod", "d0"), ("rdel-lmod", "e0"), ("both-del", "z"), ("dir/a.tar.gz", "t0")], 1);
    let remote = mk(&anc, &[("same", "s"), ("remote-only", "r1"), ("local-only", "l0"), ("both-same", "b1"), ("conflict.md", "c-remote"), ("ldel-rmod", "d1"), ("both-add", "ra"), ("dir/a.tar.gz", "t-remote")], 2);
    let local = mk(&anc, &[("same", "s"), ("remote-only", "r0"), ("local-only", "l1"), ("both-same", "b1"), ("conflict.md", "c-local"), ("rdel-lmod", "e1"), ("both-add", "la"), ("dir/a.tar.gz", "t-local"), (".hidden", "h")], 3);
    let now = 1_760_000_123_456;
    let (m, conflicts) = merge::auto_merge(&repo, "c0ffee00", now, &anc, &remote, &local).unwrap();
    let rust_sha = repo.write_manifest(&m).unwrap();
    let root = tmp.path().to_str().unwrap();
    let now_s = now.to_string();
    let go = String::from_utf8(go_helper(
        "automerge",
        &[("COMPAT_ROOT", root), ("COMPAT_NOW_MS", &now_s), ("COMPAT_CLIENT", "c0ffee00"), ("COMPAT_ANC", &anc), ("COMPAT_REMOTE", &remote), ("COMPAT_LOCAL", &local)],
    ))
    .unwrap();
    let mut lines = go.lines();
    assert_eq!(lines.next().unwrap(), rust_sha);
    assert_eq!(lines.map(str::to_string).collect::<Vec<_>>(), conflicts);
    assert_eq!(conflicts, vec!["both-add", "conflict.md", "dir/a.tar.gz", "ldel-rmod", "rdel-lmod"]);
}

// ----- end to end through a fake S3 ------------------------------------------------------

struct FakeS3 {
    child: Child,
    url: String,
}

impl FakeS3 {
    fn start(bucket: &str) -> FakeS3 {
        let mut child = Command::new(&go_tools().helper)
            .args(["-test.run", "^TestCompatHelper$"])
            .env("SHASYNC_COMPAT_MODE", "s3server")
            .env("COMPAT_BUCKET", bucket)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut line = String::new();
        BufReader::new(child.stdout.take().unwrap()).read_line(&mut line).unwrap();
        let url = line.trim().strip_prefix("URL=").expect("s3server URL line").to_string();
        assert!(url.starts_with("http://127.0.0.1:"), "fake S3 must be local: {url}");
        FakeS3 { child, url }
    }

    fn remote(&self, url: &str) -> S3Remote {
        let mut cfg = S3Config::from_url(url, "us-east-1", "test", "test").unwrap();
        cfg.endpoint = Some(self.url.clone());
        S3Remote::new(cfg).unwrap()
    }
}

impl Drop for FakeS3 {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn files_of(root: &Path) -> BTreeMap<String, Vec<u8>> {
    let repo = Repo::open_exact(root).unwrap();
    repo.scan().unwrap().into_iter().map(|p| (p.clone(), fs::read(root.join(&p)).unwrap())).collect()
}

#[tokio::test(flavor = "multi_thread")]
async fn go_and_rust_sync_through_fake_s3() {
    let s3 = FakeS3::start("compat");
    let url = "s3://compat/proj";
    let pass = "correct horse battery staple";
    let cli = GoCli::new(Some(&s3.url));
    let tmp = tempfile::tempdir().unwrap();
    let (a, b) = (tmp.path().join("a-go"), tmp.path().join("b-rust"));
    fs::create_dir(&a).unwrap();
    fs::create_dir(&b).unwrap();

    // Go creates the repo, derives the key from a passphrase (salt goes to the bucket) and pushes.
    put(&a, "note.md", b"v1\n");
    put(&a, "docs/guide.txt", b"guide\n");
    put(&a, "big.bin", &sample(250_000, 3));
    cli.ok(&a, &["init"], &[]);
    cli.ok(&a, &["remote", "set", url], &[]);
    cli.ok(&a, &["key", "set-passphrase"], &[("SHASYNC_PASSPHRASE", pass)]);
    cli.ok(&a, &["push"], &[]);
    let go_key = fs::read_to_string(a.join(".blobs/key")).unwrap();

    // Rust clones with the same passphrase over its own S3 client.
    let repo_b = Repo::init(&b).unwrap();
    repo_b.set_remote(url, Some(&s3.url), false).unwrap();
    let remote_b: Arc<dyn Remote> = Arc::new(s3.remote(url));
    let key_b = shasync_core::sync::set_passphrase(&repo_b, remote_b.as_ref(), pass.as_bytes()).await.unwrap();
    assert_eq!(key_b.to_hex(), go_key.trim());
    let rb = Syncer::new(repo_b.clone(), remote_b.clone(), key_b).unwrap();
    let rep = rb.pull().await.unwrap();
    assert!(matches!(rep.action, SyncAction::FastForwarded { .. }), "{rep:?}");
    assert_eq!(files_of(&a), files_of(&b));
    assert_eq!(repo_b.head().unwrap().unwrap(), cli.ok(&a, &["head"], &[]).trim());

    // Rust edits and pushes; Go pulls.
    put(&b, "note.md", b"v2 from rust\n");
    put(&b, "rust-new.txt", b"made in rust\n");
    fs::remove_file(b.join("docs/guide.txt")).unwrap();
    let rep = rb.push().await.unwrap();
    assert!(matches!(rep.action, SyncAction::Pushed { .. }), "{rep:?}");
    cli.ok(&a, &["pull"], &[]);
    assert_eq!(files_of(&a), files_of(&b));
    let client_b = repo_b.read_config().unwrap().client_id;
    assert!(cli.ok(&a, &["log", "--remote", "-n", "5"], &[]).contains(&format!("sync from {client_b}")));

    // Fork 1: both edit offline; Go pushes first, Rust's sync merges.
    put(&a, "note.md", b"v3 from go\n");
    put(&a, "go-only.txt", b"go\n");
    cli.ok(&a, &["commit", "-m", "go offline"], &[]);
    put(&b, "note.md", b"v3 from rust\n");
    put(&b, "rust-only.txt", b"rust\n");
    repo_b.commit("rust offline").unwrap();
    cli.ok(&a, &["push"], &[]);
    let rep = rb.sync().await.unwrap();
    let SyncAction::Merged { conflicts, pushed: true, .. } = &rep.action else { panic!("{rep:?}") };
    assert_eq!(conflicts, &vec!["note.md".to_string()]);
    assert_eq!(get(&b, "note.md").unwrap(), "v3 from go\n");
    assert_eq!(get(&b, "go-only.txt").unwrap(), "go\n");
    assert_eq!(get(&b, "rust-only.txt").unwrap(), "rust\n");
    let copy = files_of(&b).into_keys().find(|p| p.starts_with("note (modified on ")).expect("conflict copy");
    assert!(copy.ends_with(&format!(" by {client_b}).md")), "{copy}");
    assert_eq!(get(&b, &copy).unwrap(), "v3 from rust\n");
    cli.ok(&a, &["pull"], &[]);
    assert_eq!(files_of(&a), files_of(&b));

    // Fork 2: Rust pushes first, Go's pull merges, Rust fast-forwards to Go's merge.
    put(&a, "go-only.txt", b"go edit\n");
    cli.ok(&a, &["commit", "-m", "go offline 2"], &[]);
    put(&b, "rust-only.txt", b"rust edit\n");
    rb.push().await.unwrap();
    let out = cli.ok(&a, &["pull"], &[]);
    assert!(out.contains("diverged"), "{out}");
    cli.ok(&a, &["push"], &[]);
    let rep = rb.sync().await.unwrap();
    assert!(matches!(rep.action, SyncAction::FastForwarded { .. }), "{rep:?}");
    assert_eq!(files_of(&a), files_of(&b));
    assert_eq!(get(&b, "go-only.txt").unwrap(), "go edit\n");
    assert_eq!(get(&b, "rust-only.txt").unwrap(), "rust edit\n");

    // Nothing in the bucket is plaintext except HEAD, salt and the push-log.
    let raw = s3.remote(url);
    for k in raw.list("").await.unwrap() {
        let data = raw.get(&k).await.unwrap();
        if k.starts_with("blobs/") || k.starts_with("manifests/") {
            assert_eq!(&data[..5], b"SHAS2", "{k} not encrypted");
        } else {
            assert!(k == "HEAD" || k == "salt" || k.starts_with("push-log/"), "unexpected key {k}");
        }
    }
}
