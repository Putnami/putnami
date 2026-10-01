package versioncmd

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const (
	// pinRecordGo is the Go release the fixture index lists.
	pinRecordGo = "1.26.2"
	// pinRecordLinuxSHA and pinRecordDarwinSHA are the digests the fixture
	// index publishes for that release.
	pinRecordLinuxSHA  = "1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a"
	pinRecordDarwinSHA = "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"
)

// goIndexFixture is a Go release index that lists pinRecordGo for linux/amd64
// and darwin/arm64, in a Putnami home of the test's own.
type goIndexFixture struct {
	// requests counts the index requests the fixture answered.
	requests atomic.Int32
	// home is the Putnami home of the test.
	home string
	// source is the download source a pin from this index records.
	source string
}

func newGoIndexFixture(t *testing.T) *goIndexFixture {
	t.Helper()
	fixture := &goIndexFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go" {
			http.NotFound(w, r)
			return
		}
		fixture.requests.Add(1)
		_, _ = w.Write([]byte(`[{"version":"go` + pinRecordGo + `","files":[` +
			`{"filename":"go` + pinRecordGo + `.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + pinRecordLinuxSHA + `"},` +
			`{"filename":"go` + pinRecordGo + `.darwin-arm64.tar.gz","os":"darwin","arch":"arm64","kind":"archive","sha256":"` + pinRecordDarwinSHA + `"}]},` +
			`{"version":"go../` + pinRecordGo + `","files":[` +
			`{"filename":"go.linux-amd64.tar.gz","os":"linux","arch":"amd64","kind":"archive","sha256":"` + pinRecordLinuxSHA + `"}]}]`))
	}))
	t.Cleanup(server.Close)
	withToolchainTestEndpoints(t, server.URL)
	fixture.home = os.Getenv("PUTNAMI_HOME")
	fixture.source = goSourceURL
	return fixture
}

// record is the path of the pin record of pinRecordGo.
func (f *goIndexFixture) record() string {
	return filepath.Join(f.home, "toolchains", "go", "go-"+pinRecordGo+".pin.json")
}

// recordBytes are the bytes of the pin record a fetch of pinRecordGo writes.
func (f *goIndexFixture) recordBytes() []byte {
	return []byte("{\n" +
		"  \"version\": \"" + pinRecordGo + "\",\n" +
		"  \"integrities\": {\n" +
		"    \"darwin/arm64\": \"" + pinRecordDarwinSHA + "\",\n" +
		"    \"linux/amd64\": \"" + pinRecordLinuxSHA + "\"\n" +
		"  },\n" +
		"  \"source\": \"" + f.source + "\"\n" +
		"}\n")
}

// entry is the lock entry the fixture index publishes for pinRecordGo.
func (f *goIndexFixture) entry() lockfile.LockEntry {
	return lockfile.LockEntry{
		Version:     pinRecordGo,
		Integrities: map[string]string{"linux/amd64": pinRecordLinuxSHA, "darwin/arm64": pinRecordDarwinSHA},
		Source:      f.source,
	}
}

func sameGoPin(a, b lockfile.LockEntry) bool {
	return a.Version == b.Version && a.Source == b.Source && maps.Equal(a.Integrities, b.Integrities) &&
		a.Integrity == "" && b.Integrity == "" && a.ManifestHash == "" && b.ManifestHash == ""
}

// A machine that pinned a Go release once holds its entry in a pin record
// under the Putnami home. A later pin of the same release, in any workspace,
// returns that entry and makes no request. A release the index does not list
// leaves no record.
func TestAGoReleaseTheMachineRecordedPinsWithoutARequest(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "go-pin-reuses-the-machine-record",
		"a-recorded-go-release-pins-without-a-request")
	fixture := newGoIndexFixture(t)
	ctx := context.Background()

	first, err := resolveGoToolchain(ctx, t.TempDir(), pinRecordGo)
	if err != nil {
		t.Fatal(err)
	}
	if !sameGoPin(first, fixture.entry()) {
		t.Fatalf("first pin = %+v, want the entry the index publishes %+v", first, fixture.entry())
	}
	if got := fixture.requests.Load(); got != 1 {
		t.Fatalf("the first pin made %d index requests, want 1", got)
	}
	if data, err := os.ReadFile(fixture.record()); err != nil || !bytes.Equal(data, fixture.recordBytes()) {
		t.Fatalf("pin record = %q, %v; want\n%s", data, err, fixture.recordBytes())
	}

	second, err := resolveGoToolchain(ctx, t.TempDir(), pinRecordGo)
	if err != nil {
		t.Fatal(err)
	}
	if got := fixture.requests.Load(); got != 1 {
		t.Fatalf("a pin of a recorded release made %d more index requests, want none", got-1)
	}
	if !sameGoPin(second, first) {
		t.Fatalf("pin from the record = %+v, want the fetched entry %+v", second, first)
	}

	if _, err := resolveGoToolchain(ctx, t.TempDir(), "1.26.3"); err == nil {
		t.Fatal("a release the index does not list resolved")
	}
	if got := fixture.requests.Load(); got != 2 {
		t.Fatalf("a release with no record made %d index requests, want 1", got-1)
	}
	if entries, err := os.ReadDir(filepath.Dir(fixture.record())); err != nil || len(entries) != 1 {
		t.Fatalf("toolchains/go holds %v, %v; want only the record of %s", entries, err, pinRecordGo)
	}
}

// A record is used only when it is the entry a fetch would return. Any other
// content is ignored: the index is fetched, the pin is the published entry,
// and the fetched entry replaces the record.
func TestAnInvalidGoPinRecordIsIgnoredAndReplaced(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "go-pin-reuses-the-machine-record",
		"an-invalid-record-is-ignored-and-replaced")
	record := func(version, source, integrities string) string {
		return `{"version":"` + version + `","integrities":{` + integrities + `},"source":"` + source + `"}`
	}
	linux := `"linux/amd64":"` + pinRecordLinuxSHA + `"`
	otherSHA := strings.Repeat("9", 64)
	cases := map[string]func(source string) string{
		"empty":                func(string) string { return "" },
		"not JSON":             func(string) string { return "{" },
		"not an object":        func(string) string { return `"` + pinRecordGo + `"` },
		"data after the entry": func(source string) string { return record(pinRecordGo, source, linux) + "{}" },
		"another version":      func(source string) string { return record("1.26.1", source, linux) },
		"another source":       func(string) string { return record(pinRecordGo, "https://example.test/dl/", linux) },
		"no digest":            func(source string) string { return record(pinRecordGo, source, "") },
		"short digest":         func(source string) string { return record(pinRecordGo, source, `"linux/amd64":"abc"`) },
		"digest that is not hex": func(source string) string {
			return record(pinRecordGo, source, `"linux/amd64":"`+strings.Repeat("z", 64)+`"`)
		},
		"uppercase digest": func(source string) string {
			return record(pinRecordGo, source, `"linux/amd64":"`+strings.ToUpper(pinRecordLinuxSHA)+`"`)
		},
		"prefixed digest": func(source string) string {
			return record(pinRecordGo, source, `"linux/amd64":"sha256:`+pinRecordLinuxSHA+`"`)
		},
		"one bad digest among good ones": func(source string) string {
			return record(pinRecordGo, source, linux+`,"darwin/arm64":"abc"`)
		},
		"unsupported platform": func(source string) string {
			return record(pinRecordGo, source, linux+`,"plan9/amd64":"`+otherSHA+`"`)
		},
		"platform that is not os/arch": func(source string) string {
			return record(pinRecordGo, source, `"linux":"`+otherSHA+`"`)
		},
		"larger than a record": func(source string) string {
			return record(pinRecordGo, source, linux) + strings.Repeat(" ", goPinRecordMaxBytes)
		},
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newGoIndexFixture(t)
			if err := os.MkdirAll(filepath.Dir(fixture.record()), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteToolchainTestFile(t, fixture.record(), content(fixture.source))

			entry, err := resolveGoToolchain(context.Background(), t.TempDir(), pinRecordGo)
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.requests.Load(); got != 1 {
				t.Fatalf("the pin made %d index requests, want 1: the record must be ignored", got)
			}
			if !sameGoPin(entry, fixture.entry()) {
				t.Fatalf("pin = %+v, want the entry the index publishes %+v", entry, fixture.entry())
			}
			if data, err := os.ReadFile(fixture.record()); err != nil || !bytes.Equal(data, fixture.recordBytes()) {
				t.Fatalf("pin record after the fetch = %q, %v; want it replaced by\n%s", data, err, fixture.recordBytes())
			}
		})
	}
}

// A record with a valid digest for one platform more or less than the index
// lists is still the entry of a release, so it is used: the check is on the
// form of the record, and the published archives never change.
func TestAGoPinRecordIsUsedAsWritten(t *testing.T) {
	fixture := newGoIndexFixture(t)
	if err := os.MkdirAll(filepath.Dir(fixture.record()), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteToolchainTestFile(t, fixture.record(),
		`{"version":"`+pinRecordGo+`","integrities":{"windows/arm64":"`+pinRecordLinuxSHA+`"},"source":"`+fixture.source+`","note":"kept"}`)

	entry, err := resolveGoToolchain(context.Background(), t.TempDir(), pinRecordGo)
	if err != nil {
		t.Fatal(err)
	}
	want := lockfile.LockEntry{
		Version:     pinRecordGo,
		Integrities: map[string]string{"windows/arm64": pinRecordLinuxSHA},
		Source:      fixture.source,
	}
	if got := fixture.requests.Load(); got != 0 || !sameGoPin(entry, want) {
		t.Fatalf("pin = %+v after %d index requests, want the recorded %+v and no request", entry, got, want)
	}
}

// A Putnami home that cannot hold the record does not fail the pin: the entry
// comes from the index each time.
func TestAGoPinSucceedsWhenTheRecordCannotBeWritten(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "go-pin-reuses-the-machine-record",
		"an-unwritable-home-still-pins")
	cases := map[string]func(t *testing.T, fixture *goIndexFixture){
		"the home is below a file": func(t *testing.T, fixture *goIndexFixture) {
			file := filepath.Join(t.TempDir(), "file")
			mustWriteToolchainTestFile(t, file, "")
			fixture.home = filepath.Join(file, "home")
			t.Setenv("PUTNAMI_HOME", fixture.home)
		},
		"a directory is where the record goes": func(t *testing.T, fixture *goIndexFixture) {
			if err := os.MkdirAll(filepath.Join(fixture.record(), "kept"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newGoIndexFixture(t)
			prepare(t, fixture)
			for attempt := int32(1); attempt <= 2; attempt++ {
				entry, err := resolveGoToolchain(context.Background(), t.TempDir(), pinRecordGo)
				if err != nil {
					t.Fatalf("pin %d with a record that cannot be written: %v", attempt, err)
				}
				if !sameGoPin(entry, fixture.entry()) {
					t.Fatalf("pin %d = %+v, want the entry the index publishes %+v", attempt, entry, fixture.entry())
				}
				if got := fixture.requests.Load(); got != attempt {
					t.Fatalf("after pin %d the index answered %d requests, want %d", attempt, got, attempt)
				}
			}
			if info, err := os.Lstat(fixture.record()); err == nil && info.Mode().IsRegular() {
				t.Fatalf("a record was written at %s", fixture.record())
			}
		})
	}
}

// The lock a workspace gets holds the same bytes whether its Go entry came
// from the pin record or from the release index.
func TestALockPinnedFromTheRecordHoldsTheBytesOfOnePinnedFromTheIndex(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "go-pin-reuses-the-machine-record",
		"the-lock-holds-the-same-bytes-from-the-record-and-from-the-index")
	fixture := newGoIndexFixture(t)
	pinnedLock := func(wantRequests int32, from string) []byte {
		t.Helper()
		ws := t.TempDir()
		mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"), "go "+pinRecordGo+"\n")
		lf := lockfile.NewLockFile()
		lf.Version = lockfile.FormatVersionV3
		lf.SetCLI(lockfile.LockEntry{Version: "1.2.3"})
		if err := lockfile.WriteLockFile(ws, lf); err != nil {
			t.Fatal(err)
		}
		if changed, err := PinDeclaredToolchains(context.Background(), ws); err != nil || !changed {
			t.Fatalf("PinDeclaredToolchains from %s = %v, %v; want the go pin written", from, changed, err)
		}
		if got := fixture.requests.Load(); got != wantRequests {
			t.Fatalf("after the pin from %s the index answered %d requests, want %d", from, got, wantRequests)
		}
		data, err := os.ReadFile(filepath.Join(ws, lockfile.LockFilename))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	fromIndex := pinnedLock(1, "the index")
	fromRecord := pinnedLock(1, "the record")
	if !bytes.Equal(fromIndex, fromRecord) {
		t.Fatalf("lock pinned from the record =\n%s\nwant the bytes of the lock pinned from the index:\n%s", fromRecord, fromIndex)
	}
	if !bytes.Contains(fromRecord, []byte(pinRecordDarwinSHA)) || !bytes.Contains(fromRecord, []byte(pinRecordLinuxSHA)) {
		t.Fatalf("lock pinned from the record =\n%s\nwant both published digests", fromRecord)
	}

	// A metadata refresh, the step `putnami install` and `putnami init` end
	// with, reads the same record.
	ws := t.TempDir()
	mustWriteToolchainTestFile(t, filepath.Join(ws, "go.work"), "go "+pinRecordGo+"\n")
	lf := lockfile.NewLockFile()
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if entry, _ := lf.GetToolchain("go"); fixture.requests.Load() != 1 || !sameGoPin(entry, fixture.entry()) {
		t.Fatalf("refresh pinned %+v after %d index requests, want the recorded entry and no new request",
			entry, fixture.requests.Load())
	}
}

// Pins of one release that start together each write the record. Every pin
// returns the published entry, and the directory is left with one complete
// record and no temporary file.
func TestGoPinsThatStartTogetherLeaveOneCompleteRecord(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "go-pin-reuses-the-machine-record",
		"pins-that-start-together-leave-one-complete-record")
	fixture := newGoIndexFixture(t)
	const writers = 16
	workspaces := make([]string, writers)
	for i := range workspaces {
		workspaces[i] = t.TempDir()
	}
	entries := make([]lockfile.LockEntry, writers)
	errs := make([]error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entries[i], errs[i] = resolveGoToolchain(context.Background(), workspaces[i], pinRecordGo)
		}()
	}
	close(start)
	wg.Wait()

	for i := range writers {
		if errs[i] != nil || !sameGoPin(entries[i], fixture.entry()) {
			t.Fatalf("pin %d = %+v, %v; want the entry the index publishes", i, entries[i], errs[i])
		}
	}
	if data, err := os.ReadFile(fixture.record()); err != nil || !bytes.Equal(data, fixture.recordBytes()) {
		t.Fatalf("pin record = %q, %v; want one complete record\n%s", data, err, fixture.recordBytes())
	}
	dir, err := os.ReadDir(filepath.Dir(fixture.record()))
	if err != nil {
		t.Fatal(err)
	}
	if len(dir) != 1 {
		names := make([]string, len(dir))
		for i, entry := range dir {
			names[i] = entry.Name()
		}
		t.Fatalf("toolchains/go holds %v, want only the record", names)
	}
	before := fixture.requests.Load()
	if _, err := resolveGoToolchain(context.Background(), t.TempDir(), pinRecordGo); err != nil || fixture.requests.Load() != before {
		t.Fatalf("a pin after the writers: %v, %d new index requests; want the record used", err, fixture.requests.Load()-before)
	}
}

// go.work is a file of the repository. A release name that holds a path
// separator still resolves from the index, but it has no record: nothing is
// read or written under the Putnami home for it.
func TestAGoReleaseNameThatIsNotPlainHasNoRecord(t *testing.T) {
	fixture := newGoIndexFixture(t)
	for attempt := int32(1); attempt <= 2; attempt++ {
		entry, err := resolveGoToolchain(context.Background(), t.TempDir(), "../"+pinRecordGo)
		if err != nil || entry.Integrities["linux/amd64"] != pinRecordLinuxSHA {
			t.Fatalf("pin %d = %+v, %v; want the entry the index publishes", attempt, entry, err)
		}
		if got := fixture.requests.Load(); got != attempt {
			t.Fatalf("after pin %d the index answered %d requests, want %d", attempt, got, attempt)
		}
	}
	if entries, err := os.ReadDir(fixture.home); err != nil || len(entries) != 0 {
		t.Fatalf("the Putnami home holds %v, %v; want nothing written", entries, err)
	}
	if _, ok := goPinRecordPath(t.TempDir(), ""); ok {
		t.Fatal("an empty release name has a record")
	}
}
