package clientgen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// D0.4 settles that this extension DEPENDS on go.putnami.dev/api at module
// level so bin/prepare can build the framework's own emitter and package it
// beside the runtime — and that the emitter is never LINKED into that runtime.
// The two halves are one decision: a linked emitter would make the extension a
// second copy of the framework's generator, and a missing module dependency
// would make it resolve source from the consumer's checkout instead.
//
// The dependency therefore lives under the `tools` build tag alone. These tests
// hold both halves.

// TestNoCompiledPackageLinksTheFrameworkEmitter walks the real dependency graph
// of everything that ships in the runtime binary. A single ordinary import of
// go.putnami.dev/api anywhere under cmd/ or internal/ fails it.
func TestNoCompiledPackageLinksTheFrameworkEmitter(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "the-framework-emitter-is-packaged-not-linked")
	goBinary := resolveGoBinary(t)
	command := exec.Command(goBinary, "list", "-deps", "./cmd/...", "./internal/...") //nolint:gosec // resolved Go toolchain
	command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list compiled dependencies: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		packagePath := strings.TrimSpace(line)
		if packagePath == "go.putnami.dev/api" || strings.HasPrefix(packagePath, "go.putnami.dev/api/") {
			t.Errorf("compiled package %q is linked into the extension runtime; the Go emitter is packaged as a "+
				"neighboring binary and must stay a tools-only module dependency", packagePath)
		}
	}
}

// TestToolsDependencyIsTagOnly pins the mechanism that keeps the module
// requirement alive without linking it: a blank import behind //go:build tools.
// Without the file, `go mod tidy` drops go.putnami.dev/api and bin/prepare
// stops being able to build the emitter at all.
func TestToolsDependencyIsTagOnly(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "the-framework-emitter-is-packaged-not-linked")
	data, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatalf("read tools.go: %v", err)
	}
	source := string(data)
	if !strings.Contains(source, "//go:build tools") {
		t.Error("tools.go is not behind the tools build tag, so its import is compiled into the runtime")
	}
	if !strings.Contains(source, `_ "go.putnami.dev/api"`) {
		t.Error("tools.go does not pin go.putnami.dev/api; go mod tidy will drop the emitter bin/prepare builds")
	}
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), "go.putnami.dev/api v") {
		t.Error("go.mod does not require go.putnami.dev/api")
	}
}

// TestPrepareProducesBothPackagedEmitters runs the real preparation script. A
// consumer workspace that receives only the runtime fails Go generation at the
// first provider with "resolve packaged Go client emitter", far from here — so
// the presence of BOTH executables is asserted where the script that produces
// them lives.
func TestPrepareProducesBothPackagedEmitters(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "preparation-packages-the-runtime-and-the-go-emitter")
	if runtime.GOOS == "windows" {
		t.Skip("bin/prepare is a POSIX shell script")
	}
	if testing.Short() {
		t.Skip("preparation compiles the framework emitter")
	}
	goBinary := resolveGoBinary(t)
	output := t.TempDir()
	command := exec.Command("./bin/prepare", "--output", output) //nolint:gosec // the extension's own committed script
	command.Env = append(os.Environ(), "GOROOT="+goRoot(t, goBinary), "GOWORK=off")
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare the extension runtime: %v\n%s", err, combined)
	}
	for _, name := range []string{"putnami-clientgen", "putnami-client-generate-go"} {
		path := filepath.Join(output, "compiled", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("prepare did not produce %s: %v", name, err)
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not an executable regular file (mode %s)", name, info.Mode())
		}
	}
}

func resolveGoBinary(t *testing.T) string {
	t.Helper()
	if root := strings.TrimSpace(os.Getenv("GOROOT")); root != "" {
		candidate := filepath.Join(root, "bin", "go")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	path, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no Go toolchain is reachable: %v", err)
	}
	return path
}

func goRoot(t *testing.T, goBinary string) string {
	t.Helper()
	command := exec.Command(goBinary, "env", "GOROOT") //nolint:gosec // resolved Go toolchain
	output, err := command.Output()
	if err != nil {
		t.Fatalf("resolve GOROOT: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// TestPublishedArchiveDeclaresTheGoEmitter holds the archive half of the same
// decision. bin/prepare serves a source checkout; a consumer installs the
// published platform archive, which carries only what the Go packager
// cross-compiles and stages from options["@putnami/go"].executables. The
// emitter must be declared there, under the exact name the runtime resolves
// beside itself and built from the package bin/prepare builds, or every
// consumer's clientgen~generate-go fails with "resolve packaged Go client
// emitter".
func TestPublishedArchiveDeclaresTheGoEmitter(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "the-published-archive-declares-the-go-emitter")
	data, err := os.ReadFile("putnami.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Options struct {
			Go struct {
				Executables []struct {
					Name    string `json:"name"`
					Package string `json:"package"`
				} `json:"executables"`
			} `json:"@putnami/go"`
			Package struct {
				Archives bool `json:"archives"`
			} `json:"package"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse putnami.json: %v", err)
	}
	if !config.Options.Package.Archives {
		t.Fatal("clientgen no longer packages platform archives; revisit how consumers receive the Go emitter")
	}
	prepare, err := os.ReadFile(filepath.Join("bin", "prepare"))
	if err != nil {
		t.Fatal(err)
	}
	const name, pkg = "putnami-client-generate-go", "go.putnami.dev/api/cmd/clientgen"
	if !strings.Contains(string(prepare), pkg) || !strings.Contains(string(prepare), "compiled/"+name) {
		t.Fatalf("bin/prepare no longer builds %s as compiled/%s; keep the archive declaration in step", pkg, name)
	}
	for _, e := range config.Options.Go.Executables {
		if e.Name == name {
			if e.Package != pkg {
				t.Errorf("%s is built from %q, want %q", name, e.Package, pkg)
			}
			return
		}
	}
	t.Errorf(`putnami.json options["@putnami/go"].executables does not declare %s, so the published archive ships the runtime without the Go emitter`, name)
}
