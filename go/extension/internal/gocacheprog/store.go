package gocacheprog

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The helper's on-disk cache spans two directories under the resolved Go cache
// root, one for each kind of file:
//
//   - action records ("<action id>-a") live in the helper's own tree,
//     <goCacheRoot>/prog. The go command writes its own records into GOCACHE
//     under the same names but in a different format, so sharing that
//     directory would put this package's records where a go command running
//     without the helper reads its own. A private tree keeps each record with
//     the reader that understands it. Records are one short line each.
//   - data files ("<output id>-d") live in the go command's own build cache,
//     <goCacheRoot>/build (GOCACHE, toolchain.GoBuildCacheDir). A data file is
//     the raw output bytes named by their SHA-256, which is exactly the go
//     command's own data-file format. Keeping them there is what lets the two
//     kinds of go command on one machine share one copy of each object: a go
//     command runs through this helper only in a job of a
//     provider-backed run, and without it everywhere else (no provider,
//     --no-cache, a nested run, a prepare script, the wrapper's bootstrap). Under
//     GOCACHEPROG the go command writes nothing to GOCACHE, so a private data
//     tree meant each object was compiled and stored twice, once per kind.
//
// Sharing works in both directions. When a go command without the helper
// stores an output whose data file already exists with the right size and
// hash, it keeps that file and writes only its own record. When this helper
// stores one that a go command without the helper already wrote, writeData
// keeps the existing file. The go command stores linked executables as a
// directory named "<output id>-d", but only when GOCACHE is its cache: under
// GOCACHEPROG it never sends a linked executable to the helper, so the helper
// never writes to one of those paths.
//
// Both trees use Go's layout: a two-character shard, then the 64-hex id with a
// suffix. That is deliberate. The extension's collector
// (internal/jobs/cachepolicy) recognizes exactly that shape, so both trees are
// swept by the same budget and the same grace window.
//
// Three properties make the layout safe under concurrency:
//
//   - the data file is CONTENT-ADDRESSED by the output id, which the go command
//     derives as the SHA-256 of the body. Two writers of one path therefore
//     write identical bytes, and an atomic rename over an existing file is a
//     no-op in content terms. This holds whether the other writer is another
//     helper or a go command without one.
//   - the data file is renamed into place BEFORE the record that points at it,
//     so a reader never follows a record to a partial body. The reverse order
//     (record first) is a torn entry a compiler would read as truncated output.
//   - a record whose data file is gone, or has the wrong size, is a miss (see
//     get). Something other than this helper can remove a data file: the
//     collector, the go command's own trim, or `go clean -cache`. A record
//     therefore never promises a body, it only names one.

const (
	// recordSuffix and dataSuffix are Go's own build-cache entry suffixes, kept
	// so the collector's entry recognizer covers this tree unchanged.
	recordSuffix = "-a"
	dataSuffix   = "-d"

	// recordVersion prefixes every action record. A future format change bumps
	// it, and an unrecognized version reads as a miss rather than as garbage.
	recordVersion = "v1"

	// mtimeInterval mirrors cmd/go/internal/cache: an entry's modification time
	// is refreshed only once it is ALREADY this stale, so a hot cache does not
	// pay one write per lookup. The collector widens its grace window by the
	// same interval, so the two agree on what "recently used" means — without
	// this refresh a long-lived entry an active build is reading would look
	// cold and could be evicted out from under a running compiler.
	mtimeInterval = time.Hour

	// idHexLength is the hex length of an action or output id: both are 32-byte
	// sums. A different length is not something to shard or name, it is a
	// request to reject.
	idHexLength = 64
)

// entry is one cached object as the helper reports it to the go command.
type entry struct {
	// outputID is the hex output id the body was stored under.
	outputID string
	// size is the body's byte length.
	size int64
	// time is when the object entered the cache.
	time time.Time
	// diskPath is the absolute path of the body.
	diskPath string
}

// store is the helper's local cache: its own record directory and the go
// command's build cache directory it keeps its data files in.
type store struct {
	// records holds the action records: <goCacheRoot>/prog.
	records string
	// data holds the data files: <goCacheRoot>/build, the go command's GOCACHE.
	data string
}

// openStore prepares the record and data directories under the resolved Go
// cache root.
//
// A directory that cannot be created is a real failure: the go command requires
// a DiskPath on every put, so a helper with nowhere to write has nothing
// truthful to answer.
func openStore(records, data string) (*store, error) {
	records, err := prepareDir(records, "record")
	if err != nil {
		return nil, err
	}
	data, err = prepareDir(data, "data")
	if err != nil {
		return nil, err
	}
	return &store{records: records, data: data}, nil
}

// prepareDir makes one store directory absolute and creates it.
func prepareDir(dir, role string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("gocacheprog: no %s directory", role)
	}
	if !filepath.IsAbs(dir) {
		// DiskPath must be absolute; deriving it from a relative directory
		// would hand the go command a path resolved against ITS working
		// directory.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("gocacheprog: absolute %s directory: %w", role, err)
		}
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("gocacheprog: create %s directory: %w", role, err)
	}
	return dir, nil
}

// recordPath is where the action record for actionID lives.
func (s *store) recordPath(actionID string) string {
	return filepath.Join(s.records, actionID[:2], actionID+recordSuffix)
}

// dataPath is where the body identified by outputID lives: the path the go
// command itself uses for that output in GOCACHE.
func (s *store) dataPath(outputID string) string {
	return filepath.Join(s.data, outputID[:2], outputID+dataSuffix)
}

// get resolves one action id, or reports that the local cache does not hold it.
//
// Every inconsistency is a MISS, never an error: a record pointing at a body the
// collector, the go command's own trim or `go clean -cache` removed from
// GOCACHE, a truncated record, a size that disagrees with the record.
// The caller's fallback for a miss is to ask the remote cache and then to let
// the go command rebuild, which is correct for all of them.
func (s *store) get(actionID string) (entry, bool) {
	if !validID(actionID) {
		return entry{}, false
	}
	recordPath := s.recordPath(actionID)
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		return entry{}, false
	}
	found, ok := parseRecord(string(raw))
	if !ok {
		return entry{}, false
	}
	dataPath := s.dataPath(found.outputID)
	info, err := os.Stat(dataPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != found.size {
		return entry{}, false
	}
	found.diskPath = dataPath
	touch(recordPath, time.Time{})
	touch(dataPath, info.ModTime())
	return found, true
}

// put stores a body and the record that points at it, and returns the entry the
// go command is told about.
//
// body is streamed, not buffered: a linked test binary goes through this path
// and holding it twice in memory is what makes a cache helper the reason a
// build runs out of it.
func (s *store) put(actionID, outputID string, size int64, body io.Reader) (entry, error) {
	if !validID(actionID) || !validID(outputID) {
		return entry{}, fmt.Errorf("gocacheprog: invalid id (action %q, output %q)", actionID, outputID)
	}
	dataPath, err := s.writeData(outputID, size, body, false)
	if err != nil {
		return entry{}, err
	}
	stored := entry{outputID: outputID, size: size, time: time.Now(), diskPath: dataPath}
	if err := s.writeRecord(actionID, stored); err != nil {
		return entry{}, err
	}
	return stored, nil
}

// ingest installs bytes fetched from the remote cache, VERIFYING them on the
// way in.
//
// The verification is the whole reason this is not a plain copy. The go command
// never re-hashes a body it is handed: it takes the output id from the response
// and reads the file at DiskPath as that output. A provider that served bytes
// which do not hash to the announced output id would therefore have the go
// command link output nothing in this workspace produced. Verifying here is the
// only place that check can happen, so a mismatch is refused and the caller
// falls back to a local rebuild.
func (s *store) ingest(actionID, outputID string, size int64, source string) (entry, error) {
	if !validID(actionID) || !validID(outputID) {
		return entry{}, fmt.Errorf("gocacheprog: invalid id (action %q, output %q)", actionID, outputID)
	}
	file, err := os.Open(source)
	if err != nil {
		return entry{}, err
	}
	defer func() { _ = file.Close() }()
	dataPath, err := s.writeData(outputID, size, file, true)
	if err != nil {
		return entry{}, err
	}
	stored := entry{outputID: outputID, size: size, time: time.Now(), diskPath: dataPath}
	if err := s.writeRecord(actionID, stored); err != nil {
		return entry{}, err
	}
	return stored, nil
}

// writeData materializes the body at its content address. verify re-hashes the
// stream and refuses bytes that do not match the output id they were announced
// under.
//
// An existing file of the right size is left alone: the path is content
// addressed, so it already holds these bytes, and rewriting it would churn the
// disk and reset the modification time a collector reads as "last used". That
// holds for a file a go command without the helper wrote, too. The go command
// writes a data file in place but writes its last byte last, so a file that
// already has the full size is complete.
func (s *store) writeData(outputID string, size int64, body io.Reader, verify bool) (string, error) {
	path := s.dataPath(outputID)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == size {
		// Still drain the body: on the put path it is framed inside the go
		// command's stdin stream, and leaving it unread would desynchronize
		// every request after it.
		if _, err := io.Copy(io.Discard, body); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "put-*.tmp")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath) // a no-op once the rename succeeded
	}()

	var written int64
	if verify {
		sum := sha256.New()
		written, err = io.Copy(io.MultiWriter(temp, sum), body)
		if err == nil && hex.EncodeToString(sum.Sum(nil)) != outputID {
			err = fmt.Errorf("gocacheprog: object %s does not hash to its output id", outputID)
		}
	} else {
		written, err = io.Copy(temp, body)
	}
	if err != nil {
		return "", err
	}
	if written != size {
		return "", fmt.Errorf("gocacheprog: object %s is %d bytes, announced %d", outputID, written, size)
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return "", err
	}
	return path, nil
}

// writeRecord publishes the action record. It is written last and renamed into
// place, so an action id is either absent or points at a complete body.
func (s *store) writeRecord(actionID string, stored entry) error {
	path := s.recordPath(actionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "rec-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if _, err := temp.WriteString(formatRecord(stored)); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// formatRecord renders an action record: a version, the output id, the body
// size and the entry's creation time in Unix nanoseconds, space separated on
// one line.
func formatRecord(stored entry) string {
	return strings.Join([]string{
		recordVersion,
		stored.outputID,
		strconv.FormatInt(stored.size, 10),
		strconv.FormatInt(stored.time.UnixNano(), 10),
	}, " ") + "\n"
}

// parseRecord reads a record back. Anything it does not fully understand is not
// an entry.
func parseRecord(raw string) (entry, bool) {
	fields := strings.Fields(raw)
	if len(fields) != 4 || fields[0] != recordVersion || !validID(fields[1]) {
		return entry{}, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return entry{}, false
	}
	nanos, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return entry{}, false
	}
	return entry{outputID: fields[1], size: size, time: time.Unix(0, nanos)}, true
}

// touch refreshes an entry's modification time once it is older than
// mtimeInterval, the way Go's own cache marks an entry as used. A zero modTime
// means the caller does not know it and this reads it. Failure is ignored.
func touch(path string, modTime time.Time) {
	if modTime.IsZero() {
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		modTime = info.ModTime()
	}
	now := time.Now()
	if now.Sub(modTime) < mtimeInterval {
		return
	}
	_ = os.Chtimes(path, now, now)
}

// validID reports whether id is a lowercase-hex cache id of the expected
// length. It guards the path derivation above: an id is a file name here, and
// "a/../b" is not a sum.
func validID(id string) bool {
	if len(id) != idHexLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
