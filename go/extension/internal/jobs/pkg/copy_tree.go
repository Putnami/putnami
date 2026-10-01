package pkg

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// copyTree copies the tree at src to dst, which must not exist yet. It is the
// copy `cp -R src dst` makes, in Go, so packaging needs no external program on
// any OS:
//   - a directory or a regular file gets the permission bits of its source,
//     less the process umask,
//   - a symbolic link is copied as a link and never followed, src included,
//   - any other file type fails the copy.
//
// A source directory without owner permissions is still filled, as cp fills
// it: each directory is created with every owner permission and receives its
// own bits after its content is copied.
func copyTree(src, dst string) error {
	type directory struct {
		path string
		perm fs.FileMode
	}
	var directories []directory
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsDir():
			// Mkdir applies the umask. The owner bits the source lacks are
			// removed once the directory is filled.
			if err := os.Mkdir(target, mode.Perm()|0o700); err != nil {
				return err
			}
			created, err := os.Lstat(target)
			if err != nil {
				return err
			}
			directories = append(directories, directory{path: target, perm: created.Mode().Perm() & (mode.Perm() | 0o077)})
			return nil
		case mode.IsRegular():
			return copyRegularFile(path, target, mode.Perm())
		default:
			return fmt.Errorf("copy %s: unsupported file type %s", path, mode.Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chmod(directories[i].path, directories[i].perm); err != nil {
			return err
		}
	}
	return nil
}

// copyRegularFile creates target, which must not exist, with perm less the
// process umask, and copies source's bytes into it.
func copyRegularFile(source, target string, perm fs.FileMode) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}
