package clicore

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path atomically: it stages the bytes in a
// temp file in the SAME directory as the target, fsyncs and closes it, then
// rename(2)s it over the target. os.Rename over an existing file is atomic on
// POSIX, so a concurrent reader always observes either the old complete file
// or the new complete file — never a truncated one — no matter when the writer
// is killed. This mirrors internal/distributioncli WriteRegistriesState so both
// writers share one implementation.
//
// clicore is intentionally pure-stdlib (compiles under GOWORK=off), so this
// uses only os/path/filepath — no framework imports.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Stage in the same directory so the rename is same-filesystem and atomic.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup; a no-op once the rename below succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
