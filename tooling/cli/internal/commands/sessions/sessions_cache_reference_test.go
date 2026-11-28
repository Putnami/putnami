package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// withCacheReference is end, a task:end line, with the cacheReference this
// CLI records for it when entry served it: the records it stands for rebuilt
// by projection, whose digest is this CLI's.
func withCacheReference(t *testing.T, end string, key string, entry *store.TaskEntry, projection int) string {
	t.Helper()
	result := cachekey.JobResultFromEntryResult(entry.Result)
	cases := jobs.TaskResultOf(nil, result).TestCases
	records, ok := output.CachedRecords([]byte(end), result.Events, cases)
	if !ok {
		t.Fatalf("%s rebuilds no records", end)
	}
	return strings.TrimSuffix(end, "}") + fmt.Sprintf(`,"cacheReference":{"entry":"sha256:%s","result":"%s","events":%d,"testCases":%d,"projection":%d,"records":"%s"}}`,
		key, entry.ResultDigest, len(result.Events), len(cases), projection, output.CachedRecordsDigest(records))
}

// TestSessionsReadAReferenceAsTheRecordsItsEntryHolds records a session whose
// task:end names an entry of the workspace's store: the sessions commands'
// reader visits the entry's event and test case, then the task:end without
// its reference, at the task:end's position. Once the store no longer holds
// the entry, it visits the task:end as it is.
func TestSessionsReadAReferenceAsTheRecordsItsEntryHolds(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	key := strings.Repeat("e", 64)
	var result store.EntryResult
	if err := json.Unmarshal([]byte(`{"status":"success","data":{"testCases":[{"name":"TestA","suite":"a","status":"passed","durationMs":4}]},`+
		`"events":[{"v":2,"type":"diagnostic","data":{"type":"diagnostic","severity":"warning","message":"deprecated call"}}]}`), &result); err != nil {
		t.Fatal(err)
	}
	entry, err := store.NewLocalStore(storeRoot).IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key:      key,
		Result:   &result,
		Metadata: &store.EntryMetadata{Extension: "@putnami/go", Task: "test", Project: "a"},
	})
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}

	ws := t.TempDir()
	sessions := workspace_state.NewSessionStore(ws)
	const id = "20261127-120000-abc123"
	identity := `{"key":"/a:test","scope":"project","project":{"id":"/a","name":"a"},"task":{"name":"test","command":"test","kind":"go-test"},"provider":{"extension":"@putnami/go"}}`
	start := `{"protocolVersion":2,"record":"task:start","time":"2026-11-27T10:00:00Z","identity":` + identity + `}`
	end := `{"protocolVersion":2,"record":"task:end","time":"2026-11-27T10:00:02Z","identity":` + identity +
		`,"task":{"identity":` + identity + `,"inputDigest":"sha256:` + key + `","status":"success","reuse":"local-cache","exitCode":0,"durationMs":0}}`
	referenced := withCacheReference(t, end, key, entry, output.CachedRecordsProjection)
	dir := filepath.Join(sessions.Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionstream.EventsFile), []byte(start+"\n"+referenced+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	read := func() ([]string, []int64) {
		t.Helper()
		var kinds []string
		var offsets []int64
		err := readSessionRecords(sessions, id, func(record sessionstream.Record) error {
			var decoded protocolcli.SessionStreamRecord
			if err := json.Unmarshal(record.Data, &decoded); err != nil {
				t.Fatalf("record %s: %v", record.Data, err)
			}
			if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, record.Data); len(violations) != 0 {
				t.Fatalf("record %s: %+v", record.Data, violations)
			}
			kind := decoded.Record
			if decoded.CacheReference != nil {
				kind += "+reference"
			}
			kinds = append(kinds, kind)
			offsets = append(offsets, record.Position.Offset)
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return kinds, offsets
	}
	kinds, offsets := read()
	if want := []string{"task:start", "task:event", "test:case", "task:end"}; !slices.Equal(kinds, want) {
		t.Fatalf("visited %v, want %v", kinds, want)
	}
	if endAt := int64(len(start) + 1); !slices.Equal(offsets, []int64{0, endAt, endAt, endAt}) {
		t.Fatalf("visited at %v, want the rebuilt records at the task:end's offset %d", offsets, endAt)
	}

	if _, _, err := store.CleanStore(storeRoot); err != nil {
		t.Fatal(err)
	}
	if kinds, _ := read(); !slices.Equal(kinds, []string{"task:start", "task:end+reference"}) {
		t.Fatalf("with its entry gone, visited %v, want the task:end as it is", kinds)
	}
}
