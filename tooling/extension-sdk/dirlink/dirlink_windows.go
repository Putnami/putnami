//go:build windows

package dirlink

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.putnami.dev/sdk/extension/filelock"
	"golang.org/x/sys/windows"
)

// finalPathFlags asks GetFinalPathNameByHandle for a normalized path with a
// drive letter: FILE_NAME_NORMALIZED | VOLUME_NAME_DOS, both zero.
const finalPathFlags = 0

func create(target, link string) error {
	abs, err := absoluteTarget(target, link)
	if err != nil {
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	// A junction is an empty directory that carries a mount-point reparse
	// point. Mkdir reports an existing link the way os.Symlink does.
	if err := os.Mkdir(link, 0o777); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	if err := setMountPoint(link, abs); err != nil {
		// Without its reparse point the directory is no link: remove it, and
		// name it when that fails too, so the caller knows it stays.
		if removeErr := os.Remove(link); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	return nil
}

// absoluteTarget resolves a relative target against the link's directory, the
// way the kernel resolves a relative symbolic link.
func absoluteTarget(target, link string) (string, error) {
	if target == "" {
		return "", windows.ERROR_INVALID_NAME
	}
	if filepath.VolumeName(target) == "" && !os.IsPathSeparator(target[0]) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Abs(target)
}

// setMountPoint writes the IO_REPARSE_TAG_MOUNT_POINT reparse point that makes
// the empty directory dir a junction to the absolute path target. The buffer is
// REPARSE_DATA_BUFFER with its MountPointReparseBuffer: the NT substitute name
// the file system follows, then the print name os.Readlink falls back to, each
// NUL-terminated.
func setMountPoint(dir, target string) error {
	substitute, err := windows.UTF16FromString(substituteName(target))
	if err != nil {
		return err
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	const header = 8     // ReparseTag, ReparseDataLength, Reserved
	const nameFields = 8 // the four name offset and length fields
	pathBytes := 2 * (len(substitute) + len(printName))
	size := header + nameFields + pathBytes
	if size > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return windows.ERROR_FILENAME_EXCED_RANGE
	}
	buf := make([]byte, size)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	le.PutUint16(buf[4:], uint16(nameFields+pathBytes))
	le.PutUint16(buf[8:], 0)
	le.PutUint16(buf[10:], uint16(2*(len(substitute)-1)))
	le.PutUint16(buf[12:], uint16(2*len(substitute)))
	le.PutUint16(buf[14:], uint16(2*(len(printName)-1)))
	offset := header + nameFields
	for _, unit := range append(substitute, printName...) {
		le.PutUint16(buf[offset:], unit)
		offset += 2
	}

	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var returned uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &returned, nil)
}

// substituteName is the NT path of an absolute Win32 path: \??\C:\dir for a
// drive path, \??\UNC\server\share for a UNC path. os.Readlink turns it back
// into the Win32 path.
func substituteName(target string) string {
	switch {
	case strings.HasPrefix(target, `\\?\`):
		return `\??\` + target[len(`\\?\`):]
	case strings.HasPrefix(target, `\\`):
		return `\??\UNC\` + target[len(`\\`):]
	}
	return `\??\` + target
}

func replace(target, link, _ string) error {
	lock, err := filelock.Acquire(link+".lock", true, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	info, err := os.Lstat(link)
	switch {
	case err == nil && info.IsDir():
		// A junction and a directory symbolic link have no fs.ModeDir, so this
		// is a real directory. An empty one is what a create stopped between
		// its Mkdir and its reparse point leaves, and os.Remove deletes only
		// an empty directory. Any other is refused, as a Unix rename refuses it.
		if os.Remove(link) != nil {
			return &os.LinkError{Op: "junction", Old: target, New: link, Err: syscall.EISDIR}
		}
	case err == nil:
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
	case !os.IsNotExist(err):
		return err
	}
	return create(target, link)
}

// isLink relies on Go 1.23+ os.Lstat modes, winsymlink=1 by default: a
// symbolic link has fs.ModeSymlink, and a junction has fs.ModeIrregular without
// fs.ModeDir. os.Readlink then tells a junction from the other reparse points
// that share that mode.
func isLink(path string, info fs.FileInfo) bool {
	mode := info.Mode()
	if mode&fs.ModeSymlink != 0 {
		return true
	}
	if mode&fs.ModeIrregular == 0 || mode.IsDir() {
		return false
	}
	_, err := os.Readlink(path)
	return err == nil
}

// resolve opens path, which follows every symbolic link and junction on the
// way, and asks the file system for the final path of what it opened.
func resolve(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(name, 0, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), finalPathFlags)
		if err != nil {
			return "", &os.PathError{Op: "resolve", Path: path, Err: err}
		}
		if n < uint32(len(buf)) {
			return win32Path(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n)
	}
}

// win32Path drops the \\?\ prefix GetFinalPathNameByHandle puts on a drive
// or UNC path. Any other answer keeps its prefix, which Windows APIs accept.
func win32Path(final string) string {
	rest, ok := strings.CutPrefix(final, `\\?\`)
	switch {
	case !ok:
		return final
	case strings.HasPrefix(rest, `UNC\`):
		return `\\` + rest[len(`UNC\`):]
	case len(rest) >= 2 && rest[1] == ':':
		return rest
	}
	return final
}
