//go:build !windows

package rust

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

func TestStampManifestWritesTheVersionAndPinsPathDependencies(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	input := `# a comment stays
[package]
name = "app"
version = "0.1.0" # stated here
edition = "2021"

[dependencies]
serde = "1"
pricing = { path = "../pricing" }
billing = { path = "../billing", version = "0.1", features = ["path"] }
inherited = { workspace = true }

[dependencies.nested]
path = "../nested"
default-features = false

[dependencies.pinned]
version = "1"
path = "../pinned"

[dependencies.remote]
version = "2"

[dev-dependencies]
helper = { path = "../helper" }

[build-dependencies]
codegen = { path = "../codegen" }

[target.'cfg(unix)'.dependencies]
unix = { path = "../unix" }

[workspace.package]
version = "0.2.0"

[workspace.dependencies]
shared = { path = "crates/shared" }
`
	const v = "1.2.3-b"
	got := stampManifest(input, v)
	for _, want := range []string{
		"# a comment stays",
		`version = "` + v + `" # stated here`,
		`serde = "1"`,
		`pricing = { version = "=` + v + `", path = "../pricing" }`,
		`billing = { path = "../billing", version = "=` + v + `", features = ["path"] }`,
		`inherited = { workspace = true }`,
		"[dependencies.nested]\nversion = \"=" + v + "\"\npath = \"../nested\"",
		"[dependencies.pinned]\nversion = \"=" + v + "\"\npath",
		"[dependencies.remote]\nversion = \"2\"",
		`helper = { path = "../helper" }`,
		`codegen = { version = "=` + v + `", path = "../codegen" }`,
		`unix = { version = "=` + v + `", path = "../unix" }`,
		"[workspace.package]\nversion = \"" + v + "\"",
		`shared = { version = "=` + v + `", path = "crates/shared" }`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the manifest misses %q:\n%s", want, got)
		}
	}
	forms := stampManifest(`[package]
name = "a"
version = { workspace = true }

[dependencies.serde]
version = "1"

[[bin]]
name = "a"

[dependencies]
quoted = { path = '../q', version = '1' }
b.path = "../b"
a.path = "../a"
c.path = "../c"
a.version = "1"
c.version = '1'
d.version = "1"
`, v)
	for _, want := range []string{
		"version = { workspace = true }",
		"[dependencies.serde]\nversion = \"1\"",
		`quoted = { path = '../q', version = "=` + v + `" }`,
		"b.path = \"../b\"\nb.version = \"=" + v + "\"",
		"a.version = \"=" + v + "\"",
		"c.version = \"=" + v + "\"",
		"d.version = \"1\"",
	} {
		if !strings.Contains(forms, want) {
			t.Errorf("the manifest misses %q:\n%s", want, forms)
		}
	}
	if unversioned := stampManifest("[package]\nname = \"a\"\n", v); !strings.Contains(unversioned, "[package]\nversion = \""+v+"\"\nname") {
		t.Errorf("a package with no version: %s", unversioned)
	}
	inherited := "[package]\nname = \"a\"\nversion.workspace = true\n"
	if stamped := stampManifest(inherited, v); stamped != inherited {
		t.Errorf("an inherited version changed: %s", stamped)
	}
}

func runCrate(t *testing.T, task string, ctx *pctx.Context) (string, error) {
	t.Helper()
	status, _, err := Jobs(stack(t))[task](ctx, jsonl.New(), nil)
	return status, err
}

func TestACargoWorkspaceWithNoCrateStagesAndPublishesNothing(t *testing.T) {
	root := fixture(t)
	writeFile(t, root, "services/billing/Cargo.toml", "[workspace]\nmembers = [\"http\"]\n")
	writeFile(t, root, "services/billing/http/Cargo.toml", "[package]\nname = \"billing-http\"\nversion = \"0.1.0\"\n")
	writeFile(t, root, "services/billing/http/src/lib.rs", "")
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == "services/billing" {
			meta = p.Metadata
		}
	}
	if meta == nil {
		t.Fatal("the probe answers no project for the Cargo workspace")
	}
	// No cargo on PATH: a job that selected a toolchain would fail.
	t.Setenv("PATH", t.TempDir())
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: "services/billing", Path: "services/billing", FullPath: filepath.Join(root, "services/billing"),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	for _, task := range []string{"package-crate", "publish-crates"} {
		if status, err := runCrate(t, task, ctx); status != "OK" || err != nil {
			t.Errorf("%s: %s %v", task, status, err)
		}
	}
}

func TestPublishSendsACrateAtTheReleaseVersion(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	writeFile(t, root, "tools/gen/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(root, "tools/gen/run.sh"), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	if err := os.Symlink("run.sh", filepath.Join(root, "tools/gen/link.sh")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "tools/gen/target/stale", "x")
	writeFile(t, root, "tools/gen/nested/target/stale", "x")
	writeFile(t, root, "tools/gen/.git/config", "x")
	before, err := os.ReadFile(filepath.Join(root, "tools/gen/Cargo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	log := stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"crates": true}))
	if status, err := runCrate(t, "package-crate", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	staged := filepath.Join(stagedWorkspace(ctx), "tools/gen")
	manifest, err := os.ReadFile(filepath.Join(staged, "Cargo.toml")) //nolint:gosec // the test's own file
	if err != nil || !strings.Contains(string(manifest), `version = "`+testVersion+`"`) {
		t.Errorf("staged manifest: %s, %v", manifest, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "tools/gen/Cargo.toml")); string(after) != string(before) {
		t.Error("the checkout changed")
	}
	for _, gone := range []string{"target", "nested/target", ".git"} {
		if _, err := os.Stat(filepath.Join(staged, gone)); err == nil {
			t.Errorf("the copy holds %s", gone)
		}
	}
	if info, err := os.Stat(filepath.Join(staged, "run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh: %v, %v", info, err)
	}
	if link, err := os.Readlink(filepath.Join(staged, "link.sh")); err != nil || link != "run.sh" {
		t.Errorf("link: %s, %v", link, err)
	}
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	logged, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	for _, want := range []string{staged + ": cargo update --workspace", staged + ": cargo publish --locked --allow-dirty --package gen"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the log misses %q:\n%s", want, logged)
		}
	}
}

func TestPackageStagesAPathDependencyOutsideTheStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	writeFile(t, root, "tools/gen/Cargo.toml", "[package]\nname = \"gen\"\nversion = \"0.1.0\"\n\n[dependencies]\nsibling = { path = \"../sibling\" }\n")
	writeFile(t, root, "tools/sibling/Cargo.toml", "[package]\nname = \"sibling\"\nversion = \"0.1.0\"\n\n[dev-dependencies]\nfar = { path = \"../far\" }\n")
	writeFile(t, root, "tools/sibling/src/lib.rs", "")
	writeFile(t, root, "tools/far/Cargo.toml", "[package]\nname = \"far\"\nversion = \"0.1.0\"\n")
	stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, nil)
	if status, err := runCrate(t, "package-crate", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	for _, rel := range []string{"tools/sibling/src/lib.rs", "tools/far/Cargo.toml"} {
		if _, err := os.Stat(filepath.Join(stagedWorkspace(ctx), rel)); err != nil {
			t.Errorf("the copy misses %s: %v", rel, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(stagedWorkspace(ctx), "tools/sibling/Cargo.toml")) //nolint:gosec // the test's own file
	if !strings.Contains(string(data), `version = "`+testVersion+`"`) {
		t.Errorf("the sibling keeps its version: %s", data)
	}
	writeFile(t, root, "tools/gen/Cargo.toml", "[package]\nname = \"gen\"\nversion = \"0.1.0\"\n\n[dependencies]\nout = { path = \"../../../out\" }\n")
	if status, err := runCrate(t, "package-crate", ctx); err == nil || status != "FAILED" || !strings.Contains(err.Error(), "outside the Putnami workspace") {
		t.Fatalf("status = %s, err = %v", status, err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	all := append([]string{"-c", "user.email=a@example.test", "-c", "user.name=a", "-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", all...) //nolint:gosec // fixed arguments
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestACorruptGitIndexStopsTheStagedCopy(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	writeFile(t, root, ".gitignore", "*.pem\n")
	writeFile(t, root, "tools/gen/server.pem", "private")
	git(t, root, "init", "-q")
	git(t, root, "add", ".gitignore")
	writeFile(t, root, ".git/index", "not an index")
	stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, nil)
	if status, err := runCrate(t, "package-crate", ctx); err == nil || status != "FAILED" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(stagedWorkspace(ctx), "tools/gen/server.pem")); err == nil {
		t.Error("the copy holds server.pem")
	}
}

func TestTheStagedCopyHoldsTheFilesOfASubmodule(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	library := t.TempDir()
	writeFile(t, library, "foo.c", "int foo;\n")
	git(t, library, "init", "-q")
	git(t, library, "add", ".")
	git(t, library, "commit", "-q", "-m", "c")
	root := fixture(t)
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "submodule", "add", "-q", library, "tools/gen/libfoo")
	stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, nil)
	if status, err := runCrate(t, "package-crate", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(stagedWorkspace(ctx), "tools/gen/libfoo/foo.c")); err != nil {
		t.Errorf("the copy misses the submodule's file: %v", err)
	}
}

func TestPathDependenciesReadPatchAndReplace(t *testing.T) {
	got := pathDependencies(`[dependencies]
a = { path = "../a" }

[patch.crates-io]
b = { path = "../b" }

[replace]
"c:1.0.0" = { path = "../c" }

[features]
d = { path = "../d" }
`)
	if want := []string{"../a", "../b", "../c"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTheStagedCopyHoldsNoFileGitIgnores(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	writeFile(t, root, ".gitignore", "secrets/\n*.pem\n")
	writeFile(t, root, "tools/gen/secrets/key.txt", "private")
	writeFile(t, root, "tools/gen/server.pem", "private")
	writeFile(t, root, "tools/gen/src/added.rs", "// not committed, not ignored\n")
	writeFile(t, root, "tools/gen/tracked/target/kept", "skipped")
	for _, args := range [][]string{{"init", "-q"}, {"add", "tools/gen/Cargo.toml", "tools/gen/src/main.rs", ".gitignore"}} {
		cmd := exec.Command("git", args...) //nolint:gosec // fixed arguments
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.Remove(filepath.Join(root, "tools/gen/src/main.rs")); err != nil {
		t.Fatal(err)
	}
	stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, nil)
	if status, err := runCrate(t, "package-crate", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	staged := filepath.Join(stagedWorkspace(ctx), "tools/gen")
	for _, gone := range []string{"secrets/key.txt", "server.pem", "src/main.rs", "tracked/target/kept"} {
		if _, err := os.Stat(filepath.Join(staged, gone)); err == nil {
			t.Errorf("the copy holds %s", gone)
		}
	}
	for _, kept := range []string{"Cargo.toml", "src/added.rs"} {
		if _, err := os.Stat(filepath.Join(staged, kept)); err != nil {
			t.Errorf("the copy misses %s: %v", kept, err)
		}
	}
}

func TestPublishReusesAVersionTheRegistryHolds(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, nil)
	if _, err := runCrate(t, "package-crate", ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_PUBLISH_ERROR", "error: crate version `1.2.3` is already uploaded")
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	t.Setenv("STUB_PUBLISH_ERROR", "error: crate gen@1.2.3 already exists on crates.io index")
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	t.Setenv("STUB_PUBLISH_ERROR", "error: could not compile: the file already exists")
	if status, err := runCrate(t, "publish-crates", ctx); err == nil || status != "FAILED" {
		t.Fatalf("a compile error counted as reuse: status = %s, err = %v", status, err)
	}
	t.Setenv("STUB_PUBLISH_ERROR", "error: 401 unauthorized")
	if status, err := runCrate(t, "publish-crates", ctx); err == nil || status != "FAILED" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
}

func TestADryRunNamesTheCrateAndItsPathDependencies(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"dry-run": true, "crates-registry": "corp"}))
	if _, err := runCrate(t, "package-crate", ctx); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"packages": []map[string]any{
		{"name": "gen", "dependencies": []map[string]any{
			{"name": "zeta", "kind": nil, "path": "/z"},
			{"name": "beta", "kind": "build", "path": "/b"},
			{"name": "helper", "kind": "dev", "path": "/h"},
			{"name": "serde", "kind": nil},
		}},
		{"name": "zeta", "dependencies": []map[string]any{{"name": "deep", "kind": nil, "path": "/d"}, {"name": "gen", "kind": nil, "path": "/g"}}},
		{"name": "beta", "dependencies": []map[string]any{}},
		{"name": "deep", "dependencies": []map[string]any{}},
	}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, stagedWorkspace(ctx), "tools/gen/.stub-metadata.json", string(raw))
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	logged, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	want := "cargo publish --locked --allow-dirty --registry corp --dry-run --package gen --package beta --package deep --package zeta"
	if !strings.Contains(string(logged), want) {
		t.Errorf("the log misses %q:\n%s", want, logged)
	}
	writeFile(t, stagedWorkspace(ctx), "tools/gen/.stub-metadata.json", "nope")
	if status, err := runCrate(t, "publish-crates", ctx); err == nil || status != "FAILED" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
}

func TestADryRunChecksTheFilesOfACrateWithAPathDependencyOutsideItsWorkspace(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"dry-run": true}))
	if _, err := runCrate(t, "package-crate", ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stagedWorkspace(ctx), "tools/gen/.stub-metadata.json", `{"packages":[{"name":"gen","dependencies":[{"name":"sibling","kind":null,"path":"/s"}]}]}`)
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	logged, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	if !strings.Contains(string(logged), "cargo package --list --allow-dirty --package gen") || strings.Contains(string(logged), "cargo publish") {
		t.Errorf("the log: %s", logged)
	}
}

func TestADryRunOfSeveralCratesInstallsTheRustThatAcceptsThem(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"dry-run": true}))
	if _, err := runCrate(t, "package-crate", ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stagedWorkspace(ctx), "tools/gen/.stub-metadata.json", `{"packages":[{"name":"gen","dependencies":[{"name":"dep","kind":null,"path":"/d"}]},{"name":"dep","dependencies":[]}]}`)
	rustup, _ := stubRustup(t, root, defaultChannel)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runCrate(t, "publish-crates", ctx); err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	if logged, _ := os.ReadFile(rustup); !strings.Contains(string(logged), "toolchain install "+defaultChannel) { //nolint:gosec // the test's own log
		t.Errorf("rustup ran %q", logged)
	}
	if logged, _ := os.ReadFile(log); strings.Contains(string(logged), "cargo publish") { //nolint:gosec // the test's own log
		t.Errorf("the machine's cargo published: %s", logged)
	}
}

func TestPublishRefusesACrateThatIsNotStaged(t *testing.T) {
	spectest.Proves(t, feature, "publish-sends-the-staged-crate", "publish-sends-a-crate-at-the-release-version")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	if status, err := runCrate(t, "publish-crates", packageContext(t, root, nil)); err == nil || status != "FAILED" || !strings.Contains(err.Error(), "run putnami package --crates") {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	ctx := packageContext(t, root, nil)
	ctx.Version = nil
	if status, err := runCrate(t, "package-crate", ctx); err == nil || status != "FAILED" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
}

func TestPublishPushesTheImageThroughTheSharedPublisher(t *testing.T) {
	spectest.Proves(t, feature, "publish-pushes-the-image", "publish-pushes-the-image-through-the-shared-publisher")
	root := fixture(t)
	writeBinaries(t, root, "linux-x64")
	base, host := baseImage(t)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"docker": true, "docker-base-image": base}))
	if _, _, err := packageDocker(ctx, jsonl.New(), nil); err != nil {
		t.Fatal(err)
	}
	status, _, err := publishDocker(ctx, jsonl.New(), []string{"--docker-registry", host})
	if err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	repo, err := name.NewRepository(host + "/gen")
	if err != nil {
		t.Fatal(err)
	}
	tags, err := remote.List(repo)
	if err != nil || len(tags) == 0 {
		t.Fatalf("tags = %v, err = %v", tags, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/tools/gen/publish/docker/published-image.json")); err != nil {
		t.Errorf("no verified record: %v", err)
	}
	ctx.Project.Type = typeLibrary
	if status, _, err := publishDocker(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Errorf("a library: %s, %v", status, err)
	}
	if index, err := pkgmeta.ReadChannelIndex(root, "tools/gen"); err != nil || !index.HasChannel(channelDocker) {
		t.Errorf("index = %+v, %v", index, err)
	}
}

func TestAProjectNamedWithUppercaseLettersPublishesALowercaseImage(t *testing.T) {
	root := fixture(t)
	writeBinaries(t, root, "linux-x64")
	base, host := baseImage(t)
	ctx := packageContext(t, root, rawParams(t, map[string]any{"docker": true, "archives": true, "platforms": "linux/amd64", "docker-base-image": base}))
	ctx.Project.Name = "@Acme/Gen"
	if _, _, err := packageDocker(ctx, jsonl.New(), nil); err != nil {
		t.Fatal(err)
	}
	manifest, err := pkgmeta.ReadDockerManifest(root, "tools/gen")
	if err != nil || manifest.Image != "acme-gen" || !strings.HasPrefix(manifest.Tags[0], "acme-gen:") {
		t.Fatalf("manifest = %+v, %v", manifest, err)
	}
	// The shared publisher builds the image's reference from the packaged name:
	// an uppercase letter would make it invalid.
	status, _, err := publishDocker(ctx, jsonl.New(), []string{"--docker-registry", host + "/team"})
	if err != nil || status != "OK" {
		t.Fatalf("status = %s, err = %v", status, err)
	}
	repo, err := name.NewRepository(host + "/team/acme-gen")
	if err != nil {
		t.Fatal(err)
	}
	if tags, err := remote.List(repo); err != nil || len(tags) == 0 {
		t.Fatalf("tags = %v, err = %v", tags, err)
	}
	// An archive is a file name, not a reference: it keeps the project's case.
	if _, _, err := packageArchives(ctx, jsonl.New(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pkgmeta.PackageOutputDir(root, "tools/gen", channelArchives), "Acme-Gen-linux-x64.tar.gz")); err != nil {
		t.Errorf("archive: %v", err)
	}
}
