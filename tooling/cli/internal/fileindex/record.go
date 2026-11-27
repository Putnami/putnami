package fileindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
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

// RecordFilename is the record's filename under a workspace's `.putnami/`.
const RecordFilename = "input-record"

// recordLockName is the lock a saving run holds while it merges into the file.
const recordLockName = "input-record.lock"

// RecordScheme is the version of what the record's tables hold and how their
// witnesses are taken. It is part of the record's identity: a CLI that
// changes either changes it, and loads an older record empty.
const RecordScheme = "1"

// recordMaxEntries bounds the entries one file holds, and recordMaxParts the parts of
// all its entries; past either, a Save keeps only what its run recorded or
// confirmed.
var (
	recordMaxEntries = 1 << 14
	recordMaxParts   = 1 << 20
)

// RecordPart is one path a value's computation read, with the stat it had: a file
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

	// sampleRate is the share of answers a caller checks (Sample), and
	// discarded the reason the record was discarded (Discard), "" while it
	// is not.
	sampleRate float64
	discarded  string
	onDiscard  func(reason string)

	// stats holds the stats the run took to check witnesses (Hold): one stat
	// per path per run, dropped by the run's writes (Forget). under maps a
	// comparison-form directory to the held paths below it, so a write drops
	// them without a scan of every held stat.
	// stats is keyed by the exact cleaned path; folded and under are keyed
	// by the comparison form (comparisonPath), so a write spelled in another
	// case still drops the stat. forgets counts Forget calls: a stat taken
	// across one is returned but not held.
	statsMu sync.Mutex
	stats   map[string]heldStat
	folded  map[string][]string
	under   map[string][]string
	forgets uint64
}

type heldStat struct {
	st Stat
	ok bool
}

// RecordPath is the record file of the workspace at root.
func RecordPath(root string) string {
	return filepath.Join(root, ".putnami", RecordFilename)
}

// RecordIdentity is the identity a record of the running program is written and
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

// OpenRecord reads the filesystem clock in the record's directory and loads the
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

// OpenRecordReadOnly loads the record file at path to answer values, and writes
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

// minimumSamples is how many answers a run checks at least, and
// minimumSampleRate the least share of answers it checks (Sample).
const (
	minimumSamples    = 16
	minimumSampleRate = 0.01
)

// Sample reports whether the caller checks the value the record just
// answered by computing it again: each answer with a probability of 16 over
// the entries the record loaded, and at least 1 %. A value that differs is a
// witness the record should not have trusted, and the caller discards the
// record (Discard).
func (r *Record) Sample() bool {
	if !r.enabled() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sampleRate < 0 {
		return false
	}
	if r.sampleRate == 0 {
		r.sampleRate = minimumSampleRate
		if n := len(r.loaded); n > 0 && float64(minimumSamples)/float64(n) > r.sampleRate {
			r.sampleRate = float64(minimumSamples) / float64(n)
		}
	}
	return rand.Float64() < r.sampleRate //nolint:gosec // a sample, not a secret
}

// SetSampleRate sets the share of answers Sample checks: 1 checks every one,
// a negative rate none, and 0 restores the default.
func (r *Record) SetSampleRate(rate float64) {
	r.mu.Lock()
	r.sampleRate = rate
	r.mu.Unlock()
}

// Discard drops every entry of the record for the rest of the process, which
// then answers and records nothing, and makes its save write an empty record:
// a value it answered differed from the value computed again, so no entry of
// it is trusted. reason says which; the first reason is kept.
func (r *Record) Discard(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	first := r.discarded == ""
	if first {
		r.discarded = reason
	}
	r.loaded, r.fresh, r.confirmed, r.stale = map[recordKey]recordEntry{}, map[recordKey]recordEntry{}, map[recordKey]bool{}, map[recordKey]bool{}
	notify := r.onDiscard
	r.mu.Unlock()
	if first && notify != nil {
		notify(reason)
	}
}

// OnDiscard makes notify be called once, with its reason, when the record is
// discarded, from the goroutine that discards it: the run reports it while it
// renders. A nil r does nothing.
func (r *Record) OnDiscard(notify func(reason string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onDiscard = notify
	r.mu.Unlock()
}

// Forgets counts the writes the run forgot so far (Forget, ForgetAll): a
// caller that compares an answer with a collection taken later tells a write
// of the run between the two from a witness that failed.
func (r *Record) Forgets() uint64 {
	if r == nil {
		return 0
	}
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.forgets
}

// Discarded returns the reason the record was discarded, "" when it was not.
func (r *Record) Discarded() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discarded
}

// currentRecord is the record the run's computations consult; nil answers nothing.
var currentRecord atomic.Pointer[Record]

// UseRecord makes r the record CurrentRecord returns, and returns a function that
// restores the previous one.
func UseRecord(r *Record) (restore func()) {
	previous := currentRecord.Swap(r)
	return func() { currentRecord.CompareAndSwap(r, previous) }
}

// CurrentRecord is the record of the run, or nil.
func CurrentRecord() *Record {
	r := currentRecord.Load()
	if !r.enabled() {
		return nil
	}
	return r
}

// Drop drops the value recorded in table at k, from this process and from the
// file at the next save: a caller found it differs from a computation the
// run cannot tell from a write of its own. A nil r drops nothing.
func (r *Record) Drop(table, k string) {
	if r == nil {
		return
	}
	id := recordKey{table, k}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, isLoaded := r.loaded[id]; isLoaded {
		r.stale[id] = true
	}
	delete(r.fresh, id)
	delete(r.confirmed, id)
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
	if r.discarded != "" {
		r.mu.Unlock()
		return nil, nil, false
	}
	e, found := r.fresh[id]
	_, isLoaded := r.loaded[id]
	if !found && !r.stale[id] {
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
		if !filepath.IsAbs(part.Path) || !part.Absent() && !r.Recordable(part.Stat) && !r.recordableLink(part) {
			return false
		}
	}
	e := recordEntry{value: bytes.Clone(value), parts: append([]RecordPart(nil), parts...)}
	r.mu.Lock()
	if r.discarded != "" {
		r.mu.Unlock()
		return false
	}
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
	discarded := r.discarded != ""
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
	if len(fresh) == 0 && len(stale) == 0 && !aging && !discarded {
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
	if discarded {
		// A discarded record writes no entry: every run after it computes
		// each value again, and records it anew.
		return writeRecordAtomic(r.file, encodeRecord(recordFile{identity: r.identity, stamp: stamp, entries: merged}))
	}
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
	// What this run recorded or confirmed can itself exceed the caps: keep
	// entries in key order until either is reached.
	if len(merged) > recordMaxEntries || recordParts(merged) > recordMaxParts {
		keys := make([]recordKey, 0, len(merged))
		for k := range merged {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return keys[i].table < keys[j].table || keys[i].table == keys[j].table && keys[i].key < keys[j].key
		})
		capped, parts := make(map[recordKey]recordEntry, recordMaxEntries), 0
		for _, k := range keys {
			e := merged[k]
			if len(capped) == recordMaxEntries || parts+len(e.parts) > recordMaxParts {
				break
			}
			capped[k], parts = e, parts+len(e.parts)
		}
		merged = capped
	}
	return writeRecordAtomic(r.file, encodeRecord(recordFile{identity: r.identity, stamp: stamp, generation: generation, entries: merged}))
}

// recordParts counts the parts of entries.
func recordParts(entries map[recordKey]recordEntry) int {
	parts := 0
	for _, e := range entries {
		parts += len(e.parts)
	}
	return parts
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
	// A run's write named by its physical path does not reach a stat held
	// under a path spelled through a link: the parts of a value read through
	// one are stated again at every check, and none is held.
	stat := r.heldStat
	for _, part := range parts {
		if part.Stat.Mode&linkModeMask == linkMode {
			stat = func(path string) (Stat, bool) { return recordLstat(filepath.Clean(path)) }
			break
		}
	}
	for _, part := range parts {
		st, ok := stat(part.Path)
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

// linkModeMask and linkMode are the file-type bits of a stat's mode and those
// of a symbolic link, as the platform's stat decodes them.
const (
	linkModeMask = 0o170000
	linkMode     = 0o120000
)

// recordableLink reports whether part, the lstat of a link a computation met
// on the way to what it read, may be recorded on another device than the
// record's: a link's own stat moves when it is replaced or retargeted, so it
// is enough on a trusted local file system, outside the margin.
func (r *Record) recordableLink(part RecordPart) bool {
	if part.Stat.Mode&linkModeMask != linkMode || !r.enabled() || r.readOnly || !recordLocal(filepath.Dir(part.Path)) {
		return false
	}
	limit := r.clock - int64(Margin)
	return part.Stat.ModNano < limit && part.Stat.ChangeNano < limit
}

// AbsentPart is the part of a path the computation found absent: it holds
// while nothing exists at the path. A caller records one only for a path it
// found absent when it read it and finds absent again when it records.
func AbsentPart(path string) RecordPart { return RecordPart{Path: path} }

// Absent reports whether the part says its path is absent (AbsentPart).
func (p RecordPart) Absent() bool { return p.Stat == Stat{} }

func (r *Record) heldStat(path string) (Stat, bool) {
	exact := filepath.Clean(path)
	r.statsMu.Lock()
	held, found := r.stats[exact]
	generation := r.forgets
	r.statsMu.Unlock()
	if found {
		return held.st, held.ok
	}
	st, ok := recordLstat(exact)
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	if r.forgets != generation {
		// A write was forgotten while the stat was taken: it may predate
		// the write, so it answers this call and is not held.
		return st, ok
	}
	if r.stats == nil {
		r.stats, r.folded, r.under = map[string]heldStat{}, map[string][]string{}, map[string][]string{}
	}
	r.stats[exact] = heldStat{st: st, ok: ok}
	k := comparisonPath(exact)
	r.folded[k] = append(r.folded[k], exact)
	for parent := slashParent(k); ; parent = slashParent(parent) {
		r.under[parent] = append(r.under[parent], exact)
		if slashParent(parent) == parent {
			break
		}
	}
	return st, ok
}

// Forget drops the stats Hold took at, under and above each of paths, in
// any spelling of their case: a write of the run there may have moved them.
// The caller names each path as written and with its links followed. A nil
// r forgets nothing.
func (r *Record) Forget(paths []string) {
	if r == nil {
		return
	}
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	r.forgets++
	if len(r.stats) == 0 {
		return
	}
	dropFolded := func(k string) {
		for _, exact := range r.folded[k] {
			delete(r.stats, exact)
		}
		delete(r.folded, k)
	}
	for _, p := range paths {
		root := comparisonPath(p)
		dropFolded(root)
		for _, below := range r.under[root] {
			delete(r.stats, below)
		}
		delete(r.under, root)
		for parent := slashParent(root); ; parent = slashParent(parent) {
			dropFolded(parent)
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
	r.forgets++
	r.stats, r.folded, r.under = nil, nil, nil
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

// LinkHops returns each symbolic link the resolution of the directory dir
// meets, as the path where it met it, and false when dir does not resolve: a
// missing element, or more than 255 links.
func LinkHops(dir string) ([]string, bool) {
	var hops []string
	current := string(filepath.Separator)
	pending := strings.FieldsFunc(dir, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) })
	for len(pending) > 0 {
		segment := pending[0]
		pending = pending[1:]
		switch segment {
		case ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, segment)
		info, err := os.Lstat(next)
		if err != nil {
			return nil, false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		hops = append(hops, next)
		link, err := os.Readlink(next)
		if err != nil || len(hops) > 255 {
			return nil, false
		}
		if filepath.IsAbs(link) {
			current = string(filepath.Separator)
		}
		pending = append(strings.FieldsFunc(link, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }), pending...)
	}
	return hops, true
}
