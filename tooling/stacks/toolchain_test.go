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
	got, err := readVersion([]byte(`{"sdk": {"version": "8.0.401"}}`), "sdk.version")
	if err != nil || got != "8.0.401" {
		t.Fatalf("readVersion = %q, %v", got, err)
	}
	if got, _ := readVersion([]byte(`{"sdk": 1}`), "sdk.version"); got != "" {
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
