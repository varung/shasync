package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// errLocalEditsDuringSync means a file changed on disk while a sync was in
// flight, so applying the remote version would have overwritten it. Nothing was
// lost: the next sync cycle commits those edits and merges them (a true
// conflict becomes a "(modified on …)" copy, as usual).
var errLocalEditsDuringSync = errors.New("local files changed during sync; they'll be synced next cycle")

// applyManifestSafely moves the working tree from manifest `fromSHA` — what the
// tree held when this sync snapshotted it — to `target`.
//
// Unlike checkoutTarget(force=true), it is safe to run while someone is editing:
//   - only paths that differ between the two manifests are touched;
//   - each such path is replaced or removed only if it still holds exactly its
//     `fromSHA` content (or is still absent) at that moment;
//   - files that aren't in `fromSHA` — e.g. created since the snapshot — are
//     never deleted.
//
// If a path the update needs was edited, it stops with errLocalEditsDuringSync
// and leaves local HEAD alone. Paths already applied match `target`, which the
// next cycle's merge treats as "same on both sides".
func applyManifestSafely(s *Store, fromSHA, target string) error {
	from := map[string]*ManifestFile{}
	if fromSHA != "" {
		fm, err := s.readManifest(fromSHA)
		if err != nil {
			return err
		}
		from = fm.Files
	}
	tm, err := s.readManifest(target)
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(from)+len(tm.Files))
	seen := map[string]struct{}{}
	for _, files := range []map[string]*ManifestFile{from, tm.Files} {
		for p := range files {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		was, want := from[p], tm.Files[p]
		if sameFile(was, want) {
			continue
		}
		if want != nil && !s.hasObject(want.SHA) {
			return errors.New("blob " + shortSHA(want.SHA) + " missing locally for " + p)
		}
		ok, err := diskMatches(s, p, was)
		if err != nil {
			return err
		}
		if !ok {
			return errLocalEditsDuringSync
		}
		abs := filepath.Join(s.Root, filepath.FromSlash(p))
		if want == nil {
			_ = os.Chmod(abs, 0o644)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := reflinkCheckout(s.objectPath(want.SHA), abs); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if want.Mode != 0 {
			mode = os.FileMode(want.Mode)
		}
		_ = os.Chmod(abs, mode)
		if want.UpdatedAt > 0 {
			t := time.UnixMilli(want.UpdatedAt)
			_ = os.Chtimes(abs, t, t)
		}
	}
	pruneEmptyDirs(s.Root)
	return s.writeHead(target)
}

// diskMatches reports whether the working-tree file at rel still holds f's
// content (f == nil means "should not exist"). Same size and mtime is trusted,
// as in snapshotWorkingTree; otherwise the file is hashed.
func diskMatches(s *Store, rel string, f *ManifestFile) (bool, error) {
	abs := filepath.Join(s.Root, filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return f == nil, nil
	}
	if err != nil {
		return false, err
	}
	if f == nil || st.IsDir() || st.Size() != f.Size {
		return false, nil
	}
	if st.ModTime().UnixMilli() == f.UpdatedAt {
		return true, nil
	}
	sha, _, err := hashFile(abs)
	if err != nil {
		return false, err
	}
	return sha == f.SHA, nil
}

// defaultIgnores never sync, on top of .blobsignore: OS metadata and the
// scratch files editors write next to the files you're editing (vim swap and
// backup files and its "4913" write test, Emacs locks and autosaves, AppleDouble
// files, and temp files from atomic saves, including shasync's own).
var defaultIgnores = []string{
	".DS_Store", "._*", "Icon\r",
	"*.swp", "*.swo", "*.swx", "*~", "4913",
	".#*", "#*#",
	".tmp-*", ".dat.nosync*",
}
