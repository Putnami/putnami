package extension

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// First-party conformance for the exact-match contract loader.
//
// B6c made the stamp load-bearing: a manifest that declares a contract surface
// and is not stamped at protocolcli.CurrentContract does not load, and an
// extension that does not load is one a user's CLI silently does without. That
// turns every first-party manifest in this repository into a release blocker,
// and the blocker is cheap to check — so it is checked here, on every run,
// instead of at the 0.3.0 tag.
//
// The packaging side of the same invariant (staged manifest → stamp → load)
// lives with the packagers, in go/extension/internal/jobs/pkg and
// typescript/extension/internal/pkg: this file asserts what the SOURCE
// manifests are, those assert what `putnami package` makes of them.

// skipRepoWalkDir reports whether a directory is outside a first-party repo
// walk's business. EVERY dot-directory is skipped, not an enumerated few: the
// list that named only .git/.putnami/.gen still descended into .claude/worktrees,
// where agent worktrees keep full checkouts of this same repository — so the
// walk asserted on manifests from OTHER commits, and a stale checkout could fail
// (or vacuously pass) a test about the working tree. The twin walk in
// go/extension/internal/jobs/pkg keeps the same rule.
func skipRepoWalkDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist"
}

// discoverFirstPartyManifests returns every putnami.extension.json checked into
// the repository, workspace-relative. Discovery is a walk rather than a list so
// a new first-party extension is covered the day it lands; the caller asserts
// the well-known ones are present so a broken walk cannot pass vacuously.
func discoverFirstPartyManifests(t *testing.T, repoRoot string) []string {
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
		if d.Name() != ManifestFilename {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for extension manifests: %v", err)
	}
	return found
}

// firstPartyExtensionManifests are the manifests the 0.3.0 tag ships a contract
// surface from. They are named so the walk above cannot silently stop finding
// them — a suite that discovers nothing passes forever.
var firstPartyExtensionManifests = []string{
	filepath.Join("go", "extension", ManifestFilename),
	filepath.Join("typescript", "extension", ManifestFilename),
	filepath.Join("python", "extension", ManifestFilename),
	filepath.Join("tooling", "scaffold", ManifestFilename),
	filepath.Join("tooling", "clientgen-extension", ManifestFilename),
	filepath.Join("tooling", "samples", "shell-extension", ManifestFilename),
}

// TestFirstPartyManifestsLoadUnderTheCurrentContract is the load half: every
// manifest in the repository must pass through the same LoadManifest a user's
// CLI runs, and the ladder must resolve the way the manifest's own shape says it
// should — stamped at the contract its vocabulary requires when it declares a
// contract surface (proto.RequiredCLIContract: the current contract, or the
// agent-content contract for a manifest that ships agent content), unstamped
// when it is hook-only.
//
// A first-party manifest stamped one contract behind is not a warning here. It
// is an extension that vanishes: `putnami build` in a consumer workspace loses
// the commands it provides, with a skip record most users never look at.
func TestFirstPartyManifestsLoadUnderTheCurrentContract(t *testing.T) {
	repoRoot := findRepoRoot(t)
	manifests := discoverFirstPartyManifests(t, repoRoot)

	for _, required := range firstPartyExtensionManifests {
		if !containsString(manifests, required) {
			t.Fatalf("first-party manifest %s was not discovered; found %v", required, manifests)
		}
	}

	surfaces, hookOnly := 0, 0
	for _, rel := range manifests {
		t.Run(rel, func(t *testing.T) {
			manifest, err := LoadManifest(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("%s does not load under CLI contract %d: %v", rel, protocolcli.CurrentContract, err)
			}

			if proto.DeclaresContractSurface(manifest) {
				surfaces++
				if want := proto.RequiredCLIContract(manifest); manifest.CLIContract != want {
					t.Fatalf("%s declares a contract surface at cliContract %d, want %d — "+
						"an extension stamped below the contract its vocabulary requires does not load at all",
						rel, manifest.CLIContract, want)
				}
				return
			}
			hookOnly++
			if manifest.CLIContract != 0 {
				t.Errorf("%s declares no contract surface but claims cliContract %d; "+
					"the packager never stamps one, so the claim was never earned", rel, manifest.CLIContract)
			}
		})
	}

	// Both arms of the ladder must actually have been exercised. Either count at
	// zero means the fixture set changed shape and the assertions above stopped
	// asserting anything.
	if surfaces == 0 {
		t.Error("no first-party manifest declared a contract surface; the stamp assertion never ran")
	}
	if hookOnly == 0 {
		t.Error("no hook-only first-party manifest was found; the exemption assertion never ran")
	}
}

// TestFirstPartyManifestsPassThePublishGate is the publish half. The
// package-time gate runs proto.FullValidateManifest on the staged manifest and
// refuses to stamp anything it rejects, so a first-party manifest that fails it
// is one `putnami publish` cannot ship — a release-time failure discovered at
// release time. Running the same verdict here moves it to every test run.
//
// The gate itself lives in the packagers (they are the ones that write the
// stamp); what is asserted here is the input they will be given.
func TestFirstPartyManifestsPassThePublishGate(t *testing.T) {
	repoRoot := findRepoRoot(t)

	for _, rel := range firstPartyExtensionManifests {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}

			// Strict parse: the gate rejects unknown fields, so a typo in a
			// first-party manifest is a packaging failure, not a silent no-op.
			manifest, diags := proto.ParseManifest(data)
			if diag.HasErrors(diags) {
				t.Fatalf("%s does not parse strictly: %v", rel, diag.Errors(diags))
			}
			if errs := diag.Errors(proto.FullValidateManifest(manifest)); len(errs) > 0 {
				t.Fatalf("%s would fail the package-time contract gate and could not be published: %v", rel, errs)
			}
		})
	}
}

// TestFirstPartyDeclaredTasksConform pins the task-contract half of the B6 row.
// Task contract v3 is ORTHOGONAL to the CLI contract — a contract-4 manifest may
// still carry v2 tasks — but a task that DOES declare must declare correctly:
// exact paths, one owner per output, honest effects. A first-party violation
// would surface in a consumer's plan, far from the extension that caused it.
func TestFirstPartyDeclaredTasksConform(t *testing.T) {
	repoRoot := findRepoRoot(t)

	declaring := 0
	for _, rel := range firstPartyExtensionManifests {
		t.Run(rel, func(t *testing.T) {
			manifest, err := LoadManifest(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("LoadManifest(%s): %v", rel, err)
			}
			if errs := diag.Errors(proto.ValidateTaskContracts(manifest)); len(errs) > 0 {
				t.Fatalf("%s violates the v3 task contract: %v", rel, errs)
			}
			for _, task := range manifest.Tasks {
				if task.Declares != nil {
					declaring++
					break
				}
			}
		})
	}
	if declaring == 0 {
		t.Error("no first-party manifest carries a `declares` block; the v3 task-contract assertion never ran")
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// findRepoRoot locates the workspace root so the first-party manifests can be
// read from their real paths. It is a per-test-package helper — internal/watch
// keeps its own copy for the same reason — and it moved here with the loader
// tests that use it when the pure spec left for cli/model.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find Putnami repo root from %s", start)
		}
		dir = parent
	}
}
