//go:build linux

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
		Dev:        uint64(sys.Dev), //nolint:unconvert // its width differs by architecture
		Ino:        sys.Ino,
		Mode:       sys.Mode,
		UID:        sys.Uid,
		GID:        sys.Gid,
		Size:       sys.Size,
		ModNano:    sys.Mtim.Nano(),
		ChangeNano: sys.Ctim.Nano(),
	}, true
}

// localFilesystems are the filesystems whose stat the index's rule holds for:
// local, with a status-change time the kernel keeps and an attribute read that
// is never served from a cache another host can make stale. ext2/3/4, XFS,
// Btrfs, tmpfs, ZFS, F2FS, bcachefs and overlayfs.
var localFilesystems = map[int64]bool{
	0xEF53:     true,
	0x58465342: true,
	0x9123683E: true,
	0x01021994: true,
	0x2FC12FC1: true,
	0xF2F52010: true,
	0xCA451A4E: true,
	0x794C7630: true,
}

// localFilesystem reports whether dir lies on one of localFilesystems. A
// network filesystem (NFS, SMB, 9p), FUSE and FAT are not: their stat can be
// cached or carry no status-change time, so every file there is read.
func localFilesystem(dir string) bool {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return false
	}
	return localFilesystems[int64(uint32(fs.Type))] //nolint:gosec,unconvert // a 32-bit magic number, signed on some architectures
}
