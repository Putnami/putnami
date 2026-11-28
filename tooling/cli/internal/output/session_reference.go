package output

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/machine"
)

// CachedRecordsProjection is the version of CachedRecords: the bytes it
// rebuilds for a reference. It changes whenever those bytes would for the same
// entry and task:end, so that a reader of another version refuses a reference
// rather than rebuilding other bytes than its producer digested.
const CachedRecordsProjection = 1

// cacheReferences is a session recorder's state while it records reused tasks
// by reference to their cache entry (ADR 0137). Its recorder's lock guards it.
type cacheReferences struct {
	// held is, per task key, the events a cache entry supplied that the task
	// replayed, kept as they are until the task ends.
	held map[string][]jobs.RawJobEvent
	// results are, per entry key, the digest of the result.json the stream
	// references.
	results map[string]string
	// pin records the results the stream references, once the session ends.
	pin func(results []PinnedReference) error
}

// PinnedReference is one result a session's stream references: the key of
// the task-owned entry, and the SHA-256 of its result.json.
type PinnedReference struct {
	Key    string
	Result string
}

// RecordReusedTasksByReference makes the session recorder renderer is record a
// task a task-owned entry of the local store served as its task:start, the
// records the hit added, and a task:end whose cacheReference names the entry,
// and hand pin every result it references once the session ends.
// It reports false, and changes nothing, for a renderer whose session is
// recorded by the machine renderer: --output=jsonl keeps every record in its
// artifact.
func RecordReusedTasksByReference(renderer jobs.Renderer, pin func(results []PinnedReference) error) bool {
	recorder, ok := renderer.(*sessionRecordingRenderer)
	if !ok {
		return false
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.refs = &cacheReferences{held: map[string][]jobs.RawJobEvent{}, results: map[string]string{}, pin: pin}
	return true
}

// RecordsByReference reports whether renderer's session recorder can record
// reused tasks by reference: false for the machine renderer, whose
// --output=jsonl artifact keeps every record.
func RecordsByReference(renderer jobs.Renderer) bool {
	_, ok := renderer.(*sessionRecordingRenderer)
	return ok
}

// release returns the events held for the task of key and forgets them, and
// whether the recorder records by reference. The caller holds the lock.
func (r *sessionRecordingRenderer) release(key string) ([]jobs.RawJobEvent, bool) {
	if r.refs == nil {
		return nil, false
	}
	held := r.refs.held[key]
	delete(r.refs.held, key)
	return held, true
}

// endRecords are the records that end task: the task:end alone, naming the
// entry that holds its replayed records, when the result's entry supplied
// exactly the events held for it; otherwise the held events as task:event
// records, then its test:case records and its task:end, as for a task
// recorded whole.
func (r *sessionRecordingRenderer) endRecords(result *jobs.JobResult, task machine.Task, held []jobs.RawJobEvent, byReference bool, at time.Time) []protocolcli.SessionStreamRecord {
	end := machine.TaskEnd(task, at)
	if byReference {
		if reference, ok := cacheReferenceOf(result, end.Task, len(held), len(task.Result.TestCases)); ok {
			// The digest is of the records a reader rebuilds from the line
			// this task:end is written as, without its reference.
			if line, err := sanitizedMachineRecord(end); err == nil {
				if records, ok := CachedRecords(line, held, task.Result.TestCases); ok {
					reference.Projection = CachedRecordsProjection
					reference.Records = CachedRecordsDigest(records)
					end.CacheReference = reference
					r.mu.Lock()
					r.refs.results[result.EntryRecords.Key] = result.EntryRecords.Result
					r.mu.Unlock()
					return []protocolcli.SessionStreamRecord{end}
				}
			}
		}
	}
	records := make([]protocolcli.SessionStreamRecord, 0, len(held)+len(task.Result.TestCases)+1)
	for _, event := range held {
		record := machine.CachedTaskEvent(task.Identity, event)
		if record.Time == "" {
			record.Time = at.Format(time.RFC3339Nano)
		}
		records = append(records, record)
	}
	records = append(records, machine.TestCaseRecords(task.Identity, task.Result.TestCases, at, 1)...)
	return append(records, end)
}

// cacheReferenceOf is the reference a task:end whose record is task carries:
// the entry result.EntryRecords names, when that entry supplied exactly the
// held events, its key and result are lowercase SHA-256 digests, and the task
// was reused and not skipped.
func cacheReferenceOf(result *jobs.JobResult, task *protocolcli.TaskRecord, held, testCases int) (*protocolcli.TaskCacheReference, bool) {
	if result == nil || result.EntryRecords == nil || task == nil ||
		task.Reuse == protocolcli.TaskReuseNone || task.Status == protocolcli.TaskStatusSkipped {
		return nil, false
	}
	entry := result.EntryRecords
	resultHex, prefixed := strings.CutPrefix(entry.Result, "sha256:")
	for _, digest := range []string{entry.Key, resultHex} {
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
			return nil, false
		}
	}
	if !prefixed || entry.Events != held {
		return nil, false
	}
	return &protocolcli.TaskCacheReference{
		Entry:     "sha256:" + entry.Key,
		Result:    entry.Result,
		Events:    entry.Events,
		TestCases: testCases,
	}, true
}

// pinReferences hands the recorder's pin every result its stream
// references, sorted by entry key. It is called once, when the session ends.
func (r *sessionRecordingRenderer) pinReferences() error {
	r.mu.Lock()
	refs := r.refs
	var results []PinnedReference
	if refs != nil {
		results = make([]PinnedReference, 0, len(refs.results))
		for key, result := range refs.results {
			results = append(results, PinnedReference{Key: key, Result: result})
		}
	}
	r.mu.Unlock()
	if refs == nil || refs.pin == nil || len(results) == 0 {
		return nil
	}
	slices.SortFunc(results, func(a, b PinnedReference) int { return strings.Compare(a.Key, b.Key) })
	return refs.pin(results)
}

// cacheReferenceMember opens the cacheReference member of a recorded task:end
// line: the recorder writes it as the record's last member.
var cacheReferenceMember = []byte(`,"cacheReference":`)

// SplitCacheReference returns line, one record of a recorded session stream
// without its LF, without its cacheReference member, and the reference it
// carries. It reports false, and returns line, when line carries none. The
// member is the record's last, so its value ends one byte before the line.
func SplitCacheReference(line []byte) ([]byte, *protocolcli.TaskCacheReference, bool) {
	i := bytes.LastIndex(line, cacheReferenceMember)
	if i < 0 || len(line) < 2 || line[len(line)-1] != '}' {
		return line, nil, false
	}
	var reference protocolcli.TaskCacheReference
	if json.Unmarshal(line[i+len(cacheReferenceMember):len(line)-1], &reference) != nil {
		return line, nil, false
	}
	end := make([]byte, 0, i+1)
	end = append(end, line[:i]...)
	return append(end, '}'), &reference, true
}

// CachedRecordsDigest is the SHA-256 of records, each ended by an LF,
// "sha256:<hex>": the bytes a reader inserts before a reused task's task:end.
func CachedRecordsDigest(records [][]byte) string {
	digest := sha256.New()
	for _, record := range records {
		digest.Write(record)
		digest.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// CachedRecords are the records, each without its LF, that the recorder writes
// for a reused task it records whole, before end, the task's task:end line
// without its reference: a task:event record per event of events, the events
// its cache entry holds, then a test:case record per case of cases, the cases
// its result yields. It reports false when end is not a task:end record.
func CachedRecords(end []byte, events []jobs.RawJobEvent, cases []protocolcli.TestCase) ([][]byte, bool) {
	var head struct {
		Record   string                    `json:"record"`
		Time     string                    `json:"time"`
		Identity *protocolcli.TaskIdentity `json:"identity"`
	}
	if json.Unmarshal(end, &head) != nil || head.Record != protocolcli.RecordTaskEnd || head.Identity == nil {
		return nil, false
	}
	records := make([]protocolcli.SessionStreamRecord, 0, len(events)+len(cases))
	for _, event := range events {
		records = append(records, machine.CachedTaskEvent(*head.Identity, event))
	}
	records = append(records, machine.TestCaseRecords(*head.Identity, cases, time.Time{}, 0)...)
	lines := make([][]byte, 0, len(records))
	for _, record := range records {
		if record.Time == "" || record.Record == protocolcli.RecordTestCase {
			record.Time = head.Time
		}
		line, err := sanitizedMachineRecord(record)
		if err != nil {
			return nil, false
		}
		lines = append(lines, line)
	}
	return lines, true
}
