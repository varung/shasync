package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- init ---

func cmdInit() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := initStore(cwd); err != nil {
		return err
	}
	fmt.Printf("initialized %s/ in %s\n", blobsDir, cwd)
	return nil
}

// --- commit ---

func cmdCommit(args []string) error {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	msg := fs.String("m", "", "commit message")
	_ = fs.Parse(args)

	s, err := findStore()
	if err != nil {
		return err
	}
	parent, err := s.readHead()
	if err != nil {
		return err
	}

	paths, err := s.walkWorkingDir()
	if err != nil {
		return err
	}

	// For unchanged files (same path + size + mtime-ms as parent manifest),
	// we can reuse the parent's SHA without re-hashing. Otherwise hash.
	var parentFiles map[string]*ManifestFile
	if parent != "" {
		pm, err := s.readManifest(parent)
		if err != nil {
			return err
		}
		parentFiles = pm.Files
	}

	files := make(map[string]*ManifestFile, len(paths))
	var mu sync.Mutex
	type job struct{ rel string }
	jobs := make(chan job)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	workers := 8
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				abs := filepath.Join(s.Root, filepath.FromSlash(j.rel))
				st, err := os.Stat(abs)
				if err != nil {
					errs <- err
					return
				}
				updatedAt := st.ModTime().UnixMilli()
				mode := uint32(st.Mode().Perm())
				// Fast path: unchanged from parent manifest.
				if prev, ok := parentFiles[j.rel]; ok && prev.Size == st.Size() && prev.UpdatedAt == updatedAt {
					if s.hasObject(prev.SHA) {
						mu.Lock()
						files[j.rel] = &ManifestFile{SHA: prev.SHA, Size: prev.Size, UpdatedAt: prev.UpdatedAt, Mode: prev.Mode}
						mu.Unlock()
						continue
					}
				}
				sha, size, err := s.ingestFile(abs)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				files[j.rel] = &ManifestFile{SHA: sha, Size: size, UpdatedAt: updatedAt, Mode: mode}
				mu.Unlock()
			}
		}()
	}
	for _, p := range paths {
		jobs <- job{rel: p}
	}
	close(jobs)
	wg.Wait()
	select {
	case e := <-errs:
		return e
	default:
	}

	m := &Manifest{
		Version:   ManifestSchemaVersion,
		ParentSHA: parent,
		CreatedAt: nowUnixMs(),
		Message:   *msg,
		Files:     files,
	}
	sha, _, err := s.writeManifest(m)
	if err != nil {
		return err
	}
	if parent != "" && sha == parent {
		fmt.Println("nothing to commit — working tree matches HEAD")
		return nil
	}
	if err := s.writeHead(sha); err != nil {
		return err
	}
	fmt.Printf("%s  (%d files)\n", sha, len(files))
	return nil
}

// --- checkout ---

func cmdCheckout(args []string) error {
	fs := flag.NewFlagSet("checkout", flag.ExitOnError)
	force := fs.Bool("force", false, "discard uncommitted changes")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: shasync checkout <sha> [--force]")
	}
	target := fs.Arg(0)

	s, err := findStore()
	if err != nil {
		return err
	}
	if !s.hasManifest(target) {
		return fmt.Errorf("manifest %s not found locally (try: shasync pull %s)", target, target)
	}
	tm, err := s.readManifest(target)
	if err != nil {
		return err
	}

	// Safety: if working tree differs from HEAD, refuse unless --force.
	if !*force {
		dirty, err := s.hasUncommittedChanges()
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("working tree has uncommitted changes — commit first or pass --force")
		}
	}

	// Verify all objects are local.
	var missing []string
	for _, mf := range tm.Files {
		if !s.hasObject(mf.SHA) {
			missing = append(missing, mf.SHA)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d blob(s) missing locally (first: %s) — run: shasync pull %s", len(missing), missing[0], target)
	}

	// Determine current set of files (from walk) to know what to remove.
	existing, err := s.walkWorkingDir()
	if err != nil {
		return err
	}
	wantSet := make(map[string]struct{}, len(tm.Files))
	for p := range tm.Files {
		wantSet[p] = struct{}{}
	}
	// Remove files not in target.
	for _, p := range existing {
		if _, ok := wantSet[p]; !ok {
			abs := filepath.Join(s.Root, filepath.FromSlash(p))
			_ = os.Chmod(abs, 0o644)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	// Reflink each target file.
	keys := make([]string, 0, len(tm.Files))
	for p := range tm.Files {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		mf := tm.Files[p]
		abs := filepath.Join(s.Root, filepath.FromSlash(p))
		if err := reflinkCheckout(s.objectPath(mf.SHA), abs); err != nil {
			return fmt.Errorf("checkout %s: %w", p, err)
		}
		if mf.Mode != 0 {
			_ = os.Chmod(abs, os.FileMode(mf.Mode))
		} else {
			_ = os.Chmod(abs, 0o644)
		}
		// Restore mtime so status/commit can identify the file as unchanged
		// by a cheap size+mtime check (no rehashing).
		if mf.UpdatedAt > 0 {
			tt := time.UnixMilli(mf.UpdatedAt)
			_ = os.Chtimes(abs, tt, tt)
		}
	}
	// Prune empty dirs left behind.
	pruneEmptyDirs(s.Root)
	if err := s.writeHead(target); err != nil {
		return err
	}
	fmt.Printf("checked out %s  (%d files)\n", target, len(tm.Files))
	return nil
}

func pruneEmptyDirs(root string) {
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		return nil
	})
	// two passes bottom-up
	for i := 0; i < 4; i++ {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || !info.IsDir() || p == root {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if rel == blobsDir || strings.HasPrefix(rel, blobsDir+string(os.PathSeparator)) {
				return filepath.SkipDir
			}
			entries, _ := os.ReadDir(p)
			if len(entries) == 0 {
				os.Remove(p)
			}
			return nil
		})
	}
}

// hasUncommittedChanges compares the working tree against HEAD manifest using
// path + size + mtime-ms (fast, no hashing). Returns true if anything differs.
func (s *Store) hasUncommittedChanges() (bool, error) {
	head, err := s.readHead()
	if err != nil {
		return false, err
	}
	if head == "" {
		// No HEAD: dirty iff any tracked files exist.
		paths, err := s.walkWorkingDir()
		if err != nil {
			return false, err
		}
		return len(paths) > 0, nil
	}
	hm, err := s.readManifest(head)
	if err != nil {
		return false, err
	}
	paths, err := s.walkWorkingDir()
	if err != nil {
		return false, err
	}
	seen := make(map[string]bool, len(paths))
	for _, rel := range paths {
		seen[rel] = true
		abs := filepath.Join(s.Root, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil {
			return true, nil
		}
		prev, ok := hm.Files[rel]
		if !ok {
			return true, nil
		}
		if prev.Size != st.Size() || prev.UpdatedAt != st.ModTime().UnixMilli() {
			return true, nil
		}
	}
	for p := range hm.Files {
		if !seen[p] {
			return true, nil
		}
	}
	return false, nil
}

// --- status ---

func cmdStatus() error {
	s, err := findStore()
	if err != nil {
		return err
	}
	head, err := s.readHead()
	if err != nil {
		return err
	}
	var headFiles map[string]*ManifestFile
	if head != "" {
		hm, err := s.readManifest(head)
		if err != nil {
			return err
		}
		headFiles = hm.Files
		fmt.Printf("HEAD %s\n", head)
	} else {
		fmt.Println("HEAD (none)")
	}
	paths, err := s.walkWorkingDir()
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	var added, modified, removed []string
	for _, rel := range paths {
		seen[rel] = true
		abs := filepath.Join(s.Root, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil {
			return err
		}
		prev, ok := headFiles[rel]
		if !ok {
			added = append(added, rel)
			continue
		}
		if prev.Size != st.Size() || prev.UpdatedAt != st.ModTime().UnixMilli() {
			// Confirm with hash before declaring modified.
			sha, _, err := hashFile(abs)
			if err != nil {
				return err
			}
			if sha != prev.SHA {
				modified = append(modified, rel)
			}
		}
	}
	for p := range headFiles {
		if !seen[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	for _, p := range added {
		fmt.Printf("  A  %s\n", p)
	}
	for _, p := range modified {
		fmt.Printf("  M  %s\n", p)
	}
	for _, p := range removed {
		fmt.Printf("  D  %s\n", p)
	}
	if len(added)+len(modified)+len(removed) == 0 {
		fmt.Println("clean")
	}
	return nil
}

// --- log ---

func cmdLog(args []string) error {
	fsFlag := flag.NewFlagSet("log", flag.ExitOnError)
	n := fsFlag.Int("n", 20, "max entries")
	_ = fsFlag.Parse(args)
	s, err := findStore()
	if err != nil {
		return err
	}
	sha, err := s.readHead()
	if err != nil {
		return err
	}
	if sha == "" {
		fmt.Println("(no commits)")
		return nil
	}
	for i := 0; i < *n && sha != ""; i++ {
		m, err := s.readManifest(sha)
		if err != nil {
			return err
		}
		fmt.Printf("%s  %d files  %s\n", sha, len(m.Files), m.Message)
		sha = m.ParentSHA
	}
	return nil
}

// --- head ---

func cmdHead() error {
	s, err := findStore()
	if err != nil {
		return err
	}
	h, err := s.readHead()
	if err != nil {
		return err
	}
	if h == "" {
		fmt.Println("(none)")
	} else {
		fmt.Println(h)
	}
	return nil
}

// --- remote ---

func cmdRemote(args []string) error {
	s, err := findStore()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: shasync remote set <url> | shasync remote show")
	}
	switch args[0] {
	case "set":
		if len(args) != 2 {
			return fmt.Errorf("usage: shasync remote set <url>")
		}
		c, err := s.readConfig()
		if err != nil {
			return err
		}
		c.Remote = args[1]
		return s.writeConfig(c)
	case "show":
		c, err := s.readConfig()
		if err != nil {
			return err
		}
		if c.Remote == "" {
			fmt.Println("(no remote set)")
		} else {
			fmt.Println(c.Remote)
		}
		return nil
	default:
		return fmt.Errorf("unknown subcommand: remote %s", args[0])
	}
}

// --- key ---

func cmdKey(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shasync key {gen|show}")
	}
	s, err := findStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "gen":
		if _, err := os.Stat(s.keyPath()); err == nil {
			return fmt.Errorf("%s already exists — remove it explicitly if you mean to rotate (existing blobs will no longer decrypt)", s.keyPath())
		}
		k, err := generateKey()
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.keyPath(), []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("wrote %s (chmod 600)\ncopy this file to other machines out-of-band — it is never sent to the remote\n", s.keyPath())
		return nil
	case "show":
		k, err := s.loadKey()
		if err != nil {
			return err
		}
		if k == nil {
			fmt.Println("(no key configured; push/pull are plaintext)")
			return nil
		}
		fmt.Println(hex.EncodeToString(k))
		return nil
	default:
		return fmt.Errorf("unknown: shasync key %s", args[0])
	}
}

// --- push ---

func cmdPush(ctx context.Context, args []string) error {
	s, err := findStore()
	if err != nil {
		return err
	}
	c, err := s.readConfig()
	if err != nil {
		return err
	}
	if c.Remote == "" {
		return fmt.Errorf("no remote set — run: shasync remote set <url>")
	}
	cryptKey, err := s.loadKey()
	if err != nil {
		return err
	}
	r, err := newRemote(ctx, c.Remote)
	if err != nil {
		return err
	}
	var sha string
	if len(args) == 0 {
		sha, err = s.readHead()
		if err != nil {
			return err
		}
		if sha == "" {
			return fmt.Errorf("no HEAD to push")
		}
	} else {
		sha = args[0]
	}
	m, err := s.readManifest(sha)
	if err != nil {
		return err
	}

	// Walk parent chain locally and upload any manifests the remote doesn't have.
	manifestsToPush := []string{sha}
	for p := m.ParentSHA; p != ""; {
		if !s.hasManifest(p) {
			break
		}
		manifestsToPush = append(manifestsToPush, p)
		pm, _ := s.readManifest(p)
		if pm == nil {
			break
		}
		p = pm.ParentSHA
	}

	// Upload blobs referenced by the tip manifest (parent manifests' blobs were
	// uploaded when they were the tip, if they were pushed before). To be safe
	// in the "first push of a chain" case, also walk parents' files.
	seenBlobs := make(map[string]struct{})
	collect := func(mm *Manifest) {
		for _, f := range mm.Files {
			seenBlobs[f.SHA] = struct{}{}
		}
	}
	collect(m)
	for _, mSha := range manifestsToPush[1:] {
		pm, err := s.readManifest(mSha)
		if err != nil {
			return err
		}
		collect(pm)
	}

	// Upload blobs (with existence check, parallel).
	blobList := make([]string, 0, len(seenBlobs))
	for b := range seenBlobs {
		blobList = append(blobList, b)
	}
	sort.Strings(blobList)
	if err := parallelFor(ctx, 16, blobList, func(ctx context.Context, b string) error {
		return uploadLocalFile(ctx, r, cryptKey, s.objectPath(b), remoteBlobKey(b))
	}); err != nil {
		return err
	}

	// Upload manifests last so a remote-observer never sees a dangling manifest.
	for _, mSha := range manifestsToPush {
		if err := uploadLocalFile(ctx, r, cryptKey, s.manifestPath(mSha), remoteManifestKey(mSha)); err != nil {
			return err
		}
	}

	fmt.Printf("pushed %s  (%d blobs, %d manifests)\n", sha, len(blobList), len(manifestsToPush))
	return nil
}

// --- pull ---

func cmdPull(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: shasync pull <sha>")
	}
	target := args[0]
	s, err := findStore()
	if err != nil {
		return err
	}
	c, err := s.readConfig()
	if err != nil {
		return err
	}
	if c.Remote == "" {
		return fmt.Errorf("no remote set — run: shasync remote set <url>")
	}
	cryptKey, err := s.loadKey()
	if err != nil {
		return err
	}
	r, err := newRemote(ctx, c.Remote)
	if err != nil {
		return err
	}

	// Fetch manifest if missing.
	if !s.hasManifest(target) {
		if err := downloadManifest(ctx, r, s, cryptKey, target); err != nil {
			return err
		}
	}

	// Walk parent chain, pulling as needed.
	toVisit := []string{target}
	blobs := make(map[string]struct{})
	for len(toVisit) > 0 {
		cur := toVisit[0]
		toVisit = toVisit[1:]
		m, err := s.readManifest(cur)
		if err != nil {
			return err
		}
		for _, f := range m.Files {
			blobs[f.SHA] = struct{}{}
		}
		if m.ParentSHA != "" && !s.hasManifest(m.ParentSHA) {
			// Best-effort: try to pull parents too so `log` works locally.
			if err := downloadManifest(ctx, r, s, cryptKey, m.ParentSHA); err == nil {
				toVisit = append(toVisit, m.ParentSHA)
			}
		}
	}

	// Download missing blobs.
	need := make([]string, 0)
	for b := range blobs {
		if !s.hasObject(b) {
			need = append(need, b)
		}
	}
	sort.Strings(need)
	if err := parallelFor(ctx, 16, need, func(ctx context.Context, b string) error {
		return downloadBlob(ctx, r, s, cryptKey, b)
	}); err != nil {
		return err
	}

	fmt.Printf("pulled %s  (%d blobs downloaded) — run: shasync checkout %s\n", target, len(need), target)
	return nil
}

func downloadManifest(ctx context.Context, r Remote, s *Store, cryptKey []byte, sha string) error {
	return downloadAndStore(ctx, r, cryptKey, remoteManifestKey(sha), s.manifestPath(sha), sha)
}

func downloadBlob(ctx context.Context, r Remote, s *Store, cryptKey []byte, sha string) error {
	return downloadAndStore(ctx, r, cryptKey, remoteBlobKey(sha), s.objectPath(sha), sha)
}

// uploadLocalFile uploads localPath under remoteKey. If cryptKey != nil the
// file contents are AES-256-GCM encrypted in memory before upload. Skips the
// PUT if the remote key already exists.
func uploadLocalFile(ctx context.Context, r Remote, cryptKey []byte, localPath, remoteKey string) error {
	ok, err := r.Exists(ctx, remoteKey)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if cryptKey == nil {
		return r.Upload(ctx, remoteKey, f)
	}
	plaintext, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	ct, err := encryptBytes(cryptKey, plaintext)
	if err != nil {
		return err
	}
	return r.Upload(ctx, remoteKey, bytes.NewReader(ct))
}

// downloadAndStore fetches remoteKey, optionally decrypts, verifies SHA
// (when a key is in use — we already have the plaintext in memory), and
// writes to dst atomically at mode 0444.
func downloadAndStore(ctx context.Context, r Remote, cryptKey []byte, remoteKey, dst, sha string) error {
	rc, err := r.Download(ctx, remoteKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if cryptKey == nil {
		return streamToFile(rc, dst, 0o444)
	}
	ct, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	pt, err := decryptBytes(cryptKey, ct)
	if err != nil {
		return fmt.Errorf("%s: %w", remoteKey, err)
	}
	if err := verifyPlaintextSHA(pt, sha); err != nil {
		return fmt.Errorf("%s: %w", remoteKey, err)
	}
	return writeFileAtomic(dst, pt, 0o444)
}
