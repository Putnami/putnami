package runnersource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	runner "go.putnami.dev/protocol/runner"
)

// Materialize creates a new private directory containing exactly the manifest.
// The caller must supply a private parent it owns. The returned path is exposed
// only after all blobs passed verification; failure removes the staging tree.
// There is no overwrite/import operation and no process starts in this layer.
func (s *Store) Materialize(parent string, manifest runner.SourceManifest) (_ string, resultErr error) {
	if err := runner.ValidateSourceManifest(manifest); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "putnami-source-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	createdDirectories := map[string]bool{}
	for _, entry := range manifest.Entries {
		if err := sourceDirectories(root, filepath.Dir(entry.Path), createdDirectories); err != nil {
			return "", err
		}
		if entry.Kind == "symlink" {
			if err := materializeSymlink(root, entry); err != nil {
				return "", err
			}
			continue
		}
		mode := os.FileMode(0o644)
		if entry.Mode == "0755" {
			mode = 0o755
		}
		file, err := root.OpenFile(entry.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return "", err
		}
		copyErr := s.copyTo(file, entry)
		if copyErr == nil {
			copyErr = file.Chmod(mode) // Preserve the admitted mode independently of umask.
		}
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("materialize %s: %w", entry.Path, copyErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return dir, nil
}

func sourceDirectories(root *os.Root, dir string, created map[string]bool) error {
	if dir == "." {
		return nil
	}
	var prefix string
	for _, component := range strings.Split(filepath.ToSlash(dir), "/") {
		prefix = filepath.Join(prefix, component)
		if err := root.Mkdir(prefix, 0o755); err != nil {
			if !os.IsExist(err) {
				return err
			}
			// The staging root starts empty. An existing directory is safe only
			// when this exact manifest spelling created it earlier. Otherwise a
			// destination-specific case or Unicode-normalization alias merged two
			// declared directories.
			if !created[prefix] {
				return fmt.Errorf("destination path %q aliases an earlier manifest directory", filepath.ToSlash(prefix))
			}
		} else {
			created[prefix] = true
		}
		if err := root.Chmod(prefix, 0o755); err != nil {
			return err
		}
	}
	return nil
}
