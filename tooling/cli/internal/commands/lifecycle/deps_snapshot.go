package lifecycle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// writeFileAtomically replaces path with data and mode through a temporary
// file in the same directory, so path holds either its old content or data,
// never a part of data.
func writeFileAtomically(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".putnami-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// fileSnapshot holds the content of files a command is about to edit, so a
// failed edit puts each one back as it was.
type fileSnapshot struct {
	files []snapshotFile
}

// snapshotFile is one file of a fileSnapshot. A file that did not exist is
// removed again on restore.
type snapshotFile struct {
	path    string
	data    []byte
	mode    fs.FileMode
	existed bool
}

// snapshotFiles records the content of each path.
func snapshotFiles(paths ...string) (*fileSnapshot, error) {
	snapshot := &fileSnapshot{}
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			snapshot.files = append(snapshot.files, snapshotFile{path: path})
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshot.files = append(snapshot.files, snapshotFile{path: path, data: data, mode: info.Mode().Perm(), existed: true})
	}
	return snapshot, nil
}

// restoreAfter puts every recorded file back as it was and returns cause,
// stating that the files are back, or joined with each file it could not put
// back.
func (s *fileSnapshot) restoreAfter(cause error) error {
	failures := []error{cause}
	for _, file := range s.files {
		var err error
		if file.existed {
			err = writeFileAtomically(file.path, file.data, file.mode)
		} else if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = removeErr
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("restore %s: %w", file.path, err))
		}
	}
	if len(failures) == 1 {
		return fmt.Errorf("%w; every file the command edited is back as it was", cause)
	}
	return errors.Join(failures...)
}
