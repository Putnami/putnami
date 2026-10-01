package pkg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Staging times and host ownership are not release inputs. Keep archive bytes
// stable across independent staging directories and retries of one version.
//
// executables are the slash-separated paths, relative to root, of the programs
// the project declares. Each one that is a regular file is archived executable
// by every class that may read it, whatever mode it has on this machine's
// disk, so a machine whose filesystem records no execute bit packages the same
// archive. Every other entry keeps the mode archiveMode gives it.
func writeReproducibleArchive(destination, root string, executables []string) error {
	return writeArchiveOn(runtime.GOOS, destination, root, executables)
}

// writeArchiveOn is writeReproducibleArchive on a host running goos.
func writeArchiveOn(goos, destination, root string, executables []string) (err error) {
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	gz := gzip.NewWriter(file)
	defer func() { err = errors.Join(err, gz.Close()) }()
	tw := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tw.Close()) }()
	declared := make(map[string]bool, len(executables))
	for _, executable := range executables {
		declared[executable] = true
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return writeArchiveEntry(tw, goos, root, path, entry, declared)
	})
}

// readBits are the read permission bits of owner, group and others. Shifted
// right by two they are the execute bits of the same classes.
const readBits = 0o444

// archiveMode is the mode the archive records for an entry whose tar header,
// made from its disk mode, has mode headerMode, on a host running goos. A Unix
// host records the disk mode. A Windows filesystem records no permission but
// read-only, which Go reports as 0444, and everything else as 0666, or 0777 for
// a directory; there an entry gets the mode a Unix host with umask 022 gives
// it: 0755 for a directory, 0644 for a file, 0444 for a read-only file and
// 0777 for a symbolic link, as Linux reports links.
func archiveMode(goos string, info fs.FileInfo, headerMode int64) int64 {
	if goos != "windows" {
		return headerMode
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return 0o755
	case mode&fs.ModeSymlink != 0:
		return 0o777
	case mode.Perm()&0o222 == 0:
		return 0o444
	default:
		return 0o644
	}
}

// archiveLinkname is the target the archive records for a symbolic link whose
// target on disk is link, on a host running goos. Windows stores a link target
// with backslashes, whatever separator created it; the archive records it with
// slashes, as a Unix host does. A Unix host records the target verbatim, where
// a backslash is an ordinary file name character.
func archiveLinkname(goos, link string) string {
	if goos != "windows" {
		return link
	}
	return strings.ReplaceAll(link, `\`, "/")
}

func writeArchiveEntry(tw *tar.Writer, goos, root, path string, entry fs.DirEntry, executables map[string]bool) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return err
		}
	} else if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported archive entry %s", path)
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Linkname = archiveLinkname(goos, header.Linkname)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	header.Name = "./" + filepath.ToSlash(rel)
	if rel == "." {
		header.Name = "."
	}
	if info.IsDir() {
		header.Name += "/"
	}
	header.Mode = archiveMode(goos, info, header.Mode)
	if info.Mode().IsRegular() && executables[filepath.ToSlash(rel)] {
		header.Mode |= (header.Mode & readBits) >> 2
	}
	header.Uid, header.Gid = 0, 0
	header.Uname, header.Gname = "", ""
	header.ModTime = time.Unix(0, 0).UTC()
	header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
	header.Format = tar.FormatPAX
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(tw, file)
	return errors.Join(copyErr, file.Close())
}
