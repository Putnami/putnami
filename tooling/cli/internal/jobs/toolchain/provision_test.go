package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	model "go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/machineprobe"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// provisionedTool is the slash path of the fixture toolchain's executable in
// its archive: the archive nests its content under a directory of its own, as
// the Bun and uv release archives do.
var provisionedTool = "tool-1.2.3/bin/" + WithExecutableSuffix(runtime.GOOS, "tool")

// provisionedToolchain is the runtime toolchain of the fixture toolchain:
// candidates, in order, then the probe that answers its version.
func provisionedToolchain(candidates ...extensionproto.RuntimeToolchainCandidate) *extensionproto.RuntimeToolchain {
	return &extensionproto.RuntimeToolchain{
		Lock:       "tool",
		Candidates: append(candidates, extensionproto.RuntimeToolchainCandidate{From: extensionproto.RuntimeToolchainCandidateProvisioned}),
		Probe:      extensionproto.RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}"},
	}
}

// archiveFile is one regular file of a test archive.
type archiveFile struct {
	name string
	body []byte
	mode os.FileMode
}

// toolArchiveFiles returns the files of an archive whose executable, at
// executable, answers version: the fixture program and its description.
func toolArchiveFiles(t *testing.T, executable, version string) []archiveFile {
	t.Helper()
	program := fixtureproc.Write(t, filepath.Join(t.TempDir(), "tool"), fixtureproc.Program{Stdout: version + "\n"})
	body, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	description, err := os.ReadFile(program + ".fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	return []archiveFile{
		{name: executable, body: body, mode: 0o755},
		{name: executable + ".fixture.json", body: description, mode: 0o644},
	}
}

func tarGzBytes(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: int64(f.mode), Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, files []archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		header := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		header.SetMode(f.mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// archiveServer serves data at every path and counts the requests.
func archiveServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// archivedEntry is a format 5 pin of version whose host archive is served at
// url in format, with digest and executable.
func archivedEntry(version, url, format, digest, executable string) lockfile.LockEntry {
	host := machineprobe.HostPlatform()
	return lockfile.LockEntry{
		Version:     version,
		Archives:    map[string]lockfile.LockArchive{host: {URL: url, Format: format, Executable: executable}},
		Integrities: map[string]string{host: digest},
	}
}

// writeArchivedToolchainLock writes a format 5 workspace lock that pins
// identity at version with, for the host platform, an archive whose
// executable is executable, and returns the pin.
func writeArchivedToolchainLock(t *testing.T, root, identity, version, executable string) lockfile.LockEntry {
	t.Helper()
	entry := archivedEntry(version, "https://archives.example/"+identity+"-"+version+".tar.gz", "tar.gz",
		sha256Hex([]byte(identity+"@"+version)), executable)
	locked := lockfile.NewLockFile()
	locked.SetToolchain(identity, entry)
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
	return entry
}

// isolatedPutnamiHome points PUTNAMI_HOME at an empty temporary directory and
// returns it.
func isolatedPutnamiHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "putnami-home")
	t.Setenv("PUTNAMI_HOME", home)
	return home
}

func TestProvisionedFolderAndExecutableAreBelowThePutnamiHome(t *testing.T) {
	home := isolatedPutnamiHome(t)
	workspaceRoot := t.TempDir()
	if got, want := ProvisionedFolder(workspaceRoot, "go", "1.26.9"), filepath.Join(home, "toolchains", "go", "go-1.26.9"); got != want {
		t.Fatalf("ProvisionedFolder = %s, want %s", got, want)
	}
	entry := archivedEntry("1.26.9", "https://go.dev/dl/go1.26.9.tar.gz", "tar.gz", strings.Repeat("a", 64), "go/bin/go")
	entry.Archives["plan9/amd64"] = lockfile.LockArchive{URL: "https://go.dev/dl/go1.26.9.zip", Format: "zip", Executable: "go/bin/go.exe"}
	entry.Integrities["plan9/amd64"] = strings.Repeat("b", 64)
	for platform, want := range map[string]string{
		"":            filepath.Join(home, "toolchains", "go", "go-1.26.9", "go", "bin", "go"),
		"plan9/amd64": filepath.Join(home, "toolchains", "go", "go-1.26.9", "go", "bin", "go.exe"),
	} {
		got, err := ProvisionedExecutable(workspaceRoot, "go", entry, platform)
		if err != nil || got != want {
			t.Errorf("ProvisionedExecutable(platform %q) = %q, %v; want %q", platform, got, err, want)
		}
	}

	escaping := entry
	escaping.Version = "../../bin"
	format4 := lockfile.LockEntry{Version: "1.26.9", Integrities: entry.Integrities}
	for name, tc := range map[string]struct {
		entry    lockfile.LockEntry
		platform string
		want     string
	}{
		"below format 5":    {format4, "", `toolchain "go" version "1.26.9" for ` + machineprobe.HostPlatform() + `: the lock pin records no archives (lock format below 5); run ` + "`putnami install`"},
		"no platform":       {entry, "plan9/mips", `toolchain "go" version "1.26.9" for plan9/mips: the lock pin records no archive for plan9/mips; run ` + "`putnami install`"},
		"escaping version":  {escaping, "", `version "../../bin" does not name a directory`},
		"no version at all": {lockfile.LockEntry{Archives: entry.Archives}, "", "the lock pin records no version"},
	} {
		if got, err := ProvisionedExecutable(workspaceRoot, "go", tc.entry, tc.platform); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ProvisionedExecutable = %q, %v; want an error naming %q", name, got, err, tc.want)
		}
	}
}

func TestProvisionUnpacksAVerifiedArchiveAndReusesIt(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-pinned-archive-is-verified-and-unpacked")
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-complete-folder-is-reused-without-a-request")
	files := toolArchiveFiles(t, provisionedTool, "1.2.3")
	for _, tc := range []struct {
		format string
		data   []byte
	}{
		{"tar.gz", tarGzBytes(t, files)},
		{"zip", zipBytes(t, files)},
	} {
		t.Run(tc.format, func(t *testing.T) {
			home := isolatedPutnamiHome(t)
			workspaceRoot := t.TempDir()
			server, requests := archiveServer(t, tc.data)
			target := ProvisionTarget{
				Identity:  "tool",
				Entry:     archivedEntry("1.2.3", server.URL+"/tool."+tc.format, tc.format, sha256Hex(tc.data), provisionedTool),
				Toolchain: provisionedToolchain(),
			}
			want := filepath.Join(home, "toolchains", "tool", "tool-1.2.3", filepath.FromSlash(provisionedTool))

			executable, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{UserAgent: "putnami-test"})
			if err != nil || !installed || executable != want {
				t.Fatalf("Provision = %q, %v, %v; want %q installed", executable, installed, err, want)
			}
			if got, err := ProvisionedExecutable(workspaceRoot, "tool", target.Entry, ""); err != nil || got != executable {
				t.Fatalf("ProvisionedExecutable = %q, %v; want the executable Provision installed, %q", got, err, executable)
			}
			if got, want, err := probeRuntimeToolchainVersion(context.Background(), executable, target.Toolchain.Probe, "1.2.3", os.Environ()); err != nil || got != want {
				t.Fatalf("the installed executable answers %q, %v; want %q", got, err, want)
			}
			if n := requests.Load(); n != 1 {
				t.Fatalf("the install made %d requests, want 1", n)
			}

			executable, installed, err = Provision(context.Background(), workspaceRoot, target, ProvisionOptions{})
			if err != nil || installed || executable != want {
				t.Fatalf("second Provision = %q, %v, %v; want %q reused", executable, installed, err, want)
			}
			if n := requests.Load(); n != 1 {
				t.Fatalf("reusing the complete folder made %d more requests, want none", n-1)
			}
		})
	}
}

// Without a runtime toolchain, a regular executable file at the archive's
// executable completes the folder: it is reused with no request.
func TestProvisionWithoutAToolchainReusesARegularExecutable(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-complete-folder-is-reused-without-a-request")
	isolatedPutnamiHome(t)
	workspaceRoot := t.TempDir()
	server, requests := archiveServer(t, nil)
	target := ProvisionTarget{
		Identity: "tool",
		Entry:    archivedEntry("1.2.3", server.URL+"/tool.tar.gz", "tar.gz", strings.Repeat("c", 64), provisionedTool),
	}
	present := filepath.Join(ProvisionedFolder(workspaceRoot, "tool", "1.2.3"), filepath.FromSlash(provisionedTool))
	writeExecutable(t, present, "not a program the provisioner runs\n")
	executable, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{})
	if err != nil || installed || executable != present {
		t.Fatalf("Provision = %q, %v, %v; want %q reused", executable, installed, err, present)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("reusing the complete folder made %d requests, want none", n)
	}
}

func TestProvisionRefusesADigestOrProbeMismatch(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-digest-or-probe-mismatch-is-refused")
	pinned := tarGzBytes(t, toolArchiveFiles(t, provisionedTool, "1.2.3"))
	otherRelease := tarGzBytes(t, toolArchiveFiles(t, provisionedTool, "9.9.9"))
	cases := []struct {
		name   string
		served []byte
		digest string
		want   error
	}{
		{"digest mismatch", otherRelease, sha256Hex(pinned), pinnedarchive.ErrDigestMismatch},
		{"probe mismatch", otherRelease, sha256Hex(otherRelease), pinnedarchive.ErrIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPutnamiHome(t)
			workspaceRoot := t.TempDir()
			server, _ := archiveServer(t, tc.served)
			target := ProvisionTarget{
				Identity:  "tool",
				Entry:     archivedEntry("1.2.3", server.URL+"/tool.tar.gz", "tar.gz", tc.digest, provisionedTool),
				Toolchain: provisionedToolchain(),
			}
			executable, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{})
			if !errors.Is(err, tc.want) || installed || executable != "" {
				t.Fatalf("Provision = %q, %v, %v; want %v", executable, installed, err, tc.want)
			}
			prefix := `toolchain "tool" version "1.2.3" for ` + machineprobe.HostPlatform() + ": "
			if !strings.HasPrefix(err.Error(), prefix) || !strings.HasSuffix(err.Error(), "; run `putnami install`") {
				t.Fatalf("Provision error = %v, want it to name the toolchain, the version, the platform and the install", err)
			}
			if _, statErr := os.Lstat(ProvisionedFolder(workspaceRoot, "tool", "1.2.3")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused archive left the provisioned folder: %v", statErr)
			}
		})
	}
}

// A folder another layout of the release left, such as a Bun with its
// executable at bin/bun and its package cache at install/cache, keeps its
// entries when the archive is installed: the folder ends with them and the
// archive's executable. On a name both hold, the archive's entry wins. An
// archive refused after its download leaves the folder as it was.
func TestProvisionKeepsTheEntriesOfAFolderOfAnotherLayout(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-folder-of-another-layout-keeps-its-entries")
	isolatedPutnamiHome(t)
	workspaceRoot := t.TempDir()
	executable := "bun-target/" + WithExecutableSuffix(runtime.GOOS, "bun")
	folder := ProvisionedFolder(workspaceRoot, "bun", "1.2.3")
	flattened := writeRuntimeTool(t, filepath.Join(folder, "bin", "bun"), "1.2.3")
	cache := filepath.Join(folder, "install", "cache", "x")
	stale := filepath.Join(folder, "bun-target", "stale")
	for _, file := range []string{cache, stale} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	toolchain := provisionedToolchain()
	toolchain.Lock = "bun"
	provision := func(version string) (string, bool, int64, error) {
		t.Helper()
		data := zipBytes(t, toolArchiveFiles(t, executable, version))
		server, requests := archiveServer(t, data)
		target := ProvisionTarget{
			Identity:  "bun",
			Entry:     archivedEntry("1.2.3", server.URL+"/bun.zip", "zip", sha256Hex(data), executable),
			Toolchain: toolchain,
		}
		got, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{})
		return got, installed, requests.Load(), err
	}
	kept := func(paths ...string) {
		t.Helper()
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("the folder lost %s: %v", path, err)
			}
		}
	}

	if _, _, _, err := provision("9.9.9"); !errors.Is(err, pinnedarchive.ErrIncomplete) {
		t.Fatalf("Provision of an archive whose executable answers another version = %v, want %v", err, pinnedarchive.ErrIncomplete)
	}
	kept(flattened, cache, stale)

	got, installed, requests, err := provision("1.2.3")
	if err != nil || !installed || got != filepath.Join(folder, filepath.FromSlash(executable)) || requests != 1 {
		t.Fatalf("Provision = %q, %v, %v after %d requests; want the archive installed with one request", got, installed, err, requests)
	}
	kept(flattened, cache, got)
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the folder's bun-target/stale stayed beside the archive's bun-target: %v", err)
	}
}

// A provisioned candidate resolves to the executable of the host's archive in
// the provisioned folder of the toolchain's lock identity, the path
// ProvisionedExecutable names, and binds the environment from it.
func TestProvisionedCandidateResolvesTheArchiveExecutable(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioned-candidate-resolves-the-pin", "a-provisioned-candidate-resolves-the-archive-executable")
	home := isolatedPutnamiHome(t)
	workspaceRoot := t.TempDir()
	entry := writeArchivedToolchainLock(t, workspaceRoot, "compiler", "1.2.3", "compiler-1.2.3/bin/"+WithExecutableSuffix(runtime.GOOS, "compiler"))
	want, err := ProvisionedExecutable(workspaceRoot, "compiler", entry, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, filepath.Join(home, "toolchains", "compiler", "compiler-1.2.3")+string(filepath.Separator)) {
		t.Fatalf("ProvisionedExecutable = %s, want it below the provisioned folder", want)
	}
	writeRuntimeTool(t, strings.TrimSuffix(want, ".exe"), "1.2.3")
	bin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(bin, "compiler"), "9.9.9")
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{
		{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"},
		{From: extensionproto.RuntimeToolchainCandidateProvisioned},
	}
	ext := runtimeToolchainExtension(requirement)
	if err := ResolveRuntimeToolchains(workspaceRoot, ext, []string{"compiler"}, []string{"PATH=" + bin, "PUTNAMI_HOME=" + home}); err != nil {
		t.Fatal(err)
	}
	resolved := ext.RuntimeToolchains["compiler"]
	if !resolved.Available || !sameResolvedPath(t, resolved.Executable, want) {
		t.Fatalf("resolution = %+v, want the provisioned %s", resolved, want)
	}
	if got := resolved.Environment["COMPILER_ROOT"]; !sameResolvedPath(t, got, filepath.Dir(filepath.Dir(want))) {
		t.Fatalf("COMPILER_ROOT = %q, want the archive's root %s", got, filepath.Dir(filepath.Dir(want)))
	}
	locked, err := LockedRuntimeToolchainIdentityForRefs(workspaceRoot, ext, []string{"compiler"})
	if err != nil || locked != "compiler="+resolved.Identity {
		t.Fatalf("identity from the lock = %q, %v; want the resolution's %q", locked, err, resolved.Identity)
	}
}

// A provisioned candidate whose folder is missing, or whose pin is below lock
// format 5 and records no archives, matches nothing, and the error names
// `putnami install`. A hosted run installs no toolchain: its error names no
// install, and another candidate still matches.
func TestProvisionedCandidateWithoutItsFolderNamesTheInstall(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioned-candidate-resolves-the-pin", "a-missing-folder-or-a-pin-below-format-5-names-putnami-install")
	home := isolatedPutnamiHome(t)
	requirement := runtimeToolchainFixture("compiler")
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidateProvisioned}}
	base := []string{"PATH=" + t.TempDir(), "PUTNAMI_HOME=" + home}
	const header = `resolve extension "example" runtime toolchain "compiler": no candidate matched locked version "1.2.3"`
	const remedy = "; run `putnami install` to install it"

	missing := t.TempDir()
	entry := writeArchivedToolchainLock(t, missing, "compiler", "1.2.3", "compiler-1.2.3/bin/compiler")
	executable, err := ProvisionedExecutable(missing, "compiler", entry, "")
	if err != nil {
		t.Fatal(err)
	}
	err = ResolveRuntimeToolchains(missing, runtimeToolchainExtension(requirement), []string{"compiler"}, base)
	if want := header + remedy + `: candidate 1 (provisioned) at ` + executable + `: path missing`; err == nil || err.Error() != want {
		t.Fatalf("missing folder: error = %v\nwant %s", err, want)
	}

	format4 := t.TempDir()
	writeRuntimeToolchainLock(t, format4, "compiler", "1.2.3", "integrity-a")
	err = ResolveRuntimeToolchains(format4, runtimeToolchainExtension(requirement), []string{"compiler"}, base)
	if want := header + remedy + `: candidate 1 (provisioned): the lock pin records no archives (lock format below 5)`; err == nil || err.Error() != want {
		t.Fatalf("pin below format 5: error = %v\nwant %s", err, want)
	}

	bin := t.TempDir()
	host := writeRuntimeTool(t, filepath.Join(bin, "compiler"), "1.2.3")
	t.Cleanup(runcredential.SetForTest("run-bearer"))
	err = ResolveRuntimeToolchains(missing, runtimeToolchainExtension(requirement), []string{"compiler"}, base)
	if err == nil || strings.Contains(err.Error(), "putnami install") || !strings.Contains(err.Error(), "path missing") {
		t.Fatalf("hosted run with a missing folder: error = %v, want no install named", err)
	}
	withPath := requirement
	withPath.Candidates = append(slices.Clone(requirement.Candidates), extensionproto.RuntimeToolchainCandidate{From: extensionproto.RuntimeToolchainCandidatePath, Path: "compiler"})
	ext := runtimeToolchainExtension(withPath)
	if err := ResolveRuntimeToolchains(missing, ext, []string{"compiler"}, []string{"PATH=" + bin, "PUTNAMI_HOME=" + home}); err != nil {
		t.Fatalf("hosted run with a path candidate after the provisioned one: %v", err)
	}
	if got := ext.RuntimeToolchains["compiler"].Executable; !sameResolvedPath(t, got, host) {
		t.Fatalf("hosted resolution = %s, want the runner's %s", got, host)
	}
}

// ReportProvisionedRuntimeToolchains probes a provisioned candidate again
// after the provisioning run, as it probes every other candidate.
func TestProvisionReportProbesAProvisionedCandidate(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "provisioned-candidate-resolves-the-pin", "a-provisioned-candidate-resolves-the-archive-executable")
	fixtureproc.Prepare(t) // while go is still on PATH
	isolatedPutnamiHome(t)
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	entry := writeArchivedToolchainLock(t, root, "compiler", "1.2.3", "compiler-1.2.3/bin/"+WithExecutableSuffix(runtime.GOOS, "compiler"))
	logs := captureRuntimeToolchainLogs(t)
	ext := provisioningToolchainExtension()
	requirement := ext.Runtime.Toolchains["compiler"]
	requirement.Candidates = []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidateProvisioned}}
	ext.Runtime.Toolchains["compiler"] = requirement
	if err := resolveRuntimeToolchainsForCommands(root, []*model.ExtensionDescription{ext},
		map[string]bool{ToolchainProvisioningCommand: true}, os.Environ()); err != nil {
		t.Fatal(err)
	}
	executable, err := ProvisionedExecutable(root, "compiler", entry, "")
	if err != nil {
		t.Fatal(err)
	}
	writeRuntimeTool(t, strings.TrimSuffix(executable, ".exe"), "1.2.3")
	ReportProvisionedRuntimeToolchains(context.Background(), root, []*model.ExtensionDescription{ext}, nil, []string{ToolchainProvisioningCommand})
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "level=INFO") || !strings.Contains(got, "toolchain=compiler") {
		t.Fatalf("report after the folder appeared = %q, want one info line", got)
	}
}

// A release the machine holds where another candidate of the toolchain finds
// it, such as on PATH, is the release: Provision returns it and makes no
// request. A candidate that answers another version holds nothing, and the
// archive is installed.
func TestProvisionReturnsAHeldReleaseWithoutARequest(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-held-release-is-not-downloaded")
	data := tarGzBytes(t, toolArchiveFiles(t, provisionedTool, "1.2.3"))
	for _, tc := range []struct {
		name, onPath string
		held         bool
	}{
		{"pinned release on PATH", "1.2.3", true},
		{"another release on PATH", "9.9.9", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPutnamiHome(t)
			workspaceRoot := t.TempDir()
			bin := t.TempDir()
			onPath := writeRuntimeTool(t, filepath.Join(bin, "tool"), tc.onPath)
			t.Setenv("PATH", bin)
			server, requests := archiveServer(t, data)
			target := ProvisionTarget{
				Identity:  "tool",
				Entry:     archivedEntry("1.2.3", server.URL+"/tool.tar.gz", "tar.gz", sha256Hex(data), provisionedTool),
				Toolchain: provisionedToolchain(extensionproto.RuntimeToolchainCandidate{From: extensionproto.RuntimeToolchainCandidatePath, Path: "tool"}),
			}
			executable, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			folder := ProvisionedFolder(workspaceRoot, "tool", "1.2.3")
			if !tc.held {
				if !installed || executable != filepath.Join(folder, filepath.FromSlash(provisionedTool)) || requests.Load() != 1 {
					t.Fatalf("Provision = %q, %v after %d requests; want the archive installed with one request", executable, installed, requests.Load())
				}
				return
			}
			if installed || !sameResolvedPath(t, executable, onPath) {
				t.Fatalf("Provision = %q, %v; want the release on PATH %s", executable, installed, onPath)
			}
			if n := requests.Load(); n != 0 {
				t.Fatalf("a held release made %d requests, want none", n)
			}
			if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a held release created the provisioned folder: %v", err)
			}
		})
	}
}

// The executable of another platform than the host is never run: a regular
// file completes its folder, and no candidate of the host replaces it.
func TestProvisionOfAnotherPlatformRunsNothing(t *testing.T) {
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "pinned-archives-are-provisioned", "a-pinned-archive-is-verified-and-unpacked")
	isolatedPutnamiHome(t)
	workspaceRoot := t.TempDir()
	bin := t.TempDir()
	writeRuntimeTool(t, filepath.Join(bin, "tool"), "1.2.3")
	t.Setenv("PATH", bin)
	data := tarGzBytes(t, []archiveFile{{name: "tool-1.2.3/bin/tool", body: []byte("plan9 program\n"), mode: 0o755}})
	server, requests := archiveServer(t, data)
	const platform = "plan9/mips"
	entry := lockfile.LockEntry{
		Version:     "1.2.3",
		Archives:    map[string]lockfile.LockArchive{platform: {URL: server.URL + "/tool.tar.gz", Format: "tar.gz", Executable: "tool-1.2.3/bin/tool"}},
		Integrities: map[string]string{platform: sha256Hex(data)},
	}
	target := ProvisionTarget{
		Identity:  "tool",
		Entry:     entry,
		Toolchain: provisionedToolchain(extensionproto.RuntimeToolchainCandidate{From: extensionproto.RuntimeToolchainCandidatePath, Path: "tool"}),
	}
	executable, installed, err := Provision(context.Background(), workspaceRoot, target, ProvisionOptions{Platform: platform})
	want := filepath.Join(ProvisionedFolder(workspaceRoot, "tool", "1.2.3"), "tool-1.2.3", "bin", "tool")
	if err != nil || !installed || executable != want || requests.Load() != 1 {
		t.Fatalf("Provision(%s) = %q, %v, %v after %d requests; want %q installed", platform, executable, installed, err, requests.Load(), want)
	}
}

// A runtime toolchain that locks another identity than the target's is
// refused before any request.
func TestProvisionRefusesAToolchainOfAnotherIdentity(t *testing.T) {
	isolatedPutnamiHome(t)
	server, requests := archiveServer(t, nil)
	toolchain := provisionedToolchain()
	toolchain.Lock = "other"
	target := ProvisionTarget{
		Identity:  "tool",
		Entry:     archivedEntry("1.2.3", server.URL+"/tool.tar.gz", "tar.gz", strings.Repeat("d", 64), provisionedTool),
		Toolchain: toolchain,
	}
	if _, _, err := Provision(context.Background(), t.TempDir(), target, ProvisionOptions{}); err == nil || !strings.Contains(err.Error(), `the runtime toolchain locks "other"`) || requests.Load() != 0 {
		t.Fatalf("Provision = %v after %d requests; want the toolchain refused before any request", err, requests.Load())
	}
}
