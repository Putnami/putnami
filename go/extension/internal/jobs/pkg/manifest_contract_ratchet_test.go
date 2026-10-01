package pkg

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/agentartifact"
)

// A later change ratcheted the package-time gate to the contract-3
// world B6c created. Before B6c the loader adapted what it could, so "the gate
// validated it" and "a consumer can load it" were close enough to be the same
// sentence. They are not any more: a manifest the loader rejects is an extension
// that silently vanishes from every consumer's CLI. These tests pin the gate's
// postcondition — WHAT WE PUBLISH LOADS — on both of its arms, plus the
// direction the ratchet may not turn.

// The real first-party manifests are read through findRepoRoot
// (gomodule_embed_test.go), which walks up to the workspace root and skips the
// test when this module is built outside the repository.

// stagedContract reads the cliContract of a staged manifest. A manifest with no
// field reports 0, the pre-registry contract.
func stagedContract(t *testing.T, stageDir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stageDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	var staged struct {
		CLIContract int `json:"cliContract"`
	}
	if err := json.Unmarshal(data, &staged); err != nil {
		t.Fatalf("decode staged manifest: %v", err)
	}
	return staged.CLIContract
}

// TestGateAndStampManifestContract_StagedOutputLoadsUnderTheCurrentLoader is the
// round trip the packaging gate asks for: gate → stamp → LoadManifest. The gate
// validating a manifest is a statement about the document; this is the statement
// about the ARTIFACT, made through the very function a consumer's CLI calls.
func TestGateAndStampManifestContract_StagedOutputLoadsUnderTheCurrentLoader(t *testing.T) {
	dir := stageManifest(t, conformingManifest)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("gate should pass a conforming manifest: %v", err)
	}

	loaded, err := proto.LoadManifest(filepath.Join(dir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("the packaged manifest does not load under this build's loader: %v", err)
	}
	if loaded.CLIContract != protocolcli.CurrentContract {
		t.Errorf("loaded cliContract = %d, want %d", loaded.CLIContract, protocolcli.CurrentContract)
	}
	if !proto.DeclaresContractSurface(loaded) {
		t.Error("the gated manifest lost its contract surface in packaging")
	}
}

// TestGateAndStampManifestContract_RatchetsAStaleLowerClaim pins the direction
// the ratchet DOES turn. An author's manifest checked into a repo months ago
// still says 2; the stamp is earned by the validation this job just ran, so the
// staged copy says 3 and loads. Refusing here instead would make every extension
// author edit a field the packager owns.
func TestGateAndStampManifestContract_RatchetsAStaleLowerClaim(t *testing.T) {
	if protocolcli.CurrentContract < 2 {
		t.Skip("no lower contract to ratchet from")
	}
	stale := strings.Replace(conformingManifest,
		`"name": "@putnami/testext",`,
		fmt.Sprintf(`"name": "@putnami/testext",
	"cliContract": %d,`, protocolcli.CurrentContract-1), 1)

	dir := stageManifest(t, stale)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("a stale lower claim must be ratcheted up, not rejected: %v", err)
	}
	if got := stagedContract(t, dir); got != protocolcli.CurrentContract {
		t.Errorf("staged cliContract = %d, want %d", got, protocolcli.CurrentContract)
	}
	if _, err := proto.LoadManifest(filepath.Join(dir, "putnami.extension.json")); err != nil {
		t.Errorf("the ratcheted manifest does not load: %v", err)
	}
}

// TestGateAndStampManifestContract_RejectsFutureContractClaim pins the direction
// it does NOT turn. A packager cannot run the checks of a contract it does not
// implement, so silently rewriting a claim above the latest contract it reads
// down to its own would publish an artifact asserting a compliance nobody
// verified — and the loader, seeing a number it reads, would accept it. The
// remedy is upgrading putnami.
func TestGateAndStampManifestContract_RejectsFutureContractClaim(t *testing.T) {
	future := strings.Replace(conformingManifest,
		`"name": "@putnami/testext",`,
		fmt.Sprintf(`"name": "@putnami/testext",
	"cliContract": %d,`, protocolcli.LatestContract+1), 1)

	dir := stageManifest(t, future)
	err := gateAndStampManifestContract(dir)
	if err == nil {
		t.Fatal("a manifest claiming a future contract must not be packaged with a downgraded stamp")
	}
	for _, want := range []string{
		fmt.Sprintf("declares CLI contract %d", protocolcli.LatestContract+1),
		fmt.Sprintf("implements %d", protocolcli.LatestContract),
		"upgrade putnami",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gate error missing %q: %v", want, err)
		}
	}
	if got := stagedContract(t, dir); got != protocolcli.LatestContract+1 {
		t.Errorf("a rejected manifest must be left as staged; cliContract = %d, want %d",
			got, protocolcli.LatestContract+1)
	}
}

// TestGateAndStampManifestContract_RejectsUnloadableHookOnlyManifest closes the
// hole the hook-only bypass left open. `"hooks": []` parses as raw JSON, names
// no command, group or tool, and so skipped the gate entirely — and then failed
// to unmarshal in every consumer, which drops the whole extension. The bypass
// is about what the CONTRACT governs, not about whether the file is usable.
func TestGateAndStampManifestContract_RejectsUnloadableHookOnlyManifest(t *testing.T) {
	for name, manifest := range map[string]string{
		"hooks of the wrong type": `{"commands": {}, "hooks": []}`,
		"tasks of the wrong type": `{"commands": {}, "tasks": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := stageManifest(t, manifest)
			err := gateAndStampManifestContract(dir)
			if err == nil {
				t.Fatal("a hook-only manifest no consumer can load must fail packaging")
			}
			if !strings.Contains(err.Error(), "cannot be published") {
				t.Errorf("error should say the manifest cannot be published, got %v", err)
			}
			if !strings.Contains(err.Error(), "every consumer would skip the extension") {
				t.Errorf("error should name the consumer-side consequence, got %v", err)
			}
		})
	}
}

// TestGateAndStampManifestContract_HookOnlyManifestLoadsUnstamped is the other
// half: the legitimate framework-package shape stays untouched, unstamped, and
// loads — the packager's exemption and the loader's exemption are the same rule
// (DeclaresContractSurface), so neither may reject what the other accepts.
func TestGateAndStampManifestContract_HookOnlyManifestLoadsUnstamped(t *testing.T) {
	manifest := `{
	"commands": {},
	"hooks": {"preBuild": {"kind": "command", "command": "echo"}}
}`
	dir := stageManifest(t, manifest)
	if err := gateAndStampManifestContract(dir); err != nil {
		t.Fatalf("hook-only manifests must not be gated: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if string(data) != manifest {
		t.Errorf("hook-only manifest must be left byte-identical, got:\n%s", data)
	}
	loaded, err := proto.LoadManifest(filepath.Join(dir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("an unstamped hook-only manifest must still load: %v", err)
	}
	if loaded.CLIContract != 0 {
		t.Errorf("hook-only cliContract = %d, want 0 (never earned, never claimed)", loaded.CLIContract)
	}
}

// skipRepoWalkDir reports whether a directory is outside a first-party repo
// walk's business. EVERY dot-directory is skipped, not an enumerated few: the
// list that named only .git/.putnami/.gen still descended into .claude/worktrees,
// where agent worktrees keep full checkouts of this same repository — so the
// walk asserted on manifests from OTHER commits, and a stale checkout could fail
// (or vacuously pass) a test about the working tree. The twin walk in
// tooling/cli/internal/extension keeps the same rule.
func skipRepoWalkDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist"
}

// firstPartyManifests lists every putnami.extension.json checked into this
// repository. Discovery is by walk rather than a hard-coded list so a new
// first-party extension is covered the day it lands, and the count is asserted
// by the caller so a broken walk cannot make the suite vacuous.
func firstPartyManifests(t *testing.T, repoRoot string) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not this test's business
		}
		if d.IsDir() {
			// The root itself is never skipped: a repository checked out under
			// a dot-directory would otherwise make the whole walk empty.
			if path != repoRoot && skipRepoWalkDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == proto.ManifestFilename {
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return nil
			}
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for extension manifests: %v", err)
	}
	return found
}

// TestFirstPartyManifestsSurviveTheGateAndLoad is the artifact half of B6d:
// every first-party manifest in this repository, run through the REAL packaging
// gate into a staging directory, produces something this build's loader accepts.
//
// It is the closest a test can get to `putnami publish` without a registry: the
// stamp a released artifact carries is written by exactly this function, so a
// first-party manifest that would ship unloadable fails here instead of at a
// user's next `putnami build`.
func TestFirstPartyManifestsSurviveTheGateAndLoad(t *testing.T) {
	repoRoot := findRepoRoot(t)
	manifests := firstPartyManifests(t, repoRoot)

	// The suite must not pass by finding nothing. These are the extensions the
	// 0.3.0 tag ships; a walk that misses them is a broken walk.
	for _, required := range []string{
		filepath.Join("go", "extension", proto.ManifestFilename),
		filepath.Join("typescript", "extension", proto.ManifestFilename),
		filepath.Join("python", "extension", proto.ManifestFilename),
		filepath.Join("tooling", "scaffold", proto.ManifestFilename),
		filepath.Join("tooling", "clientgen-extension", proto.ManifestFilename),
	} {
		if !containsPath(manifests, required) {
			t.Fatalf("first-party manifest %s was not discovered; found %v", required, manifests)
		}
	}

	stampedSurfaces := 0
	for _, rel := range manifests {
		t.Run(rel, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			stage := stageManifest(t, string(source))
			// A manifest with agent content ships the tree built from its
			// authored source; every packager stages it before the gate.
			if _, err := agentartifact.StageExtensionContent(filepath.Join(repoRoot, filepath.Dir(rel)), stage, "", "0.0.0-gate"); err != nil {
				t.Fatalf("%s cannot stage its agent content, so it cannot be published: %v", rel, err)
			}

			if err := gateAndStampManifestContract(stage); err != nil {
				t.Fatalf("%s does not survive the package-time contract gate, so it cannot be published: %v", rel, err)
			}

			loaded, err := proto.LoadManifest(filepath.Join(stage, proto.ManifestFilename))
			if err != nil {
				t.Fatalf("the packaged form of %s does not load: %v", rel, err)
			}
			if proto.DeclaresContractSurface(loaded) {
				stampedSurfaces++
				// The stamp is the contract the manifest's vocabulary requires:
				// the current one, or the agent-content contract for a
				// manifest that ships agent content.
				if want := proto.RequiredCLIContract(loaded); loaded.CLIContract != want {
					t.Errorf("%s packages at cliContract %d, want %d", rel, loaded.CLIContract, want)
				}
				return
			}
			if loaded.CLIContract != 0 {
				t.Errorf("%s declares no contract surface but carries cliContract %d; the packager never stamps one",
					rel, loaded.CLIContract)
			}
		})
	}
	if stampedSurfaces == 0 {
		t.Fatal("no first-party manifest declared a contract surface; the stamp assertion never ran")
	}
}

// The first-party walk above reads every extension manifest in the
// repository, the content policy and authored source of each one that ships
// agent content, and the npm packager's twin of the gate. Each of those files
// is a declared test input of this project, so editing one moves the test
// task's cache key instead of replaying a verdict recorded on other bytes.
func TestFirstPartyReadsAreDeclaredTestInputs(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "repository-reads-are-declared-inputs", "every-first-party-manifest-and-content-source-the-tests-read-is-a-declared-test-input")
	repoRoot := findRepoRoot(t)
	projectRoot := filepath.Join(repoRoot, "go", "extension")

	read := []string{filepath.Join("typescript", "extension", "internal", "pkg", "manifest_contract.go")}
	withContent := 0
	for _, rel := range firstPartyManifests(t, repoRoot) {
		read = append(read, rel)
		manifest, err := proto.LoadManifest(filepath.Join(repoRoot, rel))
		if err != nil || manifest.AgentContent == nil || manifest.AgentContent.Source == "" {
			continue
		}
		withContent++
		extensionDir := filepath.Dir(rel)
		read = append(read, filepath.Join(extensionDir, "putnami.json"))
		source := filepath.Join(repoRoot, extensionDir, filepath.FromSlash(manifest.AgentContent.Source))
		if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			fromRepo, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			read = append(read, fromRepo)
			return nil
		}); err != nil {
			t.Fatalf("walk the agent-content source of %s: %v", rel, err)
		}
	}
	if withContent == 0 {
		t.Fatal("no first-party manifest ships agent content; the content assertions would pass vacuously")
	}
	assertDeclaredTestInputs(t, repoRoot, projectRoot, read)
}

// The other repository files this project's tests read are declared test
// inputs too: the prepare scripts test/scripts runs for the TypeScript and
// Python runtimes, the legacy wrappers
// TestLanguageExtensionsDeclarePreparedRuntimeWithoutLegacyWrappers proves
// absent, the secrets golden file configextract compares against, and every
// file of the module TestPrepareGoModulePackagesTransactionProtocol packages.
func TestOtherRepositoryReadsAreDeclaredTestInputs(t *testing.T) {
	repoRoot := findRepoRoot(t)
	projectRoot := filepath.Join(repoRoot, "go", "extension")
	read := []string{
		filepath.Join("typescript", "extension", "bin", "prepare"),
		filepath.Join("python", "extension", "bin", "prepare"),
		filepath.Join("typescript", "extension", "bin", "ts-run"),
		filepath.Join("python", "extension", "bin", "py-run"),
		filepath.Join("protocols", "infra", "fixtures", "equivalence", "secrets.golden.json"),
	}
	transaction := filepath.Join(repoRoot, "protocols", "transaction")
	if err := filepath.WalkDir(transaction, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		fromRepo, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		read = append(read, fromRepo)
		return nil
	}); err != nil {
		t.Fatalf("walk protocols/transaction: %v", err)
	}
	assertDeclaredTestInputs(t, repoRoot, projectRoot, read)
}

// assertDeclaredTestInputs fails for every repository-relative file in read
// that the test filePatterns of the project at projectRoot do not select.
func assertDeclaredTestInputs(t *testing.T, repoRoot, projectRoot string, read []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectRoot, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse go/extension/putnami.json: %v", err)
	}
	patterns := config.Options.Test.FilePatterns
	for _, file := range read {
		fromProject, err := filepath.Rel(projectRoot, filepath.Join(repoRoot, file))
		if err != nil {
			t.Fatal(err)
		}
		if !wsproto.SelectsPath(filepath.ToSlash(fromProject), patterns) {
			t.Errorf("%s is read by this project's tests but is not a declared test input (options.test.filePatterns in go/extension/putnami.json)", filepath.ToSlash(file))
		}
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func TestLanguageExtensionsDeclarePreparedRuntimeWithoutLegacyWrappers(t *testing.T) {
	repoRoot := findRepoRoot(t)
	expected := map[string]string{
		filepath.Join("go", "extension"):         "compiled/putnami-go",
		filepath.Join("python", "extension"):     "compiled/putnami-python",
		filepath.Join("typescript", "extension"): "compiled/putnami-ts",
	}
	for rel, executable := range expected {
		t.Run(rel, func(t *testing.T) {
			root := filepath.Join(repoRoot, rel)
			manifest, err := proto.LoadManifest(filepath.Join(root, proto.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Runtime == nil || manifest.Runtime.Prepare == nil || manifest.Runtime.Executable != executable {
				t.Fatalf("runtime = %+v, want local prepare and packaged executable %q", manifest.Runtime, executable)
			}
			for name, task := range manifest.Tasks {
				if strings.Contains(task.Command, "-run") {
					t.Errorf("task %q retains legacy wrapper command %q", name, task.Command)
				}
			}
			if manifest.Hooks != nil {
				// cacheClean/cacheGC left this map with the hook vocabulary
				// itself: the cache lifecycle is a
				// reserved COMMAND running a typed task now, so its
				// {extensionRuntime} use is covered by the task loop above.
				hooks := map[string]*proto.HookDefinition{
					"preBuild":  manifest.Hooks.PreBuild,
					"onInstall": manifest.Hooks.OnInstall,
				}
				for name, hook := range hooks {
					if hook == nil {
						continue
					}
					if hook.Command != "{extensionRuntime}" {
						t.Errorf("hook %q command = %q, want {extensionRuntime}", name, hook.Command)
					}
				}
			}
			for _, wrapper := range []string{"go-run", "py-run", "ts-run"} {
				if _, err := os.Stat(filepath.Join(root, "bin", wrapper)); !os.IsNotExist(err) {
					t.Errorf("legacy wrapper bin/%s still exists", wrapper)
				}
			}
		})
	}
}

// TestManifestContractGateHasNoTwinDrift keeps the two packagers honest. The
// gate exists once per packaging module (Go archive extensions here, npm
// extension packages in @putnami/typescript) because they are separate Go
// modules; the two copies are meant to be the same file. A ratchet applied to
// one and not the other would publish contract-3 archives and contract-2 npm
// packages from the same commit — the exact asymmetry that once cost a release.
func TestManifestContractGateHasNoTwinDrift(t *testing.T) {
	repoRoot := findRepoRoot(t)
	twins := []string{
		filepath.Join("go", "extension", "internal", "jobs", "pkg", "manifest_contract.go"),
		filepath.Join("typescript", "extension", "internal", "pkg", "manifest_contract.go"),
	}

	reference, err := os.ReadFile(filepath.Join(repoRoot, twins[0]))
	if err != nil {
		t.Fatalf("read %s: %v", twins[0], err)
	}
	for _, twin := range twins[1:] {
		other, err := os.ReadFile(filepath.Join(repoRoot, twin))
		if err != nil {
			t.Fatalf("read %s: %v", twin, err)
		}
		if string(other) != string(reference) {
			t.Errorf("%s has drifted from %s; the package-time contract gate must be byte-identical in both packagers",
				twin, twins[0])
		}
	}
}
