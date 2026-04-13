package main

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// walkWorkingDir walks s.Root and returns a sorted list of relative file paths
// (forward-slash separated), skipping .blobs/ and anything matched by .blobsignore.
func (s *Store) walkWorkingDir() ([]string, error) {
	patterns, err := s.readIgnore()
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == s.Root {
			return nil
		}
		rel, _ := filepath.Rel(s.Root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == blobsDir {
				return filepath.SkipDir
			}
			if matchIgnore(patterns, rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip symlinks — keep v1 simple.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if matchIgnore(patterns, rel) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) readIgnore() ([]string, error) {
	f, err := os.Open(filepath.Join(s.Root, ignoreFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pats []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pats = append(pats, line)
	}
	return pats, sc.Err()
}

// matchIgnore uses path.Match against each pattern and also against every path
// suffix (so "node_modules" matches "a/b/node_modules/c").
func matchIgnore(patterns []string, relPath string) bool {
	if len(patterns) == 0 {
		return false
	}
	parts := strings.Split(relPath, "/")
	for _, pat := range patterns {
		trailingSlash := strings.HasSuffix(pat, "/")
		p := strings.TrimSuffix(pat, "/")
		for i := 0; i < len(parts); i++ {
			seg := strings.Join(parts[i:], "/")
			if ok, _ := path.Match(p, seg); ok {
				return true
			}
			if ok, _ := path.Match(p, parts[i]); ok {
				return true
			}
		}
		_ = trailingSlash
	}
	return false
}
