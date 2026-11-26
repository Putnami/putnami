// Task-owned cache entries — the v3 entry model.
//
// # The model
//
// A task-owned entry records WHAT WAS DECLARED: for each output the task's v3
// contract names (protocols/extension task_contract.go), the entry says which id
// it is, whether it is a file or a subtree, which root it resolves against, its
// root-relative path, and whether the task produced bytes for it at all. It is
// the only entry model this CLI reads or writes.
//
// # Entry-format version, and why the key is not enough
//
// Cache key v5 digests the task CONTRACT — extension, task, declared
// outputs, effects, inputs. It does not digest the ENTRY PAYLOAD FORMAT, because
// the payload format is a property of the CLI that wrote the entry, not of the
// task. A format-1 entry — a whole-output capture published by a 0.4.0 or older
// CLI — therefore carries a correct v5 key and a payload this build cannot
// interpret, and the machine-global per-repo store (~/.putnami/store/<repo-id>)
// is shared by every worktree and every branch on the machine, including
// checkouts of those CLIs.
//
// The format version is consequently part of the entry's ADDRESS, not merely a
// field inside it:
//
//	address = sha256("putnami/store/entry-format\x00" + <format> + "\x00" + key)
//
// Two properties follow:
//
//   - A format-1 entry reads as absent. LookupTaskEntry(key) reads the derived
//     address, finds nothing, and reports a MISS — never a format-1 payload
//     interpreted as a declared one. No code reads the raw-key address, and GC
//     evicts what it holds.
//   - A task-owned entry is invisible to an older CLI running from another
//     worktree, which computes blobDir(key) and never looks at the derived
//     address.
//
// A future format 3 needs no migration code at all: changing the constant moves
// every address, so old entries age out through ordinary GC.
//
// # The declared-output manifest, and the explicit empty state
//
// TaskEntry.Outputs is the entry's own record of the declaration it satisfies.
// Each recorded output carries a State, and the two states are spelled out
// rather than inferred:
//
//   - TaskOutputPresent — the entry carries this output's bytes, under files/<id>.
//   - TaskOutputEmpty — the task ran successfully and legitimately produced
//     nothing for an output its declaration marks optionalEmpty (a coverage file
//     with coverage off, a client directory for a project that generates no
//     client).
//
// The state is a required string, deliberately not a bool with omitempty: a
// missing field decodes to the zero value, and "empty" must never be the
// consequence of something being absent. It is what makes the binding invariant
// checkable — MATERIALIZING AN EMPTY OUTPUT MUST NOT TOUCH ITS DESTINATION.
// An empty output is a statement about THIS task, not a statement about the
// path: the store cannot tell what else the destination holds, such as a
// committed file no task writes inside an optional project output. Whether a
// restore empties the destination is the engine's decision, because the engine
// reads the declaration. It empties an optionalEmpty directory under the
// command-output root, where the plan gives each output one owner, with the
// same EmptyTaskOutput an execution uses, and leaves every other empty output
// as it stands (scheduler.Scheduler.materializeDeclaredOutputs, CLI ADR 0065).
//
// # Merge points: the model deliberately cannot express them (finding from B3c)
//
// A merge point is a read-merge-write on a live file; an entry is an immutable
// content-addressed snapshot, and a snapshot of a file two tasks concurrently
// append to is wrong under every restore order — representing it would encode a
// value that is only correct by luck. So the model has no merge-point kind, and
// it never will: every declared output has exactly one owning task by
// construction (ONE OWNER PER OUTPUT).
//
// The invariant is enforced where ownership is decided: ValidateOutputOwnership
// in the manifest, validatePlanContract across the plan.
//
// # A task-owned entry is materialized, never symlinked
//
// A CAS-symlink all-hit fast path would point .putnami/out/<project>/~<command>
// at one blob's files/ tree, so a fully cached command would materialize zero
// bytes. It does not fit the task-owned model, and that is a decision, not an
// oversight:
//
//  1. STRUCTURAL — a task-owned entry contains exactly one task's outputs, while
//     that directory is the union of every step of the command. One symlink can
//     only name one blob, so preserving the fast path would require a per-command
//     union entry: precisely the directory-wide capture v3 exists to remove.
//
//  2. CORRECTNESS — the directory must stay writable, because sibling steps of
//     one command write into it while another step's entry is materialized. A
//     blob's files/ tree is a tree of HARDLINKS into the CAS, so an in-place
//     write through the link edits the CAS blob's own inode: the bytes stop
//     matching their digest and every other entry sharing that blob is silently
//     corrupted.
//
//  3. COST — measured by BenchmarkTaskOutputMaterializeVsSymlink (darwin/arm64,
//     Apple M1 Pro, APFS; 256 files / 4 MiB; 3×60 iterations):
//
//     symlink (rejected fast path)    ~0.05 ms/op
//     atomic materialize (this file)  ~66–84 ms/op
//
//     The cost is metadata-bound (256 creates plus 256 unlinks), not bandwidth-
//     bound, and it is bounded by the DECLARED outputs rather than by everything
//     the directory holds.
//
// A task-owned blob is therefore always materialized, never linked, rather than
// producing a link that is structurally wrong.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

const (
	// EntryFormatLegacy is the value the whole-output entries of a 0.4.0 or
	// older CLI reserve. Their blobs carry no entry descriptor, and a
	// descriptor that records this value reads as a miss.
	EntryFormatLegacy = 1

	// EntryFormatTaskOwned is the declared-capture entry this file defines: a
	// descriptor naming exactly the outputs the task's v3 contract declares.
	EntryFormatTaskOwned = 2

	// CurrentEntryFormat is the format IngestTaskEntry writes and the only one
	// LookupTaskEntry accepts. Any other value — older, newer, or corrupt —
	// reads as a miss.
	CurrentEntryFormat = EntryFormatTaskOwned
)

// entryDescriptorFilename is the per-blob file whose presence identifies a
// task-owned entry. A blob without one is never read as an entry.
const entryDescriptorFilename = "entry.json"

// taskEntryAddressDomain namespaces the address derivation so a task cache key
// can never collide with a digest computed for any other purpose.
const taskEntryAddressDomain = "putnami/store/entry-format"

// Recorded states of one declared output inside a published entry. The pair is
// closed: a descriptor carrying anything else is not interpretable and reads as
// a miss.
const (
	// TaskOutputPresent means the entry carries this output's bytes under
	// files/<id>.
	TaskOutputPresent = "present"

	// TaskOutputEmpty means the task produced nothing for an optionalEmpty
	// output. Materializing it is a no-op that never removes the destination;
	// the engine decides whether a restore empties it.
	TaskOutputEmpty = "empty"
)

var (
	// ErrIncompleteCapture reports that a REQUIRED declared output was absent
	// from (or empty in) the staging root. The ingest fails rather than
	// publishing a smaller entry: a partial entry is indistinguishable from a
	// complete one on the next hit, and would materialize less than the
	// declaration promises.
	ErrIncompleteCapture = errors.New("declared output missing from staging root")

	// ErrCaptureMismatch reports that the staging root does not hold the file
	// set the ingest was told to expect (TaskEntrySpec.Expect). The ingest
	// publishes nothing: the bytes are not the ones the caller vouches for.
	ErrCaptureMismatch = errors.New("captured files differ from the expected files")

	// ErrEntryFormat reports a descriptor this build cannot interpret. It is
	// returned only by the explicit-format helpers; the lookup path converts the
	// same condition into a miss.
	ErrEntryFormat = errors.New("unsupported entry format")
)

// DeclaredEntryOutput is one output of the task's v3 declaration, resolved for
// the run being captured. It is the INPUT to ingest: the contract the entry has
// to satisfy, not what was found on disk.
//
// Root stays symbolic (proto.OutputRoot*) and Path stays root-relative, because
// the store is machine-global and shared across worktrees: an absolute path
// recorded in an entry would be wrong for every other worktree that hits it.
type DeclaredEntryOutput struct {
	// ID is the declaration's output id. It doubles as the output's slot inside
	// the blob (files/<id>), so it must be a single safe path segment.
	ID string

	// Kind is proto.OutputKindFile or proto.OutputKindDirectory.
	Kind string

	// Root is proto.OutputRootProject, proto.OutputRootWorkspace or
	// proto.OutputRootCommandOutput. Empty means the project root, matching
	// DeclaredOutput.EffectiveRoot.
	Root string

	// Path is the resolved root-relative path, in cleaned slash form. A
	// pathFrom declaration must be resolved to its literal path before ingest —
	// the store never sees an unresolved port.
	Path string

	// Optional mirrors the declaration's optionalEmpty: producing nothing for
	// this output is a legitimate outcome of a successful run.
	Optional bool
}

// EffectiveRoot returns the output's root, defaulting to the project root.
func (o DeclaredEntryOutput) EffectiveRoot() string {
	if o.Root == "" {
		return proto.OutputRootProject
	}
	return o.Root
}

// TaskEntryOutput is one declared output as RECORDED in a published entry: the
// declaration's identity plus what the capture found for it.
type TaskEntryOutput struct {
	// ID, Kind, Root and Path are the declaration this output satisfies. They
	// are recorded rather than re-derived so a restore can be checked against
	// the contract the entry was written for, even after the manifest changed.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Root string `json:"root"`
	Path string `json:"path"`

	// Optional records whether the declaration allowed this output to be empty,
	// so a reader can tell a legitimately empty output from one that was empty
	// because an older CLI failed to capture it.
	Optional bool `json:"optional"`

	// State is TaskOutputPresent or TaskOutputEmpty. Required, and deliberately
	// not a bool: emptiness is stated, never inferred from an absent field.
	State string `json:"state"`

	// Files and Size describe the captured payload (0/0 for an empty output).
	Files int   `json:"files"`
	Size  int64 `json:"size"`
}

// Present reports whether the entry carries bytes for this output.
func (o TaskEntryOutput) Present() bool { return o.State == TaskOutputPresent }

// TaskEntry is a published task-owned cache entry: the descriptor persisted as
// entry.json, plus the payload the store resolved around it.
type TaskEntry struct {
	// Format is the entry-format version. Always CurrentEntryFormat for an
	// entry this build published or accepted.
	Format int `json:"entryFormat"`

	// Key is the task cache key (v5) this entry was published for. It is
	// recorded so a lookup can confirm the address it derived belongs to the key
	// it asked for, rather than trusting the address alone.
	Key string `json:"key"`

	// Outputs is the declared-output manifest, sorted by id so two captures of
	// one declaration produce byte-identical descriptors.
	Outputs []TaskEntryOutput `json:"outputs"`

	// --- resolved at lookup/ingest time; never part of entry.json ---

	// Address is the derived store address (see TaskEntryAddress).
	Address string `json:"-"`

	// Result is the structured job result (result.json).
	Result *EntryResult `json:"-"`

	// Metadata is the provenance sidecar (meta.json).
	Metadata *EntryMetadata `json:"-"`

	// Manifest lists every captured file with its CAS digest, mirroring the
	// remote-cache wire manifest (paths are
	// "<output id>" for a file output and "<output id>/<rel>" for a directory
	// output). protocols/cache is untouched by this model.
	Manifest *cache.Manifest `json:"-"`

	// FilesDir is the absolute path of the entry's files/ directory.
	FilesDir string `json:"-"`

	// Descriptor is entry.json exactly as it is on disk. The bytes (not a
	// re-marshaling of the struct) are what travels to the remote cache and
	// what a remote hit writes back, so one entry has one descriptor digest on
	// every machine that holds it (task_remote.go).
	Descriptor []byte `json:"-"`
}

// Output returns the recorded output with the given id.
func (e *TaskEntry) Output(id string) (TaskEntryOutput, bool) {
	if e == nil {
		return TaskEntryOutput{}, false
	}
	for _, out := range e.Outputs {
		if out.ID == id {
			return out, true
		}
	}
	return TaskEntryOutput{}, false
}

// TaskEntryAddress maps a task cache key to the store address its task-owned
// entry lives at, binding the entry-format version into the address itself.
//
// An entry of another format lives at another address even when its v5 key is
// the same, so no reader reaches a payload it cannot interpret, and bumping
// CurrentEntryFormat relocates every entry without a rewrite pass.
func TaskEntryAddress(key string) string {
	sum := sha256.Sum256([]byte(taskEntryAddressDomain + "\x00" + strconv.Itoa(CurrentEntryFormat) + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// LookupTaskEntry resolves a task cache key to its task-owned entry, or (nil,
// nil) for a miss.
//
// A miss — never an error, never a foreign payload — is the answer for every
// entry this build cannot interpret: no descriptor at the derived address (which
// is where a key with only a format-1 entry stands), a descriptor whose format is not CurrentEntryFormat, a descriptor recording a different key,
// and a descriptor that is torn or violates the model. Failing closed is the only
// safe direction: a wrongly accepted entry materializes the wrong tree, while a
// wrongly rejected one only costs a recompute.
//
// The entry's lastUsed sidecar is stamped under the store's SHARED lock, so a
// concurrent GC (which needs the exclusive lock) cannot evict the entry between
// this read and the caller's materialize.
func (s *LocalStore) LookupTaskEntry(key string) (*TaskEntry, error) {
	if key == "" {
		return nil, nil
	}
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	address := TaskEntryAddress(key)
	blobDir := s.blobDir(address)

	entry := s.loadTaskEntryOnce(blobDir, address)
	if entry == nil || entry.Key != key {
		return nil, nil
	}

	writeLastUsed(blobDir, time.Now(), s.gen)
	return entry, nil
}

// taskEntryFileStat is the stat of one file loadTaskEntry reads; a file that
// is absent or not a regular file has ok false.
type taskEntryFileStat struct {
	stat fileindex.Stat
	ok   bool
}

// taskEntryStats is the stat of each file loadTaskEntry reads from a blob
// directory: the descriptor first, then the result, metadata and manifest.
type taskEntryStats [4]taskEntryFileStat

func statTaskEntry(blobDir string) taskEntryStats {
	stat := func(name string) taskEntryFileStat {
		st, ok := fileindex.Lstat(filepath.Join(blobDir, name))
		return taskEntryFileStat{stat: st, ok: ok}
	}
	return taskEntryStats{stat(entryDescriptorFilename), stat("result.json"), stat("meta.json"), stat(manifestFilename)}
}

// settled reports whether every file of stats is a tick older than now, by
// the file index's margin (protocols/workspace ADR 0009): a write after the
// load then moves a recorded time, whatever the filesystem's tick.
func (stats taskEntryStats) settled(now time.Time) bool {
	limit := now.Add(-fileindex.Margin).UnixNano()
	for _, file := range stats {
		if file.ok && (file.stat.ModNano >= limit || file.stat.ChangeNano >= limit) {
			return false
		}
	}
	return stats[0].ok
}

// taskEntryReads counts the entries loadTaskEntryOnce read from disk. A test
// reads it around a lookup.
var taskEntryReads atomic.Int64

// taskEntryMemo is a task entry a handle loaded and the stat of its files
// when it was loaded.
type taskEntryMemo struct {
	stats taskEntryStats
	entry *TaskEntry
}

// loadTaskEntryOnce is loadTaskEntry, read once per handle while every file
// it read has the stat it had: the same file, mode, size, and modification
// and status-change times. A planning lookup and the scheduler's lookup of
// one task thereby read its four files once, not twice. An entry whose files
// were written less than the file index's margin before the load, and every
// entry on a platform whose stat the file index does not decode, is read
// again on each lookup. The caller holds the store's shared lock.
func (s *LocalStore) loadTaskEntryOnce(blobDir, address string) *TaskEntry {
	now := time.Now()
	stats := statTaskEntry(blobDir)
	if cached, ok := s.loaded.Load(address); ok {
		if memo, ok := cached.(taskEntryMemo); ok && memo.stats == stats {
			clone := *memo.entry
			return &clone
		}
		s.loaded.Delete(address)
	}
	taskEntryReads.Add(1)
	entry := loadTaskEntry(blobDir, address)
	if entry == nil {
		return nil
	}
	// A file written while it was read has two stats; the entry answers for
	// neither.
	if stats.settled(now) && statTaskEntry(blobDir) == stats {
		s.loaded.Store(address, taskEntryMemo{stats: stats, entry: entry})
	}
	clone := *entry
	return &clone
}

// loadTaskEntry reads a published task-owned entry from its blob directory, or
// nil when the blob is absent, carries no descriptor, is torn, or in a format this build does not
// read. The caller holds the store's shared lock. Every rejection is silent by
// design — see LookupTaskEntry for why every uninterpretable entry is a miss.
func loadTaskEntry(blobDir, address string) *TaskEntry {
	entry, err := readTaskDescriptor(blobDir)
	if err != nil || entry == nil {
		return nil
	}
	entry.Address = address

	resultData, err := os.ReadFile(filepath.Join(blobDir, "result.json"))
	if err != nil {
		return nil // published entries always have one; a torn blob is a miss
	}
	result, err := UnmarshalResult(resultData)
	if err != nil {
		return nil
	}
	entry.Result = result

	if metaData, err := os.ReadFile(filepath.Join(blobDir, "meta.json")); err == nil {
		if meta, err := UnmarshalMetadata(metaData); err == nil {
			entry.Metadata = meta
		}
	}
	if manifestData, err := os.ReadFile(filepath.Join(blobDir, manifestFilename)); err == nil {
		var m cache.Manifest
		if json.Unmarshal(manifestData, &m) == nil {
			entry.Manifest = &m
		}
	}
	if info, err := os.Stat(filepath.Join(blobDir, "files")); err == nil && info.IsDir() {
		entry.FilesDir = filepath.Join(blobDir, "files")
	}
	return entry
}

// TryClaimTaskEntry attempts to own the computation that will publish key's
// task-owned entry. Exactly one process wins while a lease is live. A winner
// receives an idempotent release function and a heartbeat keeps its ownership
// alive until release. A loser should call WaitForTaskEntry, then look the entry
// up normally. estimatedCost is optional for callers without a prediction; when
// supplied, only its first value is used to apply CoalescingFloor.
//
// The lease is taken on the entry's derived address, and "published" means a
// task-owned descriptor exists there. Leasing on the address rather than the raw
// key keeps publishers of two entry formats from waiting on each other: they
// produce different artifacts.
//
// Any filesystem/locking failure and any known cost below CoalescingFloor
// return winner=true with a no-op release. Leasing must never make a build less
// reliable than computing the key independently.
func (s *LocalStore) TryClaimTaskEntry(key string, estimatedCost ...time.Duration) (bool, func()) {
	cost := time.Duration(0)
	if len(estimatedCost) > 0 {
		cost = estimatedCost[0]
	}
	return s.tryClaim(TaskEntryAddress(key), cost, func() bool { return s.taskEntryPublished(key) })
}

// WaitForTaskEntry blocks until key's task-owned entry has been atomically
// published, the current owner expires/releases, or timeout elapses. On
// ErrLeaseExpired callers should call TryClaimTaskEntry again before computing,
// so one waiter takes over rather than all waiters recomputing together. On
// ErrLeaseWaitTimeout callers may compute directly as a bounded-wait reliability
// fallback.
func (s *LocalStore) WaitForTaskEntry(key string, timeout time.Duration) error {
	return s.waitForPublish(TaskEntryAddress(key), timeout, func() bool { return s.taskEntryPublished(key) })
}

// taskEntryPublished reports whether a task-owned entry for key is committed.
// The descriptor is written inside the staged directory and lands with the
// entry's single atomic rename, so its presence means the whole entry is there.
func (s *LocalStore) taskEntryPublished(key string) bool {
	fi, err := os.Stat(filepath.Join(s.blobDir(TaskEntryAddress(key)), entryDescriptorFilename))
	return err == nil && fi.Mode().IsRegular()
}

// readTaskDescriptor reads and validates entry.json. It returns (nil, nil) when
// the blob has no descriptor and an error when the descriptor
// exists but cannot be interpreted; both are misses to the lookup path, but the
// distinction matters to tests and to any future repair tooling.
func readTaskDescriptor(blobDir string) (*TaskEntry, error) {
	data, err := os.ReadFile(filepath.Join(blobDir, entryDescriptorFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseTaskDescriptor(data)
}

// parseTaskDescriptor decodes and validates descriptor bytes, whether they come
// from a published blob or off the remote blob exchange (task_remote.go). Both
// readers apply exactly the same rules — a descriptor is interpretable or it is
// not, and where it was read from cannot change the answer.
func parseTaskDescriptor(data []byte) (*TaskEntry, error) {
	var entry TaskEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("parse %s: %w", entryDescriptorFilename, err)
	}
	if entry.Format != CurrentEntryFormat {
		return nil, fmt.Errorf("%w: entry format %d, this build reads %d",
			ErrEntryFormat, entry.Format, CurrentEntryFormat)
	}
	if err := validateRecordedOutputs(entry.Outputs); err != nil {
		return nil, err
	}
	entry.Descriptor = data
	return &entry, nil
}

// validateRecordedOutputs rejects a descriptor that is syntactically parseable
// but not interpretable: an unknown state, a duplicate or unusable id, or a
// kind/root outside the closed vocabularies. Every one of these would otherwise
// decide a materialize, so they are checked once, on read.
func validateRecordedOutputs(outputs []TaskEntryOutput) error {
	seen := make(map[string]bool, len(outputs))
	for _, out := range outputs {
		if err := validateOutputID(out.ID); err != nil {
			return err
		}
		if seen[out.ID] {
			return fmt.Errorf("duplicate declared output id %q", out.ID)
		}
		seen[out.ID] = true

		switch out.State {
		case TaskOutputPresent, TaskOutputEmpty:
		default:
			return fmt.Errorf("declared output %q has unknown state %q", out.ID, out.State)
		}
		if out.State == TaskOutputEmpty && !out.Optional {
			return fmt.Errorf("declared output %q is recorded empty but is not optional", out.ID)
		}
		switch out.Kind {
		case proto.OutputKindFile, proto.OutputKindDirectory:
		default:
			return fmt.Errorf("declared output %q has unknown kind %q", out.ID, out.Kind)
		}
		switch out.Root {
		case proto.OutputRootProject, proto.OutputRootWorkspace, proto.OutputRootCommandOutput:
		default:
			return fmt.Errorf("declared output %q has unknown root %q", out.ID, out.Root)
		}
		if _, err := proto.NormalizeOutputPath(out.Path); err != nil {
			return fmt.Errorf("declared output %q has invalid path %q: %w", out.ID, out.Path, err)
		}
	}
	return nil
}

// validateOutputID checks that an output id can serve as its own slot inside the
// blob (files/<id>). Ids come from a manifest, so they are attacker-adjacent
// input as far as the store is concerned.
func validateOutputID(id string) error {
	switch {
	case id == "":
		return errors.New("declared output id cannot be empty")
	case id == "." || id == "..":
		return fmt.Errorf("declared output id %q is not a usable path segment", id)
	case id != path.Clean(id) || id != filepath.Base(id):
		return fmt.Errorf("declared output id %q must be a single path segment", id)
	case id == RemoteEntryDescriptorPath:
		// The entry descriptor travels the blob exchange at this manifest path
		// (task_remote.go). An output owning the same id would make a remote
		// payload ambiguous, so the collision is refused here rather than
		// detected later, on another machine, as an uninterpretable hit.
		return fmt.Errorf("declared output id %q is reserved for the entry descriptor", id)
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r == 0 {
			return fmt.Errorf("declared output id %q must be a single path segment", id)
		}
	}
	return nil
}
