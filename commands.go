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
	s, err := findStore()
	if err != nil {
		return err
	}
	id, err := s.ensureClientID()
	if err != nil {
		return err
	}
	fmt.Printf("initialized %s/ in %s (client %s)\n", blobsDir, cwd, id)
	return nil
}

// --- commit ---

// snapshotWorkingTree hashes the working tree into a manifest with the
// given message, writes the manifest if its content differs from local
// HEAD, and advances local HEAD. Returns (sha, committed):
//
//   - (newSHA, true) — a new commit was written (tree had changes).
//   - (parent, false) — nothing changed; local HEAD is untouched.
//
// Implicit commits on push and pull reuse this helper; explicit
// `shasync commit -m "..."` is now a thin wrapper.
func snapshotWorkingTree(s *Store, message string) (string, bool, error) {
	parent, err := s.readHead()
	if err != nil {
		return "", false, err
	}
	paths, err := s.walkWorkingDir()
	if err != nil {
		return "", false, err
	}

	// For unchanged files (same path + size + mtime-ms as parent manifest),
	// we can reuse the parent's SHA without re-hashing. Otherwise hash.
	var parentFiles map[string]*ManifestFile
	if parent != "" {
		pm, err := s.readManifest(parent)
		if err != nil {
			return "", false, err
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
		return "", false, e
	default:
	}

	// No-op detection based on file contents, not full-manifest hash:
	// message + created_at differ between implicit and explicit commits,
	// so comparing SHAs would incorrectly mark a clean tree as changed.
	if parent == "" && len(files) == 0 {
		// Fresh clone, empty working tree — no "ghost empty" commit.
		return "", false, nil
	}
	if parent != "" && manifestFilesEqual(files, parentFiles) {
		return parent, false, nil
	}

	m := &Manifest{
		Version:   ManifestSchemaVersion,
		ParentSHA: parent,
		CreatedAt: nowUnixMs(),
		Message:   message,
		Files:     files,
	}
	sha, _, err := s.writeManifest(m)
	if err != nil {
		return "", false, err
	}
	if err := s.writeHead(sha); err != nil {
		return "", false, err
	}
	return sha, true, nil
}

func manifestFilesEqual(a, b map[string]*ManifestFile) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if av.SHA != bv.SHA || av.Size != bv.Size || av.Mode != bv.Mode {
			return false
		}
	}
	return true
}

// ensureTreeCommitted is the "commit if dirty" step used by push and pull.
// If the working tree differs from local HEAD, it snapshots with an
// auto-generated message ("sync from <clientID>") and advances HEAD;
// otherwise it's a no-op. Returns the (possibly new) HEAD SHA.
func ensureTreeCommitted(s *Store, clientID string) (string, bool, error) {
	msg := fmt.Sprintf("sync from %s", clientID)
	if clientID == "" {
		msg = "sync"
	}
	return snapshotWorkingTree(s, msg)
}

func cmdCommit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("commit", flag.ExitOnError)
	msg := fs.String("m", "", "commit message")
	_ = fs.Parse(args)

	s, err := findStore()
	if err != nil {
		return err
	}
	sha, committed, err := snapshotWorkingTree(s, *msg)
	if err != nil {
		return err
	}
	if !committed {
		fmt.Println("nothing to commit — working tree matches HEAD")
		return nil
	}
	m, err := s.readManifest(sha)
	if err != nil {
		return err
	}
	fmt.Printf("%s  (%d files)\n", sha, len(m.Files))
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
	if err := checkoutTarget(s, target, *force); err != nil {
		return err
	}
	tm, _ := s.readManifest(target)
	fmt.Printf("checked out %s  (%d files)\n", target, len(tm.Files))
	return nil
}

// checkoutTarget materializes manifest `target` into the working tree and
// updates local HEAD. Safety: unless force is true, refuses if the working
// tree has uncommitted changes. Shared with the auto-merge path in push.
func checkoutTarget(s *Store, target string, force bool) error {
	if !s.hasManifest(target) {
		return fmt.Errorf("manifest %s not found locally (try: shasync pull %s)", target, target)
	}
	tm, err := s.readManifest(target)
	if err != nil {
		return err
	}
	if !force {
		dirty, err := s.hasUncommittedChanges()
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("working tree has uncommitted changes — commit first or pass --force")
		}
	}
	var missing []string
	for _, mf := range tm.Files {
		if !s.hasObject(mf.SHA) {
			missing = append(missing, mf.SHA)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d blob(s) missing locally (first: %s) — run: shasync pull %s", len(missing), missing[0], target)
	}

	existing, err := s.walkWorkingDir()
	if err != nil {
		return err
	}
	wantSet := make(map[string]struct{}, len(tm.Files))
	for p := range tm.Files {
		wantSet[p] = struct{}{}
	}
	for _, p := range existing {
		if _, ok := wantSet[p]; !ok {
			abs := filepath.Join(s.Root, filepath.FromSlash(p))
			_ = os.Chmod(abs, 0o644)
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
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
		if mf.UpdatedAt > 0 {
			tt := time.UnixMilli(mf.UpdatedAt)
			_ = os.Chtimes(abs, tt, tt)
		}
	}
	pruneEmptyDirs(s.Root)
	return s.writeHead(target)
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

func cmdLog(ctx context.Context, args []string) error {
	fsFlag := flag.NewFlagSet("log", flag.ExitOnError)
	n := fsFlag.Int("n", 20, "max entries")
	summary := fsFlag.Bool("summary", false, "omit the per-file change list")
	remoteFlag := fsFlag.Bool("remote", false, "start from remote HEAD (fetches manifests as needed)")
	_ = fsFlag.Parse(args)
	s, err := findStore()
	if err != nil {
		return err
	}
	var sha string
	if *remoteFlag {
		c, err := s.readConfig()
		if err != nil {
			return err
		}
		if c.Remote == "" {
			return fmt.Errorf("no remote set — run: shasync remote set <url>")
		}
		r, err := newRemote(ctx, c.Remote)
		if err != nil {
			return err
		}
		sha, err = readRemoteHead(ctx, r)
		if err != nil {
			return fmt.Errorf("read remote HEAD: %w", err)
		}
		if sha == "" {
			fmt.Println("(remote has no HEAD)")
			return nil
		}
		// Fetch manifests for the chain so we can render it, but only as many
		// as `-n` asks for (bounded network cost).
		cryptKey, err := s.loadKey()
		if err != nil {
			return err
		}
		cur := sha
		for i := 0; i < *n && cur != ""; i++ {
			if !s.hasManifest(cur) {
				if err := downloadManifest(ctx, r, s, cryptKey, cur); err != nil {
					return fmt.Errorf("fetch manifest %s: %w", shortSHA(cur), err)
				}
			}
			m, err := s.readManifest(cur)
			if err != nil {
				return err
			}
			cur = m.ParentSHA
		}
	} else {
		sha, err = s.readHead()
		if err != nil {
			return err
		}
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
		date := "(unknown)"
		if m.CreatedAt > 0 {
			date = time.UnixMilli(m.CreatedAt).Format("2006-01-02 15:04:05")
		}
		msg := m.Message
		if msg == "" {
			msg = "(no message)"
		}
		var parentFiles map[string]*ManifestFile
		if m.ParentSHA != "" && s.hasManifest(m.ParentSHA) {
			pm, err := s.readManifest(m.ParentSHA)
			if err != nil {
				return err
			}
			parentFiles = pm.Files
		}
		added, modified, removed := diffManifestFiles(parentFiles, m.Files)
		fmt.Printf("%s  %s  %d files  (+%d ~%d -%d)  %s\n",
			sha, date, len(m.Files), len(added), len(modified), len(removed), msg)
		if !*summary {
			for _, p := range added {
				fmt.Printf("    A  %s\n", p)
			}
			for _, p := range modified {
				fmt.Printf("    M  %s\n", p)
			}
			for _, p := range removed {
				fmt.Printf("    D  %s\n", p)
			}
		}
		sha = m.ParentSHA
	}
	return nil
}

// diffManifestFiles returns paths added / modified / removed going from
// parent -> cur. A nil parent means every path in cur is an addition.
func diffManifestFiles(parent, cur map[string]*ManifestFile) (added, modified, removed []string) {
	for p, f := range cur {
		pf, ok := parent[p]
		if !ok {
			added = append(added, p)
			continue
		}
		if pf.SHA != f.SHA {
			modified = append(modified, p)
		}
	}
	for p := range parent {
		if _, ok := cur[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)
	return
}

// --- info ---

func cmdInfo() error {
	s, err := findStore()
	if err != nil {
		return err
	}
	fmt.Printf("repo:       %s\n", s.Root)

	head, err := s.readHead()
	if err != nil {
		return err
	}
	if head == "" {
		fmt.Println("HEAD:       (none)")
	} else {
		fmt.Printf("HEAD:       %s\n", head)
		if hm, err := s.readManifest(head); err == nil {
			date := ""
			if hm.CreatedAt > 0 {
				date = time.UnixMilli(hm.CreatedAt).Format("2006-01-02 15:04:05")
			}
			msg := hm.Message
			if msg == "" {
				msg = "(no message)"
			}
			fmt.Printf("            %s  %d files  %s\n", date, len(hm.Files), msg)
		}
	}

	c, err := s.readConfig()
	if err != nil {
		return err
	}
	if c.Remote == "" {
		fmt.Println("remote:     (none)")
	} else {
		fmt.Printf("remote:     %s\n", c.Remote)
	}

	envKey := strings.TrimSpace(os.Getenv("SHASYNC_KEY")) != ""
	_, keyStatErr := os.Stat(s.keyPath())
	fileKey := keyStatErr == nil
	switch {
	case envKey && fileKey:
		fmt.Println("encryption: enabled (SHASYNC_KEY env set; .blobs/key also present — env wins)")
	case envKey:
		fmt.Println("encryption: enabled (via SHASYNC_KEY env var)")
	case fileKey:
		fmt.Printf("encryption: enabled (%s)\n", s.keyPath())
	default:
		fmt.Println("encryption: disabled — push/pull are plaintext")
	}

	objCount, objBytes := countStoreDir(s.objectsPath())
	manCount, _ := countStoreDir(s.manifestsPath())
	fmt.Printf("objects:    %d blobs, %s on disk\n", objCount, humanBytes(objBytes))
	fmt.Printf("manifests:  %d\n", manCount)

	if c.Remote != "" {
		dir := filepath.Base(s.Root)
		fmt.Println()
		fmt.Println("to clone on another machine:")
		fmt.Printf("  mkdir %s && cd %s\n", dir, dir)
		fmt.Println("  shasync init")
		fmt.Printf("  shasync remote set %s\n", c.Remote)
		if envKey || fileKey {
			fmt.Println("  shasync key set-passphrase   # or copy .blobs/key from this machine")
		}
		fmt.Println("  shasync pull")
	}
	return nil
}

func countStoreDir(root string) (count int, size int64) {
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		count++
		size += info.Size()
		return nil
	})
	return
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
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

func cmdKey(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shasync key {gen|set-passphrase|show}")
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
	case "set-passphrase":
		if _, err := os.Stat(s.keyPath()); err == nil {
			return fmt.Errorf("%s already exists — remove it first if you mean to replace it", s.keyPath())
		}
		c, err := s.readConfig()
		if err != nil {
			return err
		}
		if c.Remote == "" {
			return fmt.Errorf("configure a remote first (shasync remote set <url>) — the salt is stored in the bucket so all machines derive the same key")
		}
		r, err := newRemote(ctx, c.Remote)
		if err != nil {
			return err
		}
		salt, created, err := fetchOrCreateSalt(ctx, r)
		if err != nil {
			return err
		}
		if created {
			fmt.Println("generated new salt and uploaded to remote")
		} else {
			fmt.Println("using existing salt from remote")
		}
		pass, err := readPassphrase("passphrase: ")
		if err != nil {
			return err
		}
		if len(pass) == 0 {
			return fmt.Errorf("empty passphrase")
		}
		if os.Getenv("SHASYNC_PASSPHRASE") == "" {
			confirm, err := readPassphrase("confirm:    ")
			if err != nil {
				return err
			}
			if !bytes.Equal(pass, confirm) {
				return fmt.Errorf("passphrases do not match")
			}
		}
		fmt.Println("deriving key via Argon2id (~100ms)...")
		k := deriveKeyFromPassphrase(pass, salt)
		if err := writeFileAtomic(s.keyPath(), []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Printf("wrote %s (chmod 600)\n", s.keyPath())
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

// cmdPush publishes local work to the remote. Conceptually "commit + push":
//
//  1. Snapshot the working tree if it differs from local HEAD (implicit
//     commit with an auto-generated message).
//  2. Read remote HEAD. If it's empty, equal to local HEAD, or an ancestor
//     of local HEAD → fast-forward: upload new manifests + blobs, rewrite
//     remote HEAD.
//  3. Anything else (local behind or diverged) → refuse with a message
//     telling the user to run `shasync pull` (which will merge if needed).
//
// `--force` skips the topology check and uploads local HEAD as-is (may
// clobber concurrent work). Required for recovery scenarios.
//
// The merge machinery lives in `pull`, not here — push is outbound-only.
// Its only mutation of local state is the implicit commit; it never
// mutates the working tree.
func cmdPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	force := fs.Bool("force", false, "push local HEAD without topology check (may overwrite remote)")
	_ = fs.Parse(args)

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

	// Implicit commit: snapshot the working tree if it's dirty. This is the
	// step that makes "push" feel like "save everything to the cloud" — you
	// don't have to have run `commit` first.
	if fs.NArg() == 0 {
		if _, committed, err := ensureTreeCommitted(s, c.ClientID); err != nil {
			return err
		} else if committed {
			fmt.Println("(auto-committed working tree before push)")
		}
	}

	local, err := s.readHead()
	if err != nil {
		return err
	}
	var pushSha string
	if fs.NArg() == 0 {
		if local == "" {
			return fmt.Errorf("no HEAD to push")
		}
		pushSha = local
	} else {
		pushSha = fs.Arg(0)
	}

	if *force {
		return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, pushSha)
	}

	remote, err := readRemoteHead(ctx, r)
	if err != nil {
		return fmt.Errorf("read remote HEAD: %w", err)
	}

	// Fast-forward cases: empty remote, pushing the remote tip, or remote is
	// an ancestor of what we're pushing.
	if remote == "" || remote == pushSha {
		return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, pushSha)
	}
	ff, err := s.isAncestor(remote, pushSha)
	if err != nil {
		return err
	}
	if ff {
		return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, pushSha)
	}

	// Not a fast-forward: either we're strictly behind, or we've diverged.
	// Either way the fix is the same — pull first. `pull` will detect the
	// case and, on divergence, auto-merge into the working tree.
	return fmt.Errorf("remote has moved (remote=%s, local=%s) — run: shasync pull", shortSHA(remote), shortSHA(pushSha))
}

// uploadManifestChainAndSetHead uploads every blob referenced by sha's chain
// that the remote doesn't already have, then uploads the manifest chain (tip
// last so a remote observer never sees a dangling parent pointer), then
// atomically updates <prefix>/HEAD.
func uploadManifestChainAndSetHead(ctx context.Context, r Remote, s *Store, cryptKey []byte, sha string) error {
	m, err := s.readManifest(sha)
	if err != nil {
		return err
	}
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
	// Tip last.
	for i := len(manifestsToPush) - 1; i >= 0; i-- {
		mSha := manifestsToPush[i]
		if err := uploadLocalFile(ctx, r, cryptKey, s.manifestPath(mSha), remoteManifestKey(mSha)); err != nil {
			return err
		}
	}
	if err := writeRemoteHead(ctx, r, sha); err != nil {
		return fmt.Errorf("set remote HEAD: %w", err)
	}
	fmt.Printf("pushed %s  (%d blobs, %d manifests; remote HEAD updated)\n", sha, len(blobList), len(manifestsToPush))
	return nil
}

// doMerge is the divergence-handling step of `pull`: builds a merge manifest
// whose parent is the remote tip, downloads remote-side blobs the merge may
// keep at their original paths, and writes the manifest locally. Does NOT
// touch the working tree or local HEAD — the caller's subsequent checkout
// is the step that actually mutates the working tree. Returns the merge SHA.
//
// Caller MUST have already fetched the remote chain (fetchManifestChain) so
// findCommonAncestor can walk both sides.
func doMerge(ctx context.Context, r Remote, s *Store, cryptKey []byte, cfg *Config, local, remote string) (string, error) {
	dirty, err := s.hasUncommittedChanges()
	if err != nil {
		return "", err
	}
	if dirty {
		return "", fmt.Errorf("uncommitted changes — commit or revert before pulling a divergent remote")
	}
	ancSHA, err := s.findCommonAncestor(local, remote)
	if err != nil {
		return "", err
	}
	if ancSHA == "" {
		return "", fmt.Errorf("no common ancestor between local (%s) and remote (%s) — histories are unrelated; use `shasync push --force` if you intend to overwrite the remote", shortSHA(local), shortSHA(remote))
	}
	// The merge manifest can reference remote-only blobs at their original
	// paths; pull them down before we write the manifest.
	if err := fetchBlobsForChain(ctx, r, s, cryptKey, remote, ancSHA); err != nil {
		return "", err
	}

	clientID := cfg.ClientID
	if clientID == "" {
		// Pre-existing repos that predate this field: derive one lazily so
		// the conflict-copy filename still carries a label.
		clientID, err = s.ensureClientID()
		if err != nil {
			return "", err
		}
	}
	mergeManifest, conflicts, err := autoMerge(s, clientID, time.Now(), ancSHA, remote, local)
	if err != nil {
		return "", err
	}
	mergeSHA, _, err := s.writeManifest(mergeManifest)
	if err != nil {
		return "", err
	}

	fmt.Printf("diverged — merging local %s into remote %s\n", shortSHA(local), shortSHA(remote))
	if len(conflicts) > 0 {
		fmt.Printf("  %d conflict(s) — local versions preserved as sibling copies:\n", len(conflicts))
		for _, p := range conflicts {
			fmt.Printf("    C  %s\n", p)
		}
	}
	return mergeSHA, nil
}

// fetchBlobsForChain downloads any blobs referenced by manifests on the
// chain from tip back to (but not including) stopAt, if they aren't already
// local. Used before auto-merge so the merge manifest can reference files
// that only existed on the remote side.
func fetchBlobsForChain(ctx context.Context, r Remote, s *Store, cryptKey []byte, tip, stopAt string) error {
	need := make(map[string]struct{})
	cur := tip
	for cur != "" && cur != stopAt {
		m, err := s.readManifest(cur)
		if err != nil {
			return err
		}
		for _, f := range m.Files {
			if !s.hasObject(f.SHA) {
				need[f.SHA] = struct{}{}
			}
		}
		cur = m.ParentSHA
	}
	list := make([]string, 0, len(need))
	for b := range need {
		list = append(list, b)
	}
	sort.Strings(list)
	return parallelFor(ctx, 16, list, func(ctx context.Context, b string) error {
		return downloadBlob(ctx, r, s, cryptKey, b)
	})
}

// --- pull ---

// cmdPull with no args is the "sync" operation. Conceptually "commit + pull":
//
//  1. Snapshot the working tree if it differs from local HEAD (implicit
//     commit — so uncommitted work becomes part of the local chain and can
//     participate in a merge).
//  2. Read remote HEAD and classify vs (post-commit) local HEAD:
//     - local empty or == remote         → no-op / plain fetch
//     - remote is ancestor-of-local      → nothing to do, local is ahead
//     - local is ancestor-of-remote      → fast-forward: fetch + checkout
//     - otherwise (diverged)             → build a merge manifest whose
//       parent is remote tip; files changed on only one side are taken
//       verbatim; files changed differently on both sides keep the remote
//       version at the original path and save the local version as a
//       sibling "<stem> (modified on <date> by <clientID>)<ext>" file. The
//       merge is checked out into the working tree. A subsequent
//       `shasync push` becomes a plain fast-forward.
//
// cmdPull <sha>: download-only (legacy behavior). No implicit commit, no
// merge, no working-tree mutation. Leaves local HEAD untouched so the
// caller can inspect or `checkout` separately.
func cmdPull(ctx context.Context, args []string) error {
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

	var target string
	checkoutAfter := false
	switch len(args) {
	case 0:
		// Implicit commit: materialize any uncommitted working-tree changes
		// into a local commit *before* we read the remote. This is what makes
		// "pull" feel like "sync" — your in-progress edits become the local
		// side of the three-way merge if it turns out the chains diverged.
		if _, committed, err := ensureTreeCommitted(s, c.ClientID); err != nil {
			return err
		} else if committed {
			fmt.Println("(auto-committed working tree before pull)")
		}
		rh, err := readRemoteHead(ctx, r)
		if err != nil {
			return fmt.Errorf("read remote HEAD: %w", err)
		}
		if rh == "" {
			return fmt.Errorf("remote has no HEAD yet — push from some machine first, or pull <sha> explicitly")
		}
		target = rh
		checkoutAfter = true
	case 1:
		target = args[0]
	default:
		return fmt.Errorf("usage: shasync pull [<sha>]")
	}

	// Divergence detection only applies to the no-arg (sync) mode.
	if checkoutAfter {
		local, _ := s.readHead()
		if local == target {
			fmt.Printf("already up to date (%s)\n", shortSHA(target))
			return nil
		}
		if local != "" {
			// Need the full remote chain locally to answer ancestor queries.
			if err := fetchManifestChain(ctx, r, s, cryptKey, target); err != nil {
				return err
			}
			remoteIsAncestor, err := s.isAncestor(target, local)
			if err != nil {
				return err
			}
			if remoteIsAncestor {
				// Local is strictly ahead. Nothing to pull.
				fmt.Printf("local (%s) is already ahead of remote (%s) — run: shasync push\n", shortSHA(local), shortSHA(target))
				return nil
			}
			localIsAncestor, err := s.isAncestor(local, target)
			if err != nil {
				return err
			}
			if !localIsAncestor {
				// True divergence. Build a merge manifest; the rest of the
				// function will then fetch the merge's blobs + check it out.
				mergeSHA, err := doMerge(ctx, r, s, cryptKey, c, local, target)
				if err != nil {
					return err
				}
				target = mergeSHA
			}
			// localIsAncestor → plain fast-forward; fall through to the
			// download-and-checkout path with the original target.
		}
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

	if !checkoutAfter {
		fmt.Printf("pulled %s  (%d blobs downloaded) — run: shasync checkout %s\n", target, len(need), target)
		return nil
	}

	// Sync flow: also check out and update local HEAD.
	if err := checkoutTarget(s, target, false); err != nil {
		return fmt.Errorf("%w\n(fetched %d blob(s); re-run `shasync checkout %s` once the working tree is clean)", err, len(need), target)
	}
	tm, _ := s.readManifest(target)
	fmt.Printf("pulled %s  (%d blobs downloaded, %d files in working tree)\n", target, len(need), len(tm.Files))
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
