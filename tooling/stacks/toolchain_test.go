package stacks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
