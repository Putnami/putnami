// The release record keeps the release baselines a run read for the runs
// that follow it (tooling/cli ADR 0113 §4).
//
// A release baseline (ADR 0110) is a function of commit IDs, the project's
// prefix and its line's tag pattern: Git names its objects by their content,
// so the object a commit holds at a path, and the commits between two commits
// that touch it, never change. The record therefore keys each baseline by the
// project directory, the tag pattern and the witness of what names those
// commits (HEAD, every tag with the object it peels to, the tags HEAD reaches,
// the repository state,
// the absence of history rewrites, the Git configuration and environment),
// which the caller computes and the record treats as an opaque digest. A
// lookup under another witness misses, and the caller reads the baseline
// again.
//
// The file shares the envelope of the file index (internal/fileindex): it
// names the program that wrote it, so another CLI build loads it empty; it
// ends with the SHA-256 of what precedes it, so a damaged file loads empty;
// concurrent runs merge into it under an exclusive lock and rename a complete
// file into place; an entry no saving run recorded or confirmed in its last
// keptRuns saves is dropped; and a run that only confirms entries writes
// nothing until they come halfway to being dropped. The witness holds no
// stat, so no clock enters the record.

package fileindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/sdk/extension/filelock"
)

// ReleaseRecordFilename is the record's filename under a workspace's `.putnami/`.
const ReleaseRecordFilename = "release-record"

// releaseRecordLockName is the lock a Save takes to merge into the file on disk.
const releaseRecordLockName = "release-record.lock"

// releaseRecordScheme names the record's reading: a change in what a recorded value means
// changes it, and the record written under the old one loads empty.
const releaseRecordScheme = "release-record/1"

// maxReleaseRecordEntries bounds the record: about 150 baselines per HEAD on this
// repository, so tens of commits' worth.
var maxReleaseRecordEntries = 1 << 14

// maxReleaseRecordValue bounds one recorded value.
const maxReleaseRecordValue = 1 << 16

// ReleaseWitness is the digest of what names the commits a baseline was read from.
type ReleaseWitness [sha256.Size]byte

type releaseRecordKey struct {
	dir, pattern string
	witness      ReleaseWitness
}

type releaseRecordEntry struct {
	value []byte
	seen  uint64
}

// ReleaseRecord is the release record of one worktree, as a run reads and extends
// it. A nil ReleaseRecord answers nothing and records nothing.
type ReleaseRecord struct {
	file       string
	identity   string
	readOnly   bool
	generation uint64

	mu        sync.Mutex
	loaded    map[releaseRecordKey]releaseRecordEntry
	fresh     map[releaseRecordKey]releaseRecordEntry
	confirmed map[releaseRecordKey]bool
	// rechecks takes each witness a fresh entry was stored under again when
	// the record is saved: an entry whose witness moved meanwhile is not
	// written.
	rechecks map[ReleaseWitness]func() (ReleaseWitness, bool)

	hits atomic.Int64
}

var currentReleaseRecord atomic.Pointer[ReleaseRecord]

// UseReleaseRecord makes r the process's current release record and returns a function that
// restores the previous one.
func UseReleaseRecord(r *ReleaseRecord) (restore func()) {
	previous := currentReleaseRecord.Swap(r)
	return func() { currentReleaseRecord.Store(previous) }
}

// CurrentReleaseRecord returns the process's current release record, nil when none is in use.
func CurrentReleaseRecord() *ReleaseRecord { return currentReleaseRecord.Load() }

// ReleaseRecordPath is the record file of the workspace at root.
func ReleaseRecordPath(root string) string {
	return filepath.Join(root, ".putnami", ReleaseRecordFilename)
}

// ReleaseRecordIdentity names the record a program writes, from its executable identity
// (fileindex.ExecutableIdentity): "" when the program has none, under which
// the record answers and records nothing.
func ReleaseRecordIdentity(executable string) string {
	if executable == "" {
		return ""
	}
	return releaseRecordScheme + "\x00" + executable
}

// OpenReleaseRecord loads the record file at path under identity (see ReleaseRecordIdentity). A
// missing, damaged or foreign file loads as an empty record.
func OpenReleaseRecord(path, identity string) *ReleaseRecord {
	r := &ReleaseRecord{file: path, identity: identity, loaded: map[releaseRecordKey]releaseRecordEntry{}, fresh: map[releaseRecordKey]releaseRecordEntry{}, confirmed: map[releaseRecordKey]bool{},
		rechecks: map[ReleaseWitness]func() (ReleaseWitness, bool){}}
	if identity == "" {
		return r
	}
	r.loadReleaseRecord()
	return r
}

// OpenReleaseRecordReadOnly loads the record file at path to answer baselines, and writes
// nothing: its Save does nothing. It is the record of a run that only plans.
func OpenReleaseRecordReadOnly(path, identity string) *ReleaseRecord {
	r := OpenReleaseRecord(path, identity)
	r.readOnly = true
	return r
}

func (r *ReleaseRecord) loadReleaseRecord() {
	data, err := os.ReadFile(r.file) //nolint:gosec // the record under the workspace's .putnami directory
	if err != nil {
		return
	}
	disk, err := decodeReleaseRecord(data)
	if err != nil || disk.identity != r.identity {
		return
	}
	r.loaded, r.generation = disk.entries, disk.generation
}

func (r *ReleaseRecord) recording() bool { return r != nil && r.identity != "" }

// Lookup returns the value recorded for the baseline of dir under pattern
// with witness.
func (r *ReleaseRecord) Lookup(dir, pattern string, witness ReleaseWitness) ([]byte, bool) {
	if !r.recording() {
		return nil, false
	}
	k := releaseRecordKey{dir: dir, pattern: pattern, witness: witness}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.fresh[k]; ok {
		r.hits.Add(1)
		return e.value, true
	}
	e, ok := r.loaded[k]
	if !ok {
		return nil, false
	}
	r.confirmed[k] = true
	r.hits.Add(1)
	return e.value, true
}

// Store records value as the baseline of dir under pattern with witness.
// recheck takes the witness again: Save calls it once per witness before it
// writes, and writes no entry whose witness it does not answer again, so a
// baseline read while HEAD, a tag or the configuration moved is not kept.
func (r *ReleaseRecord) Store(dir, pattern string, witness ReleaseWitness, value []byte, recheck func() (ReleaseWitness, bool)) {
	if !r.recording() || r.readOnly || len(value) > maxReleaseRecordValue {
		return
	}
	k := releaseRecordKey{dir: dir, pattern: pattern, witness: witness}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.loaded[k]; ok && bytes.Equal(e.value, value) {
		r.confirmed[k] = true
		return
	}
	r.fresh[k] = releaseRecordEntry{value: bytes.Clone(value)}
	if _, ok := r.rechecks[witness]; !ok {
		r.rechecks[witness] = recheck
	}
}

// Hits is the number of lookups the record answered.
func (r *ReleaseRecord) Hits() int64 {
	if r == nil {
		return 0
	}
	return r.hits.Load()
}

// Save merges what this process recorded into the record file, under its
// lock, and writes it atomically. It writes nothing when this process
// recorded nothing new and confirmed no entry close to being dropped.
func (r *ReleaseRecord) Save() error {
	if !r.recording() || r.readOnly || r.file == "" {
		return nil
	}
	r.mu.Lock()
	fresh := make(map[releaseRecordKey]releaseRecordEntry, len(r.fresh))
	for k, e := range r.fresh {
		fresh[k] = e
	}
	rechecks := make(map[ReleaseWitness]func() (ReleaseWitness, bool), len(r.rechecks))
	for w, recheck := range r.rechecks {
		rechecks[w] = recheck
	}
	confirmed := make(map[releaseRecordKey]bool, len(r.confirmed))
	aging := false
	for k := range r.confirmed {
		confirmed[k] = true
		if r.generation-r.loaded[k].seen >= keptRuns/2 {
			aging = true
		}
	}
	r.mu.Unlock()
	// Each witness is taken again, outside the lock: one that moved since
	// its entries were read leaves them out.
	holds := make(map[ReleaseWitness]bool, len(rechecks))
	for w, recheck := range rechecks {
		again, ok := ReleaseWitness{}, false
		if recheck != nil {
			again, ok = recheck()
		}
		holds[w] = ok && again == w
	}
	for k := range fresh {
		if !holds[k.witness] {
			delete(fresh, k)
		}
	}
	if len(fresh) == 0 && !aging {
		return nil
	}

	dir := filepath.Dir(r.file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create release record directory: %w", err)
	}
	lock, err := filelock.Acquire(filepath.Join(dir, releaseRecordLockName), true, false)
	if err != nil {
		return fmt.Errorf("lock release record: %w", err)
	}
	defer lock.Release() //nolint:errcheck // releasing a held lock

	merged := map[releaseRecordKey]releaseRecordEntry{}
	var generation uint64
	if data, readErr := os.ReadFile(r.file); readErr == nil {
		if disk, decodeErr := decodeReleaseRecord(data); decodeErr == nil && disk.identity == r.identity {
			merged, generation = disk.entries, disk.generation
		}
	}
	generation++
	for k := range confirmed {
		if e, ok := merged[k]; ok {
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
	if len(merged) > maxReleaseRecordEntries {
		kept := make(map[releaseRecordKey]releaseRecordEntry, len(fresh)+len(confirmed))
		for k, e := range merged {
			if _, ok := fresh[k]; ok || confirmed[k] {
				kept[k] = e
			}
		}
		merged = kept
	}
	if len(merged) > maxReleaseRecordEntries {
		// What this run recorded and confirmed alone exceeds the bound: the
		// first keys in their order are kept.
		keys := make([]releaseRecordKey, 0, len(merged))
		for k := range merged {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareReleaseRecordKeys)
		for _, k := range keys[maxReleaseRecordEntries:] {
			delete(merged, k)
		}
	}
	return writeReleaseRecordAtomic(r.file, encodeReleaseRecord(releaseRecordFile{identity: r.identity, generation: generation, entries: merged}))
}

func compareReleaseRecordKeys(a, b releaseRecordKey) int {
	if c := strings.Compare(a.dir, b.dir); c != 0 {
		return c
	}
	if c := strings.Compare(a.pattern, b.pattern); c != 0 {
		return c
	}
	return bytes.Compare(a.witness[:], b.witness[:])
}

func writeReleaseRecordAtomic(target string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), "."+ReleaseRecordFilename+".*")
	if err != nil {
		return fmt.Errorf("stage release record: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name) //nolint:errcheck // no-op once renamed into place
	if _, err := temp.Write(data); err != nil {
		temp.Close() //nolint:errcheck,gosec // write already failed
		return fmt.Errorf("write release record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close release record: %w", err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("publish release record: %w", err)
	}
	return nil
}

// The file: magic, format version, the identity of the program that wrote it,
// the generation (the number of saving runs), the entry count, the entries,
// and the SHA-256 of everything before it. An entry is its directory, its
// pattern, its witness, its generation and its value.
var releaseRecordMagic = []byte("PNRR")

const releaseRecordFormatVersion uint32 = 1

var errReleaseRecordCorrupt = errors.New("fileindex: damaged release record")

type releaseRecordFile struct {
	identity   string
	generation uint64
	entries    map[releaseRecordKey]releaseRecordEntry
}

func encodeReleaseRecord(f releaseRecordFile) []byte {
	var buf bytes.Buffer
	buf.Write(releaseRecordMagic)
	writeReleaseRecordUint32(&buf, releaseRecordFormatVersion)
	writeReleaseRecordString(&buf, f.identity)
	_ = binary.Write(&buf, binary.LittleEndian, f.generation)
	writeReleaseRecordUint32(&buf, uint32(len(f.entries))) //nolint:gosec // bounded by maxReleaseRecordEntries
	for k, e := range f.entries {
		writeReleaseRecordString(&buf, k.dir)
		writeReleaseRecordString(&buf, k.pattern)
		buf.Write(k.witness[:])
		_ = binary.Write(&buf, binary.LittleEndian, e.seen)
		writeReleaseRecordString(&buf, string(e.value))
	}
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writeReleaseRecordUint32(buf *bytes.Buffer, n uint32) {
	_ = binary.Write(buf, binary.LittleEndian, n)
}

func writeReleaseRecordString(buf *bytes.Buffer, s string) {
	writeReleaseRecordUint32(buf, uint32(len(s))) //nolint:gosec // strings of the record are small
	buf.WriteString(s)
}

// releaseRecordEntryMin is the smallest encoded entry: three empty strings, a witness and
// a generation.
const releaseRecordEntryMin = 4 + 4 + sha256.Size + 8 + 4

func decodeReleaseRecord(data []byte) (releaseRecordFile, error) {
	var f releaseRecordFile
	header := len(releaseRecordMagic) + 4 + 4 + 8 + 4
	if len(data) < header+sha256.Size {
		return f, errReleaseRecordCorrupt
	}
	body, sum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if want := sha256.Sum256(body); !bytes.Equal(want[:], sum) {
		return f, errReleaseRecordCorrupt
	}
	if !bytes.Equal(body[:len(releaseRecordMagic)], releaseRecordMagic) {
		return f, errReleaseRecordCorrupt
	}
	r := bytes.NewReader(body[len(releaseRecordMagic):])
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
	if binary.Read(r, binary.LittleEndian, &version) != nil || version != releaseRecordFormatVersion {
		return f, errReleaseRecordCorrupt
	}
	identity, ok := readString()
	if !ok {
		return f, errReleaseRecordCorrupt
	}
	f.identity = identity
	if binary.Read(r, binary.LittleEndian, &f.generation) != nil ||
		binary.Read(r, binary.LittleEndian, &count) != nil {
		return f, errReleaseRecordCorrupt
	}
	if int64(count) > int64(r.Len())/releaseRecordEntryMin {
		return f, errReleaseRecordCorrupt
	}
	f.entries = make(map[releaseRecordKey]releaseRecordEntry, count)
	for i := uint32(0); i < count; i++ {
		var k releaseRecordKey
		var e releaseRecordEntry
		if k.dir, ok = readString(); !ok {
			return f, errReleaseRecordCorrupt
		}
		if k.pattern, ok = readString(); !ok {
			return f, errReleaseRecordCorrupt
		}
		if _, err := io.ReadFull(r, k.witness[:]); err != nil {
			return f, errReleaseRecordCorrupt
		}
		if binary.Read(r, binary.LittleEndian, &e.seen) != nil {
			return f, errReleaseRecordCorrupt
		}
		value, ok := readString()
		if !ok || len(value) > maxReleaseRecordValue {
			return f, errReleaseRecordCorrupt
		}
		e.value = []byte(value)
		f.entries[k] = e
	}
	if r.Len() != 0 {
		return f, errReleaseRecordCorrupt
	}
	return f, nil
}
