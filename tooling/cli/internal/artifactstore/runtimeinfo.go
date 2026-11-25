package artifactstore

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/store"
)

const (
	// runtimeInfoFile is the per-entry sidecar that records the runtime-info
	// answer of an executable inside the entry, written after a handshake that
	// passed (RecordRuntimeInfo).
	runtimeInfoFile = "runtimeinfo"
	// runtimeInfoSchema versions the record; a record of another schema is
	// not read.
	runtimeInfoSchema = 1
	// maxRuntimeInfoRecord bounds the bytes read from a record: a handshake
	// answer is at most 64 KiB, and the record adds a few members.
	maxRuntimeInfoRecord = 80 * 1024
)

// runtimeInfoRecord is the runtimeinfo sidecar: the executable it was
// recorded for, as a slash path inside the entry, that file's identity when
// the handshake passed, and the handshake's answer.
type runtimeInfoRecord struct {
	Schema     int                `json:"schema"`
	Executable string             `json:"executable"`
	File       store.FileIdentity `json:"file"`
	Info       json.RawMessage    `json:"info"`
}

// RuntimeInfoKey names the executable a handshake answers for: the published
// entry that holds it, its slash path inside the entry, and its file identity.
// The caller takes it before the handshake starts, so an answer is never
// recorded for a file that changed while it ran.
type RuntimeInfoKey struct {
	dir      string
	rel      string
	identity store.FileIdentity
}

// RuntimeInfoKeyOf returns the key of executable, and false for an executable
// outside this store's published entries, a store bookkeeping file, or one
// with no file identity on this platform: no record is kept for it.
func (s *Store) RuntimeInfoKeyOf(executable string) (RuntimeInfoKey, bool) {
	dir, rel, identity, ok := s.runtimeInfoTarget(executable)
	return RuntimeInfoKey{dir: dir, rel: rel, identity: identity}, ok
}

// RecordedRuntimeInfo returns the runtime-info answer recorded for key, and
// false unless the entry holds a record of the current schema for the same
// executable with the same file identity. The caller checks the answer against
// the identity it expects, as it checks a handshake's.
func (s *Store) RecordedRuntimeInfo(key RuntimeInfoKey) (json.RawMessage, bool) {
	if key.dir == "" {
		return nil, false
	}
	file, err := os.Open(filepath.Join(key.dir, runtimeInfoFile))
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeInfoRecord+1))
	if err != nil || len(data) > maxRuntimeInfoRecord {
		return nil, false
	}
	var record runtimeInfoRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, false
	}
	if record.Schema != runtimeInfoSchema || record.Executable != key.rel || record.File != key.identity || len(record.Info) == 0 {
		return nil, false
	}
	return record.Info, true
}

// RecordRuntimeInfo records info, the answer of a runtime-info handshake that
// passed for executable, whose key was taken before the handshake started.
// It records nothing when executable no longer has that key: the file changed
// while the handshake ran. The record replaces the entry's previous one by an
// atomic rename, under the store's shared lock, so GC does not remove the
// entry while it is written; nothing is written when the lock is not held
// before ctx ends, or when the entry is gone. A failed write is ignored: the
// next load handshakes again.
func (s *Store) RecordRuntimeInfo(ctx context.Context, executable string, key RuntimeInfoKey, info []byte) {
	if key.dir == "" || len(info) == 0 {
		return
	}
	data, err := json.Marshal(runtimeInfoRecord{
		Schema:     runtimeInfoSchema,
		Executable: key.rel,
		File:       key.identity,
		Info:       json.RawMessage(info),
	})
	if err != nil {
		return
	}
	_ = s.WithSharedContext(ctx, func() {
		if current, ok := s.RuntimeInfoKeyOf(executable); !ok || current != key {
			return
		}
		tmp, err := os.CreateTemp(key.dir, runtimeInfoFile+"-") // IsBookkeeping covers this name
		if err != nil {
			return
		}
		tmpName := tmp.Name()
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmpName)
			return
		}
		if err := os.Rename(tmpName, filepath.Join(key.dir, runtimeInfoFile)); err != nil {
			os.Remove(tmpName)
		}
	})
}

// runtimeInfoTarget resolves executable to the published entry that holds it:
// the entry directory, the executable's slash path inside it, and its file
// identity. It reports false for an executable outside the entries, a store
// bookkeeping file, or one with no identity.
func (s *Store) runtimeInfoTarget(executable string) (dir, rel string, identity store.FileIdentity, ok bool) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", "", store.FileIdentity{}, false
	}
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", "", store.FileIdentity{}, false
	}
	inSha, err := filepath.Rel(filepath.Join(root, shaDirName), resolved)
	if err != nil {
		return "", "", store.FileIdentity{}, false
	}
	parts := strings.Split(filepath.ToSlash(inSha), "/")
	if len(parts) < 3 || !isHexDigest(parts[1]) || parts[0] != parts[1][:2] {
		return "", "", store.FileIdentity{}, false
	}
	rel = strings.Join(parts[2:], "/")
	if len(parts) == 3 && IsBookkeeping(rel) {
		return "", "", store.FileIdentity{}, false
	}
	identity, ok = store.StatFileIdentity(resolved)
	if !ok {
		return "", "", store.FileIdentity{}, false
	}
	return filepath.Join(root, shaDirName, parts[0], parts[1]), rel, identity, true
}
