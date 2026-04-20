package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// --- Remote HEAD pointer --------------------------------------------------

// readRemoteHead returns the SHA stored at <prefix>/HEAD, or "" if the file
// does not exist. Remote HEAD is the single mutable pointer that lets a fresh
// clone discover the current tip.
func readRemoteHead(ctx context.Context, r Remote) (string, error) {
	ok, err := r.Exists(ctx, remoteHeadKey)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	rc, err := r.Download(ctx, remoteHeadKey)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(b))
	if len(sha) != 64 {
		return "", fmt.Errorf("remote HEAD malformed: expected 64 hex chars, got %q", sha)
	}
	return sha, nil
}

// writeRemoteHead replaces <prefix>/HEAD with sha + "\n". Plaintext: the SHA
// is already the public object key of the manifest it points to.
func writeRemoteHead(ctx context.Context, r Remote, sha string) error {
	return r.Upload(ctx, remoteHeadKey, strings.NewReader(sha+"\n"))
}

// --- Ancestor queries (local only) ----------------------------------------

// isAncestor reports whether ancestor appears in descendant's parent chain,
// inclusive (isAncestor(x, x) == true). Walks only locally-known manifests;
// if the chain hits a manifest we don't have, returns (false, nil) so callers
// can interpret that as "unknown — treat as diverged and pull more".
func (s *Store) isAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		return false, nil
	}
	cur := descendant
	for cur != "" {
		if cur == ancestor {
			return true, nil
		}
		if !s.hasManifest(cur) {
			return false, nil
		}
		m, err := s.readManifest(cur)
		if err != nil {
			return false, err
		}
		cur = m.ParentSHA
	}
	return false, nil
}

// ancestors returns the full set of SHAs in sha's parent chain (inclusive),
// stopping at the first manifest not present locally.
func (s *Store) ancestors(sha string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	cur := sha
	for cur != "" {
		out[cur] = struct{}{}
		if !s.hasManifest(cur) {
			return out, nil
		}
		m, err := s.readManifest(cur)
		if err != nil {
			return nil, err
		}
		cur = m.ParentSHA
	}
	return out, nil
}

// findCommonAncestor returns the most-recent shared ancestor of a and b.
// Walks b's chain and returns the first SHA also in a's ancestors set.
// Returns "" if none found (or if either side isn't locally complete).
func (s *Store) findCommonAncestor(a, b string) (string, error) {
	ancA, err := s.ancestors(a)
	if err != nil {
		return "", err
	}
	cur := b
	for cur != "" {
		if _, ok := ancA[cur]; ok {
			return cur, nil
		}
		if !s.hasManifest(cur) {
			return "", nil
		}
		m, err := s.readManifest(cur)
		if err != nil {
			return "", err
		}
		cur = m.ParentSHA
	}
	return "", nil
}

// --- Network-aware chain fetch --------------------------------------------

// fetchManifestChain downloads manifest tip and walks parent pointers,
// fetching any manifest we don't already have. Does not touch working tree
// or local HEAD. Safe to call before auto-merge to guarantee the local
// manifest graph is complete enough for ancestor queries.
func fetchManifestChain(ctx context.Context, r Remote, s *Store, cryptKey []byte, tip string) error {
	if tip == "" {
		return nil
	}
	cur := tip
	for cur != "" {
		if !s.hasManifest(cur) {
			if err := downloadManifest(ctx, r, s, cryptKey, cur); err != nil {
				return fmt.Errorf("fetch manifest %s: %w", cur, err)
			}
		}
		m, err := s.readManifest(cur)
		if err != nil {
			return err
		}
		cur = m.ParentSHA
	}
	return nil
}

// --- Auto-merge ------------------------------------------------------------

// autoMerge builds a merge manifest combining remote-tip and local-tip
// changes relative to ancestor. Parent = remote (we continue the remote
// chain; the local chain becomes an orphan by design — see ARCHITECTURE).
//
// For each path in any of the three manifests:
//   - local unchanged vs ancestor  → take remote
//   - remote unchanged vs ancestor → take local
//   - both changed identically     → same SHA, no conflict
//   - both changed differently     → conflict: keep remote at path,
//     add local copy at "<stem> (conflict from <host> <stamp>)<ext>"
//
// Returns the built manifest and the list of conflicted original paths.
func autoMerge(s *Store, hostname string, now time.Time, ancSHA, remoteSHA, localSHA string) (*Manifest, []string, error) {
	anc, err := s.readManifest(ancSHA)
	if err != nil {
		return nil, nil, err
	}
	rem, err := s.readManifest(remoteSHA)
	if err != nil {
		return nil, nil, err
	}
	loc, err := s.readManifest(localSHA)
	if err != nil {
		return nil, nil, err
	}

	paths := make(map[string]struct{})
	for p := range anc.Files {
		paths[p] = struct{}{}
	}
	for p := range rem.Files {
		paths[p] = struct{}{}
	}
	for p := range loc.Files {
		paths[p] = struct{}{}
	}

	out := make(map[string]*ManifestFile)
	var conflicts []string
	stamp := now.Format("2006-01-02 150405")

	for p := range paths {
		a, r, l := anc.Files[p], rem.Files[p], loc.Files[p]
		switch {
		case sameFile(r, l):
			// identical on both sides (may also be deletion)
			if r != nil {
				out[p] = r
			}
		case sameFile(a, l):
			// local didn't touch; take remote
			if r != nil {
				out[p] = r
			}
		case sameFile(a, r):
			// remote didn't touch; take local
			if l != nil {
				out[p] = l
			}
		default:
			// genuine conflict: both sides moved from ancestor, and not to the
			// same place. Keep remote at the original path (or leave it deleted
			// if remote deleted), and stash the local version as a sibling copy.
			conflicts = append(conflicts, p)
			if r != nil {
				out[p] = r
			}
			if l != nil {
				out[conflictCopyPath(p, hostname, stamp)] = l
			}
		}
	}
	sort.Strings(conflicts)

	msg := fmt.Sprintf("auto-merge %s into %s", shortSHA(localSHA), shortSHA(remoteSHA))
	if len(conflicts) > 0 {
		suf := "s"
		if len(conflicts) == 1 {
			suf = ""
		}
		msg = fmt.Sprintf("%s (%d conflict%s)", msg, len(conflicts), suf)
	}

	m := &Manifest{
		Version:   ManifestSchemaVersion,
		ParentSHA: remoteSHA,
		CreatedAt: now.UnixMilli(),
		Message:   msg,
		Files:     out,
	}
	return m, conflicts, nil
}

func sameFile(a, b *ManifestFile) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.SHA == b.SHA
}

// conflictCopyPath inserts "(conflict from <host> <stamp>)" before the file
// extension, so a resolved copy sorts next to the original in a directory
// listing. E.g. "notes/todo.md" → "notes/todo (conflict from laptop 2026-04-20 101530).md".
func conflictCopyPath(p, hostname, stamp string) string {
	dir, base := path.Split(p)
	stem, ext := base, ""
	if dot := strings.LastIndex(base, "."); dot > 0 {
		stem = base[:dot]
		ext = base[dot:]
	}
	return dir + fmt.Sprintf("%s (conflict from %s %s)%s", stem, hostname, stamp, ext)
}

func shortSHA(s string) string {
	if len(s) >= 12 {
		return s[:12]
	}
	return s
}

// --- commit-time linearity check -----------------------------------------

// checkRemoteNotAhead refuses the commit if remote HEAD is ahead of local
// HEAD or has diverged. Tolerates remote being unreachable (prints a warning
// and proceeds) so offline commits Just Work without flags when the network
// is genuinely down.
//
// Policy:
//   - remote HEAD unset           → ok (no baseline yet)
//   - remote HEAD == local HEAD   → ok (at tip)
//   - remote HEAD is ancestor of local HEAD → ok (we are ahead)
//   - local HEAD is ancestor of remote HEAD → refuse (we are behind)
//   - neither is ancestor         → refuse (diverged)
//
// Fetches remote HEAD's manifest chain (metadata only, no blobs) so ancestor
// queries have enough locally to answer authoritatively.
func checkRemoteNotAhead(ctx context.Context, s *Store, remoteURL, localHead string) error {
	r, err := newRemote(ctx, remoteURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't reach remote (%v) — committing offline\n", err)
		return nil
	}
	remoteHead, err := readRemoteHead(ctx, r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't read remote HEAD (%v) — committing offline\n", err)
		return nil
	}
	if remoteHead == "" || remoteHead == localHead {
		return nil
	}
	if localHead == "" {
		return fmt.Errorf("remote has commits but local HEAD is empty — run: shasync pull")
	}
	cryptKey, err := s.loadKey()
	if err != nil {
		return err
	}
	if err := fetchManifestChain(ctx, r, s, cryptKey, remoteHead); err != nil {
		fmt.Fprintf(os.Stderr, "warning: couldn't fetch remote chain (%v) — committing offline\n", err)
		return nil
	}
	// Is remote an ancestor of local? Then local is ahead — allowed.
	ahead, err := s.isAncestor(remoteHead, localHead)
	if err != nil {
		return err
	}
	if ahead {
		return nil
	}
	behind, err := s.isAncestor(localHead, remoteHead)
	if err != nil {
		return err
	}
	if behind {
		return fmt.Errorf("remote has new commits (%s) — run: shasync pull  (or: shasync commit --offline)", shortSHA(remoteHead))
	}
	return fmt.Errorf("remote has diverged (remote=%s, local=%s) — run: shasync pull  (or: shasync commit --offline to fork; push will auto-merge)", shortSHA(remoteHead), shortSHA(localHead))
}
