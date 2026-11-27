// Package fileindex is the machine-local index that maps a file's stat to the
// content digest read from it, so an unchanged file is not read again.
//
// The rule is git's (protocols/workspace ADR 0009): a file whose stat equals
// the stat recorded beside a digest is unchanged, and the digest answers for
// its content. The rule holds only for a file that could not have changed
// within the timestamp tick it was recorded in, so an entry is recorded only
// when every timestamp it carries is older than the index's clock, read on the
// filesystem itself before the content was read, by at least the coarsest tick
// a filesystem keeps (Margin). A file written in the same tick as its read
// therefore has no entry, and the next run reads it again.
//
// What a stat holds: device, inode, mode, owner, size, and the modification
// and status-change times to the nanosecond. A rewrite of the same size in the
// same tick moves none of the first five; it is caught by the tick rule. A
// file replaced by a rename has another inode, a permission change moves the
// mode and the status-change time, and a modification time set back by hand
// leaves the status-change time at the hour of the change.
//
// What is never trusted, and so always read: a file that is not a regular
// file (a symbolic link is read through, never recorded), a file on another
// filesystem than the index, whose clock the index did not read, a file whose
// times lie ahead of the index's clock, and every file on a platform whose
// stat this package does not decode. A clock that went back since the index
// was written discards the index. An index that does not parse, or whose
// checksum does not match, is an empty index: a damaged record costs reads,
// never a stale digest.
//
// One index file lives in a workspace's `.putnami/` directory. Concurrent runs
// share it: each loads it, records what it read, and merges its entries into
// the file on disk under a lock when it ends. An entry is decided once, when
// it is recorded, so a merge cannot make a racy entry trusted.
package fileindex

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

// Filename is the index's filename under a workspace's `.putnami/`.
const Filename = "file-index"

// lockName is the lock a Save takes to merge into the file on disk.
const lockName = "file-index.lock"

// Margin is how much older than the index's clock a file's times must be for
// its entry to be recorded: twice the coarsest modification-time tick a listed
// filesystem keeps, one second on ext3 and HFS+.
const Margin = 2 * time.Second

// RawContent is the variant of a file's raw SHA-256, the reading every caller
// that hashes a file's bytes as they are shares.
const RawContent = "sha256"

// DigestSize is the size of every digest the index holds: a SHA-256 sum.
const DigestSize = sha256.Size

// maxEntries bounds the entries one file holds; past it, a Save keeps the
// entries this process recorded or confirmed and drops the others. A test
// lowers it.
var maxEntries = 1 << 18

// keptRuns is how many saving runs an entry survives without being recorded
// or confirmed: an entry for a file no run reads any more is dropped after it.
const keptRuns = 32

// Stat is what the index compares to decide that a file is unchanged.
type Stat struct {
	Dev, Ino   uint64
	Mode       uint32
	UID, GID   uint32
	Size       int64
	ModNano    int64
	ChangeNano int64
}

type key struct{ path, variant string }

type entry struct {
	stat   Stat
	digest [DigestSize]byte
	// content is the file's bytes for an entry Content recorded, whose
	// digest is their SHA-256; nil for an entry Digest recorded.
	content []byte
	// seen is the generation of the last saving run that recorded or
	// confirmed the entry. It decides nothing about the file.
	seen uint64
}

// Index is one workspace's index for one process.
type Index struct {
	file     string // the index file; empty for an index that is never saved
	identity string // the program the digests were computed by (Open)
	readOnly bool   // answers digests, records and saves nothing
	dev      uint64 // the device of the index's directory
	clock    int64  // the filesystem clock read when the index opened, in ns

	mu         sync.Mutex
	loaded     map[key]entry
	fresh      map[key]entry
	confirmed  map[key]bool // loaded entries this process found current
	generation uint64       // the generation of the loaded file
	stale      map[key]Stat // loaded entries this process found out of date
	// unrecorded holds the content readings a read-only index read rather
	// than answered, for an index that records to read them again (Adopt).
	unrecorded map[key]bool

	hits, reads atomic.Int64
}

// reads counts every content read the digests of this package made, through
// an index or with none, since the process started.
var contentReads atomic.Int64

// ContentReads is the number of file contents Digest read since the process
// started, whichever index it went through. A test reads it around a run.
func ContentReads() int64 { return contentReads.Load() }

// readObserver, when set, is told each file an index read again.
var readObserver atomic.Pointer[func(path, variant string)]

// ObserveReads makes observe be told the path and the reading of each file
// whose content an index read rather than answered, a missing file excepted,
// and returns a function that removes it. A test names what a run read with
// it.
func ObserveReads(observe func(path, variant string)) (restore func()) {
	readObserver.Store(&observe)
	return func() { readObserver.Store(nil) }
}

// current is the index Digest goes through; nil reads every file.
var current atomic.Pointer[Index]

// Use makes idx the index Digest goes through, and returns a function that
// restores the previous one. A nil idx reads every file.
func Use(idx *Index) (restore func()) {
	previous := current.Swap(idx)
	return func() { current.CompareAndSwap(idx, previous) }
}

// Current is the index Digest goes through, or nil.
func Current() *Index { return current.Load() }

// Path is the index file of the workspace at root.
func Path(root string) string {
	return filepath.Join(root, ".putnami", Filename)
}

// Open reads the filesystem clock in the index's directory and loads the index
// file at path. identity names the program that computes the digests (see
// ExecutableIdentity): an index written under another identity loads empty,
// so a program that reads a file another way never answers with a digest
// taken the old way. Under the identity "" the index answers and records
// nothing.
//
// It returns an index that records nothing, and so reads every file, when the
// platform's stat is not decoded, the index's directory is not on a local
// filesystem whose stat the rule holds for (localFilesystem), or the clock
// cannot be read. A missing, damaged or unparsable file loads as an empty
// index.
func Open(path, identity string) *Index {
	idx := newIndex(path, identity)
	if identity == "" || !statDecoded || !onLocalFilesystem(filepath.Dir(path)) {
		return idx
	}
	dev, clock, err := readClock(filepath.Dir(path))
	if err != nil {
		return idx
	}
	idx.dev, idx.clock = dev, clock
	idx.load()
	return idx
}

// OpenReadOnly loads the index file at path to answer digests, and writes
// nothing: it creates no directory and no clock file, records nothing, and its
// Save does nothing. It is the index of a run that only plans. Its clock is
// the process's, against which a stamp ahead of it still discards the file.
func OpenReadOnly(path, identity string) *Index {
	idx := newIndex(path, identity)
	idx.readOnly = true
	dir := filepath.Dir(path)
	if identity == "" || !statDecoded || !onLocalFilesystem(dir) {
		return idx
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return idx
	}
	st, ok := statOf(info)
	if !ok {
		return idx
	}
	idx.dev, idx.clock = st.Dev, time.Now().UnixNano()
	idx.load()
	return idx
}

func newIndex(path, identity string) *Index {
	return &Index{
		file:       path,
		identity:   identity,
		loaded:     map[key]entry{},
		fresh:      map[key]entry{},
		confirmed:  map[key]bool{},
		stale:      map[key]Stat{},
		unrecorded: map[key]bool{},
	}
}

// load reads the index file. A damaged file, one written under another
// identity, and one whose stamp lies ahead of the clock, which went back since
// it was written, load nothing: an entry recorded before could now match a
// file written at its recorded time.
func (idx *Index) load() {
	data, err := os.ReadFile(idx.file) //nolint:gosec // the index file under the workspace's .putnami directory
	if err != nil {
		return
	}
	disk, err := decode(data)
	if err != nil || disk.identity != idx.identity || disk.stamp > idx.clock {
		return
	}
	idx.loaded, idx.generation = disk.entries, disk.generation
}

// confirmedAging reports whether an entry this run confirmed has gone
// unrecorded for half of keptRuns, so the run saves to keep it.
func (idx *Index) confirmedAging(confirmed map[key]bool) bool {
	for k := range confirmed {
		if idx.generation-idx.loaded[k].seen >= keptRuns/2 {
			return true
		}
	}
	return false
}

// ExecutableIdentity names the running program for Open. It folds in
// ReadingScheme, the program's build information (module versions, sums and
// VCS revision), the Go build ID of the executable file the process runs, and
// that file's path and stat. It is read once when the process starts: a binary
// replaced at its path later does not lend the process its identity. On Linux
// the file is /proc/self/exe, the inode the process runs even once it is
// deleted. Two builds that cmd/go made from different inputs have different
// Go build IDs, so they never share an index, provided each build ID lies
// within the head of the file that is searched (identityHead). A process whose
// executable cannot be read, or carries no usable build ID in that head (none,
// an empty one, or one a build redacted), has the identity "", under which the
// index answers and records nothing.
func ExecutableIdentity() string { return executableIdentity }

// ReadingScheme versions how the callers of this package read files under a
// variant name. A change to a reading that keeps its variant name raises it;
// the golden digests of the store's readings are recorded under its value.
const ReadingScheme = "1"

// identityHead is how many leading bytes of the executable are searched for
// its Go build ID: the ELF note that carries it lies within the first 4 KiB,
// and the Mach-O text marker at or just past 4 KiB. An executable whose build
// ID lies beyond the head uses no index.
const identityHead = 64 << 10

// executableIdentity is read at process start, by package initialization.
var executableIdentity = readExecutableIdentity()

func readExecutableIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	running := "/proc/self/exe"
	if _, err := os.Stat(running); err != nil {
		if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil {
			exe = resolved
		}
		running = exe
	}
	build := ""
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		build = buildInfo.String()
	}
	return identityFromFile(ReadingScheme, exe, running, build)
}

// identityFromFile is the identity of the executable named exe, read from the
// file at running: "" when it cannot be stat'ed or read.
func identityFromFile(scheme, exe, running, build string) string {
	info, err := os.Stat(running)
	if err != nil {
		return ""
	}
	head, err := readHead(running)
	if err != nil {
		return ""
	}
	return identityOf(scheme, exe, info, head, build)
}

// readHead reads the first identityHead bytes of the file at path.
func readHead(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the running executable
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	head := make([]byte, identityHead)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

// buildIDMarker opens the Go build ID the linker writes at the start of the
// text, where a Mach-O executable carries it: `\xff Go build ID: "<id>"`.
var buildIDMarker = []byte("\xff Go build ID: \"")

// elfBuildIDNote is the name, padded, of the ELF note (.note.go.buildid, type
// 4) that carries the Go build ID near the start of an ELF executable.
var elfBuildIDNote = []byte("Go\x00\x00")

// buildID is the Go build ID written in head, read from its ELF note or, with
// none, after its text marker. It is "" when head carries none, an empty one,
// one without the `/` that separates the IDs a build writes, or one that
// contains `redacted`: the checks catch a build that redacted it
// (`-buildid=`, `-buildid=redacted`), not every ID that is not the action and
// content IDs cmd/go writes. The identity relies on the build ID cmd/go
// assigns from the build's inputs: one set by hand with -ldflags=-buildid
// lies outside it.
func buildID(head []byte) string {
	id := elfBuildID(head)
	if id == "" {
		if start := bytes.Index(head, buildIDMarker); start >= 0 {
			rest := head[start+len(buildIDMarker):]
			if end := bytes.IndexByte(rest, '"'); end > 0 {
				id = string(rest[:end])
			}
		}
	}
	if !strings.Contains(id, "/") || strings.Contains(id, "redacted") {
		return ""
	}
	return id
}

// elfBuildID is the descriptor of the first Go build ID note in head: a
// 12-byte header (name size 4, descriptor size, type 4, little-endian) then
// the padded name "Go" and the descriptor.
func elfBuildID(head []byte) string {
	for offset := 0; ; {
		i := bytes.Index(head[offset:], elfBuildIDNote)
		if i < 0 {
			return ""
		}
		at := offset + i
		offset = at + 1
		if at < 12 {
			continue
		}
		header := head[at-12 : at]
		if binary.LittleEndian.Uint32(header[0:4]) != 4 || binary.LittleEndian.Uint32(header[8:12]) != 4 {
			continue
		}
		size := int(binary.LittleEndian.Uint32(header[4:8]))
		start := at + len(elfBuildIDNote)
		if size <= 0 || size > len(head)-start {
			continue
		}
		return string(bytes.TrimRight(head[start:start+size], "\x00"))
	}
}

// identityOf is the identity, under reading scheme scheme, of the executable
// at exe with stat info, first bytes head and build information build; "" when
// head carries no usable Go build ID.
func identityOf(scheme, exe string, info os.FileInfo, head []byte, build string) string {
	id := buildID(head)
	if id == "" {
		return ""
	}
	st, ok := statOf(info)
	if !ok {
		return ""
	}
	h := sha256.New()
	for _, field := range []string{scheme, build, id} {
		_ = binary.Write(h, binary.LittleEndian, uint64(len(field)))
		h.Write([]byte(field)) //nolint:errcheck // hash.Hash.Write never errors
	}
	return fmt.Sprintf("%x\x00%s\x00%d:%d:%d:%d:%d", h.Sum(nil), exe, st.Dev, st.Ino, st.Size, st.ModNano, st.ChangeNano)
}

// enabled reports whether idx may answer or record.
func (idx *Index) enabled() bool { return idx != nil && idx.clock != 0 }

// Hits is the number of digests idx answered without reading the file.
func (idx *Index) Hits() int64 {
	if idx == nil {
		return 0
	}
	return idx.hits.Load()
}

// Reads is the number of file contents read through idx.
func (idx *Index) Reads() int64 {
	if idx == nil {
		return 0
	}
	return idx.reads.Load()
}

// Digest returns the digest compute reads from the file at path, through the
// current index: the recorded digest when the file's stat equals the stat
// recorded with it under variant, otherwise compute's answer. variant names
// how compute reads the file, so two readings of one file never answer for
// each other. compute must return a DigestSize digest of what it read from
// path, or an error; an error is returned as is and records nothing.
func Digest(path, variant string, compute func() ([]byte, error)) ([]byte, error) {
	return current.Load().Digest(path, variant, compute)
}

// Digest is the package Digest through idx. A nil idx reads every file.
func (idx *Index) Digest(path, variant string, compute func() ([]byte, error)) ([]byte, error) {
	if strings.HasPrefix(variant, contentVariant) {
		// The variants of content entries are Content's own: a digest under
		// one would answer for a file's bytes.
		return read(idx, compute)
	}
	var computed []byte
	e, err := idx.answer(path, variant, func() ([]byte, error) {
		var err error
		computed, err = compute()
		return computed, err
	}, func(before Stat, digest []byte) (entry, bool) {
		var e entry
		if len(digest) != DigestSize {
			return e, false
		}
		e.stat = before
		copy(e.digest[:], digest)
		return e, true
	})
	if err != nil {
		return nil, err
	}
	if e == nil {
		return computed, nil
	}
	return append([]byte(nil), e.digest[:]...), nil
}

// MaxContent is the largest file Content records: a larger file is read by
// every run.
const MaxContent = 64 << 10

// contentVariant prefixes the variant of an entry Content records, so a
// digest entry and a content entry of one file never answer for each other.
const contentVariant = "content/"

// Content returns the bytes read returns for the file at path, through the
// current index: the recorded bytes when the file's stat equals the stat
// recorded with them under variant, otherwise read's answer. It is the
// reading of a small file a caller parses on every run, such as a
// configuration file. read must return the file's bytes as they are; an
// error is returned as is and records nothing. Only a file of at most
// MaxContent bytes whose size equals the length read returned is recorded.
// The caller owns the returned slice.
func Content(path, variant string, read func() ([]byte, error)) ([]byte, error) {
	return current.Load().Content(path, variant, read)
}

// Content is the package Content through idx. A nil idx reads every file.
func (idx *Index) Content(path, variant string, read func() ([]byte, error)) ([]byte, error) {
	var data []byte
	var wasRead bool
	e, err := idx.answer(path, contentVariant+variant, func() ([]byte, error) {
		var err error
		wasRead = true
		data, err = read()
		return data, err
	}, func(before Stat, content []byte) (entry, bool) {
		var e entry
		if len(content) > MaxContent || int64(len(content)) != before.Size {
			return e, false
		}
		e.stat = before
		e.digest = sha256.Sum256(content)
		e.content = append([]byte{}, content...)
		return e, true
	})
	if err != nil {
		return nil, err
	}
	if wasRead && variant == BytesVariant && idx.enabled() && idx.readOnly && filepath.IsAbs(path) && len(data) <= MaxContent {
		// Adopt reads the file again as it is, which is this reading only.
		// A file no run can record (not a regular file, too large, on
		// another filesystem) is not read again for nothing.
		if st, ok := lstat(path); ok && st.Dev == idx.dev {
			idx.mu.Lock()
			idx.unrecorded[key{path: path, variant: variant}] = true
			idx.mu.Unlock()
		}
	}
	if e == nil {
		return data, nil
	}
	return append([]byte{}, e.content...), nil
}

// BytesVariant names the reading of a file whose bytes the index keeps: the
// file as it is.
const BytesVariant = "bytes"

// ReadFile reads the file at path through the current index, as os.ReadFile
// does: a file of at most MaxContent bytes whose stat is the one recorded with
// its bytes is not read again. The workspace configuration loaders take a
// file's bytes from it (wsproto.UseConfigReader).
func ReadFile(path string) ([]byte, error) {
	return Content(path, BytesVariant, func() ([]byte, error) {
		return os.ReadFile(path) //nolint:gosec // a file of the workspace a caller names
	})
}

// Adopt reads again through idx each file whose bytes (BytesVariant) the
// read-only index from read rather than answered, so that idx records it under its own clock
// and rule. A process reads its configuration through a read-only index before
// it knows whether its run may write; the index of a run that records adopts
// those reads, and the next run answers them. It does nothing when idx records
// nothing.
func (idx *Index) Adopt(from *Index) {
	if !idx.enabled() || idx.readOnly || from == nil || from == idx {
		return
	}
	from.mu.Lock()
	pending := make([]key, 0, len(from.unrecorded))
	for k := range from.unrecorded {
		pending = append(pending, k)
	}
	from.unrecorded = map[key]bool{}
	from.mu.Unlock()
	for _, k := range pending {
		path := k.path
		_, _ = idx.Content(path, k.variant, func() ([]byte, error) {
			return os.ReadFile(path) //nolint:gosec // a file the process read through the index before
		})
	}
}

// answer returns the entry recorded for path under variant when the file's
// stat equals the recorded one. Otherwise it calls compute, records the entry
// record builds from its answer when the file may be recorded, and returns
// that entry, or nil when compute's answer is not recorded and the caller
// keeps it as is.
func (idx *Index) answer(path, variant string, compute func() ([]byte, error), record func(Stat, []byte) (entry, bool)) (*entry, error) {
	if !idx.enabled() || !filepath.IsAbs(path) {
		// A relative path names another file in another working directory.
		value, err := read(idx, compute)
		if err != nil {
			return nil, err
		}
		e, ok := record(Stat{}, value)
		if !ok {
			return nil, nil
		}
		return &e, nil
	}
	k := key{path: path, variant: variant}
	before, ok := lstat(path)
	if ok {
		idx.mu.Lock()
		recorded, found := idx.fresh[k]
		if !found {
			recorded, found = idx.loaded[k]
		}
		if found && recorded.stat == before {
			if _, isLoaded := idx.loaded[k]; isLoaded {
				idx.confirmed[k] = true
			}
			idx.mu.Unlock()
			idx.hits.Add(1)
			return &recorded, nil
		}
		if loaded, isLoaded := idx.loaded[k]; isLoaded && loaded.stat != before {
			idx.stale[k] = loaded.stat
		}
		delete(idx.fresh, k)
		idx.mu.Unlock()
	} else {
		idx.mu.Lock()
		if loaded, isLoaded := idx.loaded[k]; isLoaded {
			idx.stale[k] = loaded.stat
		}
		delete(idx.fresh, k)
		idx.mu.Unlock()
	}
	if !ok && missing(path) {
		// A missing file has no content to read: a loader that looks for an
		// optional file is told so by every run, and that is no read.
		value, err := compute()
		if err != nil {
			return nil, err
		}
		e, recordable := record(before, value)
		if !recordable {
			return nil, nil
		}
		return &e, nil
	}
	if observe := readObserver.Load(); observe != nil {
		(*observe)(path, variant)
	}
	value, err := read(idx, compute)
	if err != nil {
		return nil, err
	}
	e, recordable := record(before, value)
	if !recordable {
		return nil, nil
	}
	if !ok || !idx.recordable(before) {
		return &e, nil
	}
	// A file that changed while it was read has two stats; what was read
	// answers for neither.
	if after, stillOK := lstat(path); !stillOK || after != before {
		return &e, nil
	}
	idx.mu.Lock()
	idx.fresh[k] = e
	idx.mu.Unlock()
	return &e, nil
}

func read(idx *Index, compute func() ([]byte, error)) ([]byte, error) {
	contentReads.Add(1)
	if idx != nil {
		idx.reads.Add(1)
	}
	return compute()
}

// recordable reports whether a file with stat st may be recorded: a regular
// file on the index's filesystem, whose times are older than the index's
// clock by Margin. lstat answers ok only for a regular file.
func (idx *Index) recordable(st Stat) bool {
	if idx.readOnly || st.Dev != idx.dev {
		return false
	}
	limit := idx.clock - int64(Margin)
	return st.ModNano < limit && st.ChangeNano < limit
}

// Save merges what this process recorded into the index file, under the
// index's lock, and writes it atomically. It keeps the entries on disk it did
// not find out of date, and writes nothing when this process recorded and
// invalidated nothing. A clock that went back during the run discards what
// this process recorded.
func (idx *Index) Save() error {
	if !idx.enabled() || idx.readOnly || idx.file == "" {
		return nil
	}
	idx.mu.Lock()
	fresh := make(map[key]entry, len(idx.fresh))
	for k, e := range idx.fresh {
		fresh[k] = e
	}
	stale := make(map[key]Stat, len(idx.stale))
	for k, st := range idx.stale {
		stale[k] = st
	}
	confirmed := make(map[key]bool, len(idx.confirmed))
	for k := range idx.confirmed {
		confirmed[k] = true
	}
	idx.mu.Unlock()
	if len(fresh) == 0 && len(stale) == 0 && !idx.confirmedAging(confirmed) {
		// An unchanged run writes nothing, until the entries it confirmed come
		// halfway to being dropped.
		return nil
	}

	dir := filepath.Dir(idx.file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create file index directory: %w", err)
	}
	lock, err := filelock.Acquire(filepath.Join(dir, lockName), true, false)
	if err != nil {
		return fmt.Errorf("lock file index: %w", err)
	}
	defer lock.Release() //nolint:errcheck // releasing a held lock

	_, now, err := readClock(dir)
	if err != nil {
		return fmt.Errorf("read file index clock: %w", err)
	}
	if now < idx.clock {
		// The clock went back during the run: an entry recorded under the
		// clock read at its start can match a later write.
		fresh = nil
	}

	merged := map[key]entry{}
	stamp := idx.clock
	var generation uint64
	if data, readErr := os.ReadFile(idx.file); readErr == nil {
		if disk, decodeErr := decode(data); decodeErr == nil && disk.identity == idx.identity && disk.stamp <= now {
			merged, generation = disk.entries, disk.generation
			if disk.stamp > stamp {
				stamp = disk.stamp
			}
		}
	}
	generation++
	for k, st := range stale {
		if e, ok := merged[k]; ok && e.stat == st {
			delete(merged, k)
		}
	}
	for k := range confirmed {
		if e, ok := merged[k]; ok && e.stat == idx.loaded[k].stat {
			e.seen = generation
			merged[k] = e
		}
	}
	for k, e := range fresh {
		e.seen = generation
		merged[k] = e
	}
	for k, e := range merged {
		if generation-e.seen > keptRuns {
			delete(merged, k)
		}
	}
	if len(merged) > maxEntries {
		kept := make(map[key]entry, len(fresh)+len(confirmed))
		for k, e := range merged {
			if _, ok := fresh[k]; ok || confirmed[k] {
				kept[k] = e
			}
		}
		merged = kept
	}
	return writeAtomic(idx.file, encode(file{identity: idx.identity, stamp: stamp, generation: generation, entries: merged}))
}

// readClock creates a file in dir and returns the device and modification time
// the filesystem gave it: the filesystem's own clock, on its own tick.
func readClock(dir string) (uint64, int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, err
	}
	probe, err := os.CreateTemp(dir, ".file-index-clock.*")
	if err != nil {
		return 0, 0, err
	}
	name := probe.Name()
	defer os.Remove(name) //nolint:errcheck // a probe file
	if _, err := probe.Write([]byte{0}); err != nil {
		probe.Close() //nolint:errcheck,gosec // write already failed
		return 0, 0, err
	}
	if err := probe.Close(); err != nil {
		return 0, 0, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return 0, 0, err
	}
	st, ok := statFile(info)
	if !ok || st.ModNano <= 0 {
		return 0, 0, errors.New("fileindex: no clock")
	}
	return st.Dev, st.ModNano, nil
}

// Lstat returns the stat the index compares for the regular file at path; ok
// is false for any other kind of file, a missing one, and on a platform whose
// stat this package does not decode.
func Lstat(path string) (Stat, bool) { return lstat(path) }

// missing reports whether nothing exists at path.
func missing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

// lstat returns the stat of the regular file at path; ok is false for any
// other kind of file, a missing one, or an undecoded stat.
func lstat(path string) (Stat, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Stat{}, false
	}
	return statFile(info)
}

// statFile decodes a file's stat. A test replaces it to keep a coarser clock
// than the filesystem it runs on.
var statFile = statOf

func writeAtomic(target string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), "."+Filename+".*")
	if err != nil {
		return fmt.Errorf("stage file index: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name) //nolint:errcheck // no-op once renamed into place
	if _, err := temp.Write(data); err != nil {
		temp.Close() //nolint:errcheck,gosec // write already failed
		return fmt.Errorf("write file index: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close file index: %w", err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("publish file index: %w", err)
	}
	return nil
}

// The file: magic, format version, the identity of the program that wrote it,
// the stamp (the newest clock of a run that wrote it), the generation (the
// number of saving runs), the entry count, the entries, and the SHA-256 of
// everything before it. An entry is its path and variant, its stat, its
// digest, its generation and its content: the bytes of a file Content
// recorded, empty for an entry Digest recorded.
var magic = []byte("PNFI")

const formatVersion uint32 = 4

const entryFixed = 8 + 8 + 4 + 4 + 4 + 8 + 8 + 8 + DigestSize + 8 + 4

var errCorrupt = errors.New("fileindex: damaged index")

// file is the decoded index file.
type file struct {
	identity   string
	stamp      int64
	generation uint64
	entries    map[key]entry
}

func encode(f file) []byte {
	var buf bytes.Buffer
	buf.Write(magic)
	_ = binary.Write(&buf, binary.LittleEndian, formatVersion)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(f.identity)))
	buf.WriteString(f.identity)
	_ = binary.Write(&buf, binary.LittleEndian, f.stamp)
	_ = binary.Write(&buf, binary.LittleEndian, f.generation)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(f.entries)))
	for k, e := range f.entries {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(k.path)))
		buf.WriteString(k.path)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(k.variant)))
		buf.WriteString(k.variant)
		_ = binary.Write(&buf, binary.LittleEndian, e.stat)
		buf.Write(e.digest[:])
		_ = binary.Write(&buf, binary.LittleEndian, e.seen)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(e.content)))
		buf.Write(e.content)
	}
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func decode(data []byte) (file, error) {
	var f file
	header := len(magic) + 4 + 4 + 8 + 8 + 4
	if len(data) < header+DigestSize {
		return f, errCorrupt
	}
	body, sum := data[:len(data)-DigestSize], data[len(data)-DigestSize:]
	if want := sha256.Sum256(body); !bytes.Equal(want[:], sum) {
		return f, errCorrupt
	}
	if !bytes.Equal(body[:len(magic)], magic) {
		return f, errCorrupt
	}
	r := bytes.NewReader(body[len(magic):])
	readString := func() (string, bool) {
		var n uint32
		if binary.Read(r, binary.LittleEndian, &n) != nil || int64(n) > int64(r.Len()) {
			return "", false
		}
		s := make([]byte, n)
		if _, err := io.ReadFull(r, s); err != nil {
			return "", false
		}
		return string(s), true
	}
	var version, count uint32
	if binary.Read(r, binary.LittleEndian, &version) != nil || version != formatVersion {
		return f, errCorrupt
	}
	identity, ok := readString()
	if !ok {
		return f, errCorrupt
	}
	f.identity = identity
	if binary.Read(r, binary.LittleEndian, &f.stamp) != nil ||
		binary.Read(r, binary.LittleEndian, &f.generation) != nil ||
		binary.Read(r, binary.LittleEndian, &count) != nil {
		return f, errCorrupt
	}
	if int64(count) > int64(r.Len())/int64(entryFixed+8) {
		return f, errCorrupt
	}
	f.entries = make(map[key]entry, count)
	for i := uint32(0); i < count; i++ {
		path, ok := readString()
		if !ok {
			return f, errCorrupt
		}
		variant, ok := readString()
		if !ok {
			return f, errCorrupt
		}
		var e entry
		if binary.Read(r, binary.LittleEndian, &e.stat) != nil {
			return f, errCorrupt
		}
		if _, err := io.ReadFull(r, e.digest[:]); err != nil {
			return f, errCorrupt
		}
		if binary.Read(r, binary.LittleEndian, &e.seen) != nil {
			return f, errCorrupt
		}
		content, ok := readString()
		if !ok || len(content) > MaxContent {
			return f, errCorrupt
		}
		if strings.HasPrefix(variant, contentVariant) {
			// A content entry answers its bytes: they must be the ones its
			// digest and its stat's size were recorded for.
			if int64(len(content)) != e.stat.Size || sha256.Sum256([]byte(content)) != e.digest {
				return f, errCorrupt
			}
			e.content = []byte(content)
		} else if content != "" {
			return f, errCorrupt
		}
		f.entries[key{path: path, variant: variant}] = e
	}
	if r.Len() != 0 {
		return f, errCorrupt
	}
	return f, nil
}

// onLocalFilesystem reports whether dir, or its nearest existing ancestor
// when dir is still to be created, lies on a listed local filesystem. A test
// replaces it.
var onLocalFilesystem = func(dir string) bool {
	for {
		if _, err := os.Lstat(dir); err == nil {
			return localFilesystem(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// BuildIdentity is the identity of the build of the Go executable at path,
// whatever its path and stat: the SHA-256 of its build information and its
// Go build ID, in hex. It is "" when the file cannot be read or carries no
// usable build ID. Two files of one build have one identity; a CLI and a
// machine worker compare it (CLI ADR 0116).
func BuildIdentity(path string) string {
	head, err := readHead(path)
	if err != nil {
		return ""
	}
	id := buildID(head)
	if id == "" {
		return ""
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, field := range []string{info.String(), id} {
		_ = binary.Write(h, binary.LittleEndian, uint64(len(field)))
		h.Write([]byte(field)) //nolint:errcheck // hash.Hash.Write never errors
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
