package output

import (
	"bytes"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
)

// cachedRecordsGolden is what projection 1 rebuilds for the reused task of
// reusedTestResult, from its task:end: changing one byte of it changes the
// projection, so CachedRecordsProjection moves with it, and a reader of
// another projection refuses the references this one records.
const cachedRecordsGolden = `{"protocolVersion":2,"record":"task:event","time":"2026-11-27T10:00:00.5Z","identity":{"key":"/packages/my-app:build","scope":"project","project":{"id":"/packages/my-app","name":"my-app"},"task":{"name":"build","command":"build","kind":"ts-build"},"provider":{"extension":"@putnami/typescript"}},"event":{"message":"deprecated call","severity":"warning","time":"2026-11-27T10:00:00.5Z","type":"diagnostic"}}
`

// cachedRecordsGoldenDigest is CachedRecordsDigest of the records of
// cachedRecordsGolden and the rest of the projection: the digest a reference
// recorded by projection 1 carries for that task.
const cachedRecordsGoldenDigest = "sha256:aca6311505da342e2cf59b03827c9b6467677da0e46e475c927426fef2412a9b"

// TestTheRecordsProjectionIsPinned rebuilds a reused task's records from a
// fixed task:end and entry: the bytes and their digest are the ones
// projection 1 is, so a change to them cannot ship without a new projection.
func TestTheRecordsProjectionIsPinned(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reference-this-cli-cannot-rebuild-is-named")
	if CachedRecordsProjection != 1 {
		t.Fatalf("CachedRecordsProjection = %d: pin the bytes the new projection rebuilds here", CachedRecordsProjection)
	}
	end := []byte(`{"protocolVersion":2,"record":"task:end","time":"2026-11-27T10:00:02Z","identity":{"key":"/packages/my-app:build","scope":"project","project":{"id":"/packages/my-app","name":"my-app"},"task":{"name":"build","command":"build","kind":"ts-build"},"provider":{"extension":"@putnami/typescript"}},"task":{"status":"success","reuse":"local-cache"}}`)
	result := reusedTestResult(t)
	records, ok := CachedRecords(end, result.Events[:2], jobmodel.TaskResultOf(nil, result).TestCases)
	if !ok || len(records) != 5 {
		t.Fatalf("rebuilt %d records (%v), want two events and three cases", len(records), ok)
	}
	if got := append(append([]byte(nil), records[0]...), '\n'); !bytes.Equal(got, []byte(cachedRecordsGolden)) {
		t.Fatalf("projection 1 rebuilds the first record as %q, want %q", got, cachedRecordsGolden)
	}
	if got := CachedRecordsDigest(records); got != cachedRecordsGoldenDigest {
		var all []byte
		for _, record := range records {
			all = append(append(all, record...), '\n')
		}
		t.Fatalf("projection 1 rebuilds records whose digest is %s, want %s: %q", got, cachedRecordsGoldenDigest, all)
	}
}
