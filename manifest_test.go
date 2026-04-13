package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestManifestCanonicalEncoding pins down the on-disk byte format so that
// any future code producing manifest-shaped bytes (including a v2
// content-defined-chunking reader/writer) can rely on a stable contract.
// Two logically-identical manifests constructed with entries in different
// insertion orders must serialize to byte-identical JSON.
func TestManifestCanonicalEncoding(t *testing.T) {
	a := &Manifest{
		Version:   ManifestSchemaVersion,
		ParentSHA: "abc",
		CreatedAt: 1712419200000,
		Message:   "m",
		Files: map[string]*ManifestFile{
			"b/second.txt": {SHA: "bb", Size: 2, UpdatedAt: 2},
			"a/first.txt":  {SHA: "aa", Size: 1, UpdatedAt: 1},
			"zeta.bin":     {SHA: "zz", Size: 3},
		},
	}
	b := &Manifest{
		Version:   ManifestSchemaVersion,
		ParentSHA: "abc",
		CreatedAt: 1712419200000,
		Message:   "m",
		Files: map[string]*ManifestFile{
			"zeta.bin":     {SHA: "zz", Size: 3},
			"a/first.txt":  {SHA: "aa", Size: 1, UpdatedAt: 1},
			"b/second.txt": {SHA: "bb", Size: 2, UpdatedAt: 2},
		},
	}
	ba, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(ba) != string(bb) {
		t.Fatalf("canonical encoding not stable across insertion order:\nA:\n%s\nB:\n%s", ba, bb)
	}
	// File-path ordering must be byte-lexicographic — this is the contract
	// future CDC implementations rely on to produce deterministic chunk
	// boundaries across machines.
	got := string(ba)
	idxA := strings.Index(got, "\"a/first.txt\"")
	idxB := strings.Index(got, "\"b/second.txt\"")
	idxZ := strings.Index(got, "\"zeta.bin\"")
	if !(idxA < idxB && idxB < idxZ) {
		t.Fatalf("paths not in lex order: %d %d %d\n%s", idxA, idxB, idxZ, got)
	}
}

// TestReaderRejectsUnknownFields guards the forward-compat contract: a
// manifest with a top-level field this build does not know must be
// rejected, not silently interpreted.
func TestReaderRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{"version":1,"created_at":0,"files":{},"surprise_field":"x"}`)
	if _, err := parseManifest(raw, "test"); err == nil {
		t.Fatal("parseManifest accepted manifest with unknown field")
	}
}

// TestReaderRejectsFutureVersion: a v2 manifest must not be silently
// misread by this v1 build.
func TestReaderRejectsFutureVersion(t *testing.T) {
	raw := []byte(`{"version":2,"created_at":0,"chunks":["sha1","sha2"]}`)
	if _, err := parseManifest(raw, "test"); err == nil {
		t.Fatal("parseManifest accepted unknown schema version")
	}
}

// TestReaderRejectsChunksInV1: v1 with a populated chunks field is a
// contract violation and must fail.
func TestReaderRejectsChunksInV1(t *testing.T) {
	raw := []byte(`{"version":1,"created_at":0,"chunks":["a","b"]}`)
	if _, err := parseManifest(raw, "test"); err == nil {
		t.Fatal("parseManifest accepted v1 with populated chunks")
	}
}
