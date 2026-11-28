package sdd

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// FeatureFindingsMemo names the per-project memo the workspace feature
// validation keeps across runs, and the code that evaluates. The zero value
// keeps no memo.
//
// An entry holds one project's evaluation and every read it made through the
// reader, each with a digest of its answer. A run reuses an entry when the
// memo was written by the same code for the same workspace view and every
// read answers as it did; it evaluates the project otherwise. An evaluation is
// a function of the view, the project and the answers of its reads, so a
// reused entry is the evaluation the run would make, and the findings are the
// same bytes whether the memo hits, misses, is absent or is refused. A read of
// another project's file is a read like any other: a duplicate id or an
// unresolved contribution declared in project B moves the entry of every
// project whose evaluation read B's manifest.
type FeatureFindingsMemo struct {
	// Path is the memo file. Empty keeps no memo.
	Path string
	// Code identifies the code that evaluates, such as a digest of the
	// executable. A memo another code wrote is ignored. Empty keeps no memo.
	Code string
}

func (memo FeatureFindingsMemo) enabled() bool { return memo.Path != "" && memo.Code != "" }

const (
	// featureMemoMagic opens a memo file and names its format.
	featureMemoMagic = "putnami-sdd-features-memo/1\n"
	// maxFeatureMemoBytes bounds the memo file a run reads or writes.
	maxFeatureMemoBytes = 64 << 20
)

// featureOutcome is what BuildFeatureFindingsResult reads of one evaluation,
// and all the memo keeps of it.
type featureOutcome struct {
	// Snapshot reports whether the evaluation returned a snapshot.
	Snapshot    bool
	Diagnostics []diag.Diagnostic
	// Features counts each assessed feature under its source and id.
	Features []featureTally
	// Unclassified are the identities of the unclassified contributions.
	Unclassified []string
}

type featureTally struct {
	Key    string
	Counts FeatureCounts
}

func outcomeOf(result featureengine.Result) featureOutcome {
	outcome := featureOutcome{Snapshot: result.Snapshot != nil, Diagnostics: result.Diagnostics}
	if result.Snapshot == nil {
		return outcome
	}
	for _, feature := range result.Snapshot.Features {
		outcome.Features = append(outcome.Features, featureTally{
			Key:    feature.Source + "\x00" + feature.ID,
			Counts: summarizeFeatures(&featureengine.Snapshot{Features: []featureengine.FeatureAssessment{feature}}),
		})
	}
	for _, contribution := range result.Snapshot.Unclassified {
		identity := contribution.Identity
		outcome.Unclassified = append(outcome.Unclassified,
			identity.OwnerProject+"\x00"+string(identity.Kind)+"\x00"+identity.Subkind+"\x00"+identity.Key)
	}
	return outcome
}

// plus adds every count of other to counts.
func (counts FeatureCounts) plus(other FeatureCounts) FeatureCounts {
	counts.Features += other.Features
	counts.IncompleteFeatures += other.IncompleteFeatures
	counts.Requirements += other.Requirements
	counts.Evidence += other.Evidence
	counts.Verified += other.Verified
	counts.Missing += other.Missing
	counts.Stale += other.Stale
	counts.Contradicted += other.Contradicted
	counts.Unclassified += other.Unclassified
	return counts
}

// Read operations of the feature reader.
const (
	readFile    = 'f'
	readDir     = 'd'
	readBinding = 'b'
)

// readKey is one read of the feature reader: its operation and arguments.
type readKey struct {
	Op   byte
	Root string
	Path string
}

func compareReadKeys(left, right readKey) bool {
	if left.Op != right.Op {
		return left.Op < right.Op
	}
	if left.Root != right.Root {
		return left.Root < right.Root
	}
	return left.Path < right.Path
}

// tracingReader is the reader of one evaluation: it answers from the shared
// reader and records each read it is asked.
type tracingReader struct {
	shared *memoReader
	mu     sync.Mutex
	reads  map[readKey]struct{}
}

func newTracingReader(shared *memoReader) *tracingReader {
	return &tracingReader{shared: shared, reads: map[readKey]struct{}{}}
}

func (t *tracingReader) record(key readKey) {
	t.mu.Lock()
	t.reads[key] = struct{}{}
	t.mu.Unlock()
}

func (t *tracingReader) ReadFile(root, relative string) ([]byte, error) {
	t.record(readKey{readFile, root, relative})
	return t.shared.ReadFile(root, relative)
}

func (t *tracingReader) ReadDir(root, relative string) ([]featureengine.DirEntry, error) {
	t.record(readKey{readDir, root, relative})
	return t.shared.ReadDir(root, relative)
}

func (t *tracingReader) SourceBinding(root string) (string, error) {
	t.record(readKey{readBinding, root, ""})
	return t.shared.SourceBinding(root)
}

// keys returns the recorded reads, sorted.
func (t *tracingReader) keys() []readKey {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]readKey, 0, len(t.reads))
	for key := range t.reads {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return compareReadKeys(keys[i], keys[j]) })
	return keys
}

// answerDigest is the SHA-256 of one answer of the reader: what it read and
// the error it returned, each type and message of the error's chain, which is
// all an evaluation can tell of an error.
func answerDigest(op byte, data []byte, entries []featureengine.DirEntry, binding string, err error) [32]byte {
	hash := sha256.New()
	field := func(value []byte) {
		hash.Write(binary.AppendUvarint(nil, uint64(len(value))))
		hash.Write(value)
	}
	hash.Write([]byte{op})
	switch op {
	case readFile:
		field(data)
	case readDir:
		hash.Write(binary.AppendUvarint(nil, uint64(len(entries))))
		for _, entry := range entries {
			field([]byte(entry.Name))
			if entry.IsDirectory {
				hash.Write([]byte{1})
			} else {
				hash.Write([]byte{0})
			}
		}
	case readBinding:
		field([]byte(binding))
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		field([]byte(fmt.Sprintf("%T", cause)))
		field([]byte(cause.Error()))
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

// featureMemoFile is the payload of a memo file.
type featureMemoFile struct {
	// Code and View are the code that wrote the memo and the digest of the
	// workspace view its evaluations read.
	Code string
	View [32]byte
	// Reads are the reads of every entry, each once, with the digest of the
	// answer the writing run read.
	Reads   []featureMemoRead
	Entries []featureMemoEntry
}

type featureMemoRead struct {
	Key    readKey
	Answer [32]byte
}

type featureMemoEntry struct {
	Project string
	// Reads are indexes into the file's Reads.
	Reads   []int
	Outcome featureOutcome
}

// projectEvaluation is the outcome of one project's evaluation and the reads
// it made, which reused tells whether the memo answered.
type projectEvaluation struct {
	outcome featureOutcome
	reads   []readKey
	reused  bool
}

// featureMemo is the memo of one run: the entries an earlier run left that
// this run may reuse, and where to keep this run's.
type featureMemo struct {
	options FeatureFindingsMemo
	view    [32]byte
	prior   *featureMemoFile
	entries map[string]*featureMemoEntry

	mu      sync.Mutex
	current map[int]bool
}

// openFeatureMemo reads the memo options name for view. A memo that is
// absent, oversized, corrupt, written by another code or for another view
// gives no entry; it is never an error, since an evaluation replaces it.
func openFeatureMemo(options FeatureFindingsMemo, view *workspace.Workspace) *featureMemo {
	if !options.enabled() {
		return nil
	}
	digest, err := viewDigest(view)
	if err != nil {
		return nil
	}
	memo := &featureMemo{options: options, view: digest, entries: map[string]*featureMemoEntry{}, current: map[int]bool{}}
	prior, ok := readFeatureMemo(options.Path)
	if !ok || prior.Code != options.Code || prior.View != digest {
		return memo
	}
	for index := range prior.Entries {
		entry := &prior.Entries[index]
		if _, seen := memo.entries[entry.Project]; seen {
			memo.entries = map[string]*featureMemoEntry{}
			return memo
		}
		for _, read := range entry.Reads {
			if read < 0 || read >= len(prior.Reads) {
				memo.entries = map[string]*featureMemoEntry{}
				return memo
			}
		}
		memo.entries[entry.Project] = entry
	}
	memo.prior = prior
	return memo
}

// readFeatureMemo decodes the memo file at path, which holds the magic, the
// SHA-256 of the payload and the payload. ok is false for any file it does
// not hold whole and intact.
func readFeatureMemo(path string) (*featureMemoFile, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > maxFeatureMemoBytes {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFeatureMemoBytes+1))
	if err != nil || len(data) > maxFeatureMemoBytes || len(data) < len(featureMemoMagic)+sha256.Size {
		return nil, false
	}
	if string(data[:len(featureMemoMagic)]) != featureMemoMagic {
		return nil, false
	}
	data = data[len(featureMemoMagic):]
	payload := data[sha256.Size:]
	if sum := sha256.Sum256(payload); !bytes.Equal(sum[:], data[:sha256.Size]) {
		return nil, false
	}
	var memo featureMemoFile
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&memo); err != nil {
		return nil, false
	}
	return &memo, true
}

// reuse returns the outcome and reads of project's entry when every read of
// the entry answers as it did when the entry was written.
func (memo *featureMemo) reuse(project string, reader *memoReader) (projectEvaluation, bool) {
	if memo == nil {
		return projectEvaluation{}, false
	}
	entry, ok := memo.entries[project]
	if !ok {
		return projectEvaluation{}, false
	}
	keys := make([]readKey, 0, len(entry.Reads))
	for _, index := range entry.Reads {
		if !memo.answersAsBefore(index, reader) {
			return projectEvaluation{}, false
		}
		keys = append(keys, memo.prior.Reads[index].Key)
	}
	return projectEvaluation{outcome: entry.Outcome, reads: keys, reused: true}, true
}

// answersAsBefore reports whether the prior read at index answers as it did.
func (memo *featureMemo) answersAsBefore(index int, reader *memoReader) bool {
	memo.mu.Lock()
	known, ok := memo.current[index]
	memo.mu.Unlock()
	if ok {
		return known
	}
	read := memo.prior.Reads[index]
	same := reader.digest(read.Key) == read.Answer
	memo.mu.Lock()
	memo.current[index] = same
	memo.mu.Unlock()
	return same
}

// save writes the evaluations of projects as the memo, unless every one was
// reused from a memo that held these projects alone. A memo it cannot write
// is left as it was.
func (memo *featureMemo) save(reader *memoReader, projects []*workspace.Project, evaluations []projectEvaluation) {
	if memo == nil {
		return
	}
	unchanged := memo.prior != nil && len(memo.prior.Entries) == len(projects)
	for index, project := range projects {
		if !evaluations[index].reused || memo.entries[project.ID] == nil {
			unchanged = false
		}
	}
	if unchanged {
		return
	}
	file := featureMemoFile{Code: memo.options.Code, View: memo.view}
	indexes := map[readKey]int{}
	var keys []readKey
	for _, evaluation := range evaluations {
		for _, key := range evaluation.reads {
			if _, seen := indexes[key]; !seen {
				indexes[key] = -1
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return compareReadKeys(keys[i], keys[j]) })
	for index, key := range keys {
		indexes[key] = index
		file.Reads = append(file.Reads, featureMemoRead{Key: key, Answer: reader.digest(key)})
	}
	for index, project := range projects {
		entry := featureMemoEntry{Project: project.ID, Outcome: evaluations[index].outcome}
		for _, key := range evaluations[index].reads {
			entry.Reads = append(entry.Reads, indexes[key])
		}
		sort.Ints(entry.Reads)
		file.Entries = append(file.Entries, entry)
	}
	encoded, err := encodeFeatureMemo(&file)
	if err != nil || len(encoded) > maxFeatureMemoBytes {
		return
	}
	// A reader sees the earlier file or this one whole, never a part, and two
	// runs that write at once leave one of their files.
	_ = atomicWriteFile(memo.options.Path, encoded)
}

// encodeFeatureMemo is the memo file of payload: the magic, the SHA-256 of
// the payload and the payload.
func encodeFeatureMemo(payload *featureMemoFile) ([]byte, error) {
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(payload); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded.Bytes())
	file := make([]byte, 0, len(featureMemoMagic)+len(sum)+encoded.Len())
	return append(append(append(file, featureMemoMagic...), sum[:]...), encoded.Bytes()...), nil
}

// viewDigest is the SHA-256 of the workspace view an evaluation reads, every
// member of it but the indexes and the graph NewWorkspace derives from the
// projects.
func viewDigest(view *workspace.Workspace) ([32]byte, error) {
	encoded, err := json.Marshal(struct {
		Name, Version, Root string
		Config              any
		Projects            []*workspace.Project
		Warnings            []string
		WarningCodes        []workspace.WarningCode
		Tree                any
		NamesNoCommit       bool
	}{view.Name, view.Version, view.Root, view.Config, view.Projects, view.Warnings, view.WarningCodes, view.Tree, view.NamesNoCommit})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
