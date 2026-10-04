package runtimeinfo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

func TestHandleRuntimeInfo(t *testing.T) {
	var out bytes.Buffer
	handled, err := Handle([]string{Verb, Command}, &out, "@putnami/test", "1.2.3")
	if err != nil || !handled {
		t.Fatalf("Handle() = (%v, %v), want handled success", handled, err)
	}
	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Extension != "@putnami/test" || info.Version != "1.2.3" {
		t.Fatalf("identity = %q@%q", info.Extension, info.Version)
	}
	if info.Platform != runtime.GOOS+"/"+runtime.GOARCH ||
		info.CLIContract != protocolcli.CurrentContract ||
		info.RuntimeProtocol != runtimeproto.MaxKnownProtocolVersion ||
		info.RuntimeABI != runtimeproto.RuntimeABIVersion {
		t.Fatalf("protocol descriptor = %+v", info)
	}
}

func TestHandleIgnoresNormalCommands(t *testing.T) {
	handled, err := Handle([]string{"build"}, &bytes.Buffer{}, "@putnami/test", "")
	if err != nil || handled {
		t.Fatalf("Handle() = (%v, %v), want ignored", handled, err)
	}
}

// installRuntime lays out an installed extension tree at root: a manifest
// that declares name at version with compiled/runtime as its runtime, and that
// runtime. It returns the runtime's path.
func installRuntime(t *testing.T, root, name, version string) string {
	t.Helper()
	executable := filepath.Join(root, filepath.FromSlash(pkgmeta.ExecutableName(runtime.GOOS, "compiled/runtime")))
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"name": name, "version": version,
		"runtime": map[string]string{"executable": "compiled/runtime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, manifestFilename), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	return executable
}

// The version a runtime reports is the one the manifest that declares it
// carries, so one runtime build serves every version its packager stamps.
func TestManifestVersionReadsTheManifestThatDeclaresTheRuntime(t *testing.T) {
	root := t.TempDir()
	executable := installRuntime(t, root, "@putnami/test", "1.2.3-20261004-abc")
	if got := ManifestVersion(executable, "@putnami/test"); got != "1.2.3-20261004-abc" {
		t.Fatalf("ManifestVersion = %q, want the manifest's version", got)
	}

	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(root, link); err == nil {
		through := filepath.Join(link, filepath.FromSlash(pkgmeta.ExecutableName(runtime.GOOS, "compiled/runtime")))
		if got := ManifestVersion(through, "@putnami/test"); got != "1.2.3-20261004-abc" {
			t.Errorf("ManifestVersion through a link = %q, want the manifest's version", got)
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
}

// A manifest that does not declare this executable, or declares another
// extension, gives the runtime no version; so does no manifest at all.
func TestManifestVersionRefusesAManifestThatDoesNotDeclareTheRuntime(t *testing.T) {
	t.Run("another extension", func(t *testing.T) {
		executable := installRuntime(t, t.TempDir(), "@putnami/other", "1.2.3")
		if got := ManifestVersion(executable, "@putnami/test"); got != "" {
			t.Errorf("ManifestVersion = %q, want empty", got)
		}
	})
	t.Run("another executable", func(t *testing.T) {
		root := t.TempDir()
		installRuntime(t, root, "@putnami/test", "1.2.3")
		prepared := filepath.Join(root, "prepared", "runtime")
		if err := os.MkdirAll(filepath.Dir(prepared), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prepared, []byte("runtime"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ManifestVersion(prepared, "@putnami/test"); got != "" {
			t.Errorf("ManifestVersion = %q, want empty for an executable the manifest does not declare", got)
		}
	})
	t.Run("unreadable manifest", func(t *testing.T) {
		root := t.TempDir()
		executable := installRuntime(t, root, "@putnami/test", "1.2.3")
		if err := os.WriteFile(filepath.Join(root, manifestFilename), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ManifestVersion(executable, "@putnami/test"); got != "" {
			t.Errorf("ManifestVersion = %q, want empty", got)
		}
	})
	t.Run("no manifest", func(t *testing.T) {
		executable := filepath.Join(t.TempDir(), "runtime")
		if err := os.WriteFile(executable, []byte("runtime"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ManifestVersion(executable, "@putnami/test"); got != "" {
			t.Errorf("ManifestVersion = %q, want empty", got)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		if got := ManifestVersion(filepath.Join(t.TempDir(), "missing"), "@putnami/test"); got != "" {
			t.Errorf("ManifestVersion = %q, want empty", got)
		}
	})
}

// HandleFromManifest answers the handshake with the running executable's
// manifest version: none for a test binary, which no manifest declares.
func TestHandleFromManifest(t *testing.T) {
	var out bytes.Buffer
	handled, err := HandleFromManifest([]string{Verb, Command}, &out, "@putnami/test")
	if err != nil || !handled {
		t.Fatalf("HandleFromManifest() = (%v, %v), want handled success", handled, err)
	}
	var info runtimeproto.Info
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Extension != "@putnami/test" || info.Version != "" {
		t.Fatalf("identity = %q@%q, want @putnami/test with no version", info.Extension, info.Version)
	}
	if handled, err := HandleFromManifest([]string{"build"}, &bytes.Buffer{}, "@putnami/test"); handled || err != nil {
		t.Fatalf("HandleFromManifest(build) = (%v, %v), want ignored", handled, err)
	}
}
