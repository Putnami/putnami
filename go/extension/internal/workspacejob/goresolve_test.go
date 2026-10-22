package workspacejob

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// digestOf is a lowercase 64-hex SHA-256 made of one repeated hex digit.
func digestOf(c byte) string { return strings.Repeat(string(c), 64) }

// isolatedPutnamiHome points PUTNAMI_HOME at a fresh directory, so no test
// reads or writes the pin records of the real home, and returns the directory
// that holds the Go pin records.
func isolatedPutnamiHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PUTNAMI_HOME", home)
	return filepath.Join(home, "toolchains", "go")
}

// goWorkspace returns a workspace root whose go.work holds goWork, or a root
// with no go.work when goWork is empty.
func goWorkspace(t *testing.T, goWork string) string {
	t.Helper()
	root := t.TempDir()
	if goWork != "" {
		if err := os.WriteFile(filepath.Join(root, "go.work"), []byte(goWork), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// serveReleaseIndex points the release index at a local server that answers
// with handler, and returns the number of requests it received.
func serveReleaseIndex(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	previous := goReleaseIndexURL
	goReleaseIndexURL = server.URL + "/dl/?mode=json&include=all"
	t.Cleanup(func() { goReleaseIndexURL = previous })
	return &requests
}

// forbidReleaseIndex points the release index at a local server that fails
// the test when it is asked.
func forbidReleaseIndex(t *testing.T) {
	t.Helper()
	serveReleaseIndex(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the answer requested the release index")
		w.WriteHeader(http.StatusTeapot)
	})
}

func goResolveRequest(platform string, offline bool, committed *wsproto.LockToolchainEntry) wsproto.ToolchainResolveRequest {
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  "@putnami/go",
		Identities: []string{GoLockIdentity},
		Platform:   platform,
		Offline:    offline,
	}
	if committed != nil {
		request.Locked = map[string]wsproto.LockToolchainEntry{GoLockIdentity: *committed}
	}
	return request
}

// answerGo answers request for root and checks the answer against the
// protocol's two validations, as the serve function does before it writes it.
func answerGo(t *testing.T, root string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolveResult {
	t.Helper()
	result, err := AnswerToolchainResolve(root, request, os.Getenv)
	if err != nil {
		t.Fatalf("AnswerToolchainResolve: %v", err)
	}
	result.Version = wsproto.ToolchainResolveProtocolVersion
	result.Extension = request.Extension
	diags := append(wsproto.ValidateToolchainResolveResult(&result), wsproto.ValidateToolchainResolveAnswer(&request, &result)...)
	if diag.HasErrors(diags) {
		t.Fatalf("the answer breaks the toolchain-resolve contract: %v\n%+v", diags, result)
	}
	return result
}

func goPinOf(t *testing.T, result wsproto.ToolchainResolveResult) wsproto.LockToolchainEntry {
	t.Helper()
	resolution, ok := result.Toolchains[GoLockIdentity]
	if !ok || resolution.Pin == nil {
		t.Fatalf("the answer pins no Go: %+v", result.Toolchains)
	}
	return *resolution.Pin
}

func goProblemOf(t *testing.T, result wsproto.ToolchainResolveResult) wsproto.ToolchainProblem {
	t.Helper()
	resolution, ok := result.Toolchains[GoLockIdentity]
	if !ok || resolution.Problem == nil {
		t.Fatalf("the answer reports no Go problem: %+v", result.Toolchains)
	}
	return *resolution.Problem
}

// goDotDevPin is the lock version 5 entry go.dev publishes for version with
// integrities.
func goDotDevPin(version string, integrities map[string]string) wsproto.LockToolchainEntry {
	pin := wsproto.LockToolchainEntry{
		Version:     version,
		Archives:    map[string]wsproto.LockToolchainArchive{},
		Integrities: integrities,
	}
	for platform := range integrities {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive := wsproto.LockToolchainArchive{
			URL:        "https://go.dev/dl/go" + version + "." + goos + "-" + goarch + ".tar.gz",
			Format:     wsproto.LockArchiveFormatTarGz,
			Executable: "go/bin/go",
		}
		if goos == "windows" {
			archive.URL = "https://go.dev/dl/go" + version + "." + goos + "-" + goarch + ".zip"
			archive.Format = wsproto.LockArchiveFormatZip
			archive.Executable = "go/bin/go.exe"
		}
		pin.Archives[platform] = archive
	}
	return pin
}

// Rule a: the declaration is the higher exact release of go.work's go and
// toolchain directives, and a missing go.work, or one that names neither,
// declares none.
func TestGoWorkDeclarationIsTheHigherDirective(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-toolchain-pin",
		"the-release-is-the-higher-go-work-directive")
	for _, tc := range []struct {
		name, goWork, want string
	}{
		{"no go.work", "", ""},
		{"go directive", "go 1.26.1\n\nuse ./app\n", "1.26.1"},
		{"major.minor go directive", "go 1.26\n", "1.26.0"},
		{"higher toolchain", "go 1.25.7\ntoolchain go1.26.2\n", "1.26.2"},
		{"higher go", "go 1.26.3\ntoolchain go1.26.2\n", "1.26.3"},
		{"toolchain default", "go 1.26.1\ntoolchain default\n", "1.26.1"},
		{"toolchain alone", "toolchain go1.26.2\n", "1.26.2"},
		{"comment", "go 1.26.1 // the workspace minimum\n", "1.26.1"},
		{"commented out", "// go 1.26.1\nuse ./app\n", ""},
		{"neither", "use ./app\n", ""},
		{"no exact release", "go 1.27rc1\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GoWorkDeclaration(goWorkspace(t, tc.goWork))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("GoWorkDeclaration(%q) = %q, want %q", tc.goWork, got, tc.want)
			}
		})
	}
}

// Rule b: a committed lock version 5 pin at the declared release, or any
// committed pin when go.work declares none, is kept as committed, with no
// request.
func TestAnswerKeepsACommittedFormat5Pin(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-toolchain-pin",
		"a-committed-format-5-pin-of-the-release-is-kept-as-committed")
	committed := wsproto.LockToolchainEntry{
		Version: "1.26.1",
		Archives: map[string]wsproto.LockToolchainArchive{
			"linux/amd64":  {URL: "https://mirror.example/go/go1.26.1.linux-amd64.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go"},
			"darwin/arm64": {URL: "https://mirror.example/go/go1.26.1.darwin-arm64.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go"},
		},
		Integrities: map[string]string{"linux/amd64": digestOf('a'), "darwin/arm64": digestOf('b')},
	}
	for _, tc := range []struct{ name, goWork string }{
		{"declared release", "go 1.26.1\n"},
		{"no declaration", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPutnamiHome(t)
			forbidReleaseIndex(t)
			result := answerGo(t, goWorkspace(t, tc.goWork), goResolveRequest("linux/amd64", false, &committed))
			if pin := goPinOf(t, result); !reflect.DeepEqual(pin, committed) {
				t.Fatalf("pin = %+v, want the committed entry %+v", pin, committed)
			}
		})
	}
}

// Rule b: a committed lock version 4 pin at the declared release is converted
// offline. Each archive is the one the installer downloads for that platform,
// its integrities are kept, and the pin names no source.
func TestAnswerConvertsACommittedFormat4PinOffline(t *testing.T) {
	integrities := map[string]string{
		"darwin/arm64":  digestOf('1'),
		"linux/amd64":   digestOf('2'),
		"windows/amd64": digestOf('3'),
	}
	for _, tc := range []struct{ name, source, base string }{
		{"committed source", "https://mirror.example/go", "https://mirror.example/go/"},
		{"default source", "", "https://go.dev/dl/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPutnamiHome(t)
			forbidReleaseIndex(t)
			committed := wsproto.LockToolchainEntry{Version: "1.26.1", Integrities: integrities, Source: tc.source}
			result := answerGo(t, goWorkspace(t, "go 1.26.1\n"), goResolveRequest("darwin/arm64", true, &committed))
			pin := goPinOf(t, result)
			want := wsproto.LockToolchainEntry{
				Version: "1.26.1",
				Archives: map[string]wsproto.LockToolchainArchive{
					"darwin/arm64":  {URL: tc.base + "go1.26.1.darwin-arm64.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go"},
					"linux/amd64":   {URL: tc.base + "go1.26.1.linux-amd64.tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "go/bin/go"},
					"windows/amd64": {URL: tc.base + "go1.26.1.windows-amd64.zip", Format: wsproto.LockArchiveFormatZip, Executable: "go/bin/go.exe"},
				},
				Integrities: integrities,
			}
			if !reflect.DeepEqual(pin, want) {
				t.Fatalf("pin = %+v, want %+v", pin, want)
			}
			lock := GoLock{Version: committed.Version, Integrities: committed.Integrities, Source: committed.Source}
			for platform, archive := range pin.Archives {
				goos, goarch, _ := strings.Cut(platform, "/")
				installer := GoArchivePin(lock, goos, goarch)
				if archive.URL != installer.URL || archive.Format != string(installer.Format) {
					t.Errorf("%s archive = %s (%s), the installer downloads %s (%s)",
						platform, archive.URL, archive.Format, installer.URL, installer.Format)
				}
			}
		})
	}
}

// Rule c: a declared release the lock does not pin is resolved from its pin
// record under the Putnami home, with no request.
func TestAnswerResolvesTheDeclaredReleaseFromItsPinRecord(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-pin-reuses-the-machine-record",
		"a-recorded-release-is-answered-without-a-request")
	records := isolatedPutnamiHome(t)
	forbidReleaseIndex(t)
	integrities := map[string]string{"linux/amd64": digestOf('c'), "windows/arm64": digestOf('d')}
	if err := writeGoPinRecord(records, GoLock{Version: "1.26.2", Integrities: integrities, Source: goReleaseSource}); err != nil {
		t.Fatal(err)
	}
	committed := wsproto.LockToolchainEntry{Version: "1.26.1", Integrities: map[string]string{"linux/amd64": digestOf('e')}, Source: goReleaseSource}

	result := answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("linux/amd64", false, &committed))
	if pin, want := goPinOf(t, result), goDotDevPin("1.26.2", integrities); !reflect.DeepEqual(pin, want) {
		t.Fatalf("pin = %+v, want %+v", pin, want)
	}
}

// A pin record that names another source is not the release index's answer
// and is not read; neither is a version that would name a file outside the
// record directory.
func TestGoPinRecordAcceptsOnlyTheIndexEntry(t *testing.T) {
	records := t.TempDir()
	lock := GoLock{Version: "1.26.2", Integrities: map[string]string{"linux/amd64": digestOf('f')}, Source: "https://mirror.example/go/"}
	if err := writeGoPinRecord(records, lock); err != nil {
		t.Fatal(err)
	}
	if _, ok := readGoPinRecord(records, "1.26.2"); ok {
		t.Fatal("a pin record that names another source was read")
	}
	if _, ok := goPinRecordPath(records, "1.26/../../x"); ok {
		t.Fatal("a version with a path separator names a pin record")
	}
}

// Rule c: with no record, the declared release is resolved from the go.dev
// release index, and the answer is written as the pin record the CLI reads,
// so the next answer makes no request.
func TestAnswerResolvesTheDeclaredReleaseFromTheIndexAndRecordsIt(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-toolchain-pin",
		"each-platform-archive-and-digest-come-from-the-go-dev-index")
	spectest.Proves(t, "go/go-project-toolchain", "go-pin-reuses-the-machine-record",
		"a-resolved-release-is-recorded-under-the-putnami-home")
	records := isolatedPutnamiHome(t)
	index := []goRelease{
		{Version: "go1.26.1", Files: []goFile{{OS: "linux", Arch: "amd64", Kind: "archive", SHA256: digestOf('0')}}},
		{Version: "go1.26.2", Files: []goFile{
			{Filename: "go1.26.2.darwin-amd64.tar.gz", OS: "darwin", Arch: "amd64", Kind: "archive", SHA256: digestOf('1')},
			{Filename: "go1.26.2.darwin-arm64.tar.gz", OS: "darwin", Arch: "arm64", Kind: "archive", SHA256: "sha256:" + strings.ToUpper(digestOf('a'))},
			{Filename: "go1.26.2.darwin-arm64.pkg", OS: "darwin", Arch: "arm64", Kind: "installer", SHA256: digestOf('9')},
			{Filename: "go1.26.2.linux-amd64.tar.gz", OS: "linux", Arch: "amd64", Kind: "archive", SHA256: digestOf('2')},
			{Filename: "go1.26.2.linux-386.tar.gz", OS: "linux", Arch: "386", Kind: "archive", SHA256: digestOf('8')},
			{Filename: "go1.26.2.freebsd-amd64.tar.gz", OS: "freebsd", Arch: "amd64", Kind: "archive", SHA256: digestOf('7')},
			{Filename: "go1.26.2.windows-amd64.zip", OS: "windows", Arch: "amd64", Kind: "archive", SHA256: digestOf('3')},
			{Filename: "go1.26.2.linux-arm64.tar.gz", OS: "linux", Arch: "arm64", Kind: "archive", SHA256: "not a digest"},
			{Filename: "go1.26.2.src.tar.gz", Kind: "source", SHA256: digestOf('6')},
		}},
	}
	requests := serveReleaseIndex(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(index)
	})
	root := goWorkspace(t, "go 1.25.7\ntoolchain go1.26.2\n")
	request := goResolveRequest("darwin/arm64", false, nil)

	integrities := map[string]string{
		"darwin/amd64":  digestOf('1'),
		"darwin/arm64":  digestOf('a'),
		"linux/amd64":   digestOf('2'),
		"windows/amd64": digestOf('3'),
	}
	want := goDotDevPin("1.26.2", integrities)
	if pin := goPinOf(t, answerGo(t, root, request)); !reflect.DeepEqual(pin, want) {
		t.Fatalf("pin = %+v, want %+v", pin, want)
	}

	data, err := os.ReadFile(filepath.Join(records, "go-1.26.2.pin.json"))
	if err != nil {
		t.Fatalf("the answer wrote no pin record: %v", err)
	}
	var record goPinRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if wantRecord := (goPinRecord{Version: "1.26.2", Integrities: integrities, Source: "https://go.dev/dl/"}); !reflect.DeepEqual(record, wantRecord) {
		t.Fatalf("pin record = %+v, want %+v", record, wantRecord)
	}

	if pin := goPinOf(t, answerGo(t, root, request)); !reflect.DeepEqual(pin, want) {
		t.Fatalf("second pin = %+v, want %+v", pin, want)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("the release index was requested %d times, want once", got)
	}
}

// singleReleaseIndex is a go.dev release index that publishes Go version for
// linux/amd64 alone, with digest.
func singleReleaseIndex(version, digest string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]goRelease{{Version: "go" + version, Files: []goFile{
			{Filename: "go" + version + ".linux-amd64.tar.gz", OS: "linux", Arch: "amd64", Kind: "archive", SHA256: digest},
		}}})
	}
}

// Rule c: a pin record the answer cannot use, whatever makes it so, is
// ignored: the release is resolved from the index and the record is replaced
// with the index's answer.
func TestAnswerReplacesAPinRecordItCannotUse(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-pin-reuses-the-machine-record",
		"an-invalid-record-is-ignored-and-replaced")
	for _, tc := range []struct{ name, record string }{
		{"malformed", `{"version":`},
		{"another release", `{"version":"1.26.1","integrities":{"linux/amd64":"` + digestOf('a') + `"},"source":"https://go.dev/dl/"}`},
		{"another source", `{"version":"1.26.2","integrities":{"linux/amd64":"` + digestOf('a') + `"},"source":"https://mirror.example/go/"}`},
		{"a digest that is not SHA-256", `{"version":"1.26.2","integrities":{"linux/amd64":"abc"},"source":"https://go.dev/dl/"}`},
		{"no platform", `{"version":"1.26.2","integrities":{},"source":"https://go.dev/dl/"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := isolatedPutnamiHome(t)
			path := filepath.Join(records, "go-1.26.2.pin.json")
			if err := os.MkdirAll(records, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.record), 0o644); err != nil {
				t.Fatal(err)
			}
			requests := serveReleaseIndex(t, singleReleaseIndex("1.26.2", digestOf('2')))
			integrities := map[string]string{"linux/amd64": digestOf('2')}
			pin := goPinOf(t, answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("linux/amd64", false, nil)))
			if want := goDotDevPin("1.26.2", integrities); !reflect.DeepEqual(pin, want) {
				t.Fatalf("pin = %+v, want the index's %+v", pin, want)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("the release index was requested %d times, want once", got)
			}
			record, ok := readGoPinRecord(records, "1.26.2")
			if want := (GoLock{Version: "1.26.2", Integrities: integrities, Source: goReleaseSource}); !ok || !reflect.DeepEqual(record, want) {
				t.Fatalf("pin record = %+v (%v), want the index's answer %+v", record, ok, want)
			}
		})
	}
}

// Rule c: a Putnami home the answer cannot write a record in still yields the
// pin the index publishes.
func TestAnswerPinsWhenThePutnamiHomeCannotBeWritten(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-pin-reuses-the-machine-record",
		"an-unwritable-home-still-pins")
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_HOME", home)
	serveReleaseIndex(t, singleReleaseIndex("1.26.3", digestOf('3')))
	pin := goPinOf(t, answerGo(t, goWorkspace(t, "go 1.26.3\n"), goResolveRequest("linux/amd64", false, nil)))
	if want := goDotDevPin("1.26.3", map[string]string{"linux/amd64": digestOf('3')}); !reflect.DeepEqual(pin, want) {
		t.Fatalf("pin = %+v, want the index's %+v", pin, want)
	}
}

// Rule c: offline, a declared release with no committed pin and no record is
// a problem whose remedy is an install with network access.
func TestAnswerOfflineWithoutPinRecordIsAProblem(t *testing.T) {
	isolatedPutnamiHome(t)
	forbidReleaseIndex(t)
	committed := wsproto.LockToolchainEntry{Version: "1.26.1", Integrities: map[string]string{"linux/amd64": digestOf('a')}, Source: goReleaseSource}
	problem := goProblemOf(t, answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("linux/amd64", true, &committed)))
	if !strings.Contains(problem.Message, "1.26.2") || !strings.Contains(problem.Message, "offline") {
		t.Errorf("message = %q, want the declared release and the offline request", problem.Message)
	}
	if !strings.Contains(problem.Remedy, "putnami install") || !strings.Contains(problem.Remedy, "network access") {
		t.Errorf("remedy = %q, want `putnami install` with network access", problem.Remedy)
	}
}

// Rule c: a lookup that fails is a problem carrying the error, and writes no
// pin record.
func TestAnswerFailedLookupIsAProblem(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-pin-reuses-the-machine-record",
		"a-failed-lookup-writes-no-record")
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusInternalServerError)
		}, "HTTP 500"},
		{"unpublished release", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"version":"go1.26.1","files":[]}]`))
		}, "has no supported archives"},
		{"malformed index", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"not":"a list"}`))
		}, "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := isolatedPutnamiHome(t)
			serveReleaseIndex(t, tc.handler)
			problem := goProblemOf(t, answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("linux/amd64", false, nil)))
			if !strings.Contains(problem.Message, "cannot resolve Go 1.26.2") || !strings.Contains(problem.Message, tc.want) {
				t.Errorf("message = %q, want the release and %q", problem.Message, tc.want)
			}
			if _, err := os.Stat(filepath.Join(records, "go-1.26.2.pin.json")); !os.IsNotExist(err) {
				t.Errorf("a failed lookup wrote a pin record (stat: %v)", err)
			}
		})
	}
}

// Rule d: with no declaration and no committed pin, the answer omits Go, so
// the lock keeps no Go pin.
func TestAnswerOmitsGoWithoutDeclarationOrPin(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-toolchain-pin",
		"no-declaration-and-no-committed-pin-pins-no-go")
	for _, tc := range []struct{ name, goWork string }{
		{"no go.work", ""},
		{"go.work without a release", "use ./app\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPutnamiHome(t)
			forbidReleaseIndex(t)
			result := answerGo(t, goWorkspace(t, tc.goWork), goResolveRequest("linux/amd64", false, nil))
			if resolution, ok := result.Toolchains[GoLockIdentity]; ok {
				t.Fatalf("the answer resolves Go: %+v", resolution)
			}
		})
	}
}

// Rule e: a pin that has no archive for the host platform is answered as a
// problem, whether it is resolved or committed.
func TestAnswerPinWithoutHostArchiveIsAProblem(t *testing.T) {
	t.Run("resolved", func(t *testing.T) {
		records := isolatedPutnamiHome(t)
		serveReleaseIndex(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]goRelease{{Version: "go1.26.2", Files: []goFile{
				{OS: "linux", Arch: "amd64", Kind: "archive", SHA256: digestOf('a')},
			}}})
		})
		problem := goProblemOf(t, answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("darwin/arm64", false, nil)))
		if !strings.Contains(problem.Message, "1.26.2") || !strings.Contains(problem.Message, "darwin/arm64") {
			t.Errorf("message = %q, want the release and the host platform", problem.Message)
		}
		if _, ok := readGoPinRecord(records, "1.26.2"); !ok {
			t.Error("the release's pin record was not written")
		}
	})
	t.Run("unsupported host", func(t *testing.T) {
		records := isolatedPutnamiHome(t)
		forbidReleaseIndex(t)
		if err := writeGoPinRecord(records, GoLock{Version: "1.26.2", Integrities: map[string]string{"linux/amd64": digestOf('a')}, Source: goReleaseSource}); err != nil {
			t.Fatal(err)
		}
		problem := goProblemOf(t, answerGo(t, goWorkspace(t, "go 1.26.2\n"), goResolveRequest("linux/386", false, nil)))
		if !strings.Contains(problem.Message, "linux/386") {
			t.Errorf("message = %q, want the host platform", problem.Message)
		}
	})
	t.Run("committed", func(t *testing.T) {
		isolatedPutnamiHome(t)
		forbidReleaseIndex(t)
		committed := wsproto.LockToolchainEntry{Version: "1.26.1", Integrities: map[string]string{"linux/amd64": digestOf('a')}, Source: goReleaseSource}
		problem := goProblemOf(t, answerGo(t, goWorkspace(t, "go 1.26.1\n"), goResolveRequest("windows/arm64", true, &committed)))
		if !strings.Contains(problem.Message, "windows/arm64") || !strings.Contains(problem.Message, LockFileName) {
			t.Errorf("message = %q, want the lock and the host platform", problem.Message)
		}
	})
}

// An asked identity the extension does not resolve is a problem, never an
// omission that would drop its pin.
func TestAnswerReportsAnIdentityItDoesNotResolve(t *testing.T) {
	isolatedPutnamiHome(t)
	forbidReleaseIndex(t)
	request := goResolveRequest("linux/amd64", true, nil)
	request.Identities = []string{"bun", GoLockIdentity}
	result := answerGo(t, goWorkspace(t, ""), request)
	if resolution := result.Toolchains["bun"]; resolution.Problem == nil {
		t.Fatalf("bun resolution = %+v, want a problem", resolution)
	}
}

// A problem's text stays within the protocol's bound and valid UTF-8, however
// long the error it carries.
func TestProblemTextIsBounded(t *testing.T) {
	long := strings.Repeat("é", toolchainProblemMaxBytes)
	resolution := toolchainProblem(long, long)
	for _, text := range []string{resolution.Problem.Message, resolution.Problem.Remedy} {
		if len(text) > toolchainProblemMaxBytes || !utf8.ValidString(text) || text == "" {
			t.Fatalf("problem text is %d bytes (valid UTF-8: %v)", len(text), utf8.ValidString(text))
		}
	}
	if short := toolchainProblem("short", ""); short.Problem.Message != "short" || short.Problem.Remedy != "" {
		t.Fatalf("a short problem changed: %+v", short.Problem)
	}
}
