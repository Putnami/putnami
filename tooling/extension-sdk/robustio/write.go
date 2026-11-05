package robustio

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Op names the step of WriteFile that failed.
type Op string

// The steps of WriteFile, in the order it takes them.
const (
	// OpCreate creates the temporary file in the destination's directory.
	OpCreate Op = "create"
	// OpChmod gives the temporary file its mode.
	OpChmod Op = "chmod"
	// OpWrite writes the data to the temporary file.
	OpWrite Op = "write"
	// OpSync flushes the temporary file to stable storage.
	OpSync Op = "sync"
	// OpClose closes the temporary file.
	OpClose Op = "close"
	// OpRename renames the temporary file over the destination.
	OpRename Op = "rename"
	// OpSyncDir makes the rename durable. When it fails, the destination
	// already holds the new content.
	OpSyncDir Op = "sync directory"
)

// WriteError is a failed WriteFile. Its text is the text of Err, the error of
// the failed step, so a caller that wraps a WriteError reads the same as one
// that wraps Err.
type WriteError struct {
	// Op is the step that failed.
	Op Op
	// Err is the error that step returned.
	Err error
}

func (e *WriteError) Error() string { return e.Err.Error() }

func (e *WriteError) Unwrap() error { return e.Err }

// Landed reports whether the destination holds the new content: only the
// directory sync after the rename failed, so the content is in place and its
// durability is in doubt.
func (e *WriteError) Landed() bool { return e.Op == OpSyncDir }

// WriteFile replaces path with data, so a reader sees the previous file or
// the new one, never a part. It writes a temporary file named
// "."+base+".<random>.tmp" in path's directory, gives it perm whatever the
// umask, and renames it over path through Rename. A failure before the rename
// removes the temporary file and leaves path as it was.
//
// A non-nil syncDir makes the write durable: WriteFile syncs the temporary
// file before the rename and calls syncDir with path's directory after it. A
// nil syncDir syncs nothing.
//
// WriteFile creates no directory. Every error it returns is a *WriteError.
func WriteFile(path string, data []byte, perm fs.FileMode, syncDir func(dir string) error) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return &WriteError{Op: OpCreate, Err: err}
	}
	name := temp.Name()
	fail := func(op Op, err error) error {
		_ = temp.Close()
		_ = os.Remove(name)
		return &WriteError{Op: op, Err: err}
	}
	if err := temp.Chmod(perm); err != nil {
		return fail(OpChmod, err)
	}
	if _, err := temp.Write(data); err != nil {
		return fail(OpWrite, err)
	}
	if syncDir != nil {
		if err := temp.Sync(); err != nil {
			return fail(OpSync, err)
		}
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(name)
		return &WriteError{Op: OpClose, Err: err}
	}
	if err := Rename(name, path); err != nil {
		_ = os.Remove(name)
		return &WriteError{Op: OpRename, Err: err}
	}
	if syncDir != nil {
		if err := syncDir(dir); err != nil {
			return &WriteError{Op: OpSyncDir, Err: err}
		}
	}
	return nil
}
