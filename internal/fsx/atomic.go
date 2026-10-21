// Package fsx provides crash-safe file writes and inter-process locking.
package fsx

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data so readers see the old or the new
// content, never a partial write. The temp file lives beside the destination
// so the rename stays on one filesystem; mode is applied before any bytes land.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file for %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
