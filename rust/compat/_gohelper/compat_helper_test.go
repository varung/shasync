// Compat helper for the Rust port (rust/shasync-core/tests/compat_go.rs).
//
// It is NOT part of the Go package's sources: the Rust tests compile it into
// the shasync test binary with `go test -c -overlay=…`, which maps it to
// <shasync>/zz_compat_helper_test.go. The directory name starts with "_" so
// `go test ./...` never sees it. Without SHASYNC_COMPAT_MODE it does nothing.
//
// Every mode reads its inputs from COMPAT_* environment variables and writes
// its result to the file named by COMPAT_OUT.

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func TestCompatHelper(t *testing.T) {
	mode := os.Getenv("SHASYNC_COMPAT_MODE")
	if mode == "" {
		t.Skip("compat helper: SHASYNC_COMPAT_MODE not set")
	}
	out, err := compatRun(mode)
	if err != nil {
		t.Fatalf("compat %s: %v", mode, err)
	}
	if p := os.Getenv("COMPAT_OUT"); p != "" {
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func compatKey() ([]byte, error) { return decodeHexKey(os.Getenv("COMPAT_KEY")) }

func compatIn() ([]byte, error) { return os.ReadFile(os.Getenv("COMPAT_IN")) }

func compatRun(mode string) ([]byte, error) {
	switch mode {
	case "encrypt", "decrypt", "encode", "decode":
		key, err := compatKey()
		if err != nil {
			return nil, err
		}
		in, err := compatIn()
		if err != nil {
			return nil, err
		}
		switch mode {
		case "encrypt":
			return encryptBytes(key, in)
		case "decrypt":
			return decryptBytes(key, in)
		case "encode": // what uploadFile sends
			z, err := compressZstd(in)
			if err != nil {
				return nil, err
			}
			return encryptBytes(key, z)
		default: // what downloadAndStore keeps
			if !isEncryptedObject(in) {
				return nil, fmt.Errorf("not encrypted")
			}
			pt, err := decryptBytes(key, in)
			if err != nil {
				return nil, err
			}
			if isZstd(pt) {
				if pt, err = decompressZstd(pt); err != nil {
					return nil, err
				}
			}
			if err := verifyPlaintextSHA(pt, os.Getenv("COMPAT_SHA")); err != nil {
				return nil, err
			}
			return pt, nil
		}

	case "derive":
		salt, err := hex.DecodeString(os.Getenv("COMPAT_SALT"))
		if err != nil {
			return nil, err
		}
		k := deriveKeyFromPassphrase([]byte(os.Getenv("COMPAT_PASS")), salt)
		return []byte(hex.EncodeToString(k)), nil

	case "canon": // strict parse, then Go's canonical encoding
		in, err := compatIn()
		if err != nil {
			return nil, err
		}
		m, err := parseManifest(in, "compat")
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(b, '\n'), nil

	case "match": // JSON [[pattern, path], ...] → JSON [bool, ...]
		in, err := compatIn()
		if err != nil {
			return nil, err
		}
		var pairs [][2]string
		if err := json.Unmarshal(in, &pairs); err != nil {
			return nil, err
		}
		res := make([]bool, len(pairs))
		for i, p := range pairs {
			res[i] = matchIgnore([]string{p[0]}, p[1])
		}
		return json.Marshal(res)

	case "snapshot": // commit the tree at COMPAT_ROOT; prints the new HEAD
		s := &Store{Root: os.Getenv("COMPAT_ROOT")}
		sha, _, err := snapshotWorkingTree(s, os.Getenv("COMPAT_MSG"))
		return []byte(sha), err

	case "automerge": // autoMerge + writeManifest; prints "<sha>\n<conflict>\n..."
		s := &Store{Root: os.Getenv("COMPAT_ROOT")}
		var ms int64
		fmt.Sscan(os.Getenv("COMPAT_NOW_MS"), &ms)
		m, conflicts, err := autoMerge(s, os.Getenv("COMPAT_CLIENT"), time.UnixMilli(ms),
			os.Getenv("COMPAT_ANC"), os.Getenv("COMPAT_REMOTE"), os.Getenv("COMPAT_LOCAL"))
		if err != nil {
			return nil, err
		}
		sha, _, err := s.writeManifest(m)
		if err != nil {
			return nil, err
		}
		return []byte(strings.Join(append([]string{sha}, conflicts...), "\n")), nil

	case "s3server": // in-memory S3 on localhost until stdin closes
		backend := s3mem.New()
		srv := httptest.NewServer(gofakes3.New(backend).Server())
		defer srv.Close()
		if err := backend.CreateBucket(os.Getenv("COMPAT_BUCKET")); err != nil {
			return nil, err
		}
		fmt.Printf("URL=%s\n", srv.URL)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}
