// Package fileutil contains helpers for safely persisting local application state.
package fileutil

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temporary file beside path and atomically
// renames it into place. A failed or interrupted write therefore cannot leave a
// partially written destination file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		_ = os.Remove(tempPath)
	}()

	if err = file.Chmod(perm); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err = io.Copy(file, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	file = nil

	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace destination file: %w", err)
	}

	// Best effort: syncing the directory makes the rename durable on filesystems
	// that support directory fsync. Some supported platforms do not.
	if directory, openErr := os.Open(dir); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
