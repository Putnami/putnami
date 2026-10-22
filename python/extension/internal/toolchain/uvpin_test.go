package toolchain

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/uvtoolchain"
)

// uvPinRequest asks for the uv pin of a workspace on platform, with locked as
// the committed pin when it is not nil.
func uvPinRequest(platform string, offline bool, locked *wsproto.LockToolchainEntry) wsproto.ToolchainResolveRequest {
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  "@putnami/python",
		Identities: []string{UVLockIdentity},
		Platform:   platform,
		Offline:    offline,
	}
	if locked != nil {
		request.Locked = map[string]wsproto.LockToolchainEntry{UVLockIdentity: *locked}
	}
	return request
}

// serveUVReleases points uvReleaseURL at a local server that serves release
// files by path, so no test reads the uv release downloads.
func serveUVReleases(t *testing.T) *uvMirror {
	t.Helper()
	mirror := newUVMirror(t)
	previous := uvReleaseURL
	uvReleaseURL = func(version string) string { return mirror.URL + "/" + version + "/" }
	t.Cleanup(func() { uvReleaseURL = previous })
	return mirror
}

// uvChecksumsFor is the checksum file of a release that publishes an archive
// for each platform, and one archive no platform installs. Each digest is the
// SHA-256 of the archive's name.
func uvChecksumsFor(platforms ...string) (string, map[string]string) {
	lines := make([]string, 0, len(platforms)+1)
	digests := map[string]string{}
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		archive, _ := uvtoolchain.ArchiveFor(goos, goarch)
		digests[platform] = sha256Hex([]byte(archive.Name))
		lines = append(lines, digests[platform]+" *"+archive.Name)
	}
	lines = append(lines, sha256Hex([]byte("i686"))+" *uv-i686-unknown-linux-gnu.tar.gz")
	return strings.Join(lines, "\n") + "\n", digests
}

// resolveUVPinFor answers request in workspace and holds the answer to the
// toolchain-resolve contract: on its own and against its request.
func resolveUVPinFor(t *testing.T, workspace string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolution {
	t.Helper()
	resolution, err := ResolveUVPin(workspace, request)
	if err != nil {
		t.Fatalf("ResolveUVPin: %v", err)
	}
	result := wsproto.ToolchainResolveResult{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  request.Extension,
		Toolchains: map[string]wsproto.ToolchainResolution{UVLockIdentity: resolution},
	}
	diags := append(wsproto.ValidateToolchainResolveResult(&result), wsproto.ValidateToolchainResolveAnswer(&request, &result)...)
	if diag.HasErrors(diags) {
		t.Fatalf("answer %+v breaks the toolchain-resolve contract: %v", resolution, diags)
	}
	return resolution
}

func wantUVPin(t *testing.T, resolution wsproto.ToolchainResolution) wsproto.LockToolchainEntry {
	t.Helper()
	if resolution.Pin == nil {
		t.Fatalf("answer = problem %+v, want a pin", resolution.Problem)
	}
	return *resolution.Pin
}

func wantUVProblem(t *testing.T, resolution wsproto.ToolchainResolution, message, remedy string) wsproto.ToolchainProblem {
	t.Helper()
	if resolution.Problem == nil {
		t.Fatalf("answer = pin %+v, want a problem", resolution.Pin)
	}
	problem := *resolution.Problem
	if !strings.Contains(problem.Message, message) || !strings.Contains(problem.Remedy, remedy) {
		t.Fatalf("problem = %+v, want a message naming %q and a remedy naming %q", problem, message, remedy)
	}
	return problem
}

// The default release pins with the digests the extension ships, from the uv
// release downloads: a tar.gz archive holds uv in its one directory, and the
// Windows zip holds uv.exe at its top level.
func wantDefaultUVPin(t *testing.T, pin wsproto.LockToolchainEntry) {
	t.Helper()
	if pin.Version != DefaultUVVersion || pin.Source != "" {
		t.Fatalf("pin = %+v, want uv %s with no source", pin, DefaultUVVersion)
	}
	if !reflect.DeepEqual(pin.Integrities, uvtoolchain.DefaultIntegrities()) {
		t.Fatalf("integrities = %v, want the shipped %v", pin.Integrities, uvtoolchain.DefaultIntegrities())
	}
	base := "https://github.com/astral-sh/uv/releases/download/" + DefaultUVVersion + "/"
	want := map[string]wsproto.LockToolchainArchive{
		"darwin/amd64":  {URL: base + "uv-x86_64-apple-darwin.tar.gz", Format: "tar.gz", Executable: "uv-x86_64-apple-darwin/uv"},
		"darwin/arm64":  {URL: base + "uv-aarch64-apple-darwin.tar.gz", Format: "tar.gz", Executable: "uv-aarch64-apple-darwin/uv"},
		"linux/amd64":   {URL: base + "uv-x86_64-unknown-linux-musl.tar.gz", Format: "tar.gz", Executable: "uv-x86_64-unknown-linux-musl/uv"},
		"linux/arm64":   {URL: base + "uv-aarch64-unknown-linux-musl.tar.gz", Format: "tar.gz", Executable: "uv-aarch64-unknown-linux-musl/uv"},
		"windows/amd64": {URL: base + "uv-x86_64-pc-windows-msvc.zip", Format: "zip", Executable: "uv.exe"},
		"windows/arm64": {URL: base + "uv-aarch64-pc-windows-msvc.zip", Format: "zip", Executable: "uv.exe"},
	}
	if !reflect.DeepEqual(pin.Archives, want) {
		t.Fatalf("archives = %+v, want %+v", pin.Archives, want)
	}
}

// A required-version that names no release Putnami runs is a problem, with the
// message the installer gives and the declaration to write, whether or not
// the lock pins uv. A declaration too long for the protocol is cut to its
// bound.
func TestResolveUVPin_RefusesADeclarationPutnamiCannotPin(t *testing.T) {
	committed := &wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": sha256Hex(nil)}, Source: "https://mirror.example/uv/"}
	for name, tc := range map[string]struct{ spec, message, remedy string }{
		"a range":          {">=0.11", `names no single uv release`, `[tool.uv] table of pyproject.toml, then run ` + "`putnami install`"},
		"a wildcard":       {"==0.11.*", `names no single uv release`, "`putnami install`"},
		"an older release": {"==0.10.0", `older than uv ` + uvtoolchain.MinimumVersion, "`putnami install`"},
		"a long value":     {strings.Repeat("x", 2048), `[tool.uv] required-version is "xxx`, "`putnami install`"},
	} {
		t.Run(name, func(t *testing.T) {
			mirror := serveUVReleases(t)
			workspace := t.TempDir()
			declareUV(t, workspace, tc.spec)
			for _, locked := range []*wsproto.LockToolchainEntry{nil, committed} {
				problem := wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, locked)), tc.message, tc.remedy)
				if len(problem.Message) > uvProblemMaxBytes {
					t.Fatalf("message is %d bytes, above the %d the protocol allows", len(problem.Message), uvProblemMaxBytes)
				}
			}
			if got := mirror.requests(); len(got) != 0 {
				t.Fatalf("the answer read %v", got)
			}
		})
	}
}

// A committed version 5 pin of the declared release, or of any release when
// pyproject.toml declares none, is the answer as committed: a mirror it names
// is kept.
func TestResolveUVPin_KeepsACommittedVersion5Pin(t *testing.T) {
	mirror := serveUVReleases(t)
	committed := wsproto.LockToolchainEntry{
		Version: "0.11.20",
		Archives: map[string]wsproto.LockToolchainArchive{
			"linux/amd64":   {URL: "https://mirror.example/uv/uv-x86_64-unknown-linux-musl.tar.gz", Format: "tar.gz", Executable: "uv-x86_64-unknown-linux-musl/uv"},
			"windows/amd64": {URL: "http://mirror.example/uv/uv-x86_64-pc-windows-msvc.zip", Format: "zip", Executable: "uv.exe"},
		},
		Integrities: map[string]string{"linux/amd64": sha256Hex([]byte("linux")), "windows/amd64": sha256Hex([]byte("windows"))},
	}
	for name, spec := range map[string]string{"declared": "==0.11.20", "undeclared": ""} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if spec != "" {
				declareUV(t, workspace, spec)
			}
			pin := wantUVPin(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, &committed)))
			if !reflect.DeepEqual(pin, committed) {
				t.Fatalf("pin = %+v, want the committed %+v", pin, committed)
			}
		})
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("the answer read %v", got)
	}
}

// A committed version 3 or 4 pin is kept as the archives the installer
// downloads from its source, offline: each platform uv publishes an archive
// for keeps its integrity, and the pin records no source. A source the lock
// cannot record, an integrity that is not a SHA-256, and a release Putnami
// does not run are problems.
func TestResolveUVPin_ConvertsACommittedVersion4Pin(t *testing.T) {
	mirror := serveUVReleases(t)
	linux, windows := sha256Hex([]byte("linux")), sha256Hex([]byte("windows"))
	committed := wsproto.LockToolchainEntry{
		Version:     "0.11.20",
		Integrities: map[string]string{"linux/amd64": strings.ToUpper(linux), "windows/arm64": windows, "freebsd/amd64": sha256Hex([]byte("freebsd"))},
		Source:      "https://mirror.example/uv/0.11.20",
	}

	t.Run("converted", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.11.20")
		pin := wantUVPin(t, resolveUVPinFor(t, workspace, uvPinRequest("windows/arm64", true, &committed)))
		want := wsproto.LockToolchainEntry{
			Version: "0.11.20",
			Archives: map[string]wsproto.LockToolchainArchive{
				"linux/amd64":   {URL: "https://mirror.example/uv/0.11.20/uv-x86_64-unknown-linux-musl.tar.gz", Format: "tar.gz", Executable: "uv-x86_64-unknown-linux-musl/uv"},
				"windows/arm64": {URL: "https://mirror.example/uv/0.11.20/uv-aarch64-pc-windows-msvc.zip", Format: "zip", Executable: "uv.exe"},
			},
			Integrities: map[string]string{"linux/amd64": linux, "windows/arm64": windows},
		}
		if !reflect.DeepEqual(pin, want) {
			t.Fatalf("pin = %+v, want %+v", pin, want)
		}
	})

	t.Run("the installer's archive", func(t *testing.T) {
		pin := wantUVPin(t, resolveUVPinFor(t, t.TempDir(), uvPinRequest("linux/amd64", true, &committed)))
		archive, _ := uvArchiveFor(UVRelease{Version: committed.Version, Source: committed.Source}, "linux", "amd64")
		if got := pin.Archives["linux/amd64"]; got.URL != archive.pin.URL || got.Format != string(archive.pin.Format) {
			t.Fatalf("archive = %+v, want the one the installer downloads, %+v", got, archive.pin)
		}
	})

	for name, tc := range map[string]struct {
		entry           wsproto.LockToolchainEntry
		message, remedy string
	}{
		"a source with credentials": {
			entry:   wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": linux}, Source: "https://user:secret@mirror.example/uv/"},
			message: "toolchains.uv in putnami.lock.json: the source carries credentials", remedy: "Remove toolchains.uv",
		},
		"a source that is not a URL": {
			entry:   wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": linux}, Source: "file:///srv/uv/"},
			message: "not an http or https URL", remedy: "Remove toolchains.uv",
		},
		"an integrity that is not a SHA-256": {
			entry:   wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": "sha256-abc"}, Source: "https://mirror.example/uv/"},
			message: "SHA-256 recorded for linux/amd64", remedy: "Remove toolchains.uv",
		},
		"a release Putnami does not run": {
			entry:   wsproto.LockToolchainEntry{Version: "0.4.0", Integrities: map[string]string{"linux/amd64": linux}, Source: "https://mirror.example/uv/"},
			message: "pins a release Putnami cannot run", remedy: `required-version = "==` + uvtoolchain.MinimumVersion + `"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			problem := wantUVProblem(t, resolveUVPinFor(t, t.TempDir(), uvPinRequest("linux/amd64", true, &tc.entry)), tc.message, tc.remedy)
			if strings.Contains(problem.Message+problem.Remedy, "secret") {
				t.Fatalf("problem %+v repeats the credentials of the source", problem)
			}
		})
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("the answer read %v", got)
	}
}

// A declared default release the lock does not pin takes the digests the
// extension ships, offline.
func TestResolveUVPin_PinsTheDeclaredDefaultReleaseOffline(t *testing.T) {
	mirror := serveUVReleases(t)
	workspace := t.TempDir()
	declareUV(t, workspace, "=="+DefaultUVVersion)
	committed := &wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": sha256Hex(nil)}, Source: "https://mirror.example/uv/"}
	for _, locked := range []*wsproto.LockToolchainEntry{nil, committed} {
		wantDefaultUVPin(t, wantUVPin(t, resolveUVPinFor(t, workspace, uvPinRequest("darwin/arm64", true, locked))))
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("the answer read %v", got)
	}
}

// Another declared release the lock does not pin takes the digests its
// checksum file lists, from the release downloads the archives are pinned to.
func TestResolveUVPin_LooksUpAnotherDeclaredRelease(t *testing.T) {
	mirror := serveUVReleases(t)
	checksums, digests := uvChecksumsFor(uvtoolchain.Platforms()...)
	mirror.archives["/0.12.0/"+uvtoolchain.ChecksumsFile] = []byte(checksums)
	workspace := t.TempDir()
	declareUV(t, workspace, "==0.12.0")
	committed := &wsproto.LockToolchainEntry{Version: DefaultUVVersion, Integrities: uvtoolchain.DefaultIntegrities(), Source: uvtoolchain.ReleaseURL(DefaultUVVersion)}

	pin := wantUVPin(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/arm64", false, committed)))
	if pin.Version != "0.12.0" || pin.Source != "" || !reflect.DeepEqual(pin.Integrities, digests) {
		t.Fatalf("pin = %+v, want uv 0.12.0 with the digests %v and no source", pin, digests)
	}
	want := wsproto.LockToolchainArchive{URL: mirror.URL + "/0.12.0/uv-aarch64-unknown-linux-musl.tar.gz", Format: "tar.gz", Executable: "uv-aarch64-unknown-linux-musl/uv"}
	if got := pin.Archives["linux/arm64"]; got != want {
		t.Fatalf("linux/arm64 archive = %+v, want %+v", got, want)
	}
	if got := mirror.requests(); !reflect.DeepEqual(got, []string{"/0.12.0/" + uvtoolchain.ChecksumsFile}) {
		t.Fatalf("requests = %v, want the checksum file only", got)
	}
}

// An offline request reads no checksum file, so another declared release the
// lock does not pin is a problem that says to install with network access.
func TestResolveUVPin_RefusesALookupOffline(t *testing.T) {
	mirror := serveUVReleases(t)
	workspace := t.TempDir()
	declareUV(t, workspace, "==0.12.0")
	wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", true, nil)),
		"declares uv 0.12.0, which putnami.lock.json does not pin", "with network access")
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("the offline answer read %v", got)
	}
}

// A checksum file that cannot be read, or lists no archive Putnami installs,
// is a problem that carries the cause.
func TestResolveUVPin_ReportsAFailedLookup(t *testing.T) {
	mirror := serveUVReleases(t)
	mirror.archives["/0.12.1/"+uvtoolchain.ChecksumsFile] = []byte("not a checksum line\n")
	workspace := t.TempDir()

	declareUV(t, workspace, "==0.12.0")
	wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, nil)),
		"cannot read the SHA-256 digests of uv 0.12.0: GET "+mirror.URL+"/0.12.0/sha256.sum: HTTP 404", "published release")

	declareUV(t, workspace, "==0.12.1")
	wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, nil)),
		"uv 0.12.1 publishes no SHA-256 for a supported archive", "Declare another release")

	mirror.archives["/0.12.2/"+uvtoolchain.ChecksumsFile] = make([]byte, uvChecksumsMaxBytes+1)
	declareUV(t, workspace, "==0.12.2")
	wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, nil)),
		"sha256.sum: the file exceeds 1048576 bytes", "reachable")

	unreachable := httpUnreachable(t)
	uvReleaseURL = func(version string) string { return unreachable + "/" + version + "/" }
	wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("linux/amd64", false, nil)),
		"cannot read the SHA-256 digests of uv 0.12.2: Get ", "reachable")
}

// httpUnreachable returns the URL of a local server that is already closed.
func httpUnreachable(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	return server.URL
}

// A workspace that neither declares nor pins uv pins the default release, the
// one workspace-install declares, offline.
func TestResolveUVPin_PinsTheDefaultReleaseWithoutDeclarationOrPin(t *testing.T) {
	mirror := serveUVReleases(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, RootPyproject), []byte("[project]\nname = \"ws\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{workspace, t.TempDir()} {
		wantDefaultUVPin(t, wantUVPin(t, resolveUVPinFor(t, root, uvPinRequest("windows/amd64", true, nil))))
	}
	if got := mirror.requests(); len(got) != 0 {
		t.Fatalf("the answer read %v", got)
	}
}

// A pin serves the host platform, or the answer is a problem: on a platform
// Putnami installs no uv on, for a committed pin with no archive for the host,
// and for a release whose checksum file lists none for it.
func TestResolveUVPin_AnswersAProblemForAPinThatCannotServeTheHost(t *testing.T) {
	mirror := serveUVReleases(t)
	checksums, _ := uvChecksumsFor("linux/amd64", "darwin/arm64")
	mirror.archives["/0.12.0/"+uvtoolchain.ChecksumsFile] = []byte(checksums)

	t.Run("a platform Putnami installs no uv on", func(t *testing.T) {
		wantUVProblem(t, resolveUVPinFor(t, t.TempDir(), uvPinRequest("freebsd/amd64", true, nil)),
			"Putnami installs no uv on freebsd/amd64, the host platform: it installs uv on "+strings.Join(uvtoolchain.Platforms(), ", "), "on one of those platforms")
	})

	t.Run("a committed pin", func(t *testing.T) {
		committed := &wsproto.LockToolchainEntry{Version: "0.11.20", Integrities: map[string]string{"linux/amd64": sha256Hex(nil)}, Source: "https://mirror.example/uv/"}
		wantUVProblem(t, resolveUVPinFor(t, t.TempDir(), uvPinRequest("darwin/arm64", true, committed)),
			"toolchains.uv in putnami.lock.json records no archive of uv 0.11.20 for darwin/arm64", "Remove toolchains.uv")
		version5 := &wsproto.LockToolchainEntry{
			Version:     "0.11.20",
			Archives:    map[string]wsproto.LockToolchainArchive{"linux/amd64": {URL: "https://mirror.example/uv.tar.gz", Format: "tar.gz", Executable: "uv"}},
			Integrities: map[string]string{"linux/amd64": sha256Hex(nil)},
		}
		wantUVProblem(t, resolveUVPinFor(t, t.TempDir(), uvPinRequest("darwin/arm64", true, version5)),
			"records no archive of uv 0.11.20 for darwin/arm64", "Remove toolchains.uv")
	})

	t.Run("a looked-up release", func(t *testing.T) {
		workspace := t.TempDir()
		declareUV(t, workspace, "==0.12.0")
		wantUVProblem(t, resolveUVPinFor(t, workspace, uvPinRequest("windows/amd64", false, nil)),
			mirror.URL+"/0.12.0/sha256.sum records no archive of uv 0.12.0 for windows/amd64", "Declare another release")
	})
}

// A problem's message and remedy fit the protocol's bound, cut without
// splitting a UTF-8 sequence.
func TestClipProblemText(t *testing.T) {
	if got := clipProblemText("short"); got != "short" {
		t.Fatalf("clipProblemText(short) = %q", got)
	}
	clipped := clipProblemText(strings.Repeat("é", uvProblemMaxBytes))
	if len(clipped) > uvProblemMaxBytes || !strings.HasSuffix(clipped, "...") || !strings.HasPrefix(clipped, "éé") {
		t.Fatalf("clipped = %d bytes %q, want at most %d bytes ending in ...", len(clipped), clipped, uvProblemMaxBytes)
	}
	if !strings.HasSuffix(strings.TrimSuffix(clipped, "..."), "é") {
		t.Fatalf("clipped %q splits a UTF-8 sequence", clipped)
	}
}
