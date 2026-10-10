//! Ignore rules: a faithful port of Go's `path.Match` plus shasync's
//! `matchIgnore`, `readIgnore` and `defaultIgnores`.

use std::path::Path;

/// Patterns that never sync, on top of `.blobsignore` (Go: `defaultIgnores`).
pub const DEFAULT_IGNORES: &[&str] = &[
    ".DS_Store", "._*", "Icon\r", "*.swp", "*.swo", "*.swx", "*~", "4913", ".#*", "#*#", ".tmp-*", ".dat.nosync*",
];

pub const IGNORE_FILE: &str = ".blobsignore";

/// Default ignores followed by the non-empty, non-`#` lines of
/// `<root>/.blobsignore` (each line whitespace-trimmed).
pub fn read_ignore_patterns(root: &Path) -> std::io::Result<Vec<String>> {
    let mut pats: Vec<String> = DEFAULT_IGNORES.iter().map(|s| s.to_string()).collect();
    match std::fs::read(root.join(IGNORE_FILE)) {
        Ok(b) => {
            let text = String::from_utf8_lossy(&b);
            for line in text.split('\n') {
                let line = line.trim();
                if line.is_empty() || line.starts_with('#') {
                    continue;
                }
                pats.push(line.to_string());
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    Ok(pats)
}

/// Go's `matchIgnore`: each pattern (trailing `/` stripped) is matched against
/// every path suffix and every single segment of `rel_path`. Directories are
/// tested with a trailing `/` appended.
pub fn match_ignore(patterns: &[String], rel_path: &str) -> bool {
    if patterns.is_empty() {
        return false;
    }
    let parts: Vec<&str> = rel_path.split('/').collect();
    for pat in patterns {
        let p = pat.strip_suffix('/').unwrap_or(pat);
        for i in 0..parts.len() {
            let seg = parts[i..].join("/");
            if path_match(p, &seg) || path_match(p, parts[i]) {
                return true;
            }
        }
    }
    false
}

/// Go's `path.Match`, returning `false` on a malformed pattern (Go returns
/// `false, ErrBadPattern`, and shasync ignores the error).
pub fn path_match(pattern: &str, name: &str) -> bool {
    matches!(go_match(pattern.as_bytes(), name.as_bytes()), Ok(true))
}

struct BadPattern;

fn go_match(mut pattern: &[u8], mut name: &[u8]) -> Result<bool, BadPattern> {
    'pattern: while !pattern.is_empty() {
        let (star, chunk, rest) = scan_chunk(pattern);
        pattern = rest;
        if star && chunk.is_empty() {
            return Ok(!name.contains(&b'/'));
        }
        let (t, ok, err) = match_chunk(chunk, name);
        if ok && (t.is_empty() || !pattern.is_empty()) {
            name = t;
            continue;
        }
        if err {
            return Err(BadPattern);
        }
        if star {
            let mut i = 0;
            while i < name.len() && name[i] != b'/' {
                let (t, ok, err) = match_chunk(chunk, &name[i + 1..]);
                if ok {
                    if pattern.is_empty() && !t.is_empty() {
                        i += 1;
                        continue;
                    }
                    name = t;
                    continue 'pattern;
                }
                if err {
                    return Err(BadPattern);
                }
                i += 1;
            }
        }
        while !pattern.is_empty() {
            let (_, chunk, rest) = scan_chunk(pattern);
            pattern = rest;
            if match_chunk(chunk, b"").2 {
                return Err(BadPattern);
            }
        }
        return Ok(false);
    }
    Ok(name.is_empty())
}

fn scan_chunk(mut pattern: &[u8]) -> (bool, &[u8], &[u8]) {
    let mut star = false;
    while !pattern.is_empty() && pattern[0] == b'*' {
        pattern = &pattern[1..];
        star = true;
    }
    let mut inrange = false;
    let mut i = 0;
    while i < pattern.len() {
        match pattern[i] {
            b'\\' => {
                if i + 1 < pattern.len() {
                    i += 1;
                }
            }
            b'[' => inrange = true,
            b']' => inrange = false,
            b'*' if !inrange => break,
            _ => {}
        }
        i += 1;
    }
    (star, &pattern[..i], &pattern[i..])
}

/// Decodes one rune like Go's `utf8.DecodeRune`: invalid input yields
/// (U+FFFD, 1); empty input yields (U+FFFD, 0).
fn decode_rune(s: &[u8]) -> (u32, usize) {
    if s.is_empty() {
        return (0xFFFD, 0);
    }
    let n = match s[0] {
        0x00..=0x7F => return (s[0] as u32, 1),
        0xC2..=0xDF => 2,
        0xE0..=0xEF => 3,
        0xF0..=0xF4 => 4,
        _ => return (0xFFFD, 1),
    };
    if s.len() < n {
        return (0xFFFD, 1);
    }
    match std::str::from_utf8(&s[..n]) {
        Ok(st) => (st.chars().next().unwrap() as u32, n),
        Err(_) => (0xFFFD, 1),
    }
}

/// Returns (rest, ok, bad_pattern).
fn match_chunk<'a>(mut chunk: &[u8], mut s: &'a [u8]) -> (&'a [u8], bool, bool) {
    let mut failed = false;
    while !chunk.is_empty() {
        if !failed && s.is_empty() {
            failed = true;
        }
        match chunk[0] {
            b'[' => {
                let mut r: u32 = 0;
                if !failed {
                    let (rr, n) = decode_rune(s);
                    r = rr;
                    s = &s[n..];
                }
                chunk = &chunk[1..];
                let mut negated = false;
                if !chunk.is_empty() && chunk[0] == b'^' {
                    negated = true;
                    chunk = &chunk[1..];
                }
                let mut matched = false;
                let mut nrange = 0;
                loop {
                    if !chunk.is_empty() && chunk[0] == b']' && nrange > 0 {
                        chunk = &chunk[1..];
                        break;
                    }
                    let lo = match get_esc(chunk) {
                        Some((lo, rest)) => {
                            chunk = rest;
                            lo
                        }
                        None => return (b"", false, true),
                    };
                    let mut hi = lo;
                    if chunk[0] == b'-' {
                        match get_esc(&chunk[1..]) {
                            Some((h, rest)) => {
                                hi = h;
                                chunk = rest;
                            }
                            None => return (b"", false, true),
                        }
                    }
                    if lo <= r && r <= hi {
                        matched = true;
                    }
                    nrange += 1;
                }
                if matched == negated {
                    failed = true;
                }
            }
            b'?' => {
                if !failed {
                    if s[0] == b'/' {
                        failed = true;
                    }
                    let (_, n) = decode_rune(s);
                    s = &s[n..];
                }
                chunk = &chunk[1..];
            }
            c => {
                let lit = if c == b'\\' {
                    chunk = &chunk[1..];
                    if chunk.is_empty() {
                        return (b"", false, true);
                    }
                    chunk[0]
                } else {
                    c
                };
                if !failed {
                    if lit != s[0] {
                        failed = true;
                    }
                    s = &s[1..];
                }
                chunk = &chunk[1..];
            }
        }
    }
    if failed {
        (b"", false, false)
    } else {
        (s, true, false)
    }
}

/// Go's `getEsc`. `None` = bad pattern. On success the returned rest is
/// guaranteed non-empty (Go errors otherwise).
fn get_esc(mut chunk: &[u8]) -> Option<(u32, &[u8])> {
    if chunk.is_empty() || chunk[0] == b'-' || chunk[0] == b']' {
        return None;
    }
    if chunk[0] == b'\\' {
        chunk = &chunk[1..];
        if chunk.is_empty() {
            return None;
        }
    }
    let (r, n) = decode_rune(chunk);
    if r == 0xFFFD && n == 1 {
        return None;
    }
    let rest = &chunk[n..];
    if rest.is_empty() {
        return None;
    }
    Some((r, rest))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn go_path_match_cases() {
        // Cases from Go's path/match_test.go.
        let cases: &[(&str, &str, bool)] = &[
            ("abc", "abc", true),
            ("*", "abc", true),
            ("*c", "abc", true),
            ("a*", "a", true),
            ("a*", "abc", true),
            ("a*", "ab/c", false),
            ("a*/b", "abc/b", true),
            ("a*/b", "a/c/b", false),
            ("a*b*c*d*e*/f", "axbxcxdxe/f", true),
            ("a*b*c*d*e*/f", "axbxcxdxexxx/f", true),
            ("a*b*c*d*e*/f", "axbxcxdxe/xxx/f", false),
            ("a*b*c*d*e*/f", "axbxcxdxexxx/fff", false),
            ("a*b?c*x", "abxbbxdbxebxczzx", true),
            ("a*b?c*x", "abxbbxdbxebxczzy", false),
            ("ab[c]", "abc", true),
            ("ab[b-d]", "abc", true),
            ("ab[e-g]", "abc", false),
            ("ab[^c]", "abc", false),
            ("ab[^b-d]", "abc", false),
            ("ab[^e-g]", "abc", true),
            ("a\\*b", "a*b", true),
            ("a\\*b", "ab", false),
            ("a?b", "a☺b", true),
            ("a[^a]b", "a☺b", true),
            ("a???b", "a☺b", false),
            ("a[^a][^a][^a]b", "a☺b", false),
            ("[a-ζ]*", "α", true),
            ("*[a-ζ]", "A", false),
            ("a?b", "a/b", false),
            ("a*b", "a/b", false),
            ("[\\]a]", "]", true),
            ("[\\-]", "-", true),
            ("[x\\-]", "x", true),
            ("[x\\-]", "-", true),
            ("[x\\-]", "z", false),
            ("[\\-x]", "x", true),
            ("[\\-x]", "-", true),
            ("[\\-x]", "a", false),
            ("[]a]", "]", false),
            ("[-]", "-", false),
            ("[x-]", "x", false),
            ("[x-]", "-", false),
            ("[x-]", "z", false),
            ("[-x]", "x", false),
            ("[-x]", "-", false),
            ("[-x]", "a", false),
            ("\\", "a", false),
            ("[a-b-c]", "a", false),
            ("[", "a", false),
            ("[^", "a", false),
            ("[^bc", "a", false),
            ("a[", "a", false),
            ("a[", "ab", false),
            ("a[", "x", false),
            ("a/b[", "x", false),
            ("*x", "xxx", true),
        ];
        for (p, n, want) in cases {
            assert_eq!(path_match(p, n), *want, "Match({p:?}, {n:?})");
        }
    }

    #[test]
    fn ignore_semantics() {
        let pats: Vec<String> = DEFAULT_IGNORES.iter().map(|s| s.to_string()).chain(["node_modules/".to_string(), "build".to_string()]).collect();
        assert!(match_ignore(&pats, ".DS_Store"));
        assert!(match_ignore(&pats, "a/b/.DS_Store"));
        assert!(match_ignore(&pats, "note.md~"));
        assert!(match_ignore(&pats, "x/node_modules/"));
        assert!(match_ignore(&pats, "x/node_modules/y.js"));
        assert!(match_ignore(&pats, "build/"));
        assert!(!match_ignore(&pats, "notes/todo.md"));
        assert!(!match_ignore(&pats, "builder.txt"));
    }
}
