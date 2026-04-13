//go:build !linux && !darwin

package main

import (
	"io"
	"os"
	"path/filepath"
)

// cloneOrCopyFile on unsupported platforms just does a regular copy.
func cloneOrCopyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		os.Remove(dst)
		return err
	}
	return df.Close()
}

func reflinkCheckout(objectPath, workPath string) error {
	if err := os.MkdirAll(filepath.Dir(workPath), 0o755); err != nil {
		return err
	}
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
