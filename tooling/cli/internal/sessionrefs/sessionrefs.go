// Package sessionrefs resolves the cache references of a recorded session
// stream (CLI ADR 0137): a reused task's task:end names the result of the
// task-owned entry of the local store that holds the records its replay would
// have written, and a reader rebuilds them from that result. The session
// reporter's view and packages and the sessions commands read a recorded
// stream through it.
package sessionrefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/store"
)

// File is the document a session directory holds when its run records reused
// tasks by reference: it says so, and names the store the references resolve
// in. A session without it holds every record.
const File = "references.json"

// fileVersion is the version of File.
const fileVersion = 1

// declaration is File's content.
type declaration struct {
	Version int    `json:"version"`
	Store   string `json:"store"`
}

// ErrNoReference is a record that names no cache entry.
var ErrNoReference = errors.New("the record names no cache entry")

// ErrUnavailable is a reference whose result no store this reader opens
// holds: its entry was collected and no pin kept the result, or the store was
// cleaned.
var ErrUnavailable = errors.New("the cache entry the reference names is not in the store")

// ProjectionError is a reference this CLI cannot rebuild as its producer
// would have written it: it names another projection, or the result it names,
// verified byte for byte, yields other records than the reference counts or
// records that hash to another digest. The difference is this CLI's code; a
// CLI of the recording projection rebuilds it.
type ProjectionError struct {
	// Recorded is the projection the reference names.
	Recorded int
}

func (e *ProjectionError) Error() string {
	if e.Recorded != output.CachedRecordsProjection {
		return fmt.Sprintf("the session's reused tasks were recorded with records projection %d, and this CLI rebuilds projection %d: deliver it with the CLI that recorded it", e.Recorded, output.CachedRecordsProjection)
	}
	return fmt.Sprintf("this CLI's records projection %d rebuilds a reused task's records to other records than the CLI that recorded the session: deliver it with that CLI", e.Recorded)
}

// Entries reads the result a cache reference names.
// store.LocalStore.ReadReferencedResult is the implementation every reader
// uses.
type Entries interface {
	ReadReferencedResult(key, result string) *store.EntryResult
}

// Declare writes File into the session directory dir, naming storeRoot, the
// store the run's references resolve in. The run records by reference only
// once it did.
func Declare(dir, storeRoot string) error {
	root, err := filepath.Abs(storeRoot)
	if err != nil {
		return err
	}
	data, err := json.Marshal(declaration{Version: fileVersion, Store: root})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, File+".tmp-*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, File)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Declared reports whether the session recorded in dir records reused tasks
// by reference. A File that does not read still declares it: its stream may
// hold references.
func Declared(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, File))
	return err == nil
}

// Of is the store the references of the session recorded in dir resolve in:
// the store its File names, or, without one, the store of the workspace
// whose .putnami/sessions holds dir. It is nil when neither names one.
func Of(dir string) Entries {
	if data, err := os.ReadFile(filepath.Join(dir, File)); err == nil {
		var declared declaration
		if json.Unmarshal(data, &declared) == nil && declared.Version == fileVersion && filepath.IsAbs(declared.Store) {
			return store.NewLocalStore(declared.Store)
		}
	}
	sessions := filepath.Dir(filepath.Clean(dir))
	putnami := filepath.Dir(sessions)
	if filepath.Base(sessions) != "sessions" || filepath.Base(putnami) != ".putnami" {
		return nil
	}
	return store.NewLocalStore(store.ResolveStoreRoot(filepath.Dir(putnami)))
}

// Resolve rebuilds what line stands for, a task:end record of a recorded
// session stream, without its LF, whose cacheReference names a cache entry:
// the task:event and test:case records the entry yields, each without its
// LF, as the recorder writes them for a task it records whole, and line
// without its reference. It returns ErrNoReference for a line that carries
// none, ErrUnavailable when entries holds no result the reference names, and
// a *ProjectionError when this CLI's projection is not the one that recorded
// the reference, or the result, whose bytes entries verified, yields other
// record counts or records than the ones it counted and digested.
func Resolve(entries Entries, line []byte) ([][]byte, []byte, error) {
	end, reference, ok := output.SplitCacheReference(line)
	if !ok {
		return nil, nil, ErrNoReference
	}
	if reference.Projection != output.CachedRecordsProjection {
		return nil, nil, &ProjectionError{Recorded: reference.Projection}
	}
	key, prefixed := strings.CutPrefix(reference.Entry, "sha256:")
	if !prefixed || entries == nil {
		return nil, nil, ErrUnavailable
	}
	recorded := entries.ReadReferencedResult(key, reference.Result)
	if recorded == nil {
		return nil, nil, ErrUnavailable
	}
	result := cachekey.JobResultFromEntryResult(recorded)
	cases := jobs.TaskResultOf(nil, result).TestCases
	records, ok := output.CachedRecords(end, result.Events, cases)
	if !ok || len(result.Events) != reference.Events || len(cases) != reference.TestCases ||
		output.CachedRecordsDigest(records) != reference.Records {
		return nil, nil, &ProjectionError{Recorded: reference.Projection}
	}
	return records, end, nil
}

// Resolved is the bytes a reader sends in place of line and its LF: the
// records Resolve rebuilds and line without its reference, each ended by an
// LF. A line that carries no reference, or whose reference does not resolve
// in entries, is returned as it is, with its LF, so a reader of the bytes
// still sees which task's records are not there. It returns the
// *ProjectionError of a reference this CLI cannot rebuild, and no bytes: a
// reader sends none rather than other ones.
func Resolved(entries Entries, line []byte) ([]byte, error) {
	records, end, err := Resolve(entries, line)
	var projection *ProjectionError
	switch {
	case errors.As(err, &projection):
		return nil, err
	case err != nil:
		return append(append(make([]byte, 0, len(line)+1), line...), '\n'), nil
	}
	size := len(end) + 1
	for _, record := range records {
		size += len(record) + 1
	}
	out := make([]byte, 0, size)
	for _, record := range records {
		out = append(out, record...)
		out = append(out, '\n')
	}
	out = append(out, end...)
	return append(out, '\n'), nil
}
