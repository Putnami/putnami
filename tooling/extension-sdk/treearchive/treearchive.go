// Package treearchive stages a directory tree and packs it into a reproducible
// .tar.gz without starting a process, so packaging works the same on every
// platform, Windows included, where neither cp nor a POSIX tar is guaranteed.
//
// A tree it copies or packs holds directories and regular files only. A
// symbolic link, a Windows directory junction or any other special file fails
// the call with an error that names the entry: an archive installed on every
// host carries no links, because an extractor that refuses them, as Putnami's
// does on Windows, would refuse the whole archive.
//
// The archive bytes depend only on the tree's paths, contents and execute
// bits: entries follow filepath.WalkDir's lexical order, names are relative and
// slash-separated, every timestamp is the Unix epoch, ownership is uid and gid 0
// with no user or group name, a directory is 0755, a file is 0755 when any
// execute bit is set and 0644 otherwise, and the gzip header carries no name,
// no time and the unknown OS byte.
package treearchive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.putnami.dev/sdk/extension/dirlink"
)

// epoch is the modification time of every archive entry.
var epoch = time.Unix(0, 0).UTC()

// LinkError reports a link in a tree, which no tree this package handles
// carries.
type LinkError struct {
	// Path is the entry's slash-separated path relative to the tree root.
	Path string
	// Target is the link's target as the file system stores it.
	Target string
}

func (e *LinkError) Error() string {
	return e.Path + " is a symbolic link to " + e.Target
}

// CopyTree copies the directory tree at src into dst, creating dst. skip, when
// not nil, receives each entry's slash-separated path relative to src; when it
// answers true the entry, and everything under a directory, stays out of dst.
// A file keeps its permission bits. A link fails the copy with a *LinkError and
// any other special file with an error naming it.
func CopyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if rel != "." && skip != nil && skip(slashRel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entryInfo(path, slashRel, d)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// WriteTarGz packs the tree at root into a new gzip-compressed tar at
// archivePath, root itself excluded. It removes a partial archive when it
// fails.
func WriteTarGz(archivePath, root string) (err error) {
	file, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			_ = os.Remove(archivePath)
		}
	}()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		return writeEntry(tw, path, filepath.ToSlash(rel), d)
	}); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// entryInfo returns the file information of a directory or regular file entry
// and refuses every other kind.
func entryInfo(path, rel string, d fs.DirEntry) (fs.FileInfo, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	if dirlink.IsLink(path, info) || info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("%s is a link: %w", rel, err)
		}
		return nil, &LinkError{Path: rel, Target: target}
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is neither a regular file nor a directory (%s)", rel, info.Mode().Type())
	}
	return info, nil
}

func writeEntry(tw *tar.Writer, path, rel string, d fs.DirEntry) error {
	info, err := entryInfo(path, rel, d)
	if err != nil {
		return err
	}
	header := &tar.Header{
		Name:    rel,
		ModTime: epoch,
		Mode:    0o644,
	}
	switch {
	case info.IsDir():
		header.Typeflag = tar.TypeDir
		header.Name += "/"
		header.Mode = 0o755
		return tw.WriteHeader(header)
	case info.Mode().Perm()&0o111 != 0:
		header.Mode = 0o755
	}
	header.Typeflag = tar.TypeReg
	header.Size = info.Size()
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("%s changed while it was archived", rel)
	}
	return nil
}

func copyFile(src, dst string, perm fs.FileMode) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only handle
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}
