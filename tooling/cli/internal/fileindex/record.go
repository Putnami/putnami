package fileindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/sdk/extension/filelock"
)

// The input record is the machine-local record of values a run computed
// from the work tree, each kept with its witness: the stats of the paths the
// computation read (tooling/cli ADR 0113). The next run answers a value from
// the record when every part of its witness holds, and computes it otherwise.
//
// The record follows the rule of the file index (internal/fileindex,
// protocols/workspace ADR 0009) and shares its envelope:
//
//   - A part is recorded only when both of its times are older than the
//     filesystem clock the record read when it opened by more than
//     Margin, and it lies on the record's device. A path written
//     during the run, or in the two seconds before it, makes its value
//     unrecorded, and the next run computes it again.
//   - The file names the program that wrote it: a record another build wrote
//     loads empty.
//   - The file ends with the SHA-256 of what precedes it; a damaged file
//     loads empty, and one whose stamp lies ahead of the clock discards
//     itself.
//   - Concurrent runs merge under a lock and rename a complete file into
//     place. A value is decided when it is recorded, so a merge cannot make a
//     racy value trusted.
//
// The record holds tables. A table is named by the computation that owns it
// (the input digests of cache keys hold one), and its keys and values are
// that computation's: the record compares no value, only stats.
//
// It lives beside the file index because it is the file index's rule applied
// to a computation rather than to one file, under the same envelope.

// Filename is the record's filename under a workspace's `.putnami/`.
const RecordFilename = "input-record"

// lockName is the lock a saving run holds while it merges into the file.
const recordLockName = "input-record.lock"

// Scheme is the version of what the record's tables hold and how their
// witnesses are taken. It is part of the record's identity: a CLI that
// changes either changes it, and loads an older record empty.
const RecordScheme = "1"

// maxEntries bounds the entries one file holds, and maxParts the parts of
// all its entries; past either, a Save keeps only what its run recorded or
// confirmed.
var (
	recordMaxEntries = 1 << 14
	recordMaxParts   = 1 << 20
)

// Part is one path a value's computation read, with the stat it had: a file
// it read, a directory it listed, or the nearest existing directory above a
// path it found absent.
type RecordPart struct {
	Path string
	Stat Stat
}

type recordKey struct{ table, key string }

type recordEntry struct {
	value []byte
	parts []RecordPart
	// seen is the generation of the last saving run that recorded or
	// confirmed the entry. It decides nothing about the work tree.
	seen uint64
}

// Record is one workspace's record for one process.
type Record struct {
	file     string
	identity string
	readOnly bool
	dev      uint64
	clock    int64
	// now reads the clock again when the record saves: the filesystem's,
	// or the fixed clock of OpenRecordAt.
	now func(dir string) (int64, error)

	mu         sync.Mutex
	loaded     map[recordKey]recordEntry
	fresh      map[recordKey]recordEntry
	confirmed  map[recordKey]bool
	stale      map[recordKey]bool
	generation uint64

	answered, computed atomic.Int64

	// stats holds the stats the run took to check witnesses (Hold): one stat
	// per path per run, dropped by the run's writes (Forget). under maps a
	// comparison-form directory to the held paths below it, so a write drops
	// them without a scan of every held stat.
	statsMu sync.Mutex
	stats   map[string]heldStat
	under   map[string][]string
}

type heldStat struct {
	st Stat
	ok bool
}

// Path is the record file of the workspace at root.
func RecordPath(root string) string {
	return filepath.Join(root, ".putnami", RecordFilename)
}

// Identity is the identity a record of the running program is written and
// read under: the record's scheme and the file index's identity of the
// executable. A process with no executable identity has none, and its record
// answers and records nothing.
func RecordIdentity() string {
	exe := ExecutableIdentity()
	if exe == "" {
		return ""
	}
	return "inputrecord/" + RecordScheme + "\x00" + exe
}

// readClock, localDirectory and lstat are the file index's; a test replaces
// them.
var (
	recordClock = readClock
	recordLocal = func(dir string) bool { return statDecoded && onLocalFilesystem(dir) }
	recordLstat = LstatAny
)

// Open reads the filesystem clock in the record's directory and loads the
// record file at path. It returns a record that answers and records nothing
// under the identity "", where the platform's stat is not decoded, where the
// directory is not on a listed local filesystem, or when the clock cannot be
// read.
func OpenRecord(path, identity string) *Record {
	r := newRecord(path, identity)
	if identity == "" || !recordLocal(filepath.Dir(path)) {
		return r
	}
	dev, clock, err := recordClock(filepath.Dir(path))
	if err != nil {
		return r
	}
	r.dev, r.clock = dev, clock
	r.load()
	return r
}

// OpenReadOnly loads the record file at path to answer values, and writes
// nothing: no directory, no clock file, no entry. Its clock is the process's.
func OpenRecordReadOnly(path, identity string, now int64) *Record {
	r := newRecord(path, identity)
	r.readOnly = true
	dir := filepath.Dir(path)
	if identity == "" || !recordLocal(dir) {
		return r
	}
	st, ok := recordLstat(dir)
	if !ok {
		return r
	}
	r.dev, r.clock = st.Dev, now
	r.load()
	return r
}

func newRecord(path, identity string) *Record {
	return &Record{
		now: func(dir string) (int64, error) {
			_, clock, err := recordClock(dir)
			return clock, err
		},
		file:      path,
		identity:  identity,
		loaded:    map[recordKey]recordEntry{},
		fresh:     map[recordKey]recordEntry{},
		confirmed: map[recordKey]bool{},
		stale:     map[recordKey]bool{},
	}
}

func (r *Record) enabled() bool { return r != nil && r.clock != 0 }

func (r *Record) load() {
	data, err := os.ReadFile(r.file) //nolint:gosec // the record under the workspace's .putnami directory
	if err != nil {
		return
	}
	disk, err := decodeRecord(data)
	if err != nil || disk.identity != r.identity || disk.stamp > r.clock {
		return
	}
	r.loaded, r.generation = disk.entries, disk.generation
}

// current is the record the run's computations consult; nil answers nothing.
var currentRecord atomic.Pointer[Record]

// Use makes r the record Current returns, and returns a function that
// restores the previous one.
func UseRecord(r *Record) (restore func()) {
	previous := currentRecord.Swap(r)
	return func() { currentRecord.CompareAndSwap(r, previous) }
}

// Current is the record of the run, or nil.
func CurrentRecord() *Record {
	r := currentRecord.Load()
	if !r.enabled() {
		return nil
	}
	return r
}

// Answer returns the value recorded in table at k when holds accepts its
// parts. A value whose parts do not hold is dropped from the file at the next
// save. holds must take the stat of every part now and compare it with the
// recorded one.
func (r *Record) Answer(table, k string, holds func(parts []RecordPart) bool) ([]byte, []RecordPart, bool) {
	if !r.enabled() {
		return nil, nil, false
	}
	id := recordKey{table, k}
	r.mu.Lock()
	e, found := r.fresh[id]
	_, isLoaded := r.loaded[id]
	if !found {
		e, found = r.loaded[id]
	}
	r.mu.Unlock()
	if !found {
		return nil, nil, false
	}
	if !holds(e.parts) {
		r.mu.Lock()
		if isLoaded {
			r.stale[id] = true
		}
		delete(r.fresh, id)
		r.mu.Unlock()
		return nil, nil, false
	}
	r.mu.Lock()
	if isLoaded {
		r.confirmed[id] = true
	}
	r.mu.Unlock()
	r.answered.Add(1)
	return e.value, e.parts, true
}

// Recordable reports whether a part with stat st may be recorded: on the
// record's device, with both times older than the record's clock by
// Margin.
func (r *Record) Recordable(st Stat) bool {
	if !r.enabled() || r.readOnly || st.Dev != r.dev {
		return false
	}
	limit := r.clock - int64(Margin)
	return st.ModNano < limit && st.ChangeNano < limit
}

// Put records value in table at k with its parts, and reports whether it did:
// a value with a part Recordable refuses, with a relative path, or with no
// part at all, is not recorded.
func (r *Record) Put(table, k string, value []byte, parts []RecordPart) bool {
	r.computed.Add(1)
	if !r.enabled() || r.readOnly || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !filepath.IsAbs(part.Path) || !part.Absent() && !r.Recordable(part.Stat) {
			return false
		}
	}
	e := recordEntry{value: bytes.Clone(value), parts: append([]RecordPart(nil), parts...)}
	r.mu.Lock()
	r.fresh[recordKey{table, k}] = e
	r.mu.Unlock()
	return true
}

// Answered is how many values the record answered in this process.
func (r *Record) Answered() int64 {
	if r == nil {
		return 0
	}
	return r.answered.Load()
}

// Computed is how many values this process computed and offered to the
// record, recorded or not.
func (r *Record) Computed() int64 {
	if r == nil {
		return 0
	}
	return r.computed.Load()
}

// Save merges what this process recorded into the record file, under its
// lock, and writes it atomically. It writes nothing when the process recorded
// and invalidated nothing, until the entries it confirmed come halfway to
// being dropped. A clock that went back during the run discards what this
// process recorded.
func (r *Record) Save() error {
	if !r.enabled() || r.readOnly || r.file == "" {
		return nil
	}
	r.mu.Lock()
	fresh := make(map[recordKey]recordEntry, len(r.fresh))
	for k, e := range r.fresh {
		fresh[k] = e
	}
	stale := make(map[recordKey]bool, len(r.stale))
	for k := range r.stale {
		stale[k] = true
	}
	confirmed := make(map[recordKey]bool, len(r.confirmed))
	aging := false
	for k := range r.confirmed {
		confirmed[k] = true
		if r.generation-r.loaded[k].seen >= keptRuns/2 {
			aging = true
		}
	}
	r.mu.Unlock()
	if len(fresh) == 0 && len(stale) == 0 && !aging {
		return nil
	}

	dir := filepath.Dir(r.file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create input record directory: %w", err)
	}
	lock, err := filelock.Acquire(filepath.Join(dir, recordLockName), true, false)
	if err != nil {
		return fmt.Errorf("lock input record: %w", err)
	}
	defer lock.Release() //nolint:errcheck // releasing a held lock

	now, err := r.now(dir)
	if err != nil {
		return fmt.Errorf("read input record clock: %w", err)
	}
	if now < r.clock {
		fresh = nil
	}

	merged := map[recordKey]recordEntry{}
	stamp := r.clock
	var generation uint64
	if data, readErr := os.ReadFile(r.file); readErr == nil { //nolint:gosec // the record under the workspace's .putnami directory
		if disk, decodeErr := decodeRecord(data); decodeErr == nil && disk.identity == r.identity && disk.stamp <= now {
			merged, generation = disk.entries, disk.generation
			if disk.stamp > stamp {
				stamp = disk.stamp
			}
		}
	}
	generation++
	for k := range stale {
		if e, ok := merged[k]; ok && sameParts(e.parts, r.loaded[k].parts) {
			delete(merged, k)
		}
	}
	for k := range confirmed {
		if e, ok := merged[k]; ok && sameParts(e.parts, r.loaded[k].parts) {
			e.seen = generation
			merged[k] = e
		}
	}
	for k, e := range fresh {
		e.seen = generation
		merged[k] = e
	}
	parts := 0
	for k, e := range merged {
		if generation-e.seen > keptRuns {
			delete(merged, k)
			continue
		}
		parts += len(e.parts)
	}
	if len(merged) > recordMaxEntries || parts > recordMaxParts {
		kept := make(map[recordKey]recordEntry, len(fresh)+len(confirmed))
		for k, e := range merged {
			if _, ok := fresh[k]; ok || confirmed[k] {
				kept[k] = e
			}
		}
		merged = kept
	}
	return writeRecordAtomic(r.file, encodeRecord(recordFile{identity: r.identity, stamp: stamp, generation: generation, entries: merged}))
}

func sameParts(a, b []RecordPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeRecordAtomic(target string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), "."+RecordFilename+".*")
	if err != nil {
		return fmt.Errorf("stage input record: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name) //nolint:errcheck // no-op once renamed into place
	if _, err := temp.Write(data); err != nil {
		temp.Close() //nolint:errcheck,gosec // write already failed
		return fmt.Errorf("write input record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close input record: %w", err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("publish input record: %w", err)
	}
	return nil
}

// The file: magic, format version, the identity of the program that wrote it,
// the stamp (the newest clock of a run that wrote it), the generation, the
// table of distinct paths, the entries, and the SHA-256 of everything before
// it. An entry is its table and key, its generation, its value, and its parts,
// each a path's index in the table and a stat.
var recordMagic = []byte("PNIR")

const recordFormatVersion uint32 = 1

var errRecordCorrupt = errors.New("inputrecord: damaged record")

type recordFile struct {
	identity   string
	stamp      int64
	generation uint64
	entries    map[recordKey]recordEntry
}

func encodeRecord(f recordFile) []byte {
	keys := make([]recordKey, 0, len(f.entries))
	pathIndex := map[string]uint32{}
	var paths []string
	for k, e := range f.entries {
		keys = append(keys, k)
		for _, p := range e.parts {
			if _, ok := pathIndex[p.Path]; !ok {
				pathIndex[p.Path] = uint32(len(paths)) //nolint:gosec // bounded by maxParts
				paths = append(paths, p.Path)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].table != keys[j].table {
			return keys[i].table < keys[j].table
		}
		return keys[i].key < keys[j].key
	})
	var buf bytes.Buffer
	buf.Write(recordMagic)
	putRecordU32(&buf, recordFormatVersion)
	putRecordBytes(&buf, []byte(f.identity))
	putRecordU64(&buf, uint64(f.stamp)) //nolint:gosec // a clock in ns, positive
	putRecordU64(&buf, f.generation)
	putRecordU32(&buf, uint32(len(paths))) //nolint:gosec // bounded by maxParts
	for _, p := range paths {
		putRecordBytes(&buf, []byte(p))
	}
	putRecordU32(&buf, uint32(len(keys))) //nolint:gosec // bounded by maxEntries
	for _, k := range keys {
		e := f.entries[k]
		putRecordBytes(&buf, []byte(k.table))
		putRecordBytes(&buf, []byte(k.key))
		putRecordU64(&buf, e.seen)
		putRecordBytes(&buf, e.value)
		putRecordU32(&buf, uint32(len(e.parts))) //nolint:gosec // bounded by maxParts
		for _, p := range e.parts {
			putRecordU32(&buf, pathIndex[p.Path])
			putRecordStat(&buf, p.Stat)
		}
	}
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func decodeRecord(data []byte) (recordFile, error) {
	if len(data) < len(recordMagic)+sha256.Size || !bytes.Equal(data[:len(recordMagic)], recordMagic) {
		return recordFile{}, errRecordCorrupt
	}
	body, sum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if want := sha256.Sum256(body); !bytes.Equal(want[:], sum) {
		return recordFile{}, errRecordCorrupt
	}
	d := recordDecoder{data: body[len(recordMagic):]}
	if d.u32() != recordFormatVersion {
		return recordFile{}, errRecordCorrupt
	}
	f := recordFile{identity: string(d.bytes())}
	f.stamp = int64(d.u64()) //nolint:gosec // written from a positive clock
	f.generation = d.u64()
	paths := make([]string, d.count(4))
	for i := range paths {
		paths[i] = string(d.bytes())
	}
	n := d.count(8)
	f.entries = make(map[recordKey]recordEntry, n)
	for range n {
		k := recordKey{table: string(d.bytes()), key: string(d.bytes())}
		e := recordEntry{seen: d.u64(), value: d.bytes()}
		e.parts = make([]RecordPart, d.count(4+recordStatSize))
		for i := range e.parts {
			index := d.u32()
			if int(index) >= len(paths) {
				d.err = errRecordCorrupt
				break
			}
			e.parts[i] = RecordPart{Path: paths[index], Stat: d.stat()}
		}
		if d.err != nil {
			return recordFile{}, d.err
		}
		f.entries[k] = e
	}
	if d.err != nil || len(d.data) != 0 {
		return recordFile{}, errRecordCorrupt
	}
	return f, nil
}

const recordStatSize = 8 + 8 + 4 + 4 + 4 + 8 + 8 + 8

func putRecordU32(buf *bytes.Buffer, v uint32) { _ = binary.Write(buf, binary.LittleEndian, v) }
func putRecordU64(buf *bytes.Buffer, v uint64) { _ = binary.Write(buf, binary.LittleEndian, v) }

func putRecordBytes(buf *bytes.Buffer, b []byte) {
	putRecordU32(buf, uint32(len(b))) //nolint:gosec // paths, keys and values well under 4 GiB
	buf.Write(b)
}

func putRecordStat(buf *bytes.Buffer, st Stat) {
	putRecordU64(buf, st.Dev)
	putRecordU64(buf, st.Ino)
	putRecordU32(buf, st.Mode)
	putRecordU32(buf, st.UID)
	putRecordU32(buf, st.GID)
	putRecordU64(buf, uint64(st.Size))       //nolint:gosec // a size, non-negative
	putRecordU64(buf, uint64(st.ModNano))    //nolint:gosec // round-trips through int64
	putRecordU64(buf, uint64(st.ChangeNano)) //nolint:gosec // round-trips through int64
}

type recordDecoder struct {
	data []byte
	err  error
}

func (d *recordDecoder) take(n int) []byte {
	if d.err != nil || n < 0 || n > len(d.data) {
		d.err = errRecordCorrupt
		return nil
	}
	out := d.data[:n]
	d.data = d.data[n:]
	return out
}

func (d *recordDecoder) u32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (d *recordDecoder) u64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (d *recordDecoder) bytes() []byte {
	n := d.u32()
	return bytes.Clone(d.take(int(n)))
}

// count reads a count of items of at least minSize bytes each, refusing one
// the remaining bytes cannot hold.
func (d *recordDecoder) count(minSize int) int {
	n := int(d.u32())
	if d.err == nil && n*minSize > len(d.data) {
		d.err = errRecordCorrupt
		return 0
	}
	return n
}

func (d *recordDecoder) stat() Stat {
	return Stat{
		Dev:        d.u64(),
		Ino:        d.u64(),
		Mode:       d.u32(),
		UID:        d.u32(),
		GID:        d.u32(),
		Size:       int64(d.u64()), //nolint:gosec // round-trips through uint64
		ModNano:    int64(d.u64()), //nolint:gosec // round-trips through uint64
		ChangeNano: int64(d.u64()), //nolint:gosec // round-trips through uint64
	}
}

// Hold reports whether every part's path has, now, the stat recorded with
// it. It takes each path's stat once per run, until Forget drops it.
func (r *Record) Hold(parts []RecordPart) bool {
	for _, part := range parts {
		st, ok := r.heldStat(part.Path)
		if part.Absent() {
			if ok {
				return false
			}
			continue
		}
		if !ok || st != part.Stat {
			return false
		}
	}
	return true
}

// AbsentPart is the part of a path the computation found absent: it holds
// while nothing exists at the path. A caller records one only for a path it
// found absent when it read it and finds absent again when it records.
func AbsentPart(path string) RecordPart { return RecordPart{Path: path} }

// Absent reports whether the part says its path is absent (AbsentPart).
func (p RecordPart) Absent() bool { return p.Stat == Stat{} }

func (r *Record) heldStat(path string) (Stat, bool) {
	k := comparisonPath(path)
	r.statsMu.Lock()
	held, found := r.stats[k]
	r.statsMu.Unlock()
	if found {
		return held.st, held.ok
	}
	st, ok := recordLstat(path)
	r.statsMu.Lock()
	if r.stats == nil {
		r.stats, r.under = map[string]heldStat{}, map[string][]string{}
	}
	r.stats[k] = heldStat{st: st, ok: ok}
	for parent := slashParent(k); ; parent = slashParent(parent) {
		r.under[parent] = append(r.under[parent], k)
		if slashParent(parent) == parent {
			break
		}
	}
	r.statsMu.Unlock()
	return st, ok
}

// Forget drops the stats Hold took at, under and above each of paths: a
// write of the run there may have moved them. The caller names each path as
// written and with its links followed. A nil r forgets nothing.
func (r *Record) Forget(paths []string) {
	if r == nil {
		return
	}
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	if len(r.stats) == 0 {
		return
	}
	for _, p := range paths {
		root := comparisonPath(p)
		delete(r.stats, root)
		for _, below := range r.under[root] {
			delete(r.stats, below)
		}
		delete(r.under, root)
		for parent := slashParent(root); ; parent = slashParent(parent) {
			delete(r.stats, parent)
			if slashParent(parent) == parent {
				break
			}
		}
	}
}

// ForgetAll drops every stat Hold took: the run wrote where it cannot name.
func (r *Record) ForgetAll() {
	if r == nil {
		return
	}
	r.statsMu.Lock()
	r.stats, r.under = nil, nil
	r.statsMu.Unlock()
}

// comparisonPath is path absolute, cleaned, in slash form and in lower case,
// so a write spelled with another case on a filesystem that folds it reaches
// the stat (the store's regions compare the same way).
func comparisonPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
}

// slashParent is the parent of a comparison-form path, itself at the root.
func slashParent(p string) string {
	i := strings.LastIndex(p, "/")
	switch {
	case i < 0:
		return p
	case i == 0:
		return "/"
	default:
		return p[:i]
	}
}

// OpenRecordAt opens the record file at path as OpenRecord does, with clock
// in place of the filesystem's: a part is recordable when its times are older
// than clock by Margin. It is for a test that writes the tree and records it
// in the same second; a run opens its record with OpenRecord.
func OpenRecordAt(path, identity string, clock int64) *Record {
	r := newRecord(path, identity)
	dir := filepath.Dir(path)
	if identity == "" || !recordLocal(dir) {
		return r
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r
	}
	st, ok := recordLstat(dir)
	if !ok {
		return r
	}
	r.dev, r.clock = st.Dev, clock
	r.now = func(string) (int64, error) { return clock, nil }
	r.load()
	return r
}
