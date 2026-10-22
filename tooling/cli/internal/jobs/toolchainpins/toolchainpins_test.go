package toolchainpins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

var hostPlatform = lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH)

// fixtureExtension is one path extension of a fixture workspace: its name,
// the identities it resolves and the lock identity of its one runtime
// toolchain, "" for none.
type fixtureExtension struct {
	name     string
	resolves []string
	runs     string
}

// writeWorkspace writes a workspace whose projects are the path extensions
// exts, and returns its root.
func writeWorkspace(t *testing.T, exts ...fixtureExtension) string {
	t.Helper()
	root := t.TempDir()
	includes := make([]string, 0, len(exts))
	for i, ext := range exts {
		dir := fmt.Sprintf("ext-%d", i)
		includes = append(includes, fmt.Sprintf("%q", dir))
		writeFile(t, filepath.Join(root, dir, "putnami.json"), fmt.Sprintf(`{"name":%q}`, ext.name))
		toolchains := ""
		if ext.runs != "" {
			toolchains = fmt.Sprintf(`,
    "toolchains": {
      "compiler": {
        "lock": %q,
        "candidates": [{"from": "path", "path": %q}],
        "probe": {"args": ["--version"], "expect": "{version}"}
      }
    }`, ext.runs, ext.runs)
		}
		resolves := make([]string, 0, len(ext.resolves))
		for _, identity := range ext.resolves {
			resolves = append(resolves, fmt.Sprintf("%q", identity))
		}
		resolvesMember := ""
		if len(resolves) > 0 {
			resolvesMember = `,
    "resolves": [` + strings.Join(resolves, ",") + `]`
		}
		writeFile(t, filepath.Join(root, dir, "putnami.extension.json"), fmt.Sprintf(`{
  "name": %q,
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": {
    "executable": "bin/runtime"%s%s
  }
}`, ext.name, protocolcli.CurrentContract, toolchains, resolvesMember))
	}
	writeFile(t, filepath.Join(root, wsproto.WorkspaceConfigFilename),
		`{"name":"pins-ws","includes":[`+strings.Join(includes, ",")+`]}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pin is a lock version 5 pin of version for the host platform, whose digest
// and archive derive from seed.
func pin(version, seed string) *wsproto.LockToolchainEntry {
	return &wsproto.LockToolchainEntry{
		Version:     version,
		Integrities: map[string]string{hostPlatform: strings.Repeat(seed, 64)},
		Archives: map[string]wsproto.LockToolchainArchive{hostPlatform: {
			URL:        "https://example.test/" + seed + "/" + version + ".tar.gz",
			Format:     wsproto.LockArchiveFormatTarGz,
			Executable: "bin/tool",
		}},
	}
}

// fakeProviders answers each extension's request from its table and records
// every request.
type fakeProviders struct {
	mu       sync.Mutex
	answers  map[string]map[string]wsproto.ToolchainResolution
	failures map[string]error
	asked    []wsproto.ToolchainResolveRequest
}

func (f *fakeProviders) provider(ext *extension.ExtensionDescription) pinsProvider {
	return fakeProvider{f: f, name: ext.Name}
}

func (f *fakeProviders) requests() []wsproto.ToolchainResolveRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

type fakeProvider struct {
	f    *fakeProviders
	name string
}

func (p fakeProvider) ResolveToolchains(request wsproto.ToolchainResolveRequest) (wsproto.ToolchainResolveResult, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.asked = append(p.f.asked, request)
	if err := p.f.failures[p.name]; err != nil {
		return wsproto.ToolchainResolveResult{}, err
	}
	return wsproto.ToolchainResolveResult{
		Version: wsproto.ToolchainResolveProtocolVersion, Extension: p.name, Toolchains: p.f.answers[p.name],
	}, nil
}

// writeLock writes a lock at version with the toolchain entries and a CLI
// entry, and returns its bytes.
func writeLock(t *testing.T, root string, version int, toolchains map[string]lockfile.LockEntry) []byte {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.Version = version
	lf.SetCLI(lockfile.LockEntry{Version: "1.2.3", Integrities: map[string]string{"linux/amd64": strings.Repeat("c", 64)}})
	lf.Extensions["@acme/registry"] = lockfile.LockEntry{Version: "0.4.0", Source: "https://registry.test/acme"}
	for identity, entry := range toolchains {
		lf.SetToolchain(identity, entry)
	}
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatal(err)
	}
	return readLockBytes(t, root)
}

func readLockBytes(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readLock(t *testing.T, root string) *lockfile.LockFile {
	t.Helper()
	lf, err := lockfile.ReadLockFile(root)
	if err != nil || lf == nil {
		t.Fatalf("read lock: %v (present %v)", err, lf != nil)
	}
	return lf
}

// v3Pin is a lock version 3 toolchain entry: a source and no archives.
func v3Pin(version string) lockfile.LockEntry {
	return lockfile.LockEntry{
		Version:     version,
		Integrities: map[string]string{hostPlatform: strings.Repeat("d", 64)},
		Source:      "https://vendor.test/" + version + "/",
	}
}

// An explicit run asks each extension that resolves once, with the committed
// entries of its identities, and records exactly the answered pins at lock
// version 5: an omitted identity and one no extension answers for are
// removed, and every other entry keeps its content.
func TestAnExplicitRunRecordsExactlyTheAnsweredPins(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t,
		fixtureExtension{name: "@acme/go", resolves: []string{"go", "gopls"}, runs: "go"},
		fixtureExtension{name: "@acme/ts", resolves: []string{"bun"}},
		fixtureExtension{name: "@acme/plain"},
	)
	writeLock(t, root, lockfile.FormatVersionV3, map[string]lockfile.LockEntry{
		"go": v3Pin("1.26.0"), "gopls": v3Pin("0.18.0"), "node": v3Pin("22.0.0"),
	})
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/go": {"go": {Pin: pin("1.26.1", "a")}},
		"@acme/ts": {"bun": {Pin: pin("1.4.0", "b")}},
	}}

	result, err := resolve(context.Background(), root, Options{}, providers.provider)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	requests := providers.requests()
	if len(requests) != 2 || requests[0].Extension != "@acme/go" || requests[1].Extension != "@acme/ts" {
		t.Fatalf("requests = %+v, want one for @acme/go then one for @acme/ts", requests)
	}
	goRequest := requests[0]
	if !slices.Equal(goRequest.Identities, []string{"go", "gopls"}) || goRequest.Platform != hostPlatform || goRequest.Offline {
		t.Fatalf("@acme/go request = %+v, want go and gopls for %s, online", goRequest, hostPlatform)
	}
	if got := goRequest.Locked["go"]; got.Version != "1.26.0" || got.Source != "https://vendor.test/1.26.0/" || len(goRequest.Locked) != 2 {
		t.Fatalf("@acme/go locked = %+v, want the committed go and gopls entries as committed", goRequest.Locked)
	}
	if len(requests[1].Locked) != 0 {
		t.Fatalf("@acme/ts locked = %+v, want none: the lock pins no bun", requests[1].Locked)
	}

	if !result.Written || !slices.Equal(result.Added, []string{"bun"}) || !slices.Equal(result.Changed, []string{"go"}) ||
		!slices.Equal(result.Removed, []string{"gopls", "node"}) {
		t.Fatalf("result = written %v, added %v, changed %v, removed %v; want bun added, go changed, gopls and node removed",
			result.Written, result.Added, result.Changed, result.Removed)
	}
	lf := readLock(t, root)
	if lf.Version != lockfile.FormatVersionV5 {
		t.Fatalf("lock version = %d, want %d", lf.Version, lockfile.FormatVersionV5)
	}
	if got := sortedKeys(lf.Toolchains); !slices.Equal(got, []string{"bun", "go"}) {
		t.Fatalf("toolchains = %v, want bun and go", got)
	}
	if got := lf.Toolchains["go"]; got.Version != "1.26.1" || got.Archives[hostPlatform].URL != "https://example.test/a/1.26.1.tar.gz" || got.Source != "" {
		t.Fatalf("toolchains.go = %+v, want the answered pin", got)
	}
	if cli, _ := lf.GetCLI(); cli.Version != "1.2.3" || lf.Extensions["@acme/registry"].Version != "0.4.0" {
		t.Fatalf("cli %+v, extensions %+v: want every other entry as committed", cli, lf.Extensions)
	}
	if answer := result.Answers["go"]; answer.Extension != "@acme/go" || answer.Alias != "compiler" ||
		answer.Toolchain == nil || answer.Toolchain.Lock != "go" {
		t.Fatalf("answers.go = %+v, want @acme/go and its compiler toolchain", answer)
	}
	if answer := result.Answers["bun"]; answer.Extension != "@acme/ts" || answer.Alias != "" || answer.Toolchain != nil {
		t.Fatalf("answers.bun = %+v, want @acme/ts with no runtime toolchain", answer)
	}
}

// An explicit run creates a missing lock when it pins something, and writes
// nothing when the toolchains do not change.
func TestAnExplicitRunWritesOnlyAChange(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t, fixtureExtension{name: "@acme/go", resolves: []string{"go"}})
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/go": {"go": {Pin: pin("1.26.1", "a")}},
	}}
	result, err := resolve(context.Background(), root, Options{}, providers.provider)
	if err != nil || !result.Written {
		t.Fatalf("resolve without a lock = written %v, %v; want the lock created", result.Written, err)
	}
	before, err := os.Stat(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatal(err)
	}
	result, err = resolve(context.Background(), root, Options{}, providers.provider)
	if err != nil || result.Written || len(result.Added)+len(result.Changed)+len(result.Removed) != 0 {
		t.Fatalf("second resolve = %+v, %v; want no change and no write", result, err)
	}
	after, err := os.Stat(filepath.Join(root, lockfile.LockFilename))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("an unchanged run replaced the lock: %v", err)
	}
	if len(providers.requests()) != 2 {
		t.Fatalf("requests = %d, want one per run", len(providers.requests()))
	}
}

// The implicit run keeps a committed lock version 5 pin as committed, takes
// the answer for an identity the lock does not pin, and removes nothing.
func TestTheImplicitRunKeepsACommittedPinAndFillsAMissingOne(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t, fixtureExtension{name: "@acme/go", resolves: []string{"go", "gopls"}})
	committedGo := lockfile.ToolchainEntryFromProtocol(*pin("1.26.0", "e"))
	committedNode := lockfile.ToolchainEntryFromProtocol(*pin("22.0.0", "f"))
	writeLock(t, root, lockfile.FormatVersionV5, map[string]lockfile.LockEntry{"go": committedGo, "node": committedNode})
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/go": {"go": {Pin: pin("1.26.1", "a")}, "gopls": {Pin: pin("0.18.1", "b")}},
	}}

	result, err := resolve(context.Background(), root, Options{Implicit: true}, providers.provider)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !result.Written || !slices.Equal(result.Added, []string{"gopls"}) || len(result.Changed)+len(result.Removed) != 0 {
		t.Fatalf("result = %+v, want gopls added and nothing else", result)
	}
	lf := readLock(t, root)
	if got := lf.Toolchains["go"]; !sameEntry(got, committedGo) {
		t.Fatalf("toolchains.go = %+v, want the committed pin %+v", got, committedGo)
	}
	if got := lf.Toolchains["node"]; !sameEntry(got, committedNode) {
		t.Fatalf("toolchains.node = %+v, want the committed pin kept", got)
	}
	if got := lf.Toolchains["gopls"]; got.Version != "0.18.1" {
		t.Fatalf("toolchains.gopls = %+v, want the answered pin", got)
	}
}

// The implicit run creates no lock, and writes none it cannot write at lock
// version 5: a pin committed below it that no extension answered for records
// no archives. It keeps the settled toolchains in memory.
func TestTheImplicitRunWritesNoLockItCannotKeep(t *testing.T) {
	t.Parallel()
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/go": {"go": {Pin: pin("1.26.1", "a")}},
	}}

	root := writeWorkspace(t, fixtureExtension{name: "@acme/go", resolves: []string{"go"}})
	result, err := resolve(context.Background(), root, Options{Implicit: true}, providers.provider)
	if err != nil || result.Written || result.Toolchains["go"].Version != "1.26.1" {
		t.Fatalf("implicit resolve without a lock = %+v, %v; want the pin in memory and no lock", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, lockfile.LockFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the implicit run created a lock: %v", err)
	}

	root = writeWorkspace(t, fixtureExtension{name: "@acme/go", resolves: []string{"go"}})
	before := writeLock(t, root, lockfile.FormatVersionV3, map[string]lockfile.LockEntry{"uv": v3Pin("0.11.0")})
	result, err = resolve(context.Background(), root, Options{Implicit: true}, providers.provider)
	if err != nil || result.Written || !slices.Equal(result.Added, []string{"go"}) || result.Toolchains["uv"].Version != "0.11.0" {
		t.Fatalf("implicit resolve over a version 3 uv pin = %+v, %v; want go added in memory, uv kept, no write", result, err)
	}
	if after := readLockBytes(t, root); string(after) != string(before) {
		t.Fatalf("the implicit run rewrote the lock:\n%s", after)
	}
}

// A hosted run asks offline and writes nothing; the settled toolchains stay
// in the result.
func TestAHostedRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t, fixtureExtension{name: "@acme/go", resolves: []string{"go"}})
	before := writeLock(t, root, lockfile.FormatVersionV3, map[string]lockfile.LockEntry{"go": v3Pin("1.26.0")})
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/go": {"go": {Pin: pin("1.26.1", "a")}},
	}}
	result, err := resolve(context.Background(), root, Options{Hosted: true, Offline: true}, providers.provider)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.Written || !result.Hosted || result.Toolchains["go"].Version != "1.26.1" || !slices.Equal(result.Changed, []string{"go"}) {
		t.Fatalf("result = %+v, want go changed in memory, hosted and unwritten", result)
	}
	if requests := providers.requests(); len(requests) != 1 || !requests[0].Offline {
		t.Fatalf("requests = %+v, want one offline request", requests)
	}
	if after := readLockBytes(t, root); string(after) != string(before) {
		t.Fatalf("a hosted run rewrote the lock:\n%s", after)
	}
}

// Problems and disagreements fail the run with one invalid-configuration
// error listing each identity's message and remedy, whose next step is the
// first remedy. Nothing is written.
func TestProblemsAndDisagreementsFailTheRunTogether(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t,
		fixtureExtension{name: "@acme/a", resolves: []string{"go", "uv"}},
		fixtureExtension{name: "@acme/b", resolves: []string{"go", "zig"}},
	)
	before := writeLock(t, root, lockfile.FormatVersionV5, nil)
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/a": {
			"go": {Pin: pin("1.26.1", "a")},
			"uv": {Problem: &wsproto.ToolchainProblem{Message: "uv 0.1.0 publishes no archive for this host", Remedy: "declare uv 0.11.32 in pyproject.toml"}},
		},
		"@acme/b": {
			"go":  {Pin: pin("1.26.2", "b")},
			"zig": {Problem: &wsproto.ToolchainProblem{Message: "no zig release is declared"}},
		},
	}}

	_, err := resolve(context.Background(), root, Options{}, providers.provider)
	if err == nil {
		t.Fatal("resolve succeeded with problems")
	}
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Errorf("error %v is not classified as invalid configuration", err)
	}
	for _, want := range []string{
		`toolchain "go": @acme/a and @acme/b answer different pins (1.26.1 and 1.26.2)`,
		`toolchain "uv": @acme/a: uv 0.1.0 publishes no archive for this host (remedy: declare uv 0.11.32 in pyproject.toml)`,
		`toolchain "zig": @acme/b: no zig release is declared`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if next := protocolcli.SuggestedNext(err); next != "declare uv 0.11.32 in pyproject.toml" {
		t.Errorf("next = %q, want the remedy of the first problem", next)
	}
	if after := readLockBytes(t, root); string(after) != string(before) {
		t.Fatalf("a failed run rewrote the lock:\n%s", after)
	}
}

// A disagreement alone names `putnami install` as the next step, and two
// extensions that answer one identity with the same pin agree.
func TestOnlyDifferentPinsDisagree(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t,
		fixtureExtension{name: "@acme/a", resolves: []string{"go"}},
		fixtureExtension{name: "@acme/b", resolves: []string{"go"}},
	)
	providers := &fakeProviders{answers: map[string]map[string]wsproto.ToolchainResolution{
		"@acme/a": {"go": {Pin: pin("1.26.1", "a")}},
		"@acme/b": {"go": {Pin: pin("1.26.1", "a")}},
	}}
	result, err := resolve(context.Background(), root, Options{}, providers.provider)
	if err != nil || result.Answers["go"].Extension != "@acme/a" {
		t.Fatalf("resolve = %+v, %v; want the agreed pin answered by the first extension", result.Answers, err)
	}

	providers.answers["@acme/b"] = map[string]wsproto.ToolchainResolution{"go": {Pin: pin("1.26.1", "b")}}
	_, err = resolve(context.Background(), root, Options{}, providers.provider)
	if err == nil || !strings.Contains(err.Error(), "@acme/a and @acme/b answer different pins") {
		t.Fatalf("resolve = %v, want the two extensions named", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami install" {
		t.Errorf("next = %q, want putnami install", next)
	}
}

// An extension that cannot answer fails the run, named, after every other
// extension was asked, and nothing is written.
func TestAFailedAnswerNamesTheExtension(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t,
		fixtureExtension{name: "@acme/a", resolves: []string{"go"}},
		fixtureExtension{name: "@acme/b", resolves: []string{"bun"}},
	)
	failure := wsproto.NewProbeFailure(wsproto.ProbeFailureTimeout, "@acme/a", "toolchain-resolve did not answer within 1m0s")
	providers := &fakeProviders{
		answers:  map[string]map[string]wsproto.ToolchainResolution{"@acme/b": {"bun": {Pin: pin("1.4.0", "b")}}},
		failures: map[string]error{"@acme/a": failure},
	}
	_, err := resolve(context.Background(), root, Options{}, providers.provider)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "extension @acme/a did not answer for its toolchains") {
		t.Fatalf("resolve = %v, want the failure of @acme/a named", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami install" {
		t.Errorf("next = %q, want putnami install", next)
	}
	if len(providers.requests()) != 2 {
		t.Errorf("requests = %d, want both extensions asked", len(providers.requests()))
	}
	if _, statErr := os.Stat(filepath.Join(root, lockfile.LockFilename)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed run wrote a lock: %v", statErr)
	}
}

// A workspace whose extensions resolve nothing asks nothing, and an explicit
// run removes every committed pin.
func TestAWorkspaceWithoutResolvingExtensionsAsksNothing(t *testing.T) {
	t.Parallel()
	root := writeWorkspace(t, fixtureExtension{name: "@acme/plain", runs: "go"})
	writeLock(t, root, lockfile.FormatVersionV5, map[string]lockfile.LockEntry{
		"go": lockfile.ToolchainEntryFromProtocol(*pin("1.26.0", "e")),
	})
	providers := &fakeProviders{}
	result, err := resolve(context.Background(), root, Options{}, providers.provider)
	if err != nil || len(providers.requests()) != 0 || !result.Written || !slices.Equal(result.Removed, []string{"go"}) {
		t.Fatalf("resolve = %+v, %v with %d requests; want go removed and nothing asked", result, err, len(providers.requests()))
	}
	if lf := readLock(t, root); len(lf.Toolchains) != 0 {
		t.Fatalf("toolchains = %+v, want none", lf.Toolchains)
	}
}
