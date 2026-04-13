//go:build linux

package main

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneOrCopyFile tries a reflink (FICLONE) and falls back to a regular copy.
// It creates the destination file; parent dirs must already exist.
func cloneOrCopyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	// Create destination with a tmp name, then rename — atomic against readers.
	tmp := dst + ".tmp"
	df, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	// Try FICLONE. On cross-filesystem or unsupported FS this returns EXDEV/EINVAL/EOPNOTSUPP.
	if err := unix.IoctlFileClone(int(df.Fd()), int(sf.Fd())); err == nil {
		df.Close()
		return os.Rename(tmp, dst)
	}

	// Fallback: stream-copy.
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		os.Remove(tmp)
		return err
	}
	if err := df.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// reflinkCheckout materializes objectPath into workPath, replacing any existing file.
func reflinkCheckout(objectPath, workPath string) error {
	if err := os.MkdirAll(filepath.Dir(workPath), 0o755); err != nil {
		return err
	}
	// Remove any existing file (including read-only).
	if st, err := os.Lstat(workPath); err == nil {
		_ = os.Chmod(workPath, 0o644)
		if st.IsDir() {
			if err := os.RemoveAll(workPath); err != nil {
				return err
			}
		} else {
			if err := os.Remove(workPath); err != nil {
				return err
			}
		}
	}
	return cloneOrCopyFile(objectPath, workPath)
}
