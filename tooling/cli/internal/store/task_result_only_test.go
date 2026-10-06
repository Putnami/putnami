package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

const resultOnlyKeyA = "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
const resultOnlyKeyB = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee"

// resultOnlyHit is a task-owned provider hit as a result-only restore returns
// it: the full wire manifest, whose blobs are on no exchange.
func resultOnlyHit(key string) RemoteTaskEntryHit {
	return RemoteTaskEntryHit{
		Key:      key,
		Result:   &EntryResult{Status: "success", Events: []cache.ActionEvent{{Type: "summary", Message: "42 checks"}}},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build~transpile", Project: "pkg", DurationMs: 1500},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: RemoteEntryDescriptorPath, Digest: cache.DigestOf([]byte("descriptor")), Mode: 0o644, Size: 10},
			{Path: "dist/main.js", Digest: cache.DigestOf([]byte("built")), Mode: 0o644, Size: 5},
		}},
	}
}

// TestResultOnlyEntryRecordsTheResultAndNoFile publishes a result-only entry
// and reads it back: the result and provenance survive, the directory holds no
// file tree and no manifest, and no CAS blob was written.
func TestResultOnlyEntryRecordsTheResultAndNoFile(t *testing.T) {
	t.Parallel()
	s := NewLocalStore(t.TempDir())
	published, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA))
	if err != nil {
		t.Fatalf("PublishResultOnlyTaskEntry: %v", err)
	}
	if published.Key != resultOnlyKeyA || published.Address != ResultOnlyTaskEntryAddress(resultOnlyKeyA) {
		t.Fatalf("published = %+v", published)
	}

	got := s.LookupResultOnlyTaskEntry(resultOnlyKeyA)
	if got == nil || got.Result == nil || got.Result.Status != "success" ||
		len(got.Result.Events) != 1 || got.Result.Events[0].Message != "42 checks" {
		t.Fatalf("lookup = %+v, want the recorded result", got)
	}
	if got.Metadata == nil || got.Metadata.Task != "build~transpile" || got.Metadata.DurationMs != 1500 ||
		got.Metadata.Size != 0 || len(got.Metadata.OutputFiles) != 0 || got.Metadata.Hash != got.Address {
		t.Fatalf("metadata = %+v", got.Metadata)
	}
	blobDir := s.blobDir(got.Address)
	for _, name := range []string{"files", manifestFilename, entryDescriptorFilename, "result.json", "meta.json"} {
		if _, err := os.Stat(filepath.Join(blobDir, name)); !os.IsNotExist(err) {
			t.Errorf("result-only entry holds %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "cas")); !os.IsNotExist(err) {
		t.Errorf("publishing a result-only entry wrote the CAS: %v", err)
	}
	if s.LookupResultOnlyTaskEntry(resultOnlyKeyB) != nil {
		t.Fatal("another key read the entry")
	}
}

// TestResultOnlyEntryIsAddressedApartFromEveryOtherEntry keeps the result-only
// record invisible to the task-owned and legacy readers, so no restore can
// mistake it for an entry that holds files, and no remote path can name it.
func TestResultOnlyEntryIsAddressedApartFromEveryOtherEntry(t *testing.T) {
	t.Parallel()
	address := ResultOnlyTaskEntryAddress(resultOnlyKeyA)
	for name, other := range map[string]string{
		"task-owned": TaskEntryAddress(resultOnlyKeyA),
		"remote":     RemoteTaskEntryKey(resultOnlyKeyA),
		"failure":    TaskFailureAddress(resultOnlyKeyA),
		"raw key":    resultOnlyKeyA,
		"other key":  ResultOnlyTaskEntryAddress(resultOnlyKeyB),
	} {
		if address == other {
			t.Errorf("the result-only address equals the %s address", name)
		}
	}

	s := NewLocalStore(t.TempDir())
	if _, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA)); err != nil {
		t.Fatalf("PublishResultOnlyTaskEntry: %v", err)
	}
	if entry, err := s.LookupTaskEntry(resultOnlyKeyA); err != nil || entry != nil {
		t.Errorf("LookupTaskEntry served a result-only entry: %+v %v", entry, err)
	}
	if legacy, err := s.Get(address); err != nil || legacy != nil {
		t.Errorf("the legacy reader interpreted the result-only blob: %+v %v", legacy, err)
	}
	if failure := s.LookupTaskFailure(resultOnlyKeyA); failure != nil {
		t.Errorf("LookupTaskFailure served a result-only entry: %+v", failure)
	}
}

// TestResultOnlyPublishRefusesALegacyPayload accepts only a task-owned hit:
// one entry descriptor with a well-formed digest, a result with a status, and
// a key. Anything else publishes nothing.
func TestResultOnlyPublishRefusesALegacyPayload(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*RemoteTaskEntryHit){
		"no key":    func(h *RemoteTaskEntryHit) { h.Key = "" },
		"no result": func(h *RemoteTaskEntryHit) { h.Result = nil },
		"no status": func(h *RemoteTaskEntryHit) { h.Result = &EntryResult{} },
		"no manifest": func(h *RemoteTaskEntryHit) {
			h.Manifest = nil
		},
		"no descriptor": func(h *RemoteTaskEntryHit) {
			h.Manifest = &cache.Manifest{Files: h.Manifest.Files[1:]}
		},
		"two descriptors": func(h *RemoteTaskEntryHit) {
			h.Manifest = &cache.Manifest{Files: append([]cache.FileEntry{h.Manifest.Files[0]}, h.Manifest.Files...)}
		},
		"malformed descriptor digest": func(h *RemoteTaskEntryHit) {
			files := append([]cache.FileEntry(nil), h.Manifest.Files...)
			files[0].Digest = "sha256:short"
			h.Manifest = &cache.Manifest{Files: files}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := NewLocalStore(t.TempDir())
			hit := resultOnlyHit(resultOnlyKeyA)
			mutate(&hit)
			if _, err := s.PublishResultOnlyTaskEntry(hit); err == nil {
				t.Fatal("published an uninterpretable hit")
			}
			if s.LookupResultOnlyTaskEntry(resultOnlyKeyA) != nil {
				t.Fatal("a refused hit left an entry behind")
			}
		})
	}
	s := NewLocalStore(t.TempDir())
	hit := resultOnlyHit(resultOnlyKeyA)
	hit.Manifest = &cache.Manifest{Files: hit.Manifest.Files[1:]}
	if _, err := s.PublishResultOnlyTaskEntry(hit); !errors.Is(err, ErrEntryFormat) {
		t.Fatalf("a legacy payload error = %v, want ErrEntryFormat", err)
	}
}

// TestUnreadableResultOnlyEntryIsAMiss pins the fail-closed rule: an absent,
// torn, foreign, wrongly formatted or statusless entry reads as no entry.
func TestUnreadableResultOnlyEntryIsAMiss(t *testing.T) {
	t.Parallel()
	record := func(format int, key string, result *EntryResult) string {
		data, _ := json.Marshal(resultOnlyRecord{Format: format, Key: key, Result: result})
		return string(data)
	}
	success := &EntryResult{Status: "success"}
	cases := map[string]map[string]string{
		"torn record":    {resultOnlyRecordFilename: `{"format":1,"key":"`},
		"another format": {resultOnlyRecordFilename: record(CurrentResultOnlyTaskEntryFormat+1, resultOnlyKeyA, success)},
		"another key":    {resultOnlyRecordFilename: record(CurrentResultOnlyTaskEntryFormat, resultOnlyKeyB, success)},
		"no result":      {resultOnlyRecordFilename: record(CurrentResultOnlyTaskEntryFormat, resultOnlyKeyA, nil)},
		"statusless":     {resultOnlyRecordFilename: record(CurrentResultOnlyTaskEntryFormat, resultOnlyKeyA, &EntryResult{})},
		"legacy files only": {
			"result.json": `{"status":"success"}`,
			"meta.json":   `{"hash":"x"}`,
		},
		"empty directory": {},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := NewLocalStore(t.TempDir())
			dir := s.blobDir(ResultOnlyTaskEntryAddress(resultOnlyKeyA))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for file, content := range files {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := s.LookupResultOnlyTaskEntry(resultOnlyKeyA); got != nil {
				t.Fatalf("lookup served an uninterpretable entry: %+v", got)
			}
		})
	}
	if got := NewLocalStore(t.TempDir()).LookupResultOnlyTaskEntry(resultOnlyKeyA); got != nil {
		t.Fatalf("an empty store served an entry: %+v", got)
	}
}

// TestResultOnlyEntryParticipatesInGarbageCollection proves the entry is an
// ordinary blob to GC: counted in the store's usage, stamped with recency,
// keeping no CAS blob alive, and evicted like any other entry.
func TestResultOnlyEntryParticipatesInGarbageCollection(t *testing.T) {
	t.Parallel()
	s := NewLocalStore(t.TempDir())
	if _, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA)); err != nil {
		t.Fatalf("PublishResultOnlyTaskEntry: %v", err)
	}
	scan, state := scanStore(s.Root(), false)
	if state != scanDone {
		t.Fatal("scanStore refused a store holding only a result-only entry")
	}
	if len(scan.entries) != 1 {
		t.Fatalf("scanStore found %d entries, want the result-only one", len(scan.entries))
	}
	if scan.entries[0].lastUsed.IsZero() || scan.entries[0].gen < 0 {
		t.Errorf("the result-only entry has no recency: %+v", scan.entries[0])
	}
	if len(scan.entries[0].blobs) != 0 {
		t.Errorf("the result-only entry keeps CAS blobs alive: %+v", scan.entries[0].blobs)
	}
	recordInfo, err := os.Stat(filepath.Join(s.blobDir(ResultOnlyTaskEntryAddress(resultOnlyKeyA)), resultOnlyRecordFilename))
	if err != nil {
		t.Fatal(err)
	}
	if scan.total < recordInfo.Size() {
		t.Errorf("store usage = %d bytes: the result-only record escapes the byte budget", scan.total)
	}

	if _, err := RunGC([]string{s.Root()}, GCOptions{MaxBytes: 1}); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if got := s.LookupResultOnlyTaskEntry(resultOnlyKeyA); got != nil {
		t.Fatalf("an evicted result-only entry still served: %+v", got)
	}
}

// TestConcurrentResultOnlyPublishesKeepOneWholeEntry races publishers of one
// key against readers: every publisher succeeds, and every reader observes
// either no entry or a whole one.
func TestConcurrentResultOnlyPublishesKeepOneWholeEntry(t *testing.T) {
	t.Parallel()
	s := NewLocalStore(t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	torn := make(chan *ResultOnlyTaskEntry, 64)
	for range 8 {
		wg.Go(func() {
			if _, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA)); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			for range 8 {
				if got := s.LookupResultOnlyTaskEntry(resultOnlyKeyA); got != nil && (got.Result == nil || got.Result.Status != "success") {
					torn <- got
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	close(torn)
	for err := range errs {
		t.Errorf("a concurrent publish failed: %v", err)
	}
	for got := range torn {
		t.Errorf("a reader observed a torn entry: %+v", got)
	}
	if s.LookupResultOnlyTaskEntry(resultOnlyKeyA) == nil {
		t.Fatal("no entry survived the race")
	}
}

// TestResultOnlyEntryDoesNotShadowAFullEntry keeps the two models apart in
// both directions: a full entry published after a result-only one is served
// by LookupTaskEntry with its files, and the result-only entry stays readable.
func TestResultOnlyEntryDoesNotShadowAFullEntry(t *testing.T) {
	t.Parallel()
	s := NewLocalStore(t.TempDir())
	if _, err := s.PublishResultOnlyTaskEntry(resultOnlyHit(resultOnlyKeyA)); err != nil {
		t.Fatalf("PublishResultOnlyTaskEntry: %v", err)
	}
	staging := t.TempDir()
	out := DeclaredEntryOutput{ID: "out", Kind: proto.OutputKindFile, Root: proto.OutputRootProject, Path: "out.txt"}
	stage(t, staging, out, "", "content")
	if _, err := s.IngestTaskEntry(staging, taskSpec(resultOnlyKeyA, out)); err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	entry, err := s.LookupTaskEntry(resultOnlyKeyA)
	if err != nil || entry == nil || entry.FilesDir == "" {
		t.Fatalf("LookupTaskEntry = %+v %v, want the full entry", entry, err)
	}
	if s.LookupResultOnlyTaskEntry(resultOnlyKeyA) == nil {
		t.Fatal("publishing the full entry hid the result-only one")
	}
}
