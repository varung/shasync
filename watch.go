package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

func cmdWatch(ctx context.Context, args []string) error {
	fsFlag := flag.NewFlagSet("watch", flag.ExitOnError)
	debounce := fsFlag.Duration("debounce", 2*time.Second, "quiet period after last file change before syncing")
	poll := fsFlag.Duration("poll", 30*time.Second, "interval to poll remote for changes from other machines")
	_ = fsFlag.Parse(args)

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

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer watcher.Close()

	patterns, err := s.readIgnore()
	if err != nil {
		return err
	}

	if err := watchTree(watcher, s.Root, patterns); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("watching %s  (debounce: %s, poll: %s)\n", s.Root, *debounce, *poll)

	syncAndLog(ctx, s, r, cryptKey, c)

	debounceCh := make(chan struct{}, 1)
	var debounceTimer *time.Timer
	pollTicker := time.NewTicker(*poll)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopping watch")
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if shouldIgnoreEvent(s, ev.Name, patterns) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = watchTree(watcher, ev.Name, patterns)
				}
			}
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(*debounce, func() {
				select {
				case debounceCh <- struct{}{}:
				default:
				}
			})

		case <-debounceCh:
			syncAndLog(ctx, s, r, cryptKey, c)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)

		case <-pollTicker.C:
			syncAndLog(ctx, s, r, cryptKey, c)
		}
	}
}

// syncOnce performs a full sync cycle: commit local changes, pull (with
// auto-merge if diverged), then push.
func syncOnce(ctx context.Context, s *Store, r Remote, cryptKey []byte, c *Config) error {
	sha, committed, err := ensureTreeCommitted(s, c.ClientID)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	remote, err := readRemoteHead(ctx, r)
	if err != nil {
		return fmt.Errorf("read remote HEAD: %w", err)
	}

	if remote == "" {
		if sha == "" {
			return nil
		}
		return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, sha)
	}

	local, err := s.readHead()
	if err != nil {
		return err
	}
	if local == "" {
		local = sha
	}

	if local == remote {
		if committed {
			return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, local)
		}
		return nil
	}

	if err := fetchManifestChain(ctx, r, s, cryptKey, remote); err != nil {
		return fmt.Errorf("fetch chain: %w", err)
	}

	remoteIsAnc, err := s.isAncestor(remote, local)
	if err != nil {
		return err
	}
	if remoteIsAnc {
		return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, local)
	}

	localIsAnc, err := s.isAncestor(local, remote)
	if err != nil {
		return err
	}
	if localIsAnc {
		return fetchAndCheckout(ctx, r, s, cryptKey, remote)
	}

	// Diverged: merge, checkout, push.
	mergeSHA, err := doMerge(ctx, r, s, cryptKey, c, local, remote)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if err := fetchAndCheckout(ctx, r, s, cryptKey, mergeSHA); err != nil {
		return err
	}
	return uploadManifestChainAndSetHead(ctx, r, s, cryptKey, mergeSHA)
}

func fetchAndCheckout(ctx context.Context, r Remote, s *Store, cryptKey []byte, target string) error {
	if !s.hasManifest(target) {
		if err := downloadManifest(ctx, r, s, cryptKey, target); err != nil {
			return err
		}
	}
	m, err := s.readManifest(target)
	if err != nil {
		return err
	}
	var need []string
	for _, f := range m.Files {
		if !s.hasObject(f.SHA) {
			need = append(need, f.SHA)
		}
	}
	if err := parallelFor(ctx, 16, need, func(ctx context.Context, b string) error {
		return downloadBlob(ctx, r, s, cryptKey, b)
	}); err != nil {
		return err
	}
	return checkoutTarget(s, target, true)
}

func syncAndLog(ctx context.Context, s *Store, r Remote, cryptKey []byte, c *Config) {
	before, _ := s.readHead()
	err := syncOnce(ctx, s, r, cryptKey, c)
	after, _ := s.readHead()
	stamp := time.Now().Format("15:04:05")
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "%s  error: %v\n", stamp, err)
		return
	}
	if before == after {
		fmt.Printf("%s  up to date (%s)\n", stamp, shortSHA(after))
	} else {
		fmt.Printf("%s  synced %s → %s\n", stamp, shortSHA(before), shortSHA(after))
	}
}

func watchTree(w *fsnotify.Watcher, root string, patterns []string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == blobsDir || strings.HasPrefix(rel, blobsDir+"/") {
			return filepath.SkipDir
		}
		if rel != "." && matchIgnore(patterns, rel+"/") {
			return filepath.SkipDir
		}
		_ = w.Add(p)
		return nil
	})
}

func shouldIgnoreEvent(s *Store, path string, patterns []string) bool {
	if !strings.HasPrefix(path, s.Root) {
		return true
	}
	rel, err := filepath.Rel(s.Root, path)
	if err != nil {
		return true
	}
	rel = filepath.ToSlash(rel)
	if rel == blobsDir || strings.HasPrefix(rel, blobsDir+"/") {
		return true
	}
	return matchIgnore(patterns, rel)
}
