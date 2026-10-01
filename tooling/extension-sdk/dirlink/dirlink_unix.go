//go:build unix

package dirlink

import (
	"io/fs"
	"os"
	"path/filepath"
)

func create(target, link string) error {
	return os.Symlink(target, link)
}

func replace(target, link, tmp string) error {
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func isLink(_ string, info fs.FileInfo) bool {
	return info.Mode()&fs.ModeSymlink != 0
}

func resolve(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
