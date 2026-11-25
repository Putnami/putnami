package store

import (
	"os"
	"time"
)

// FileIdentity is the stat identity of one file.
type FileIdentity struct {
	Size         int64  `json:"size"`
	ModTimeNs    int64  `json:"modTimeNs"`
	ChangeTimeNs int64  `json:"changeTimeNs"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	// Links is the number of hard links to the file.
	Links uint64 `json:"links"`
}

// FileIdentityOf returns the identity of the file info describes, and false on a platform
// that does not expose a change time and an inode: there, no record is keyed by
// a file.
func FileIdentityOf(info os.FileInfo) (FileIdentity, bool) {
	if info == nil || !info.Mode().IsRegular() {
		return FileIdentity{}, false
	}
	ctime, dev, ino, links, ok := fileChangeTime(info)
	if !ok {
		return FileIdentity{}, false
	}
	return FileIdentity{
		Size:         info.Size(),
		ModTimeNs:    info.ModTime().UnixNano(),
		ChangeTimeNs: ctime,
		Device:       dev,
		Inode:        ino,
		Links:        links,
	}, true
}

// StatFileIdentity is FileIdentityOf for the file at path, following symbolic links.
func StatFileIdentity(path string) (FileIdentity, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return FileIdentity{}, false
	}
	return FileIdentityOf(info)
}

// Records kept between runs sit in the artifact store root
// (ResolveArtifactStoreRoot): files a run reads instead of starting a process.
// Each is written by an atomic rename and read only when it parses whole.
const (
	// ToolchainProbeDir holds runtime toolchain probe answers, one file per
	// binary identity.
	ToolchainProbeDir = "toolchain-probes"
	// InputResolveDir holds input-resolve answers, one file per workspace
	// root and request, sharded by the first two digits of its name.
	InputResolveDir = "input-resolve"
)

// RecordRefresh is how old a record's modification time may grow while runs
// read it before a read stamps it again (KeepRecord), so the artifact store's
// GC measures a record's idleness from its last use within this bound.
const RecordRefresh = 24 * time.Hour

// KeepRecord stamps the modification time of the record file a run just
// read when it is older than RecordRefresh, so GC keeps a record that runs
// still read. A failed stamp is ignored.
func KeepRecord(file *os.File) {
	info, err := file.Stat()
	if err != nil {
		return
	}
	now := time.Now()
	if now.Sub(info.ModTime()) < RecordRefresh {
		return
	}
	_ = os.Chtimes(file.Name(), now, now)
}
