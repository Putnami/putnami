package swift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const serverManifest = `// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Server",
    // .executable(name: "Commented", targets: ["Commented"]),
    dependencies: [.package(path: "../Pricing")],
    targets: [
        .executableTarget(name: "Server", dependencies: ["Pricing"]),
        .testTarget(name: "ServerTests", dependencies: ["Server"]),
    ]
)
`

func TestAnExecutablePackageServesAndRuns(t *testing.T) {
	spectest.Proves(t, feature, "applications-serve-and-run", "an-executable-package-serves-and-runs")
	root := fixture(t)
	stackstest.WriteFile(t, root, "Packages/Server/Package.swift", serverManifest)
	stackstest.WriteFile(t, root, "Packages/Server/Sources/Server/main.swift", "print(\"up\")\n")
	s := stubTools(t)
	st := stack(t)
	types := map[string]string{}
	for _, p := range probeWith(t, root, "Packages/Server").Projects {
		types[p.Path] = p.Type
	}
	if !reflect.DeepEqual(types, map[string]string{"App": "", "Packages/Networking": "library", "Packages/Pricing": "library", "Packages/Server": "application"}) {
		t.Errorf("types = %v", types)
	}

	server := filepath.Join(root, "Packages/Server")
	status, _, _, err := runJobWith(t, root, "Packages/Server", "serve", ServeJob(st), nil)
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	status, _, _, err = runJobWith(t, root, "Packages/Server", "serve", ServeJob(st), job.Params{"port": json.RawMessage(`8081`), "args": json.RawMessage(`"--verbose"`)})
	if status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	ports, _ := os.ReadFile(filepath.Join(s.dir, "run.log"))
	if string(ports) != "PORT=3000\nPORT=8081\n" || !strings.Contains(s.ran(t), server+": swift run --force-resolved-versions Server\n") || !strings.Contains(s.ran(t), server+": swift run --force-resolved-versions Server -- --verbose\n") {
		t.Errorf("serve ran %q, %s", ports, s.ran(t))
	}

	t.Setenv("STUB_RUN_EXIT", "4")
	status, data, _, err := runJobWith(t, root, "Packages/Server", "run", RunJob(st), job.Params{"args": json.RawMessage(`"migrate --dry-run"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 4 || !strings.Contains(s.ran(t), server+": swift run --force-resolved-versions Server -- migrate --dry-run\n") {
		t.Errorf("run forwards the exit code: %s %v %v %s", status, err, data, s.ran(t))
	}
	if status, _, _, _ := runJobWith(t, root, "Packages/Server", "serve", ServeJob(st), nil); status != "FAILED" {
		t.Errorf("a server that exits on its own fails serve: %s", status)
	}
	t.Setenv("STUB_RUN_EXIT", "0")

	// A package with several executables names one with --entrypoint.
	stackstest.WriteFile(t, root, "Packages/Server/Package.swift", strings.Replace(serverManifest, "    targets: [\n",
		"    products: [.executable(name: \"admin\", targets: [\"Admin\"]), .executable(name: \"Server\", targets: [\"Server\"])],\n    targets: [\n", 1))
	if status, _, _, err := runJobWith(t, root, "Packages/Server", "run", RunJob(st), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodeNoExecutable) {
		t.Errorf("several executables need --entrypoint: %s %v", status, err)
	}
	if status, _, _, err := runJobWith(t, root, "Packages/Server", "run", RunJob(st), job.Params{"entrypoint": json.RawMessage(`"admin"`)}); status != "OK" || err != nil || !strings.Contains(s.ran(t), "swift run --force-resolved-versions admin\n") {
		t.Errorf("--entrypoint names the product: %s %v", status, err)
	}

	for _, rel := range []string{"App", "Packages/Pricing"} {
		if status, _, _, err := runJobWith(t, root, rel, "run", RunJob(st), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodeNoExecutable) {
			t.Errorf("%s has nothing to run: %s %v", rel, status, err)
		}
	}
}

func probeWith(t *testing.T, root string, extra ...string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	paths := append(append([]string{}, candidates...), extra...)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestExecutableProductsReadsProductsAndTargets(t *testing.T) {
	got := ExecutableProducts([]byte(`/* .executable(name: "Gone", targets: []) */
products: [.executable(name: "tool", targets: ["Tool"]), .library(name: "Lib", targets: ["Lib"])],
targets: [.executableTarget(name: "Tool"), .executableTarget(name: "server")]`))
	if !reflect.DeepEqual(got, []string{"tool"}) {
		t.Errorf("a product names the executable its target builds: %v", got)
	}
	if got := ExecutableProducts([]byte(`targets: [.executableTarget(name: "server"), .executableTarget(name: "Tool")]`)); !reflect.DeepEqual(got, []string{"Tool", "server"}) {
		t.Errorf("with no product, the executable targets: %v", got)
	}
}
