package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pinsDirName is the store directory that holds one pin per recorded session
// whose event stream references task-owned entries of the store, and the
// results those pins keep (CLI ADR 0137).
const pinsDirName = "pins"

// pinObjectsDirName, under pinsDirName, holds each pinned result.json once,
// named by the hex of its SHA-256: <hex>.json.
const pinObjectsDirName = "objects"

// pinVersion is the version of a pin document. A version 1 pin named entry
// addresses; it keeps nothing, and is deleted where it is met.
const pinVersion = 2

// pinMaxBytes bounds the pin a reader reads: a session of a hundred thousand
// referenced results stays well inside it.
const pinMaxBytes = 16 << 20

// pinnedDigest is the hex of a SHA-256, as a pin and a kept result's name
// spell it.
var pinnedDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sessionPin is one pin: the session directory that references entries, and
// the hex SHA-256 of the result.json each of those references names.
type sessionPin struct {
	Version int      `json:"version"`
	Session string   `json:"session"`
	Results []string `json:"results"`
}

// PinnedResult is one result a session's stream references: the key the
// store files its task-owned entry under, and the SHA-256 of that entry's
// result.json, "sha256:<hex>".
type PinnedResult struct {
	Key    string
	Result string
}

// PinSession keeps, while the session recorded in sessionDir exists, the
// result.json of each entry results names, which is all a reader of that
// session's references reads (ReadReferencedResult). Each result is kept
// once in the store, by its digest: hard-linked from its entry, or copied
// where the file system links nothing, so it costs no byte while the entry
// stands. The entries themselves stay under ordinary collection. An entry the
// store no longer holds keeps nothing. PinSession replaces the session's
// earlier pin, atomically, and deletes the pins of sessions whose directory is
// gone (pinnedResults); an empty results removes the pin. It holds the store's shared lock,
// so collection, which deletes the results no pin names under the exclusive
// lock, never deletes one it is pinning (sweepPinnedResults).
func (s *LocalStore) PinSession(sessionDir string, results []PinnedResult) error {
	dir, err := filepath.Abs(sessionDir)
	if err != nil {
		return err
	}
	release := s.lockShared()
	defer release()
	path := s.pinPath(dir)
	defer pinnedResults(s.root, true)
	if len(results) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	objects := filepath.Join(s.root, pinsDirName, pinObjectsDirName)
	if err := os.MkdirAll(objects, 0o755); err != nil {
		return err
	}
	kept := make([]string, 0, len(results))
	for _, result := range results {
		digest, ok := strings.CutPrefix(result.Result, "sha256:")
		if !ok || !pinnedDigest.MatchString(digest) {
			continue
		}
		if s.keepResult(objects, digest, result.Key) {
			kept = append(kept, digest)
		}
	}
	slices.Sort(kept)
	kept = slices.Compact(kept)
	data, err := json.Marshal(sessionPin{Version: pinVersion, Session: dir, Results: kept})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// keepResult puts the result.json of key's entry at objects/<digest>.json
// unless one is there, and reports whether one is there now. It keeps the
// file only when its bytes hash to digest: the entry may have been replaced
// since the session referenced it.
func (s *LocalStore) keepResult(objects, digest, key string) bool {
	object := filepath.Join(objects, digest+".json")
	if _, err := os.Lstat(object); err == nil {
		return true
	}
	source := filepath.Join(s.blobDir(TaskEntryAddress(key)), "result.json")
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return false
	}
	tmp := object + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := os.Link(source, tmp); err != nil {
		if copyErr := copyFileOpts(source, tmp, false); copyErr != nil {
			_ = os.Remove(tmp)
			return false
		}
	}
	data, err := os.ReadFile(tmp)
	if sum := sha256.Sum256(data); err != nil || hex.EncodeToString(sum[:]) != digest {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, object); err != nil {
		_ = os.Remove(tmp)
		_, statErr := os.Lstat(object)
		return statErr == nil
	}
	return true
}

// ReadReferencedResult reads the result a cache reference names: the
// result.json whose SHA-256 is result, "sha256:<hex>", kept by a pin or held
// by key's task-owned entry, read in that order. It returns nil when neither
// holds those bytes. It stamps nothing: reading a session keeps no entry
// alive.
func (s *LocalStore) ReadReferencedResult(key, result string) *EntryResult {
	digest, ok := strings.CutPrefix(result, "sha256:")
	if !ok || !pinnedDigest.MatchString(digest) {
		return nil
	}
	if data, err := os.ReadFile(filepath.Join(s.root, pinsDirName, pinObjectsDirName, digest+".json")); err == nil {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) == digest {
			if parsed, err := UnmarshalResult(data); err == nil {
				return parsed
			}
		}
	}
	entry := s.ReadTaskEntry(key)
	if entry == nil || entry.ResultDigest != result {
		return nil
	}
	return entry.Result
}

// pinPath is the pin of the session recorded in the absolute directory dir.
func (s *LocalStore) pinPath(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(s.root, pinsDirName, hex.EncodeToString(sum[:16])+".json")
}

// sweepPinnedResults deletes, under the store's exclusive lock, every pin
// whose session directory no longer exists, then every kept result no pin
// names. A pin this build cannot read keeps every result: it may name any of
// them. A store with nothing to delete is not locked: a pass first looks
// without the lock. It reports whether it did not skip the store for a lock
// it could not take.
func sweepPinnedResults(root string, nonBlocking bool) bool {
	objects := filepath.Join(root, pinsDirName, pinObjectsDirName)
	if _, err := os.Stat(objects); err != nil || !unpinnedResults(root, false) {
		return true
	}
	s := NewLocalStore(root)
	var release func()
	if nonBlocking {
		var locked bool
		if release, locked = s.tryLockExclusive(); !locked {
			return false
		}
	} else {
		release = s.lockExclusive()
	}
	defer release()
	unpinnedResults(root, true)
	return true
}

// unpinnedResults reports whether the store at root holds a pin whose session
// directory no longer exists, a pin of version 1, or, when every pin reads, a
// kept result no pin names. With remove set, the caller holds the exclusive
// lock, and it deletes them.
func unpinnedResults(root string, remove bool) bool {
	live, complete, stale := pinnedResults(root, remove)
	files, err := os.ReadDir(filepath.Join(root, pinsDirName, pinObjectsDirName))
	if err != nil || !complete {
		return stale
	}
	for _, file := range files {
		digest, isObject := strings.CutSuffix(file.Name(), ".json")
		if isObject && live[digest] {
			continue
		}
		if !remove {
			return true
		}
		stale = true
		_ = os.Remove(filepath.Join(root, pinsDirName, pinObjectsDirName, file.Name()))
	}
	return stale
}

// pinnedResults returns the digests the pins of the store at root name,
// whether every pin read, a pin this build cannot read may name any result,
// and whether a pin is stale: its session directory no longer exists, its
// session was pruned, or it is of version 1. With remove set, it deletes the
// stale ones.
func pinnedResults(root string, remove bool) (live map[string]bool, complete, stale bool) {
	pinsDir := filepath.Join(root, pinsDirName)
	files, err := os.ReadDir(pinsDir)
	if err != nil {
		return nil, true, false
	}
	live, complete = map[string]bool{}, true
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(pinsDir, file.Name())
		pin, version := readPin(path)
		gone := pin == nil && version == 1
		if pin != nil {
			_, err := os.Stat(pin.Session)
			gone = errors.Is(err, fs.ErrNotExist)
		}
		if gone {
			stale = true
			if remove {
				_ = os.Remove(path)
			}
			continue
		}
		if pin == nil {
			complete = false
			continue
		}
		for _, digest := range pin.Results {
			live[digest] = true
		}
	}
	return live, complete, stale
}

// readPin reads the pin at path, and the version it names. The pin is nil
// when the file cannot be read or is not a pin of this version; the version
// is 0 when the file names none.
func readPin(path string) (*sessionPin, int) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > pinMaxBytes {
		return nil, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0
	}
	var pin sessionPin
	if json.Unmarshal(data, &pin) != nil {
		return nil, 0
	}
	if pin.Version != pinVersion || !filepath.IsAbs(pin.Session) {
		return nil, pin.Version
	}
	return &pin, pin.Version
}

// writeFileAtomic replaces path with data through a temporary sibling.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
