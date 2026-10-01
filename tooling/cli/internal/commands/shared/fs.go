package shared

import (
	"fmt"
	"os"
	"path/filepath"
)

// FileExists reports whether path exists (and is statable). It does not
// distinguish a regular file from a directory: callers that care about the
// difference check os.FileInfo themselves.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// AtomicWriteFile writes data to path via a same-directory temp file and
// rename(2), so a reader never observes a partially written artifact and a
// failed write never truncates the existing good file.
func AtomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".contracts-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp artifact in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the temp has been renamed into place
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp artifact: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp artifact: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("promote artifact to %s: %w", path, err)
	}
	return nil
}
