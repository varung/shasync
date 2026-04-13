package main

// The current supported manifest schema version. A reader will refuse any
// manifest with a version it does not understand — future format additions
// (e.g. content-defined chunking, see help.go / design notes) will bump this.
const ManifestSchemaVersion = 1

// Manifest is the versioned snapshot record. Its content-addressed SHA *is*
// the version identifier.
//
// Canonical encoding (part of the on-disk contract — do not change casually):
//
//   - Serialized as JSON with encoding/json defaults: UTF-8, no HTML escaping
//     concerns for this schema, no trailing whitespace beyond one final '\n'.
//   - Top-level fields appear in struct-declaration order.
//   - Map keys inside `files` are sorted byte-lexicographically (the default
//     for encoding/json). File paths are stored with forward slashes.
//   - Integer fields are emitted as JSON integers, not floats.
//   - Empty/zero fields with `omitempty` are omitted entirely; readers treat
//     absence as the zero value.
//
// Two machines with byte-identical working trees and matching parent/message
// must produce byte-identical manifest bytes, and therefore the same SHA.
//
// Forward-compat: readers reject unknown top-level fields (see readManifest)
// so a future v2 manifest cannot be silently misinterpreted by a v1 reader.
// The `Chunks` field is reserved for a future content-defined-chunking
// format (v2): when populated, `Files` will be absent and the manifest's
// logical content is the concatenation of the chunks' entries.
type Manifest struct {
	Version   int                      `json:"version"`
	ParentSHA string                   `json:"parent_sha,omitempty"`
	CreatedAt int64                    `json:"created_at"`
	Message   string                   `json:"message,omitempty"`
	Files     map[string]*ManifestFile `json:"files,omitempty"`
	// Chunks is reserved for a future chunked manifest format. Always empty
	// in v1; a v1 writer MUST NOT populate it and a v1 reader rejects any
	// manifest whose version it does not understand before looking at this
	// field. Declared here so the JSON schema is stable.
	Chunks []string `json:"chunks,omitempty"`
}

type ManifestFile struct {
	SHA       string `json:"sha"`
	Size      int64  `json:"size"`
	UpdatedAt int64  `json:"updated_at,omitempty"` // unix ms
	Mode      uint32 `json:"mode,omitempty"`       // unix perm bits; 0 means default
}
