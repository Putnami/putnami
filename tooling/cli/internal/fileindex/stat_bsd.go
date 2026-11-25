//go:build darwin || freebsd

package fileindex

import (
	"os"
	"syscall"
)

const statDecoded = true

// statOf decodes info's stat; ok is false where it is not a syscall stat.
func statOf(info os.FileInfo) (Stat, bool) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Stat{}, false
	}
	return Stat{
		Dev:        uint64(sys.Dev), //nolint:gosec,unconvert // a device number, whatever its width
		Ino:        uint64(sys.Ino), //nolint:unconvert // its width differs by platform
		Mode:       uint32(sys.Mode),
		UID:        sys.Uid,
		GID:        sys.Gid,
		Size:       sys.Size,
		ModNano:    sys.Mtimespec.Nano(),
		ChangeNano: sys.Ctimespec.Nano(),
	}, true
}

// localFilesystems are the filesystems whose stat the index's rule holds for:
// local, with a status-change time the kernel keeps.
var localFilesystems = map[string]bool{"apfs": true, "hfs": true, "ufs": true, "zfs": true, "tmpfs": true}

// localFilesystem reports whether dir lies on one of localFilesystems. A
// network filesystem, FUSE and FAT are not: every file there is read.
func localFilesystem(dir string) bool {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return false
	}
	name := make([]byte, 0, len(fs.Fstypename))
	for _, c := range fs.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return localFilesystems[string(name)]
}
