//! Byte-exact reimplementation of the parts of Go's `encoding/json` that
//! shasync manifests depend on: string escaping (with Go's default HTML
//! escaping of `<`, `>`, `&` and of U+2028/U+2029) and `MarshalIndent`
//! layout with a two-space indent.

const HEX: &[u8; 16] = b"0123456789abcdef";

/// Appends `s` as a JSON string literal exactly as Go's `json.Marshal` would
/// (escapeHTML = true, which is what `Marshal`/`MarshalIndent` use).
pub fn write_string(out: &mut Vec<u8>, s: &str) {
    out.push(b'"');
    for c in s.chars() {
        match c {
            '\\' => out.extend_from_slice(b"\\\\"),
            '"' => out.extend_from_slice(b"\\\""),
            '\u{08}' => out.extend_from_slice(b"\\b"),
            '\u{0c}' => out.extend_from_slice(b"\\f"),
            '\n' => out.extend_from_slice(b"\\n"),
            '\r' => out.extend_from_slice(b"\\r"),
            '\t' => out.extend_from_slice(b"\\t"),
            '<' | '>' | '&' | '\u{00}'..='\u{1f}' => {
                let b = c as u8;
                out.extend_from_slice(b"\\u00");
                out.push(HEX[(b >> 4) as usize]);
                out.push(HEX[(b & 0xf) as usize]);
            }
            '\u{2028}' => out.extend_from_slice(b"\\u2028"),
            '\u{2029}' => out.extend_from_slice(b"\\u2029"),
            _ => {
                let mut buf = [0u8; 4];
                out.extend_from_slice(c.encode_utf8(&mut buf).as_bytes());
            }
        }
    }
    out.push(b'"');
}

/// Writes a newline followed by `depth` levels of two-space indentation.
pub fn newline_indent(out: &mut Vec<u8>, depth: usize) {
    out.push(b'\n');
    for _ in 0..depth {
        out.extend_from_slice(b"  ");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn enc(s: &str) -> String {
        let mut v = Vec::new();
        write_string(&mut v, s);
        String::from_utf8(v).unwrap()
    }

    #[test]
    fn escapes_like_go() {
        assert_eq!(enc("a/b.txt"), r#""a/b.txt""#);
        assert_eq!(enc("<&>"), r#""\u003c\u0026\u003e""#);
        assert_eq!(enc("q\"b\\"), r#""q\"b\\""#);
        assert_eq!(enc("\n\r\t\u{8}\u{c}\u{1}\u{1f}"), r#""\n\r\t\b\f\u0001\u001f""#);
        assert_eq!(enc("\u{7f}é日\u{2028}\u{2029}"), "\"\u{7f}é日\\u2028\\u2029\"");
    }
}
