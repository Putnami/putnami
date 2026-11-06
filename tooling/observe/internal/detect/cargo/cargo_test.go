package cargo

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-rust"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func byPath(result wsproto.ProbeResult) map[string]wsproto.ProbeProject {
	projects := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		projects[p.Path] = p
	}
	return projects
}

func metadata(t *testing.T, p wsproto.ProbeProject) Metadata {
	t.Helper()
	m, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("metadata of %s: %v", p.Path, err)
	}
	return m
}

func paths(result wsproto.ProbeResult) []string {
	out := make([]string, 0, len(result.Projects))
	for _, p := range result.Projects {
		out = append(out, p.Path)
	}
	return out
}

func noDiagnostics(t *testing.T, result wsproto.ProbeResult) {
	t.Helper()
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", result.Diagnostics)
	}
}

// cargoWorkspace is a virtual Cargo workspace whose api and worker depend on
// pricing; tools/ is a package outside it.
var cargoWorkspace = map[string]string{
	"Cargo.toml": `[workspace]
resolver = "2"
members = ["crates/*"]
exclude = ["crates/legacy"]

[workspace.package]
version = "0.3.0"

[workspace.dependencies]
pricing = { path = "crates/pricing" }
serde = { version = "1", features = ["derive"] }
`,
	"Cargo.lock": "version = 4\n",
	"crates/api/Cargo.toml": `[package]
name = "api"
version.workspace = true
edition = "2021"

[dependencies]
pricing = { workspace = true }
serde.workspace = true
`,
	"crates/api/src/main.rs": "fn main() {}\n",
	"crates/worker/Cargo.toml": `[package]
name = "worker"
version = "0.1.0"

[dependencies]
anyhow = "1"

[dev-dependencies.pricing]
path = "../pricing"
`,
	"crates/pricing/Cargo.toml":           "[package]\nname = \"pricing\"\nversion = \"0.2.0\"\n",
	"crates/legacy/Cargo.toml":            "[package]\nname = \"legacy\"\nversion = \"0.1.0\"\n",
	"crates/notes.md":                     "not a member\n",
	"tools/Cargo.toml":                    "[package]\nname = \"tools\"\n\n[target.'cfg(unix)'.build-dependencies]\npricing = { path = \"../crates/pricing\", version = \"0.2\" }\n",
	"tools/Cargo.lock":                    "version = 4\n",
	"target/package/api-0.3.0/Cargo.toml": "[package]\nname = \"api\"\n",
	"vendor/serde/Cargo.toml":             "[package]\nname = \"serde\"\n",
	".cargo/registry/x/Cargo.toml":        "[package]\nname = \"x\"\n",
}

func TestCargoWorkspaceMembersAreWorkspaceComponents(t *testing.T) {
	spectest.Proves(t, feature, "rust-crates-are-components", "cargo-workspace-members-are-workspace-components")
	result := Detect(writeTree(t, cargoWorkspace))
	noDiagnostics(t, result)
	if got := paths(result); !reflect.DeepEqual(got, []string{"crates/api", "crates/legacy", "crates/pricing", "crates/worker", "tools"}) {
		t.Fatalf("components %v", got)
	}
	projects := byPath(result)
	for dir, source := range map[string]string{
		"crates/api": SourceWorkspace, "crates/pricing": SourceWorkspace, "crates/worker": SourceWorkspace,
		"crates/legacy": SourceManifest, "tools": SourceManifest,
	} {
		if got := metadata(t, projects[dir]).Source; got != source {
			t.Errorf("%s source %q, want %q", dir, got, source)
		}
	}
	api := projects["crates/api"]
	// version.workspace = true inherits [workspace.package] version.
	if api.SourceName != "api" || api.SourceFile != "crates/api/Cargo.toml" || api.Version != "0.3.0" {
		t.Errorf("api %+v", api)
	}
	if !reflect.DeepEqual(api.WatchedFiles, []string{"crates/api/Cargo.toml", "Cargo.lock"}) {
		t.Errorf("api watched files %v", api.WatchedFiles)
	}
	if got := metadata(t, projects["tools"]).Lockfile; got != "tools/Cargo.lock" {
		t.Errorf("tools lockfile %q", got)
	}
	if got := metadata(t, projects["crates/legacy"]).Lockfile; got != "" {
		t.Errorf("legacy lockfile %q", got)
	}
}

func TestARootPackageIsAWorkspaceMember(t *testing.T) {
	spectest.Proves(t, feature, "rust-crates-are-components", "cargo-workspace-members-are-workspace-components")
	result := Detect(writeTree(t, map[string]string{
		"Cargo.toml":          "[package]\nname = \"app\"\nversion = \"1.0.0\"\n\n[workspace]\nmembers = [\"lib/**\"]\n\n[dependencies]\nlib = { path = \"lib/core\" }\n",
		"Cargo.lock":          "",
		"lib/core/Cargo.toml": "[package]\nname = \"core\"\n",
	}))
	noDiagnostics(t, result)
	projects := byPath(result)
	if got := paths(result); !reflect.DeepEqual(got, []string{".", "lib/core"}) {
		t.Fatalf("components %v", got)
	}
	root := projects["."]
	if metadata(t, root).Source != SourceWorkspace || !reflect.DeepEqual(root.Dependencies, []string{"lib/core"}) || metadata(t, root).Lockfile != "Cargo.lock" {
		t.Errorf("root %+v", root)
	}
	if metadata(t, projects["lib/core"]).Source != SourceWorkspace {
		t.Errorf("core %+v", projects["lib/core"])
	}
}

func TestAPackageOutsideTheWorkspaceIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "rust-crates-are-components", "a-package-outside-the-workspace-is-a-manifest-component")
	result := Detect(writeTree(t, map[string]string{
		"a/Cargo.toml":       "[package]\nname = \"a\"\n\n[dependencies]\nb = { path = \"../b\" }\n",
		"b/Cargo.toml":       "[package]\nname = \"b\"\n",
		"virtual/Cargo.toml": "[workspace]\nmembers = []\n",
		"config/Cargo.toml":  "[profile.release]\nlto = true\n",
	}))
	noDiagnostics(t, result)
	if got := paths(result); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("components %v", got)
	}
	for _, p := range result.Projects {
		if metadata(t, p).Source != SourceManifest {
			t.Errorf("%s %+v", p.Path, p)
		}
	}
	if got := byPath(result)["a"].Dependencies; !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("a depends on %v", got)
	}
}

func TestAMalformedCargoTomlIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "rust-crates-are-components", "a-malformed-cargo-toml-is-a-diagnostic")
	result := Detect(writeTree(t, map[string]string{
		"Cargo.toml":        "[workspace]\nmembers = [\"a\", \"../outside\", \"b\"]\n",
		"a/Cargo.toml":      "[package\nname = \"a\"\n",
		"b/Cargo.toml":      "[package]\nname = \"b\"\n",
		"broken/Cargo.toml": "name = \"x\"\nname = \"y\"\n",
	}))
	if got := paths(result); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("components %v", got)
	}
	fields := make([]string, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		if d.Code != CodeManifestInvalid {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
		fields = append(fields, d.Field)
	}
	if !reflect.DeepEqual(fields, []string{"Cargo.toml", "a/Cargo.toml", "broken/Cargo.toml"}) {
		t.Errorf("diagnostics %v", result.Diagnostics)
	}
	// A root manifest that does not parse is reported once, with no workspace.
	result = Detect(writeTree(t, map[string]string{"Cargo.toml": "[workspace\n", "a/Cargo.toml": "[package]\nname = \"a\"\n"}))
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Field != "Cargo.toml" {
		t.Errorf("diagnostics %v", result.Diagnostics)
	}
	if got := paths(result); !reflect.DeepEqual(got, []string{"a"}) || metadata(t, result.Projects[0]).Source != SourceManifest {
		t.Errorf("components %v", result.Projects)
	}
}

func TestAPathDependencyIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "rust-crate-edges", "a-path-dependency-is-an-edge")
	projects := byPath(Detect(writeTree(t, cargoWorkspace)))
	// A dev-dependency table, and a build-dependency of a [target] platform.
	for _, dir := range []string{"crates/worker", "tools"} {
		if got := projects[dir].Dependencies; !reflect.DeepEqual(got, []string{"crates/pricing"}) {
			t.Errorf("%s depends on %v", dir, got)
		}
	}
}

func TestAWorkspaceDependencyWithAPathIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "rust-crate-edges", "a-workspace-dependency-with-a-path-is-an-edge")
	projects := byPath(Detect(writeTree(t, cargoWorkspace)))
	if got := projects["crates/api"].Dependencies; !reflect.DeepEqual(got, []string{"crates/pricing"}) {
		t.Errorf("api depends on %v", got)
	}
}

func TestARegistryDependencyIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "rust-crate-edges", "a-registry-dependency-is-no-edge")
	result := Detect(writeTree(t, map[string]string{
		"Cargo.toml": "[workspace]\nmembers = [\"a\", \"b\"]\n\n[workspace.dependencies]\nb = \"1\"\nlog = { version = \"0.4\" }\n",
		"a/Cargo.toml": `[package]
name = "a"

[dependencies]
b = { workspace = true }
log = { workspace = true }
regex = "1"
tokio = { git = "https://github.com/tokio-rs/tokio" }
outside = { path = "../../elsewhere" }
absolute = { path = "/opt/crate" }
nothing = { path = "missing" }
itself = { path = "." }
`,
		"b/Cargo.toml": "[package]\nname = \"b\"\n",
	}))
	noDiagnostics(t, result)
	for _, p := range result.Projects {
		if len(p.Dependencies) != 0 {
			t.Errorf("%s depends on %v", p.Path, p.Dependencies)
		}
	}
	// workspace = true outside a workspace names nothing.
	result = Detect(writeTree(t, map[string]string{
		"a/Cargo.toml": "[package]\nname = \"a\"\n\n[dependencies]\nb = { workspace = true }\n",
		"b/Cargo.toml": "[package]\nname = \"b\"\n",
	}))
	if got := byPath(result)["a"].Dependencies; len(got) != 0 {
		t.Errorf("a depends on %v", got)
	}
}

func TestTheCargoAnswerIsAValidProbeResult(t *testing.T) {
	spectest.Proves(t, feature, "rust-probe-answer", "the-cargo-answer-is-a-valid-probe-result")
	root := writeTree(t, cargoWorkspace)
	candidates, diags := Candidates(root)
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	var request bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&request, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName, Reason: wsproto.ProbeReasonLoad, Paths: candidates,
	}); err != nil {
		t.Fatal(err)
	}
	var answer bytes.Buffer
	handled, err := wsproto.ServeProbe(wsproto.ProbeControlArgs(), &request, &answer, Answer(root))
	if !handled || err != nil {
		t.Fatalf("served %v: %v", handled, err)
	}
	result, problems := wsproto.ParseAndValidateProbeResult(answer.Bytes())
	if diag.HasErrors(problems) {
		t.Fatalf("the answer does not validate: %v\n%s", problems, answer.Bytes())
	}
	if !reflect.DeepEqual(byPath(*result)["crates/api"].Dependencies, []string{"crates/pricing"}) {
		t.Fatalf("the answer lost the edge: %s", answer.Bytes())
	}
	if strings.Contains(answer.String(), root) {
		t.Errorf("the answer names the checkout directory: %s", answer.Bytes())
	}
}

func TestANextestCaseGoesToTheCrateItsBinaryNames(t *testing.T) {
	spectest.Proves(t, feature, "rust-report-discovery", "a-nextest-case-goes-to-the-crate-its-binary-names")
	result := Detect(writeTree(t, map[string]string{
		"Cargo.toml":               "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/api/Cargo.toml":    "[package]\nname = \"api-server\"\n",
		"crates/core/Cargo.toml":   "[package]\nname = \"core\"\n",
		"examples/core/Cargo.toml": "[package]\nname = \"core\"\n",
	}))
	locate := Locator(result)
	for id, want := range map[string]string{
		"api-server":              "crates/api",
		"api_server::bin/api":     "crates/api",
		"api-server::integration": "crates/api",
		"core":                    "crates/core",
		"core::example/demo":      "crates/core",
	} {
		if file, component, ok := locate(id); !ok || component != want || file != want+"/Cargo.toml" {
			t.Errorf("locate(%q) = %q %q %v, want %s", id, file, component, ok, want)
		}
	}
	for _, id := range []string{"", "worker", "tests.test_api.TestLogin", "github.com/acme/core", "core::"} {
		if _, _, ok := locate(id); ok {
			t.Errorf("locate(%q) found a crate", id)
		}
	}
}
