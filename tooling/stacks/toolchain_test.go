package stacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExpectedVersionReadsTheNearestFile(t *testing.T) {
	root := t.TempDir()
	tc := mustProfile(t).Toolchain
	writeFile(t, root, "rust-toolchain.toml", "# pinned\n[toolchain]\nchannel = \"1.82.0\" # stable\ncomponents = [\"clippy\"]\n")
	got, err := tc.ExpectedVersion(root, "crates/api", ".")
	if err != nil || got != (Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}) {
		t.Fatalf("root file: %+v, %v", got, err)
	}
	writeFile(t, root, "crates/api/rust-toolchain", "\n1.80.1\n")
	got, err = tc.ExpectedVersion(root, "crates/api", ".")
	if err != nil || got != (Expected{Version: "1.80.1", Source: "crates/api/rust-toolchain"}) {
		t.Fatalf("nearest file: %+v, %v", got, err)
	}
	tc.Version = "1.79.0"
	if got, _ := tc.ExpectedVersion(root, "crates/api", "."); got != (Expected{Version: "1.79.0", Source: "profile"}) {
		t.Fatalf("toolchain.version outranks the files: %+v", got)
	}
}

func TestExpectedVersionStopsAtTheStackRoot(t *testing.T) {
	root := t.TempDir()
	tc := mustProfile(t).Toolchain
	writeFile(t, root, "rust-toolchain", "1.82.0\n")
	got, err := tc.ExpectedVersion(root, "tools/gen", "tools/gen")
	if err != nil || got.Version != "" {
		t.Fatalf("a file above the stack root is not read: %+v, %v", got, err)
	}
	writeFile(t, root, "crates/rust-toolchain", "1.70.0\n")
	got, err = tc.ExpectedVersion(root, "crates/api", ".")
	if err != nil || got.Source != "rust-toolchain" {
		t.Fatalf("a directory between the project and the root is not read: %+v, %v", got, err)
	}
}

func TestReadVersionReadsJSONKeys(t *testing.T) {
	got, err := readVersion("global.json", []byte(`{"sdk": {"version": "8.0.401"}}`), "sdk.version")
	if err != nil || got != "8.0.401" {
		t.Fatalf("readVersion = %q, %v", got, err)
	}
	if got, _ := readVersion("global.json", []byte(`{"sdk": 1}`), "sdk.version"); got != "" {
		t.Fatalf("a missing key reads nothing: %q", got)
	}
}

func TestCompareDecidesTheCheck(t *testing.T) {
	tc := mustProfile(t).Toolchain
	if v := tc.Compare(Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}, "1.82.0"); v.Code != "" || v.Fail {
		t.Errorf("match: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "1.82", Source: "rust-toolchain.toml"}, "1.82.3"); v.Code != "" {
		t.Errorf("a major.minor channel matches its patch releases: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "1.82", Source: "rust-toolchain.toml"}, "1.82.0-nightly"); v.Code != CodeToolchainMismatch {
		t.Errorf("a major.minor channel names stable releases only: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "1.8", Source: "rust-toolchain.toml"}, "1.82.0"); v.Code != CodeToolchainMismatch {
		t.Errorf("1.8 does not name 1.82: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}, "1.80.0"); v.Code != CodeToolchainMismatch || !v.Fail {
		t.Errorf("mismatch fails: %+v", v)
	}
	tc.Check = CheckWarn
	if v := tc.Compare(Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}, "1.80.0"); v.Code != CodeToolchainMismatch || v.Fail {
		t.Errorf("warn does not fail: %+v", v)
	}
	if v := tc.Compare(Expected{Version: "stable", Source: "rust-toolchain.toml"}, "1.80.0"); v.Code != CodeToolchainUnpinned || v.Fail {
		t.Errorf("a channel is unpinned: %+v", v)
	}
	if v := tc.Compare(Expected{}, "1.80.0"); v.Code != CodeToolchainUnpinned || v.Fail {
		t.Errorf("no expected version is unpinned: %+v", v)
	}
}

func TestMachineVersionReadsTheCommandOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bin/rustc", "#!/bin/sh\necho 'rustc 1.82.0 (f6e511eec 2024-10-15)'\n")
	if err := os.Chmod(filepath.Join(dir, "bin/rustc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "bin"))
	tc := mustProfile(t).Toolchain
	got, err := tc.MachineVersion(context.Background(), dir, os.Environ())
	if err != nil || got != "1.82.0" {
		t.Fatalf("MachineVersion = %q, %v", got, err)
	}
	tc.Command = []string{"no-such-rustc"}
	if _, err := tc.MachineVersion(context.Background(), dir, os.Environ()); !errors.Is(err, ErrToolchainMissing) {
		t.Fatalf("a missing command: %v", err)
	}
}

func TestAnExactVersionWinsOverARange(t *testing.T) {
	root := t.TempDir()
	tc := Toolchain{Command: []string{"php"}, Pattern: `(?<version>.*)`, Files: []VersionFile{{Path: ".php-version"}, {Path: "composer.json", Key: "require.php"}}}
	writeFile(t, root, "composer.json", `{"require": {"php": ">=8.4"}}`)
	writeFile(t, root, "packages/pricing/composer.json", `{"require": {"php": "^8.2"}}`)
	if got, _ := tc.ExpectedVersion(root, "packages/pricing", "."); got != (Expected{Version: "^8.2", Source: "packages/pricing/composer.json"}) {
		t.Errorf("with no exact version, the nearest range: %+v", got)
	}
	writeFile(t, root, ".php-version", "8.4.10\n")
	if got, _ := tc.ExpectedVersion(root, "packages/pricing", "."); got != (Expected{Version: "8.4.10", Source: ".php-version"}) {
		t.Errorf("a root exact version wins over the package's range: %+v", got)
	}
	writeFile(t, root, "packages/pricing/.php-version", "stable\n")
	if got, _ := tc.ExpectedVersion(root, "packages/pricing", "."); got.Version != "stable" {
		t.Errorf("a value that is no range ends the search: %+v", got)
	}
}

func stubVersionTool(t *testing.T, dir, name, version string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'tool "+version+"'\n"), 0o755); err != nil { //nolint:gosec // an executable stub
		t.Fatal(err)
	}
	return path
}

func TestSelectPrefersThePathToolchainAndFallsBackOnThePinnedOne(t *testing.T) {
	tc := Toolchain{Command: []string{"stubtool", "--version"}, Pattern: `^tool (?<version>[0-9.]+)`, Provisioned: "STUB_PINNED"}
	expected := Expected{Version: "^8.3", Source: "composer.json"}
	pinned := stubVersionTool(t, t.TempDir(), "stubtool", "8.4.23")
	bin := t.TempDir()
	ctx := context.Background()

	t.Setenv("PATH", bin)
	env := []string{"PATH=" + bin, "STUB_PINNED=" + pinned}
	got, err := tc.Select(ctx, bin, expected, env)
	if err != nil || got.Executable != pinned || got.Verdict.Code != "" {
		t.Fatalf("a missing toolchain falls back on the pinned one: %+v %v", got, err)
	}
	if got.Program(tc, "stubtool") != pinned || got.Program(tc, "other") != "other" {
		t.Errorf("only the toolchain command is redirected: %q", got.Program(tc, "stubtool"))
	}
	if !strings.HasPrefix(environmentValue(got.Env, "PATH"), filepath.Dir(pinned)+string(os.PathListSeparator)) {
		t.Errorf("the pinned directory comes first on PATH: %v", got.Env)
	}

	stubVersionTool(t, bin, "stubtool", "8.5.1")
	got, err = tc.Select(ctx, bin, expected, env)
	if err != nil || got.Executable != "" {
		t.Errorf("a toolchain on PATH in the range runs: %+v %v", got, err)
	}

	stubVersionTool(t, bin, "stubtool", "7.4.0")
	got, err = tc.Select(ctx, bin, expected, env)
	if err != nil || got.Executable != pinned || got.Verdict.Fail {
		t.Errorf("a toolchain on PATH outside the range gives way: %+v %v", got, err)
	}

	warn := tc
	warn.Check = CheckWarn
	got, err = warn.Select(ctx, bin, expected, env)
	if err != nil || got.Executable != "" || got.Verdict.Code != CodeToolchainMismatch {
		t.Errorf("warn keeps the toolchain on PATH: %+v %v", got, err)
	}

	got, err = tc.Select(ctx, bin, Expected{Version: "^9.0", Source: "composer.json"}, env)
	if err != nil || got.Executable != "" || !got.Verdict.Fail {
		t.Errorf("a pinned toolchain outside the range leaves the failure of the one on PATH: %+v %v", got, err)
	}

	got, err = tc.Select(ctx, bin, expected, []string{"PATH=" + bin})
	if err != nil || got.Executable != "" || !got.Verdict.Fail {
		t.Errorf("with no pinned toolchain, the check fails as before: %+v %v", got, err)
	}

	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if _, err = tc.Select(ctx, empty, expected, []string{"PATH=" + empty}); !errors.Is(err, ErrToolchainMissing) {
		t.Errorf("with nothing to run, the toolchain is missing: %v", err)
	}
}

func TestPrependPathAddsAPathWhenThereIsNone(t *testing.T) {
	if got := prependPath([]string{"A=1"}, "/x"); !slices.Equal(got, []string{"A=1", "PATH=/x"}) {
		t.Errorf("prependPath = %v", got)
	}
}

func TestProgramFindsAProgramBesideThePinnedToolchain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "rustc", "")
	writeFile(t, dir, "cargo", "")
	tc := Toolchain{Command: []string{"rustc", "--version"}}
	selected := Selection{Executable: filepath.Join(dir, "rustc")}
	if got := selected.Program(tc, "rustc"); got != filepath.Join(dir, "rustc") {
		t.Errorf("toolchain command = %s", got)
	}
	if got := selected.Program(tc, "cargo"); got != filepath.Join(dir, "cargo") {
		t.Errorf("sibling = %s", got)
	}
	if got := selected.Program(tc, "sh"); got != "sh" {
		t.Errorf("another program = %s", got)
	}
	if got := (Selection{}).Program(tc, "cargo"); got != "cargo" {
		t.Errorf("the machine's toolchain = %s", got)
	}
}

func TestAppendPathKeepsTheMachineFirst(t *testing.T) {
	got := appendPath([]string{"A=1", "PATH=/machine"}, "/tools")
	want := "PATH=/machine" + string(os.PathListSeparator) + "/tools"
	if got[1] != want {
		t.Errorf("PATH = %s", got[1])
	}
	if got := appendPath([]string{"A=1"}, "/tools"); got[1] != "PATH=/tools" {
		t.Errorf("no PATH = %v", got)
	}
}

func TestReadVersionReadsYAMLKeys(t *testing.T) {
	pubspec := `name: shop # the package
environment: # the SDKs
  # the Dart SDK
  sdk: ^3.7.0 # pub constraint
  flutter: ">=3.29.0"
dependencies:
  environment:
    sdk: not-this
  flutter:
    sdk: flutter
`
	for key, want := range map[string]string{"environment.sdk": "^3.7.0", "environment.flutter": ">=3.29.0", "name": "shop", "environment.dart": "", "dependencies.flutter.sdk": "flutter", "environment": ""} {
		if got, err := readVersion("pubspec.yaml", []byte(pubspec), key); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", key, got, err, want)
		}
	}
	if got, _ := readVersion("pubspec.yml", []byte("environment:\n    sdk: '>=3.0.0 <4.0.0'\n"), "environment.sdk"); got != ">=3.0.0 <4.0.0" {
		t.Errorf("a .yml file with a deeper indent = %q", got)
	}
}

func TestReadVersionReadsYAMLFlowMappings(t *testing.T) {
	for _, c := range []struct{ pubspec, key, want string }{
		{"name: shop\nenvironment: {sdk: ^3.7.0, flutter: \">=3.29.0\"}\n", "environment.sdk", "^3.7.0"},
		{"name: shop\nenvironment: {sdk: ^3.7.0, flutter: \">=3.29.0\"}\n", "environment.flutter", ">=3.29.0"},
		{"environment: {sdk: '>=3.0.0 <4.0.0'} # the SDK\n", "environment.sdk", ">=3.0.0 <4.0.0"},
		{"environment: {\"sdk\":\"^3.7.0\"}\n", "environment.sdk", "^3.7.0"},
		{"environment: {\n  sdk: ^3.7.0, # the Dart SDK\n  flutter: '>=3.29.0'\n}\nname: shop\n", "environment.flutter", ">=3.29.0"},
		{"environment:\n  {sdk: >=3.0.0 <4.0.0}\n", "environment.sdk", ">=3.0.0 <4.0.0"},
		{"{name: shop, environment: {sdk: ^3.7.0}}\n", "environment.sdk", "^3.7.0"},
		{"dependencies: {flutter: {sdk: flutter}}\n", "dependencies.flutter.sdk", "flutter"},
		{"environment: {sdk: ^3.7.0}\n", "environment.flutter", ""},
		{"environment: {sdk: [3]}\n", "environment.sdk", ""},
		{"environment: {sdk: ^3.7.0\n", "environment.sdk", ""},
		{"environment: {description: it's, sdk: ^3.7.0}\n", "environment.sdk", "^3.7.0"},
	} {
		if got, err := readVersion("pubspec.yaml", []byte(c.pubspec), c.key); err != nil || got != c.want {
			t.Errorf("%s of %q = %q, %v; want %q", c.key, c.pubspec, got, err, c.want)
		}
	}
}

func TestPubDialectsReadARangeAsPubDoes(t *testing.T) {
	tc := mustProfile(t).Toolchain
	sdk, flutter := tc, tc
	sdk.Ranges, flutter.Ranges = RangesPubSDK, RangesPubFlutter
	for _, c := range []struct {
		check             Toolchain
		expected, machine string
		code              string
	}{
		{tc, "^0.0.3", "0.0.9", CodeToolchainMismatch},
		{sdk, "^0.0.3", "0.0.9", ""},
		{sdk, "^0.0.3", "0.1.0", CodeToolchainMismatch},
		{sdk, "^3.7.0", "3.9.2", ""},
		{sdk, "^3.7.0", "4.0.0", CodeToolchainMismatch},
		{sdk, ">=3.7.0<4.0.0", "3.8.0", ""},
		{sdk, ">=3.7.0<4.0.0", "4.0.0", CodeToolchainMismatch},
		{tc, ">=2.17.0 <3.0.0", "3.7.2", CodeToolchainMismatch},
		{sdk, ">=2.17.0 <3.0.0", "3.7.2", ""},
		{sdk, ">=2.17.0 <3.0.0-0", "3.7.2", ""},
		{sdk, ">=2.12.0-0 <3.0.0", "3.7.2", ""},
		{sdk, ">=2.17.0 <3.0.0", "4.0.0", CodeToolchainMismatch},
		{sdk, ">=2.10.0 <3.0.0", "3.7.2", CodeToolchainMismatch},
		{sdk, ">=2.17.0 <3.1.0", "3.7.2", CodeToolchainMismatch},
		{flutter, ">=3.0.0 <3.10.0", "3.29.2", ""},
		{flutter, ">=3.0.0 <3.10.0", "2.10.5", CodeToolchainMismatch},
		{flutter, "3.29.2", "3.32.0", ""},
		{flutter, "3.29.2", "3.27.4", CodeToolchainMismatch},
		{sdk, "any", "3.7.2", CodeToolchainUnpinned},
	} {
		if v := c.check.Compare(Expected{Version: c.expected, Source: "pubspec.yaml"}, c.machine); v.Code != c.code {
			t.Errorf("%q %s against %s = %+v, want %q", c.check.Ranges, c.expected, c.machine, v, c.code)
		}
	}
	if v := flutter.Compare(Expected{Version: "3.29.2", Source: "profile"}, "3.35.0"); v.Code != CodeToolchainMismatch || !v.Fail {
		t.Errorf("toolchain.version reads plain, whatever the dialect: %+v", v)
	}
	if v := sdk.Compare(Expected{Version: ">=2.17.0 <3.0.0", Source: "profile"}, "3.7.2"); v.Code != CodeToolchainMismatch || strings.Contains(v.Message, "as pub reads it") {
		t.Errorf("a range toolchain.version reads plain too: %+v", v)
	}
	if v := sdk.Compare(Expected{Version: "^3.7.0", Source: "pubspec.yaml"}, "3.6.0"); !strings.Contains(v.Message, "as pub reads it") {
		t.Errorf("a dialect names itself in the message: %+v", v)
	}
}

func TestFurtherChecksInheritTheCheckMode(t *testing.T) {
	p := mustProfile(t)
	p.Toolchain.Check = CheckWarn
	p.Toolchain.Also = []Toolchain{{Command: []string{"cargo", "--version"}, Pattern: `cargo (?<version>\S+)`, Files: []VersionFile{{Path: "cargo-version"}}}}
	checks := p.Toolchain.Checks()
	if len(checks) != 2 || checks[0].Also != nil || checks[1].Check != CheckWarn {
		t.Fatalf("checks = %+v", checks)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, root := p.TaskPatterns(TaskLint); !slices.Contains(root, "cargo-version") {
		t.Errorf("a further check's file is an input: %v", root)
	}
	p.Toolchain.Also[0].Also = []Toolchain{p.Toolchain.Also[0]}
	if err := p.Validate(); err == nil {
		t.Error("a further check names no checks of its own")
	}
	p.Toolchain.Also = []Toolchain{{Command: []string{"cargo"}, Pattern: `(?<version>.*)`, Ranges: "npm"}}
	if err := p.Validate(); err == nil {
		t.Error("an unknown range dialect is refused")
	}
}

func TestAFlutterConstraintReadsPlainForALanguage39Pubspec(t *testing.T) {
	root := t.TempDir()
	flutter := Toolchain{Command: []string{"flutter"}, Pattern: `Flutter (?<version>\S+)`, Ranges: RangesPubFlutter,
		Files: []VersionFile{{Path: "pubspec.yaml", Key: "environment.flutter"}}}
	writeFile(t, root, "old/pubspec.yaml", "environment:\n  sdk: ^3.7.0\n  flutter: \">=3.29.0 <3.30.0\"\n")
	writeFile(t, root, "new/pubspec.yaml", "environment:\n  sdk: ^3.9.0\n  flutter: \">=3.29.0 <3.30.0\"\n")
	old, err := flutter.ExpectedVersion(root, "old", "old")
	if err != nil || old.Reading != "" {
		t.Fatalf("old = %+v %v", old, err)
	}
	if v := flutter.Compare(old, "3.32.0"); v.Code != "" {
		t.Errorf("below language 3.9 the upper bound is ignored: %+v", v)
	}
	newer, err := flutter.ExpectedVersion(root, "new", "new")
	if err != nil || newer.Reading != ReadingPlain {
		t.Fatalf("new = %+v %v", newer, err)
	}
	if v := flutter.Compare(newer, "3.32.0"); v.Code != CodeToolchainMismatch {
		t.Errorf("from language 3.9 the upper bound holds: %+v", v)
	}
	if v := flutter.Compare(Expected{Version: ">=3.29.0-0<3.30.0", Source: "pubspec.yaml", Reading: ReadingPlain}, "3.29.2"); v.Code != "" {
		t.Errorf("a plain reading keeps pub's syntax: %+v", v)
	}
}

func TestPinnedSelectsTheInstalledToolchain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "rustc", "#!/bin/sh\necho 'rustc 1.90.0'\n")
	if err := os.Chmod(filepath.Join(dir, "rustc"), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := mustProfile(t).Toolchain
	tc.Provisioned = "STUB_PINNED"
	sel, err := tc.Pinned(context.Background(), dir, Expected{Version: "1.90.0"}, []string{"STUB_PINNED=" + filepath.Join(dir, "rustc"), "PATH=/usr/bin"})
	if err != nil || sel.Executable != filepath.Join(dir, "rustc") || sel.Verdict.Code != "" || !strings.HasPrefix(pathValue(sel.Env), dir) {
		t.Errorf("Pinned = %+v %v", sel, err)
	}
	if _, err := tc.Pinned(context.Background(), dir, Expected{}, []string{"PATH=/usr/bin"}); !errors.Is(err, ErrToolchainMissing) {
		t.Errorf("no installed toolchain: %v", err)
	}
}

func TestAPinnedToolchainSetsItsHome(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := stubVersionTool(t, filepath.Join(home, "bin"), "stubjava", "21.0.13")
	tc := Toolchain{Command: []string{"stubjava", "-version"}, Pattern: `^tool (?<version>[0-9.]+)`, Provisioned: "STUB_PINNED", Home: "STUB_HOME"}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	got, err := tc.Select(context.Background(), bin, Expected{Version: "21", Source: ".java-version"}, []string{"PATH=" + bin, "STUB_PINNED=" + pinned, "STUB_HOME=/elsewhere"})
	if err != nil || got.Executable != pinned {
		t.Fatalf("select: %+v %v", got, err)
	}
	if v := environmentValue(got.Env, "STUB_HOME"); v != home {
		t.Errorf("the home of the pinned toolchain replaces the machine's: %q", v)
	}
	for name, tc := range map[string]Toolchain{
		"a home without a provisioned toolchain": {Command: []string{"java"}, Pattern: `(?<version>.*)`, Home: "JAVA_HOME"},
		"a home that is no variable name":        {Command: []string{"java"}, Pattern: `(?<version>.*)`, Provisioned: "P", Home: "java home"},
	} {
		if tc.validate() == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}

func TestProgramFindsAToolOnTheSelectionPath(t *testing.T) {
	tools := t.TempDir()
	writeFile(t, tools, "mvn", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(tools, "mvn"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tools, "notes", "")
	tc := Toolchain{Command: []string{"javac", "-version"}}
	selected := Selection{Env: []string{"PATH=" + t.TempDir() + string(os.PathListSeparator) + tools}}
	if got := selected.Program(tc, "mvn"); got != filepath.Join(tools, "mvn") {
		t.Errorf("a tool on the selection's PATH = %s", got)
	}
	if got := selected.Program(tc, "notes"); got != "notes" {
		t.Errorf("a file that is not executable is no program: %s", got)
	}
	if got := selected.Program(tc, "./gradlew"); got != "./gradlew" {
		t.Errorf("a path is kept: %s", got)
	}
	writeFile(t, tools, "javac", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(tools, "javac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := selected.Program(tc, "javac"); got != "javac" {
		t.Errorf("the machine's toolchain command stays bare, unlike a pinned one: %s", got)
	}
	relative := Selection{Env: []string{"PATH=relative-bin"}}
	if got := relative.Program(tc, "mvn"); got != "mvn" {
		t.Errorf("a relative PATH entry is no place to find a tool: %s", got)
	}
}
