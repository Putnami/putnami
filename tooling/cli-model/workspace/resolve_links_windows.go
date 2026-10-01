//go:build windows

package workspace

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// procGetFinalPathNameByHandleW is the kernel32 call the syscall package does
// not wrap. kernel32.dll is a known DLL, so the loader maps the system copy.
var procGetFinalPathNameByHandleW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// resolveLinks opens path, which follows every symbolic link and junction on
// the way, and asks the file system for the final path of what it opened: the
// same calls, flags and result form as dirlink.Resolve.
func resolveLinks(path string) (string, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)
	h, err := syscall.CreateFile(name, 0, share, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		n, err := finalPathNameByHandle(h, buf)
		if err != nil {
			return "", &os.PathError{Op: "resolve", Path: path, Err: err}
		}
		if n < uint32(len(buf)) {
			return win32Path(syscall.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n)
	}
}

// finalPathNameByHandle calls GetFinalPathNameByHandleW with
// FILE_NAME_NORMALIZED | VOLUME_NAME_DOS, both zero. It returns the length of
// the path without its terminating NUL, or, when buf is too small, the length
// buf needs.
func finalPathNameByHandle(h syscall.Handle, buf []uint16) (uint32, error) {
	n, _, callErr := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if n != 0 {
		return uint32(n), nil
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		return 0, errno
	}
	return 0, syscall.EINVAL
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
