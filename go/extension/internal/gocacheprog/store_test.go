package gocacheprog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
)

// newStore opens a store laid out the way the helper lays it out under one
// cache root: records in <root>/prog, data files in the go command's own
// <root>/build.
func newStore(t *testing.T) *store {
	t.Helper()
	root := t.TempDir()
	local, err := openStore(filepath.Join(root, progDirName), toolchain.GoBuildCacheDir(root))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	return local
}

func sumOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// TestStoreWritesTheDataBeforeTheRecord pins the write ORDER, which is the
// store's whole atomicity argument: an action record is published last, so a
// concurrent reader that finds a record always finds a complete body behind it.
// The reverse order would hand a compiler a truncated archive.
func TestStoreWritesTheDataBeforeTheRecord(t *testing.T) {
	local := newStore(t)
	body := []byte("archive")
	action := strings.Repeat("a", idHexLength)
	output := sumOf(body)

	stored, err := local.put(action, output, int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	record, err := os.Stat(local.recordPath(action))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	data, err := os.Stat(stored.diskPath)
	if err != nil {
		t.Fatalf("stat data: %v", err)
	}
	if record.ModTime().Before(data.ModTime()) {
		t.Fatalf("record (%s) predates its data (%s); a reader could follow it to a partial body",
			record.ModTime(), data.ModTime())
	}
	if stored.diskPath != local.dataPath(output) {
		t.Fatalf("data path = %q, want the content address %q", stored.diskPath, local.dataPath(output))
	}
}

// TestRecordWithoutDataIsAMiss is the collector's half of the contract: the
// sweeper evicts "-a" and "-d" files independently, and the body lives in the
// go command's own directory, where its own trim and `go clean -cache` remove
// files too. A record whose body is gone must read as absent rather than as a
// hit with an unreadable path.
func TestRecordWithoutDataIsAMiss(t *testing.T) {
	local := newStore(t)
	body := []byte("evicted body")
	action := strings.Repeat("b", idHexLength)
	stored, err := local.put(action, sumOf(body), int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.Remove(stored.diskPath); err != nil {
		t.Fatalf("remove data: %v", err)
	}
	if _, ok := local.get(action); ok {
		t.Fatal("a record pointing at a removed body reported a hit")
	}
}

// TestTruncatedBodyIsAMiss covers the other half: a body that no longer matches
// the size the record announced is not this action's output.
func TestTruncatedBodyIsAMiss(t *testing.T) {
	local := newStore(t)
	body := []byte("a body that will be truncated")
	action := strings.Repeat("c", idHexLength)
	stored, err := local.put(action, sumOf(body), int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(stored.diskPath, body[:3], 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, ok := local.get(action); ok {
		t.Fatal("a truncated body reported a hit")
	}
}

// TestTwoActionsShareOneBody pins that the data file is content addressed: two
// compilations that produce identical output store one copy, and neither write
// disturbs the other.
func TestTwoActionsShareOneBody(t *testing.T) {
	local := newStore(t)
	body := []byte("identical output")
	output := sumOf(body)
	first := strings.Repeat("d", idHexLength)
	second := strings.Repeat("e", idHexLength)

	one, err := local.put(first, output, int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	two, err := local.put(second, output, int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if one.diskPath != two.diskPath {
		t.Fatalf("identical output stored twice: %q and %q", one.diskPath, two.diskPath)
	}
	got, ok := local.get(second)
	if !ok {
		t.Fatal("second action missed")
	}
	stored, err := os.ReadFile(got.diskPath)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("shared body = %q (%v), want %q", stored, err, body)
	}
}

// TestIngestRefusesBytesThatDoNotHashToTheOutputID is the only verification in
// the whole path. The go command never re-hashes what a helper hands it, so a
// provider serving bytes that do not match the announced output id would have
// it link output nothing in this workspace produced.
func TestIngestRefusesBytesThatDoNotHashToTheOutputID(t *testing.T) {
	local := newStore(t)
	honest := []byte("what the action really produced")
	tampered := []byte("what a provider served instead")
	source := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(source, tampered, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	action := strings.Repeat("f", idHexLength)

	if _, err := local.ingest(action, sumOf(honest), int64(len(tampered)), source); err == nil {
		t.Fatal("ingest accepted bytes that do not hash to their output id")
	}
	if _, ok := local.get(action); ok {
		t.Fatal("a refused ingest still published a record")
	}
	if _, err := os.Stat(local.dataPath(sumOf(honest))); !os.IsNotExist(err) {
		t.Fatalf("a refused ingest left a data file behind: %v", err)
	}
}

// TestIngestRefusesAShortBlob covers the size half of the same check.
func TestIngestRefusesAShortBlob(t *testing.T) {
	local := newStore(t)
	body := []byte("complete body")
	source := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	action := strings.Repeat("1", idHexLength)
	if _, err := local.ingest(action, sumOf(body), int64(len(body))+1, source); err == nil {
		t.Fatal("ingest accepted a body shorter than its announced size")
	}
}

// TestInvalidIDsAreRejected keeps a cache id from becoming a path: the store
// derives file names from it, so anything that is not a 64-character lowercase
// hex sum is refused before it reaches filepath.Join.
func TestInvalidIDsAreRejected(t *testing.T) {
	local := newStore(t)
	valid := strings.Repeat("2", idHexLength)
	for _, id := range []string{"", "../../etc/passwd", strings.Repeat("A", idHexLength), strings.Repeat("2", 63)} {
		if _, err := local.put(id, valid, 0, bytes.NewReader(nil)); err == nil {
			t.Errorf("put accepted action id %q", id)
		}
		if _, err := local.put(valid, id, 0, bytes.NewReader(nil)); err == nil {
			t.Errorf("put accepted output id %q", id)
		}
		if _, ok := local.get(id); ok {
			t.Errorf("get accepted action id %q", id)
		}
	}
}

// TestRecordRoundTripsItsFields pins the record format, which two processes
// share through the filesystem and which a version bump has to keep readable.
func TestRecordRoundTripsItsFields(t *testing.T) {
	want := entry{outputID: strings.Repeat("3", idHexLength), size: 4242, time: time.Unix(0, 1_700_000_000_123_456_789)}
	got, ok := parseRecord(formatRecord(want))
	if !ok {
		t.Fatalf("a record this package wrote did not parse: %q", formatRecord(want))
	}
	if got.outputID != want.outputID || got.size != want.size || !got.time.Equal(want.time) {
		t.Fatalf("record round-tripped as %+v, want %+v", got, want)
	}
	for _, bad := range []string{
		"",
		"v2 " + want.outputID + " 1 1",
		"v1 not-hex 1 1",
		"v1 " + want.outputID + " notanumber 1",
		"v1 " + want.outputID + " 1",
		"v1 " + want.outputID + " -1 1",
	} {
		if _, ok := parseRecord(bad); ok {
			t.Errorf("parseRecord accepted %q", bad)
		}
	}
}

// TestTouchRefreshesOnlyStaleEntries pins the "last used" bookkeeping the
// collector reads. A hot entry must not pay a write per lookup, and a cold one
// that is being read must stop looking cold — the collector's grace window is
// what keeps a running compiler's inputs alive.
func TestTouchRefreshesOnlyStaleEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fresh := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, fresh, fresh); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	touch(path, time.Time{})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.ModTime().After(fresh.Add(time.Second)) {
		t.Fatalf("a one-minute-old entry was rewritten (mtime %s)", info.ModTime())
	}

	stale := time.Now().Add(-3 * mtimeInterval)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	touch(path, time.Time{})
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("a stale entry kept mtime %s after a lookup", info.ModTime())
	}
}

// TestStoreLayoutMatchesTheCollector pins the file names against the extension
// collector's recognizer (internal/jobs/cachepolicy): a two-character shard and
// a 64-hex name with "-a" or "-d". A layout the collector does not recognize is
// a cache nothing ever bounds.
func TestStoreLayoutMatchesTheCollector(t *testing.T) {
	local := newStore(t)
	body := []byte("collected body")
	action := strings.Repeat("4", idHexLength)
	stored, err := local.put(action, sumOf(body), int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	for path, root := range map[string]string{local.recordPath(action): local.records, stored.diskPath: local.data} {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 2 || len(parts[0]) != 2 {
			t.Fatalf("%s is not <shard>/<name>", relative)
		}
		name := parts[1]
		if len(name) != idHexLength+2 || !strings.HasPrefix(name, parts[0]) {
			t.Fatalf("%s is not a sharded 64-hex entry name", relative)
		}
		if suffix := name[idHexLength:]; suffix != recordSuffix && suffix != dataSuffix {
			t.Fatalf("%s has suffix %q, want %q or %q", relative, suffix, recordSuffix, dataSuffix)
		}
		if !validID(name[:idHexLength]) {
			t.Fatalf("%s is not named after a hex id", relative)
		}
	}
}

// TestOpenStoreRefusesAnEmptyRoot keeps the one failure that must stay a
// failure: a helper with nowhere to write cannot answer a put honestly.
func TestOpenStoreRefusesAnEmptyRoot(t *testing.T) {
	valid := t.TempDir()
	if _, err := openStore("   ", valid); err == nil {
		t.Fatal("openStore accepted an empty record directory")
	}
	if _, err := openStore(valid, "   "); err == nil {
		t.Fatal("openStore accepted an empty data directory")
	}
}

// TestPutKeepsItsDataInTheGoCommandsOwnDirectory pins where each kind of file
// lands. The body goes to the go command's own GOCACHE directory,
// at the path the go command itself would give that output, so a go command
// running without the helper finds it instead of storing a second copy. The
// record stays in the helper's own tree, because the go command reads a file
// of that name in GOCACHE in its own format.
func TestPutKeepsItsDataInTheGoCommandsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	local, err := openStore(filepath.Join(root, progDirName), toolchain.GoBuildCacheDir(root))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	body := []byte("an archive both kinds of go command compile")
	action := strings.Repeat("5", idHexLength)
	output := sumOf(body)

	stored, err := local.put(action, output, int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	goCommandPath := filepath.Join(toolchain.GoBuildCacheDir(root), output[:2], output+dataSuffix)
	if stored.diskPath != goCommandPath {
		t.Fatalf("body stored at %q, want the go command's own path %q", stored.diskPath, goCommandPath)
	}
	record := filepath.Join(root, progDirName, action[:2], action+recordSuffix)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the record is not in the helper's own tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(toolchain.GoBuildCacheDir(root), action[:2], action+recordSuffix)); !os.IsNotExist(err) {
		t.Fatalf("the helper wrote a record into the go command's directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, progDirName, output[:2], output+dataSuffix)); !os.IsNotExist(err) {
		t.Fatalf("the helper kept a second copy of the body in its own tree: %v", err)
	}
}

// TestPutReusesADataFileTheGoCommandAlreadyWrote covers the other direction of
// the sharing: a go command running without the helper compiled this output
// first. Its data file is complete and content addressed, so the helper answers
// with it and neither rewrites it nor stores another copy.
func TestPutReusesADataFileTheGoCommandAlreadyWrote(t *testing.T) {
	local := newStore(t)
	body := []byte("an archive the go command stored without the helper")
	action := strings.Repeat("6", idHexLength)
	output := sumOf(body)
	existing := local.dataPath(output)
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(existing, body, 0o644); err != nil {
		t.Fatalf("write the go command's data file: %v", err)
	}
	written := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(existing, written, written); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	before, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	stored, err := local.put(action, output, int64(len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if stored.diskPath != existing {
		t.Fatalf("put answered %q, want the existing data file %q", stored.diskPath, existing)
	}
	after, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(before, after) || !after.ModTime().Equal(written) {
		t.Fatal("put replaced a complete data file the go command had already written")
	}
	entries, err := os.ReadDir(filepath.Dir(existing))
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the shard holds %d files after the put, want only the shared data file", len(entries))
	}
	got, ok := local.get(action)
	if !ok || got.diskPath != existing {
		t.Fatalf("get = %+v (hit %t), want a hit on the shared data file", got, ok)
	}
}
