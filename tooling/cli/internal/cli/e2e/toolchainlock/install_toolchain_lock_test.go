package toolchainlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// pinsExtensionFixtureEnv makes the test binary run as the runtime of a
// fixture extension that answers toolchain-resolve (runPinsExtensionFixture).
// Its value is the directory that holds the extension's answer and records.
const pinsExtensionFixtureEnv = "PUTNAMI_TOOLCHAINLOCK_PINS_FIXTURE"

// pinsAnswer is what a fixture extension answers: its toolchains, or Fail,
// the message of an extension that cannot answer.
type pinsAnswer struct {
	Toolchains map[string]wsproto.ToolchainResolution `json:"toolchains,omitempty"`
	Fail       string                                 `json:"fail,omitempty"`
}

// pinsExtension is one fixture extension of a workspace: its name, the lock
// identities it resolves, and the directory of its answer and records.
type pinsExtension struct {
	name     string
	resolves []string
	dir      string
}

// answer sets what the extension answers to its next requests.
func (e pinsExtension) answer(t *testing.T, answer pinsAnswer) {
	t.Helper()
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(e.dir, "answer.json"), string(data), 0o644)
}

// requests returns every toolchain-resolve request the extension received.
func (e pinsExtension) requests(t *testing.T) []wsproto.ToolchainResolveRequest {
	t.Helper()
	var requests []wsproto.ToolchainResolveRequest
	for _, line := range readLines(t, filepath.Join(e.dir, "requests.jsonl")) {
		var request wsproto.ToolchainResolveRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatalf("request record %q: %v", line, err)
		}
		requests = append(requests, request)
	}
	return requests
}

// installs returns, for each run of the extension's workspace-install task,
// the toolchains the lock pinned when it ran.
func (e pinsExtension) installs(t *testing.T) []string {
	t.Helper()
	return readLines(t, filepath.Join(e.dir, "installs.txt"))
}

// writePinsWorkspace writes a workspace whose extensions are local sources
// that answer toolchain-resolve through their runtime, the test binary
// (runPinsExtensionFixture), and run a workspace-install task that records
// the lock's toolchain pins. Each extension resolves its identities and starts
// with an empty answer.
func writePinsWorkspace(t *testing.T, wsRoot string, extensions ...pinsExtension) []pinsExtension {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	includes := []string{`"app"`}
	for i := range extensions {
		ext := &extensions[i]
		dirName := fmt.Sprintf("ext-%d", i)
		includes = append(includes, `"`+dirName+`"`)
		ext.dir = filepath.Join(t.TempDir(), dirName)
		extRoot := filepath.Join(wsRoot, dirName)
		writeFixtureFile(t, filepath.Join(extRoot, "putnami.json"), `{"name":"`+ext.name+`"}`, 0o644)
		info := fmt.Sprintf(
			`{"extension":%q,"version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
			ext.name, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
			runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
		writeFixtureFile(t, filepath.Join(extRoot, "bin", "runtime-template"), "#!/bin/sh\n"+
			"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n"+
			"  printf '%s\\n' '"+info+"'\n"+
			"  exit 0\n"+
			"fi\n"+
			pinsExtensionFixtureEnv+"='"+ext.dir+"'\n"+
			"export "+pinsExtensionFixtureEnv+"\n"+
			"exec '"+testBinary+"' \"$@\"\n", 0o755)
		writeFixtureFile(t, filepath.Join(extRoot, "bin", "prepare"), "#!/bin/sh\nset -eu\n"+
			"output=\"$2\"\n"+
			"mkdir -p \"$output/bin\"\n"+
			"cp \"$(dirname \"$0\")/runtime-template\" \"$output/bin/runtime\"\n"+
			"chmod +x \"$output/bin/runtime\"\n"+
			// The prepare runs the runtime it wrote once, so the handshake that
			// follows within its deadline does not also pay for the host's
			// first-launch check of the new file.
			"\"$output/bin/runtime\" __putnami runtime-info >/dev/null\n", 0o755)
		resolves, err := json.Marshal(ext.resolves)
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(extRoot, "putnami.extension.json"), `{
  "name": "`+ext.name+`",
  "version": "0.1.0",
  "cliContract": `+fmt.Sprint(protocolcli.CurrentContract)+`,
  "runtime": {
    "executable": "bin/runtime",
    "resolves": `+string(resolves)+`,
    "prepare": {
      "command": "{extensionRoot}/bin/prepare",
      "args": ["--output", "{runtimeOutput}"],
      "inputs": ["bin/prepare", "bin/runtime-template"]
    }
  },
  "commands": {
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "install-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["install"],
      "cwd": "{workspaceRoot}",
      "cache": false,
      "timeoutMs": -1
    }
  }
}`, 0o644)
		ext.answer(t, pinsAnswer{})
	}
	writeFixtureFile(t, filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename),
		`{"name":"pins-ws","includes":[`+strings.Join(includes, ",")+`]}`, 0o644)
	writeFixtureFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app"}`, 0o644)
	workspace.InvalidateLoadCache(wsRoot)
	return extensions
}

// runPinsExtensionFixture is the runtime of a fixture extension, run by the
// test binary under pinsExtensionFixtureEnv. It answers toolchain-resolve
// with the answer the test set, recording each request, and records the
// lock's toolchain pins on install.
func runPinsExtensionFixture(dir string, args []string) int {
	handled, err := wsproto.ServeToolchainResolve(args, os.Stdin, os.Stdout,
		func(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
			data, err := json.Marshal(request)
			if err != nil {
				return wsproto.ToolchainResolveResult{}, err
			}
			if err := appendLine(filepath.Join(dir, "requests.jsonl"), string(data)); err != nil {
				return wsproto.ToolchainResolveResult{}, err
			}
			if data, err = os.ReadFile(filepath.Join(dir, "answer.json")); err != nil {
				return wsproto.ToolchainResolveResult{}, err
			}
			var answer pinsAnswer
			if err := json.Unmarshal(data, &answer); err != nil {
				return wsproto.ToolchainResolveResult{}, err
			}
			if answer.Fail != "" {
				return wsproto.ToolchainResolveResult{}, errors.New(answer.Fail)
			}
			return wsproto.ToolchainResolveResult{Toolchains: answer.Toolchains}, nil
		})
	if handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(args) == 0 || args[0] != "install" {
		return 0
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lock, err := lockfile.ReadLockFile(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pins := []string{}
	if lock != nil {
		for _, identity := range slices.Sorted(maps.Keys(lock.Toolchains)) {
			pins = append(pins, identity+"="+lock.Toolchains[identity].Version)
		}
	}
	if err := appendLine(filepath.Join(dir, "installs.txt"), "pins: "+strings.Join(pins, " ")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fixturePin is a lock version 5 pin of version for this host.
func fixturePin(version string) *wsproto.LockToolchainEntry {
	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	return &wsproto.LockToolchainEntry{
		Version:     version,
		Integrities: map[string]string{platform: sha256Hex([]byte(version))},
		Archives: map[string]wsproto.LockToolchainArchive{platform: {
			URL: "https://example.test/toolchain-" + version + ".tar.gz", Format: wsproto.LockArchiveFormatTarGz, Executable: "bin/tool",
		}},
	}
}

// sourcePin is a lock version 3 or 4 pin of version: an integrity and the
// release's base URL, and no archive.
func sourcePin(version string) lockfile.LockEntry {
	return lockfile.LockEntry{
		Version:     version,
		Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): sha256Hex([]byte("source " + version))},
		Source:      "https://example.test/releases/",
	}
}

// setUpPinsTest isolates the Putnami home, store and artifacts of one test,
// and lets its install run as a command run by hand.
func setUpPinsTest(t *testing.T) string {
	t.Helper()
	clitest.RequireShell(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	hometest.Temp(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	return t.TempDir()
}

// writeCommittedLock writes lf as the workspace's committed lock and returns
// its bytes.
func writeCommittedLock(t *testing.T, wsRoot string, lf *lockfile.LockFile) []byte {
	t.Helper()
	if err := lockfile.WriteLockFile(wsRoot, lf); err != nil {
		t.Fatal(err)
	}
	return readLockBytes(t, wsRoot)
}

func readLockBytes(t *testing.T, wsRoot string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsRoot, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// installWorkspace runs `putnami install` in wsRoot through the real engine
// and returns its error and transcript.
func installWorkspace(ctx context.Context, t *testing.T, wsRoot string) (string, error) {
	t.Helper()
	var out strings.Builder
	var err error
	streams := clitest.CaptureStdoutStderr(t, func() {
		err = lifecycle.Install(ctx, wsRoot, wsproto.Load(wsRoot), nil,
			lifecycle.LifecycleEnv{Out: &out, RunJob: cli.RunWorkspaceJob})
	})
	return out.String() + streams, err
}

// An explicit install asks each extension that resolves toolchains once, with
// the committed lock entries of its identities, and records exactly the pins
// they answer before the workspace installers run: an identity no answer pins
// leaves the lock, and a lock below version 5 is written at version 5. The
// chain is the real one: Install → the toolchain pins phase → each
// extension's runtime → DepsInstall → RunWorkspaceJob → Engine.Run → the
// extensions' install task.
func TestInstall_RecordsTheAnsweredPinsBeforeTheInstallers(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "extensions-answer-pins", "install-asks-each-extension-that-resolves-once")
	spectest.Proves(t, "cli/toolchain-lock", "extensions-answer-pins", "the-request-carries-the-committed-entries")
	spectest.Proves(t, "cli/toolchain-lock", "install-pins-before-installers", "explicit-install-writes-the-answered-pins-at-format-5")
	spectest.Proves(t, "cli/toolchain-lock", "install-pins-before-installers", "an-identity-no-answer-pins-is-removed")
	spectest.Proves(t, "cli/toolchain-lock", "install-pins-before-installers", "a-format-3-lock-is-rewritten-at-format-5")
	wsRoot := setUpPinsTest(t)
	exts := writePinsWorkspace(t, wsRoot,
		pinsExtension{name: "@acme/go-pins", resolves: []string{"go", "gofmt"}},
		pinsExtension{name: "@acme/node-pins", resolves: []string{"node"}})
	goExt, nodeExt := exts[0], exts[1]
	goPin, nodePin := fixturePin("1.26.1"), fixturePin("22.1.0")
	// The go extension pins go and omits gofmt, which the workspace does not
	// declare.
	goExt.answer(t, pinsAnswer{Toolchains: map[string]wsproto.ToolchainResolution{"go": {Pin: goPin}}})
	nodeExt.answer(t, pinsAnswer{Toolchains: map[string]wsproto.ToolchainResolution{"node": {Pin: nodePin}}})

	committed := &lockfile.LockFile{
		Version:    lockfile.FormatVersionV3,
		Extensions: map[string]lockfile.LockEntry{},
		Templates:  map[string]lockfile.LockEntry{},
		Toolchains: map[string]lockfile.LockEntry{
			"go":    sourcePin("1.25.0"),
			"gofmt": sourcePin("1.25.0"),
			// No extension resolves uv.
			"uv": sourcePin("0.9.0"),
		},
	}
	writeCommittedLock(t, wsRoot, committed)

	if transcript, err := installWorkspace(context.Background(), t, wsRoot); err != nil {
		t.Fatalf("putnami install: %v\n%s", err, transcript)
	}

	got, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || got == nil {
		t.Fatalf("read lock: %v", err)
	}
	if got.Version != lockfile.FormatVersionV5 {
		t.Fatalf("lock version = %d, want 5", got.Version)
	}
	recorded := map[string]wsproto.LockToolchainEntry{}
	for identity, entry := range got.Toolchains {
		recorded[identity] = entry.ProtocolToolchainEntry()
	}
	if want := map[string]wsproto.LockToolchainEntry{"go": *goPin, "node": *nodePin}; !reflect.DeepEqual(recorded, want) {
		t.Fatalf("toolchains = %+v, want exactly the answered pins %+v", recorded, want)
	}

	platform := lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)
	goRequests := goExt.requests(t)
	wantGo := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  goExt.name,
		Identities: []string{"go", "gofmt"},
		Locked: map[string]wsproto.LockToolchainEntry{
			"go":    committed.Toolchains["go"].ProtocolToolchainEntry(),
			"gofmt": committed.Toolchains["gofmt"].ProtocolToolchainEntry(),
		},
		Platform: platform,
	}
	if len(goRequests) != 1 || !reflect.DeepEqual(goRequests[0], wantGo) {
		t.Fatalf("%s was asked %+v, want once with %+v", goExt.name, goRequests, wantGo)
	}
	if nodeRequests := nodeExt.requests(t); len(nodeRequests) != 1 || !slices.Equal(nodeRequests[0].Identities, []string{"node"}) ||
		len(nodeRequests[0].Locked) != 0 {
		t.Fatalf("%s was asked %+v, want once for node, with no committed entry", nodeExt.name, nodeRequests)
	}
	// Each installer ran once, with the answered pins already in the lock.
	for _, ext := range exts {
		if installs := ext.installs(t); !slices.Equal(installs, []string{"pins: go=1.26.1 node=22.1.0"}) {
			t.Fatalf("%s installed with %v, want one run that sees the answered pins", ext.name, installs)
		}
	}
}

// The first-use install that a command runs implicitly restores a checkout
// from its committed lock: a committed version 5 pin stays even when the
// extension answers another, an identity the lock lacks takes the answer, and
// nothing is removed.
func TestInstall_TheFirstUseInstallKeepsACommittedPinAndFillsAMissingOne(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "implicit-install-fills-missing-pins",
		"the-implicit-install-keeps-a-committed-pin-and-fills-a-missing-one")
	wsRoot := setUpPinsTest(t)
	exts := writePinsWorkspace(t, wsRoot, pinsExtension{name: "@acme/pins", resolves: []string{"go", "node"}})
	committedGo, committedUv := fixturePin("1.25.0"), fixturePin("0.9.0")
	answeredNode := fixturePin("22.1.0")
	exts[0].answer(t, pinsAnswer{Toolchains: map[string]wsproto.ToolchainResolution{
		"go": {Pin: fixturePin("1.26.1")}, "node": {Pin: answeredNode},
	}})
	committed := lockfile.NewLockFile()
	committed.SetToolchain("go", lockfile.ToolchainEntryFromProtocol(*committedGo))
	committed.SetToolchain("uv", lockfile.ToolchainEntryFromProtocol(*committedUv))
	writeCommittedLock(t, wsRoot, committed)

	if transcript, err := installWorkspace(shared.WithImplicitInstall(context.Background()), t, wsRoot); err != nil {
		t.Fatalf("first-use install: %v\n%s", err, transcript)
	}

	got, err := lockfile.ReadLockFile(wsRoot)
	if err != nil || got == nil {
		t.Fatalf("read lock: %v", err)
	}
	recorded := map[string]wsproto.LockToolchainEntry{}
	for identity, entry := range got.Toolchains {
		recorded[identity] = entry.ProtocolToolchainEntry()
	}
	want := map[string]wsproto.LockToolchainEntry{"go": *committedGo, "node": *answeredNode, "uv": *committedUv}
	if got.Version != lockfile.FormatVersionV5 || !reflect.DeepEqual(recorded, want) {
		t.Fatalf("lock version %d, toolchains %+v; want version 5 with %+v", got.Version, recorded, want)
	}
	if requests := exts[0].requests(t); len(requests) != 1 {
		t.Fatalf("the extension was asked %d times, want once", len(requests))
	}
	if installs := exts[0].installs(t); !slices.Equal(installs, []string{"pins: go=1.25.0 node=22.1.0 uv=0.9.0"}) {
		t.Fatalf("installed with %v, want one run that sees the settled pins", installs)
	}
}

// A problem an extension answers, two extensions that answer one identity
// with different pins, and an extension that cannot answer each stop the
// install before any installer, leave the committed lock as it is, and say
// what to do next.
func TestInstall_FailedAnswersStopTheInstall(t *testing.T) {
	const (
		problem = "go.work declares go 1.99.0, which go.dev does not publish"
		remedy  = "set the go directive of go.work to a published release"
		cannot  = "go.work: unknown directive"
	)
	cases := []struct {
		name       string
		check      string
		extensions []pinsExtension
		answers    []pinsAnswer
		want       []string
		wantNext   string
		invalid    bool
	}{
		{
			name:       "a problem",
			check:      "a-problem-fails-install-with-its-remedy",
			extensions: []pinsExtension{{name: "@acme/go-pins", resolves: []string{"go"}}},
			answers: []pinsAnswer{{Toolchains: map[string]wsproto.ToolchainResolution{
				"go": {Problem: &wsproto.ToolchainProblem{Message: problem, Remedy: remedy}},
			}}},
			want:     []string{`toolchain "go"`, "@acme/go-pins: " + problem, remedy},
			wantNext: remedy,
			invalid:  true,
		},
		{
			name:  "two extensions that disagree",
			check: "two-extensions-that-disagree-fail-install",
			extensions: []pinsExtension{
				{name: "@acme/go-pins", resolves: []string{"go"}},
				{name: "@acme/other-go-pins", resolves: []string{"go"}},
			},
			answers: []pinsAnswer{
				{Toolchains: map[string]wsproto.ToolchainResolution{"go": {Pin: fixturePin("1.26.1")}}},
				{Toolchains: map[string]wsproto.ToolchainResolution{"go": {Pin: fixturePin("1.26.2")}}},
			},
			want:     []string{`toolchain "go"`, "@acme/go-pins and @acme/other-go-pins answer different pins (1.26.1 and 1.26.2)"},
			wantNext: "putnami install",
			invalid:  true,
		},
		{
			name:       "an extension that cannot answer",
			check:      "a-failed-answer-names-the-extension",
			extensions: []pinsExtension{{name: "@acme/go-pins", resolves: []string{"go"}}},
			answers:    []pinsAnswer{{Fail: cannot}},
			want:       []string{"extension @acme/go-pins did not answer for its toolchains", cannot},
			wantNext:   "putnami install",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spectest.Proves(t, "cli/toolchain-lock", "failed-answers-stop-the-install", tc.check)
			wsRoot := setUpPinsTest(t)
			exts := writePinsWorkspace(t, wsRoot, tc.extensions...)
			for i, ext := range exts {
				ext.answer(t, tc.answers[i])
			}
			committed := lockfile.NewLockFile()
			committed.SetToolchain("go", lockfile.ToolchainEntryFromProtocol(*fixturePin("1.25.0")))
			before := writeCommittedLock(t, wsRoot, committed)

			transcript, err := installWorkspace(context.Background(), t, wsRoot)
			if err == nil {
				t.Fatalf("putnami install succeeded, want it stopped\n%s", transcript)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if next := protocolcli.SuggestedNext(err); next != tc.wantNext {
				t.Errorf("next step = %q, want %q", next, tc.wantNext)
			}
			if invalid := errors.Is(err, protocolcli.ErrInvalidConfig); invalid != tc.invalid {
				t.Errorf("error classified as invalid configuration = %v, want %v", invalid, tc.invalid)
			}
			if after := readLockBytes(t, wsRoot); !bytes.Equal(before, after) {
				t.Errorf("the failed install changed the lock:\n%s\nto\n%s", before, after)
			}
			for _, ext := range exts {
				if installs := ext.installs(t); len(installs) != 0 {
					t.Errorf("%s installed %v after a failed toolchain pins phase, want no installer run", ext.name, installs)
				}
			}
		})
	}
}

// writeRegistryPinsWorkspace writes a workspace whose one extension,
// @acme/pins, comes from a fixture registry, as a hosted run requires: it
// resolves go through its runtime, the test binary
// (runPinsExtensionFixture), and runs a workspace-install task.
func writeRegistryPinsWorkspace(t *testing.T, wsRoot string) (pinsExtension, *fixtureRegistry) {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ext := pinsExtension{name: "@acme/pins", resolves: []string{"go"}, dir: t.TempDir()}
	info := fmt.Sprintf(
		`{"extension":%q,"version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		ext.name, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimeScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n" +
		"  printf '%s\\n' '" + info + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		pinsExtensionFixtureEnv + "='" + ext.dir + "'\n" +
		"export " + pinsExtensionFixtureEnv + "\n" +
		"exec '" + testBinary + "' \"$@\"\n"
	manifest := `{
  "name": "@acme/pins",
  "version": "0.1.0",
  "cliContract": ` + fmt.Sprint(protocolcli.CurrentContract) + `,
  "runtime": { "executable": "bin/runtime", "resolves": ["go"] },
  "commands": {
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    }
  },
  "tasks": {
    "install-task": { "kind": "command", "command": "{extensionRuntime}", "args": ["install"], "cwd": "{workspaceRoot}", "cache": false, "timeoutMs": -1 }
  }
}`
	registry := newFixtureRegistry(t, map[string][]byte{
		"/acme/pins/download": tarGz(t, []tarEntry{
			{name: "bin/runtime", mode: 0o755, content: runtimeScript},
			{name: "putnami.extension.json", mode: 0o644, content: manifest},
		}),
	})
	t.Setenv("PUTNAMI_REGISTRY_URL", registry.URL)
	clitest.WriteFile(t, filepath.Join(wsRoot, wsproto.WorkspaceConfigFilename),
		`{"name":"pins-ws","includes":["app"],"extensions":["@acme/pins"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app"}`)
	ext.answer(t, pinsAnswer{})
	workspace.InvalidateLoadCache(wsRoot)
	return ext, registry
}

// A hosted install asks the extensions offline and writes no pin: the lock a
// local install committed stays byte for byte, even when the answer changes a
// pin. The installers still run, on the committed pins.
func TestInstall_AHostedRunWritesNoPins(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "hosted-runs-write-no-pins", "a-hosted-run-writes-no-pins")
	wsRoot := setUpPinsTest(t)
	ext, registry := writeRegistryPinsWorkspace(t, wsRoot)
	ext.answer(t, pinsAnswer{Toolchains: map[string]wsproto.ToolchainResolution{"go": {Pin: fixturePin("1.25.0")}}})
	// The committed lock is the one a local install leaves.
	if transcript, err := installWorkspace(context.Background(), t, wsRoot); err != nil {
		t.Fatalf("local putnami install: %v\n%s", err, transcript)
	}
	before := readLockBytes(t, wsRoot)

	ext.answer(t, pinsAnswer{Toolchains: map[string]wsproto.ToolchainResolution{"go": {Pin: fixturePin("1.26.1")}}})
	t.Cleanup(runcredential.SetForTest("run-bearer"))
	// What runcredential.Capture leaves in a hosted run's environment.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if transcript, err := installWorkspace(context.Background(), t, wsRoot); err != nil {
		t.Fatalf("hosted putnami install: %v\n%s", err, transcript)
	}
	if after := readLockBytes(t, wsRoot); !bytes.Equal(before, after) {
		t.Fatalf("a hosted install changed the lock:\n%s\nto\n%s", before, after)
	}
	if requests := ext.requests(t); len(requests) != 2 || requests[0].Offline || !requests[1].Offline {
		t.Fatalf("the extension was asked %+v, want once by the local install and once, offline, by the hosted one", requests)
	}
	if installs := ext.installs(t); !slices.Equal(installs, []string{"pins: go=1.25.0", "pins: go=1.25.0"}) {
		t.Fatalf("installed with %v, want each run on the committed pin", installs)
	}
	if unexpected := registry.unexpected(); len(unexpected) > 0 {
		t.Fatalf("the registry was asked for artifacts the fixture does not serve: %v", unexpected)
	}
}

// markerRuns counts how many times a fixture's task appended to marker.
func markerRuns(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "ran")
}

func writeFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
