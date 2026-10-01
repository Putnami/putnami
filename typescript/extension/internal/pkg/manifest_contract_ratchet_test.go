package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// A later change ratcheted the package-time gate to the contract-3
// world B6c created. This is the npm half — the packager that stages the
// framework packages' manifests — and it must reach the same verdicts as the Go
// archive packager, because both ship from the same commit.

// findRepoRoot walks up from the test's working directory to the workspace root
// so the real first-party manifests can be read. It skips rather than fails when
// this module is built outside the repository.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("workspace root not found from %s; skipping repo-integration test", start)
		}
		dir = parent
	}
}

func stageSourceManifest(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, proto.ManifestFilename), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCopyHookFiles_StagedOutputLoadsUnderTheCurrentLoader is the round trip the
// B6 dataset row asks for: package → stamp → LoadManifest. The gate validating a
// manifest is a statement about the document; this is the statement about the
// npm PACKAGE, made through the very function a consumer's CLI calls.
func TestCopyHookFiles_StagedOutputLoadsUnderTheCurrentLoader(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "packaged-loadability", "a-staged-package-loads-under-the-claimed-contract")
	projDir := stageSourceManifest(t, conformingCLIManifest)
	outDir := t.TempDir()

	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("copyHookFiles should pass a conforming manifest: %v", err)
	}

	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the packaged manifest does not load under this build's loader: %v", err)
	}
	if loaded.CLIContract != protocolcli.CurrentContract {
		t.Errorf("loaded cliContract = %d, want %d", loaded.CLIContract, protocolcli.CurrentContract)
	}
}

// TestCopyHookFiles_RatchetsAStaleLowerClaim pins the direction the ratchet does
// turn: an author's checked-in `2` is replaced by the `3` the packaging build
// just earned, rather than failing a package job over a field the packager owns.
func TestCopyHookFiles_RatchetsAStaleLowerClaim(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "packaged-loadability", "a-stale-lower-claim-is-ratcheted-to-the-earned-one")
	if protocolcli.CurrentContract < 2 {
		t.Skip("no lower contract to ratchet from")
	}
	stale := strings.Replace(conformingCLIManifest,
		`"name": "@putnami/testext",`,
		fmt.Sprintf(`"name": "@putnami/testext",
	"cliContract": %d,`, protocolcli.CurrentContract-1), 1)

	projDir := stageSourceManifest(t, stale)
	outDir := t.TempDir()
	if err := copyHookFiles(projDir, outDir, "1.0.0"); err != nil {
		t.Fatalf("a stale lower claim must be ratcheted up, not rejected: %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the ratcheted manifest does not load: %v", err)
	}
	if loaded.CLIContract != protocolcli.CurrentContract {
		t.Errorf("staged cliContract = %d, want %d", loaded.CLIContract, protocolcli.CurrentContract)
	}
}

// TestCopyHookFiles_RejectsFutureContractClaim pins the direction it does not
// turn: this packager cannot certify a contract it does not implement, so a
// higher claim fails the package job instead of being quietly downgraded into a
// stamp the loader would then believe.
func TestCopyHookFiles_RejectsFutureContractClaim(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "packaged-loadability", "a-contract-claim-newer-than-the-packager-is-rejected")
	future := strings.Replace(conformingCLIManifest,
		`"name": "@putnami/testext",`,
		fmt.Sprintf(`"name": "@putnami/testext",
	"cliContract": %d,`, protocolcli.LatestContract+1), 1)

	projDir := stageSourceManifest(t, future)
	outDir := t.TempDir()
	err := copyHookFiles(projDir, outDir, "1.0.0")
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
}

// TestCopyHookFiles_RejectsUnloadableHookOnlyManifest closes the hole the
// hook-only bypass left open. `"hooks": []` parses as raw JSON, names no
// command, group or tool, and so skipped the gate entirely — and then failed to
// unmarshal in every consumer, which drops the whole extension. The bypass is
// about what the CONTRACT governs, not about whether the file is usable, and
// this is the framework-package path where hook-only manifests actually ship.
func TestCopyHookFiles_RejectsUnloadableHookOnlyManifest(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "packaged-loadability", "an-unloadable-hook-only-manifest-fails-packaging")
	for name, manifest := range map[string]string{
		"hooks of the wrong type": `{"commands": {}, "hooks": []}`,
		"tasks of the wrong type": `{"commands": {}, "tasks": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			projDir := stageSourceManifest(t, manifest)
			outDir := t.TempDir()
			err := copyHookFiles(projDir, outDir, "1.0.0")
			if err == nil {
				t.Fatal("a hook-only manifest no consumer can load must fail packaging")
			}
			if !strings.Contains(err.Error(), "every consumer would skip the extension") {
				t.Errorf("error should name the consumer-side consequence, got %v", err)
			}
		})
	}
}

// TestFirstPartyNpmManifestsPackageAndLoad is the artifact half of B6d for the
// npm packager: every framework package that ships a putnami.extension.json is
// staged through the REAL package job and must produce something this build's
// loader accepts. These manifests are hook-only by design, which is exactly the
// case the stamp does not cover — so loading is the only evidence there is.
func TestFirstPartyNpmManifestsPackageAndLoad(t *testing.T) {
	repoRoot := findRepoRoot(t)
	// The npm packager stages the TypeScript framework packages. They are named
	// rather than walked because the walk belongs to the Go packager's suite
	// (which covers every manifest in the repo); here the point is that the
	// packages this job actually handles survive it.
	projects := []string{
		filepath.Join("typescript", "framework", "application"),
		filepath.Join("typescript", "framework", "events"),
		filepath.Join("typescript", "framework", "web"),
	}

	for _, rel := range projects {
		t.Run(rel, func(t *testing.T) {
			projDir := filepath.Join(repoRoot, rel)
			source, err := os.ReadFile(filepath.Join(projDir, proto.ManifestFilename))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}

			outDir := t.TempDir()
			if err := copyHookFiles(projDir, outDir, "9.9.9"); err != nil {
				t.Fatalf("%s does not survive the package job, so it cannot be published: %v", rel, err)
			}

			loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
			if err != nil {
				t.Fatalf("the packaged form of %s does not load: %v", rel, err)
			}
			if proto.DeclaresContractSurface(loaded) {
				if want := proto.RequiredCLIContract(loaded); loaded.CLIContract != want {
					t.Errorf("%s packages at cliContract %d, want %d", rel, loaded.CLIContract, want)
				}
			} else if loaded.CLIContract != 0 {
				t.Errorf("%s declares no contract surface but carries cliContract %d; the packager never stamps one",
					rel, loaded.CLIContract)
			}

			// The source manifest is never touched by packaging.
			after, err := os.ReadFile(filepath.Join(projDir, proto.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(source) {
				t.Errorf("%s was modified in the working tree by packaging", rel)
			}

			var staged map[string]any
			data, _ := os.ReadFile(filepath.Join(outDir, proto.ManifestFilename))
			if err := json.Unmarshal(data, &staged); err != nil {
				t.Fatal(err)
			}
			if staged["version"] != "9.9.9" {
				t.Errorf("packaged version = %v, want the publish version", staged["version"])
			}
		})
	}
}
