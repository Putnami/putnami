package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// The Go packager's distribution matrix is the SDK's, entry for entry and in
// the same order, so every archive packager publishes under the same keys.
func TestArchivePlatformsAreTheSDKMatrix(t *testing.T) {
	sdk := pkgmeta.ArchivePlatforms()
	if len(sdk) == 0 || len(ArchivePlatforms) != len(sdk) {
		t.Fatalf("ArchivePlatforms = %#v, SDK matrix = %#v", ArchivePlatforms, sdk)
	}
	for i, platform := range sdk {
		want := Target{GOOS: platform.GOOS, GOARCH: platform.GOARCH, Suffix: platform.Suffix}
		if ArchivePlatforms[i] != want {
			t.Errorf("ArchivePlatforms[%d] = %#v, want the SDK entry %#v", i, ArchivePlatforms[i], want)
		}
	}
}

// --- SuffixFromDockerPlatform ---

func TestSuffixFromDockerPlatform_LinuxAmd64(t *testing.T) {
	got := SuffixFromDockerPlatform("linux/amd64")
	if got != "linux-x64" {
		t.Errorf("SuffixFromDockerPlatform(linux/amd64) = %q, want %q", got, "linux-x64")
	}
}

func TestSuffixFromDockerPlatform_LinuxArm64(t *testing.T) {
	got := SuffixFromDockerPlatform("linux/arm64")
	if got != "linux-arm64" {
		t.Errorf("SuffixFromDockerPlatform(linux/arm64) = %q, want %q", got, "linux-arm64")
	}
}

func TestSuffixFromDockerPlatform_DarwinArm64(t *testing.T) {
	got := SuffixFromDockerPlatform("darwin/arm64")
	if got != "darwin-arm64" {
		t.Errorf("SuffixFromDockerPlatform(darwin/arm64) = %q, want %q", got, "darwin-arm64")
	}
}

func TestSuffixFromDockerPlatform_Unknown(t *testing.T) {
	got := SuffixFromDockerPlatform("windows/amd64")
	if got != "windows-x64" {
		t.Errorf("SuffixFromDockerPlatform(windows/amd64) = %q, want %q", got, "windows-x64")
	}
}

// --- DeriveBinaryName ---

func TestDeriveBinaryName_FromEntrypoint(t *testing.T) {
	got := DeriveBinaryName("./cmd/putnami-go", "@putnami/go")
	if got != "putnami-go" {
		t.Errorf("DeriveBinaryName = %q, want %q", got, "putnami-go")
	}
}

func TestDeriveBinaryName_CmdFallback(t *testing.T) {
	got := DeriveBinaryName("cmd", "@putnami/ci")
	if got != "putnami-ci" {
		t.Errorf("DeriveBinaryName = %q, want %q", got, "putnami-ci")
	}
}

func TestDeriveBinaryName_DotFallback(t *testing.T) {
	got := DeriveBinaryName(".", "@putnami/typescript")
	if got != "putnami-typescript" {
		t.Errorf("DeriveBinaryName = %q, want %q", got, "putnami-typescript")
	}
}

func TestDeriveBinaryName_AbsoluteEntrypoint(t *testing.T) {
	got := DeriveBinaryName("./cmd/my-server", "any/package")
	if got != "my-server" {
		t.Errorf("DeriveBinaryName = %q, want %q", got, "my-server")
	}
}

// run and serve start the binary they build, and Windows starts only a file
// with an executable suffix: exec reports "executable file not found" for a
// bare name. Every other host keeps the name DeriveBinaryName gives.
func TestHostBinaryNameCarriesTheWindowsExecutableSuffix(t *testing.T) {
	for _, tc := range []struct {
		goos, entrypoint, pkg, want string
	}{
		{"windows", "./cmd/api", "@acme/api", "api.exe"},
		{"windows", ".", "@acme/api", "acme-api.exe"},
		{"windows", "./cmd/tool.exe", "@acme/api", "tool.exe"},
		{"linux", "./cmd/api", "@acme/api", "api"},
		{"darwin", ".", "@acme/api", "acme-api"},
	} {
		if got := HostBinaryName(tc.goos, tc.entrypoint, tc.pkg); got != tc.want {
			t.Errorf("HostBinaryName(%q, %q, %q) = %q, want %q", tc.goos, tc.entrypoint, tc.pkg, got, tc.want)
		}
	}
}

func TestDeriveBinaryName_ScopedPackageName(t *testing.T) {
	got := DeriveBinaryName("cmd", "@org/tool")
	if got != "org-tool" {
		t.Errorf("DeriveBinaryName = %q, want %q", got, "org-tool")
	}
}

// --- HasMainGo ---

func TestHasMainGo_WithMainGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasMainGo(dir) {
		t.Error("HasMainGo should return true when main.go exists")
	}
}

func TestHasMainGo_WithOtherGoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasMainGo(dir) {
		t.Error("HasMainGo should return true when any .go file exists")
	}
}

func TestHasMainGo_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if HasMainGo(dir) {
		t.Error("HasMainGo should return false for empty dir")
	}
}

func TestHasMainGo_NonExistentDir(t *testing.T) {
	if HasMainGo("/nonexistent/path/xyz") {
		t.Error("HasMainGo should return false for nonexistent dir")
	}
}

func TestHasMainGo_OnlyNonGoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasMainGo(dir) {
		t.Error("HasMainGo should return false when no .go files present")
	}
}

// --- ReadGoEntrypoint ---

func TestReadGoEntrypoint_Default(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "an-absent-configuration-falls-back-to-the-documented-default")
	dir := t.TempDir()
	got := ReadGoEntrypoint(dir)
	if got != "./cmd/putnami-go" {
		t.Errorf("ReadGoEntrypoint default = %q, want %q", got, "./cmd/putnami-go")
	}
}

func TestReadGoEntrypoint_WithConfig(t *testing.T) {
	dir := t.TempDir()
	rc := map[string]any{
		"options": map[string]any{
			"@putnami/go": map[string]any{
				"entrypoint": "./cmd/my-server",
			},
		},
	}
	data, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadGoEntrypoint(dir)
	if got != "./cmd/my-server" {
		t.Errorf("ReadGoEntrypoint = %q, want %q", got, "./cmd/my-server")
	}
}

func TestReadGoProjectConfigLegacyFallbackAndCanonicalPrecedence(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"options":{"@putnami/go":{"entrypoint":"./cmd/legacy","executables":[{"name":"legacy-tool","package":"./cmd/legacy-tool"}]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".putnamirc.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadGoEntrypoint(dir); got != "./cmd/legacy" {
		t.Errorf("legacy entrypoint = %q, want ./cmd/legacy", got)
	}
	if got, err := ReadGoExecutables(dir); err != nil || !reflect.DeepEqual(got, []Executable{{Name: "legacy-tool", Package: "./cmd/legacy-tool"}}) {
		t.Errorf("legacy executables = %#v, %v", got, err)
	}
	current := `{"options":{"@putnami/go":{"entrypoint":"./cmd/current","executables":[]}}}`
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadGoEntrypoint(dir); got != "./cmd/current" {
		t.Errorf("canonical entrypoint = %q, want ./cmd/current", got)
	}
	if got, err := ReadGoExecutables(dir); err != nil || len(got) != 0 {
		t.Errorf("canonical executables = %#v, %v; want none", got, err)
	}
}

func TestReadGoEntrypoint_WithConfigNoEntrypoint(t *testing.T) {
	dir := t.TempDir()
	rc := map[string]any{
		"options": map[string]any{
			"@putnami/go": map[string]any{},
		},
	}
	data, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadGoEntrypoint(dir)
	if got != "./cmd/putnami-go" {
		t.Errorf("ReadGoEntrypoint no entrypoint key = %q, want %q", got, "./cmd/putnami-go")
	}
}

func TestReadGoEntrypoint_MalformedJSON(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "an-unreadable-configuration-falls-back-to-the-documented-default")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ReadGoEntrypoint(dir)
	if got != "./cmd/putnami-go" {
		t.Errorf("ReadGoEntrypoint malformed = %q, want %q", got, "./cmd/putnami-go")
	}
}

func TestReadGoEntrypoint_NoGoOptions(t *testing.T) {
	dir := t.TempDir()
	rc := map[string]any{
		"options": map[string]any{
			"@putnami/typescript": map[string]any{},
		},
	}
	data, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "putnami.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ReadGoEntrypoint(dir)
	if got != "./cmd/putnami-go" {
		t.Errorf("ReadGoEntrypoint no go options = %q, want %q", got, "./cmd/putnami-go")
	}
}

// --- ResolveServeEntrypoint ---

// helper to create a cmd/<name>/main.go structure.
func mkCmdMain(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "cmd", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveServeEntrypoint_ExplicitEntrypoint(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "a-configured-entrypoint-overrides-the-automatic-choice")
	dir := t.TempDir()
	got, err := ResolveServeEntrypoint(dir, "my-project", "./cmd/custom")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/custom" {
		t.Errorf("got %q, want %q", got, "./cmd/custom")
	}
}

func TestResolveServeEntrypoint_RootMainGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "a-root-main-package-is-selected-automatically")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	got, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "." {
		t.Errorf("got %q, want %q", got, ".")
	}
}

func TestResolveServeEntrypoint_SingleCmdSubdir(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "a-single-cmd-subdirectory-is-selected-automatically")
	dir := t.TempDir()
	mkCmdMain(t, dir, "api")

	got, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/api" {
		t.Errorf("got %q, want %q", got, "./cmd/api")
	}
}

func TestResolveServeEntrypoint_PrefersServe(t *testing.T) {
	dir := t.TempDir()
	mkCmdMain(t, dir, "serve")
	mkCmdMain(t, dir, "worker")
	mkCmdMain(t, dir, "migrate")

	got, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/serve" {
		t.Errorf("got %q, want %q", got, "./cmd/serve")
	}
}

func TestResolveServeEntrypoint_PrefersServer(t *testing.T) {
	dir := t.TempDir()
	mkCmdMain(t, dir, "server")
	mkCmdMain(t, dir, "worker")

	got, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/server" {
		t.Errorf("got %q, want %q", got, "./cmd/server")
	}
}

func TestResolveServeEntrypoint_PrefersProjectName(t *testing.T) {
	dir := t.TempDir()
	mkCmdMain(t, dir, "my-project")
	mkCmdMain(t, dir, "worker")

	got, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/my-project" {
		t.Errorf("got %q, want %q", got, "./cmd/my-project")
	}
}

func TestResolveServeEntrypoint_PrefersApi(t *testing.T) {
	dir := t.TempDir()
	mkCmdMain(t, dir, "api")
	mkCmdMain(t, dir, "worker")

	got, err := ResolveServeEntrypoint(dir, "unrelated-name", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "./cmd/api" {
		t.Errorf("got %q, want %q", got, "./cmd/api")
	}
}

func TestResolveServeEntrypoint_AmbiguousError(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "explicit-entrypoint", "an-ambiguous-cmd-layout-is-an-error-rather-than-a-guess")
	dir := t.TempDir()
	mkCmdMain(t, dir, "worker")
	mkCmdMain(t, dir, "migrate")

	_, err := ResolveServeEntrypoint(dir, "unrelated", "")
	if err == nil {
		t.Fatal("expected error for ambiguous cmd/ entries")
	}
}

func TestResolveServeEntrypoint_NothingFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveServeEntrypoint(dir, "my-project", "")
	if err == nil {
		t.Fatal("expected error when no main package found")
	}
}

// --- host and platform-spec resolution ---

// TestHostTarget pins that the host is a first-class member of the platform
// vocabulary: a fully-formed Target with the same archive suffix a distribution
// build would use, so a caller never has to special-case "the default".
func TestHostTarget(t *testing.T) {
	got := HostTarget()
	if got.GOOS != runtime.GOOS || got.GOARCH != runtime.GOARCH {
		t.Fatalf("HostTarget() = %s/%s, want %s/%s", got.GOOS, got.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if want := SuffixFromDockerPlatform(runtime.GOOS + "/" + runtime.GOARCH); got.Suffix != want {
		t.Errorf("HostTarget().Suffix = %q, want %q", got.Suffix, want)
	}
}

func TestParsePlatformSpec(t *testing.T) {
	host := HostTarget()

	tests := []struct {
		name    string
		entries []string
		want    []string
	}{
		{name: "nil is no request", entries: nil},
		{name: "blank entries are no request", entries: []string{"", "   ", ","}},
		{name: "single pair", entries: []string{"linux/amd64"}, want: []string{"linux/amd64"}},
		{name: "comma separated", entries: []string{"linux/amd64,darwin/arm64"},
			want: []string{"linux/amd64", "darwin/arm64"}},
		{name: "space separated", entries: []string{"linux/amd64 darwin/arm64"},
			want: []string{"linux/amd64", "darwin/arm64"}},
		{name: "array entries", entries: []string{"linux/amd64", "darwin/arm64"},
			want: []string{"linux/amd64", "darwin/arm64"}},
		{name: "host token", entries: []string{"host"}, want: []string{host.GOOS + "/" + host.GOARCH}},
		{name: "all token", entries: []string{"all"},
			want: []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}},
		{name: "duplicates collapse", entries: []string{"linux/amd64", "linux/amd64"},
			want: []string{"linux/amd64"}},
		{name: "os expands to its matrix members", entries: []string{"darwin"},
			want: []string{"darwin/amd64", "darwin/arm64"}},
		{name: "windows expands to its matrix member", entries: []string{"windows"},
			want: []string{"windows/amd64"}},
		{name: "unknown platform is synthesized", entries: []string{"windows/arm64"},
			want: []string{"windows/arm64"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePlatformSpec(tt.entries)
			if err != nil {
				t.Fatalf("ParsePlatformSpec(%v): %v", tt.entries, err)
			}
			names := make([]string, 0, len(got))
			for _, p := range got {
				names = append(names, p.GOOS+"/"+p.GOARCH)
			}
			if len(tt.want) == 0 {
				if len(names) != 0 {
					t.Fatalf("ParsePlatformSpec(%v) = %v, want no targets", tt.entries, names)
				}
				return
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("ParsePlatformSpec(%v) = %v, want %v", tt.entries, names, tt.want)
			}
		})
	}
}

// TestParsePlatformSpecIsDeterministic pins the property the cache depends on:
// one spec always resolves to one platform set, in one order. A set that
// depended on map iteration would make two runs of the same plan-time parameter
// produce different output trees.
func TestParsePlatformSpecIsDeterministic(t *testing.T) {
	spec := []string{"darwin/arm64,linux/amd64", "all", "host"}
	first, err := ParsePlatformSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got, _ := ParsePlatformSpec(spec); !reflect.DeepEqual(got, first) {
			t.Fatalf("ParsePlatformSpec is not deterministic: %#v != %#v", got, first)
		}
	}
}

// TestTargetsForReusesArchiveEntries pins that a request naming a distribution
// platform reuses that entry rather than synthesizing a look-alike: the archive
// suffix is what the package channels index binaries by, so a second Target for
// linux/amd64 carrying "linux-amd64" instead of "linux-x64" would split the tree.
func TestTargetsForReusesArchiveEntries(t *testing.T) {
	got, err := TargetsFor("linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("TargetsFor(linux/amd64) = %#v, want one target", got)
	}
	if got[0] != (Target{GOOS: "linux", GOARCH: "amd64", Suffix: "linux-x64"}) {
		t.Errorf("TargetsFor(linux/amd64) = %#v, want the ArchivePlatforms entry", got[0])
	}
	if empty, err := TargetsFor(""); err == nil || empty != nil {
		t.Errorf("TargetsFor(\"\") = (%v, %v); an empty request must be rejected, not resolved to an empty-GOOS target", empty, err)
	}
}

// TestTargetsForResolvesVocabularyTokens pins that the platform vocabulary has
// ONE resolver.
//
// `host` and `all` were handled in ParsePlatformSpec only, so they worked for
// `--platforms` and silently did not for `--target`, which calls TargetsFor
// directly: `--target host` synthesized Target{GOOS: "host"} and failed several
// layers down as `GOOS=host go build`, a documented word producing a cryptic
// toolchain error. Both entry points now resolve through here, so a token cannot
// mean one thing to one flag and nothing to the other.
func TestTargetsForResolvesVocabularyTokens(t *testing.T) {
	mustResolve := func(request string) []Target {
		t.Helper()
		targets, err := TargetsFor(request)
		if err != nil {
			t.Fatalf("TargetsFor(%q): %v", request, err)
		}
		return targets
	}
	if got, want := mustResolve(PlatformHost), []Target{HostTarget()}; !reflect.DeepEqual(got, want) {
		t.Errorf("TargetsFor(%q) = %#v, want the host target", PlatformHost, got)
	}
	if got := mustResolve(PlatformAll); !reflect.DeepEqual(got, ArchivePlatforms) {
		t.Errorf("TargetsFor(%q) = %#v, want the archive matrix", PlatformAll, got)
	}
	for _, token := range []string{PlatformHost, PlatformAll} {
		for _, target := range mustResolve(token) {
			if target.GOOS == token {
				t.Errorf("TargetsFor(%q) synthesized GOOS=%q; the token was treated as an operating system",
					token, target.GOOS)
			}
			if target.GOOS == "" || target.GOARCH == "" {
				t.Errorf("TargetsFor(%q) produced an incomplete target %#v", token, target)
			}
		}
	}

	// `all` must not be mutable through the returned slice: it is a copy, or a
	// caller appending to it would edit the package-level matrix in place.
	resolved := mustResolve(PlatformAll)
	resolved[0] = Target{GOOS: "tampered"}
	if ArchivePlatforms[0].GOOS == "tampered" {
		t.Error("TargetsFor(all) aliased ArchivePlatforms; a caller can rewrite the distribution matrix")
	}

	// The two entry points must agree, which is the whole point of one resolver.
	viaSpec, _ := ParsePlatformSpec([]string{PlatformHost})
	if got, want := viaSpec, mustResolve(PlatformHost); !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePlatformSpec(host) = %#v, TargetsFor(host) = %#v; the vocabularies drifted", got, want)
	}
}

// TestTargetsForRejectsNonPlatforms pins that a request which cannot be a
// platform fails HERE.
//
// The synthesizing branch exists so a target outside the archive matrix
// (windows/arm64, linux/riscv64) still compiles, and that generosity is what
// used to swallow garbage: a malformed `platforms` parameter or a typo became
// Target{GOOS: "{\"oops\":1}"} and surfaced as `unsupported GOOS/GOARCH pair`
// from the toolchain, several layers from the mistake. Worse, `platforms` is a
// cache-key parameter, so the nonsense was recorded as a legitimate request.
//
// The rule is structural, not a fixed allow-list: `go tool dist list` grows, and
// rejecting a platform this extension has not heard of would be worse than the
// problem being solved.
func TestTargetsForRejectsNonPlatforms(t *testing.T) {
	for _, request := range []string{
		"", `{"oops":1}`, "linux/", "/amd64", "Linux/amd64", "linux amd64",
		"linux/amd64/extra", "../etc", "linux/amd-64",
	} {
		if got, err := TargetsFor(request); err == nil {
			t.Errorf("TargetsFor(%q) = %#v with no error; a value that cannot be a platform must be rejected "+
				"rather than reaching the toolchain as a GOOS", request, got)
		}
	}

	// Generosity preserved: platforms outside the archive matrix still resolve,
	// including ones Go may only add later.
	for _, request := range []string{"windows/arm64", "linux/riscv64", "js/wasm", "freebsd", "plan9/386"} {
		if _, err := TargetsFor(request); err != nil {
			t.Errorf("TargetsFor(%q) rejected a legitimate platform: %v", request, err)
		}
	}

	// A rejected entry fails the whole spec rather than being skipped: a partial
	// platform set is the one outcome worse than an error, because it silently
	// builds less than was asked for.
	if got, err := ParsePlatformSpec([]string{"linux/amd64", `{"oops":1}`}); err == nil {
		t.Errorf("ParsePlatformSpec kept %#v and dropped the invalid entry; a typo would silently narrow the build", got)
	}
}

// --- package-channel scoping ---

func names(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.GOOS+"/"+t.GOARCH)
	}
	return out
}

// TestDistributionTargetsScopeByChannel is the core of channel scoping: the
// distribution platform set is what the ACTIVE CHANNELS consume, not a fixed
// matrix. A docker channel owes one platform because an image carries one
// binary; an archive channel owes the declared release matrix; a channel that
// carries no binary owes nothing.
func TestDistributionTargetsScopeByChannel(t *testing.T) {
	full := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}

	tests := []struct {
		name string
		req  DistributionRequest
		want []string
	}{
		{
			name: "docker alone compiles exactly the image platform",
			req:  DistributionRequest{Channels: PackageChannels{Docker: true}},
			want: []string{"linux/amd64"},
		},
		{
			name: "docker honors an explicit image platform",
			req:  DistributionRequest{Channels: PackageChannels{Docker: true}, DockerPlatform: "linux/arm64"},
			want: []string{"linux/arm64"},
		},
		{
			name: "archives keep the whole archive matrix when nothing is declared",
			req:  DistributionRequest{Channels: PackageChannels{Archives: true}},
			want: full,
		},
		{
			name: "archives build the declared targets only",
			req: DistributionRequest{
				Channels: PackageChannels{Archives: true},
				Declared: []string{"linux/amd64,darwin/arm64"},
			},
			want: []string{"linux/amd64", "darwin/arm64"},
		},
		{
			name: "the go module channel carries no binary",
			req:  DistributionRequest{Channels: PackageChannels{GoModule: true}},
			want: nil,
		},
		{
			name: "template archives carry no binary",
			req:  DistributionRequest{Channels: PackageChannels{TemplateArchives: true}},
			want: nil,
		},
		{
			name: "archives and docker take the union",
			req: DistributionRequest{
				Channels:       PackageChannels{Archives: true, Docker: true},
				Declared:       []string{"darwin/arm64"},
				DockerPlatform: "linux/amd64",
			},
			want: []string{"darwin/arm64", "linux/amd64"},
		},
		{
			name: "an image platform already in the matrix is not built twice",
			req: DistributionRequest{
				Channels:       PackageChannels{Archives: true, Docker: true},
				DockerPlatform: "linux/amd64",
			},
			want: full,
		},
		{
			name: "no channel named keeps the declared matrix",
			req:  DistributionRequest{},
			want: full,
		},
		{
			name: "--target wins over every channel",
			req: DistributionRequest{
				Channels:       PackageChannels{Archives: true, Docker: true},
				DockerPlatform: "linux/arm64",
				Target:         "darwin/arm64",
			},
			want: []string{"darwin/arm64"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DistributionTargets(tt.req)
			if err != nil {
				t.Fatalf("DistributionTargets(%+v): %v", tt.req, err)
			}
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("platforms = %v, want none", names(got))
				}
				return
			}
			if !reflect.DeepEqual(names(got), tt.want) {
				t.Errorf("platforms = %v, want %v", names(got), tt.want)
			}
		})
	}
}

// TestDistributionTargetsRejectMalformedInput pins that a platform request that
// cannot be honored fails instead of resolving to something else. A docker
// channel silently widened to two platforms would assemble an image from a
// binary the caller never named.
func TestDistributionTargetsRejectMalformedInput(t *testing.T) {
	if got, err := DistributionTargets(DistributionRequest{
		Channels: PackageChannels{Docker: true}, DockerPlatform: "linux",
	}); err == nil {
		t.Errorf("DistributionTargets with image platform %q = %v; an image carries one platform, "+
			"so an os-only value must be rejected rather than expanded", "linux", names(got))
	}
	if _, err := DistributionTargets(DistributionRequest{
		Channels: PackageChannels{Archives: true}, Declared: []string{`{"oops":1}`},
	}); err == nil {
		t.Error("a malformed `platforms` entry resolved silently; a typo must not become a release matrix")
	}
}

// TestDockerTargetMatchesArchiveSuffix pins that the image's compile target is
// the SAME Target the archive matrix carries: the package channels index
// binaries by Suffix, so a second linux/amd64 target spelled "linux-amd64"
// would put the image's binary in a directory the packager never looks in.
func TestDockerTargetMatchesArchiveSuffix(t *testing.T) {
	got, err := dockerTarget("linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != ArchivePlatforms[0] {
		t.Errorf("dockerTarget(linux/amd64) = %#v, want the archive entry %#v", got, ArchivePlatforms[0])
	}
	if got.Suffix != SuffixFromDockerPlatform("linux/amd64") {
		t.Errorf("dockerTarget suffix = %q, want %q", got.Suffix, SuffixFromDockerPlatform("linux/amd64"))
	}
}

// TestDeclaredTargetsNeverWiden pins the release-channel half of the contract:
// a declaration narrows, absence keeps the default, and nothing adds a platform
// the project did not ask for.
func TestDeclaredTargetsNeverWiden(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "a-declared-matrix-never-widens-beyond-what-was-declared")
	got, err := DeclaredTargets(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ArchivePlatforms) {
		t.Errorf("DeclaredTargets(nil) = %v, want the archive matrix", names(got))
	}
	// The returned slice must not alias the package-level matrix: a caller that
	// appends to it would rewrite the default for every later call in the process.
	got[0] = Target{GOOS: "mutated"}
	if ArchivePlatforms[0].GOOS == "mutated" {
		t.Fatal("DeclaredTargets returned the ArchivePlatforms backing array; one caller can now rewrite the matrix")
	}

	narrowed, err := DeclaredTargets([]string{"linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names(narrowed), []string{"linux/amd64"}) {
		t.Errorf("DeclaredTargets([linux/amd64]) = %v, want exactly that platform", names(narrowed))
	}
}

// TestPackageChannelsForAcceptsBothArchiveSpellings pins that a project
// declaring `extension-archives` and one declaring `archives` are scoped
// identically. They are one channel under two names, and a project that packaged
// archives but was scoped as if it had none would compile nothing to stage.
func TestPackageChannelsForAcceptsBothArchiveSpellings(t *testing.T) {
	for _, name := range []string{ChannelArchives, ChannelExtensionArchives} {
		if got := PackageChannelsFor([]string{name}); !got.Archives {
			t.Errorf("PackageChannelsFor(%q).Archives = false, want true", name)
		}
	}
	if got := PackageChannelsFor([]string{"npm", "pypi"}); got.Any() {
		t.Errorf("PackageChannelsFor(foreign channels) = %+v, want no Go channel", got)
	}
}

// TestPlatformsSpecReadsOneShape pins that the `platforms` parameter decodes
// identically from the JSON array a putnami.json option carries and the string a
// CLI flag delivers — and that anything else is an error rather than a
// synthesized GOOS that would be recorded as a legitimate cache key.
func TestPlatformsSpecReadsOneShape(t *testing.T) {
	if got, err := PlatformsSpec(nil); err != nil || got != nil {
		t.Errorf("PlatformsSpec(nil) = (%v, %v), want (nil, nil)", got, err)
	}
	got, err := PlatformsSpec([]byte(`["linux/amd64","darwin/arm64"]`))
	if err != nil || !reflect.DeepEqual(got, []string{"linux/amd64", "darwin/arm64"}) {
		t.Errorf("PlatformsSpec(array) = (%v, %v)", got, err)
	}
	got, err = PlatformsSpec([]byte(`"linux/amd64,darwin/arm64"`))
	if err != nil || !reflect.DeepEqual(got, []string{"linux/amd64,darwin/arm64"}) {
		t.Errorf("PlatformsSpec(string) = (%v, %v)", got, err)
	}
	if _, err := PlatformsSpec([]byte(`{"oops":1}`)); err == nil {
		t.Error("PlatformsSpec accepted an object; a malformed parameter must not become a platform")
	}
}

func TestReadGoExecutables(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	t.Run("absent config and absent option declare none", func(t *testing.T) {
		for _, body := range []string{"", `{"options": {"@putnami/go": {"entrypoint": "./cmd/x"}}}`, `{}`} {
			got, err := ReadGoExecutables(write(t, body))
			if err != nil || got != nil {
				t.Errorf("ReadGoExecutables(%q) = %#v, %v; want nil, nil", body, got, err)
			}
		}
	})
	t.Run("declared executables are returned in order", func(t *testing.T) {
		got, err := ReadGoExecutables(write(t, `{"options": {"@putnami/go": {"executables": [
			{"name": "b-tool", "package": "example.com/b/cmd/b"},
			{"name": "a-tool", "package": "./cmd/a"}
		]}}}`))
		want := []Executable{{Name: "b-tool", Package: "example.com/b/cmd/b"}, {Name: "a-tool", Package: "./cmd/a"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ReadGoExecutables = %#v, %v; want %#v", got, err, want)
		}
	})
	for name, body := range map[string]string{
		"a path as name": `{"options": {"@putnami/go": {"executables": [{"name": "../x", "package": "./cmd/x"}]}}}`,
		"an empty name":  `{"options": {"@putnami/go": {"executables": [{"name": "", "package": "./cmd/x"}]}}}`,
		"no package":     `{"options": {"@putnami/go": {"executables": [{"name": "x"}]}}}`,
		"a duplicate":    `{"options": {"@putnami/go": {"executables": [{"name": "x", "package": "./a"}, {"name": "x", "package": "./b"}]}}}`,
		"a wrong shape":  `{"options": {"@putnami/go": {"executables": "x"}}}`,
		"malformed JSON": `{`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if got, err := ReadGoExecutables(write(t, body)); err == nil {
				t.Errorf("ReadGoExecutables accepted %s: %#v", name, got)
			}
		})
	}
}
