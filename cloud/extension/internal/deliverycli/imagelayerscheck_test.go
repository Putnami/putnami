package deliverycli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The packaging precondition. Its whole job is to make ONE failure
// impossible: `putnami package` reporting success for an image assembled from
// layers this checkout no longer describes. Every test below stages exactly one
// way that can happen and proves the check refuses it — and the happy path
// proves it does not refuse a set that is merely OLD but still current, since a
// producer whose inputs have not moved re-emits the same bytes.

// imageLayersCheckManifest declares one layer per producer family — plus the
// image-owned, digest-pinned form of `oci-file` — so the identity derivation is
// exercised for all six producers and for both halves of the pin rule.
const imageLayersCheckManifest = `{
  "layers": [
    {"name": "go-toolchain", "producer": "go-toolchain", "path": "/usr/local/go"},
    {"name": "staticcheck", "producer": "go-tool", "path": "/usr/local/bin/staticcheck",
     "tool": "staticcheck", "package": "honnef.co/go/tools/cmd/staticcheck", "module": "honnef.co/go/tools"},
    {"name": "putnami", "producer": "putnami-cli", "path": "/usr/local/bin/putnami"},
    {"name": "bun", "producer": "oci-file", "path": "/usr/local/bin/bun",
     "image": "oven/bun", "tag": "{version}-slim", "pin": "BUN_VERSION", "source": "/usr/local/bin/bun"},
    {"name": "uv", "producer": "oci-file", "path": "/usr/local/bin/uv",
     "image": "ghcr.io/astral-sh/uv", "tag": "{version}", "version": "0.11.32",
     "digest": "` + imageLayersUVDigest + `", "source": "/uv"},
    {"name": "putnami-warm", "producer": "putnami-warm", "path": "/root/.putnami/artifacts"},
    {"name": "workspace", "producer": "dirs", "path": "/workspace"}
  ]
}`

// The identities the fixture's layers were produced under, written out as
// literals rather than through the producers' own label functions: the point of
// this suite is that produce and check agree, and a fixture that called the
// same helper the check calls could not tell if they stopped agreeing.
const (
	// A derived tool is RESTORED from the lock-pinned @putnami/go artifact,
	// so the artifact that supplied the bytes is part of what selected them and
	// therefore part of the identity the check compares.
	imageLayersCheckStaticcheckID = "honnef.co/go/tools v0.7.0 honnef.co/go/tools/cmd/staticcheck" +
		" from @putnami/go 0.1.0-goext " + imageLayersCheckGoExtensionSHA
	imageLayersCheckBunID       = "oven/bun:1.3.14-slim /usr/local/bin/bun"
	imageLayersCheckUVID        = "ghcr.io/astral-sh/uv:0.11.32@" + imageLayersUVDigest + " /uv"
	imageLayersCheckWorkspaceID = "/workspace 0755"
	// imageLayersCheckGoExtensionSHA is the lock integrity the fixture records
	// for @putnami/go on linux/amd64.
	imageLayersCheckGoExtensionSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// imageLayersCheckFixture materializes a workspace whose pins are all declared,
// an image project, and a produced layer set that matches both.
func imageLayersCheckFixture(t *testing.T) (workspaceRoot, project string) {
	t.Helper()
	workspaceRoot = imagePinWorkspace(t)
	// The CLI layer derives BOTH its version and the integrity it is verified
	// against from the lock, so the fixture records the platform entry a real
	// `putnami install` writes.
	// The CLI and warm layers derive BOTH their versions and the integrities
	// they are verified against from the lock, so the fixture records the
	// platform entries a real `putnami install` writes.
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"), []byte(
		`{"cli":{"version":"0.1.0-dedb6e331","integrities":{"linux/amd64":"`+strings.Repeat("a", 64)+`"}},`+
			`"extensions":{`+
			`"@putnami/go":{"version":"0.1.0-goext","integrities":{"linux/amd64":"`+imageLayersCheckGoExtensionSHA+`"}},`+
			`"@putnami/typescript":{"version":"0.1.0-tsext","integrities":{"linux/amd64":"`+strings.Repeat("c", 64)+`"}}}}`), 0o600); err != nil {
		t.Fatalf("write putnami.lock.json: %v", err)
	}
	project = imageLayersProject(t, imageLayersCheckManifest)
	produce := func(producer, name, path, artifact, version, body string) {
		t.Helper()
		outDir := imageLayersOutputDir(project)
		file := filepath.Join(outDir, filepath.FromSlash(artifact))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", artifact, err)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", artifact, err)
		}
		sum, size, err := imageLayersHashFile(file)
		if err != nil {
			t.Fatalf("hash %s: %v", artifact, err)
		}
		record := imageLayerRecord{
			Name: name, Producer: producer, Path: path, SHA256: sum, Size: size, Version: version,
		}
		if strings.HasSuffix(artifact, ".tar") {
			record.Tar = filepath.Base(artifact)
		} else {
			record.File = artifact
		}
		if err := imageLayersWriteRecord(outDir, record); err != nil {
			t.Fatalf("write the %s record: %v", name, err)
		}
	}
	produce("go-toolchain", "go-toolchain", "/usr/local/go", "go-toolchain.tar", "1.26.1", "toolchain-tar-bytes\n")
	produce("go-tool", "staticcheck", "/usr/local/bin/staticcheck", "bin/staticcheck",
		imageLayersCheckStaticcheckID, "staticcheck-elf\n")
	produce("putnami-cli", "putnami", "/usr/local/bin/putnami", "bin/putnami",
		"0.1.0-dedb6e331 "+strings.Repeat("a", 64), "cli-elf\n")
	produce("oci-file", "bun", "/usr/local/bin/bun", "bin/bun", imageLayersCheckBunID, "bun-elf\n")
	produce("oci-file", "uv", "/usr/local/bin/uv", "bin/uv", imageLayersCheckUVID, "uv-elf\n")
	produce("putnami-warm", "putnami-warm", "/root/.putnami/artifacts", "putnami-warm.tar",
		"@putnami/go 0.1.0-goext "+imageLayersCheckGoExtensionSHA+
			", @putnami/typescript 0.1.0-tsext "+strings.Repeat("c", 64), "warm-store-tar-bytes\n")
	produce("dirs", "workspace", "/workspace", "workspace.tar", imageLayersCheckWorkspaceID, "workspace-dir-tar-bytes\n")
	return workspaceRoot, project
}

func imageLayersCheck(t *testing.T, workspaceRoot, project string) ([]string, error) {
	t.Helper()
	var out []string
	err := ImageLayers(map[string]any{"project": project, "check": true, "json": true},
		nil, workspaceRoot, nil, imageLayersIO(&out))
	return out, err
}

// TestImageLayersCheckAcceptsAProducedSet is the baseline: a layer set produced
// from the pins the workspace still declares is current, and the check says so
// without touching the network or executing anything it verified.
func TestImageLayersCheckAcceptsAProducedSet(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	out, err := imageLayersCheck(t, workspaceRoot, project)
	if err != nil {
		t.Fatalf("cloud image-layers --check: %v", err)
	}
	got := decodeCommandJSON(t, out)
	if got["status"] != "current" {
		t.Fatalf("status = %v, want current", got["status"])
	}
	layers, ok := got["layers"].([]any)
	if !ok || len(layers) != 7 {
		t.Fatalf("layers = %v, want one entry per declared layer", got["layers"])
	}
}

// TestImageLayersCheckRefusesAManifestEditThatSelectsOtherBytes closes the hole
// the version-only comparison left open: several
// image-layers.json fields choose WHICH artifact a layer carries without any
// version string moving, so a hand edit to one of them left the check green and
// packaging reusing the previous bytes. Each case edits exactly one of them and
// changes nothing else.
func TestImageLayersCheckRefusesAManifestEditThatSelectsOtherBytes(t *testing.T) {
	// The digest uv is pinned by. For an image-owned reference the digest IS the
	// pin, so this edit is the sharpest form of the hole: the version is not
	// even the thing that selects the bytes.
	otherDigest := "sha256:" + strings.Repeat("4", 64)
	cases := map[string]struct {
		old, new string
		wants    []string
	}{
		"uv's pinned digest moved under an unchanged version": {
			old:   `"digest": "` + imageLayersUVDigest + `"`,
			new:   `"digest": "` + otherDigest + `"`,
			wants: []string{"uv", imageLayersUVDigest, otherDigest},
		},
		"bun's tag template stopped selecting the slim image": {
			old:   `"tag": "{version}-slim"`,
			new:   `"tag": "{version}"`,
			wants: []string{"bun", "oven/bun:1.3.14-slim", "oven/bun:1.3.14 "},
		},
		"staticcheck's layer now builds another command of the same module": {
			old:   `"package": "honnef.co/go/tools/cmd/staticcheck"`,
			new:   `"package": "honnef.co/go/tools/cmd/keyify"`,
			wants: []string{"staticcheck", "honnef.co/go/tools/cmd/staticcheck", "honnef.co/go/tools/cmd/keyify"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspaceRoot, project := imageLayersCheckFixture(t)
			edited := strings.Replace(imageLayersCheckManifest, tc.old, tc.new, 1)
			if edited == imageLayersCheckManifest {
				t.Fatalf("the fixture manifest no longer carries %s", tc.old)
			}
			if err := os.WriteFile(filepath.Join(project, imageLayersManifestName), []byte(edited), 0o600); err != nil {
				t.Fatalf("rewrite the manifest: %v", err)
			}
			_, err := imageLayersCheck(t, workspaceRoot, project)
			if err == nil {
				t.Fatal("want a refusal when the manifest now selects other bytes than the produced set carries")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestImageLayersCheckRefusesACLIRepublishedUnderTheSameVersion: the lock
// carries the version AND the integrity the download is verified against, and
// the CLI has been republished under an unchanged version string before. The
// produced binary still hashes to what its record claims — it is simply not the
// artifact the lock now names.
func TestImageLayersCheckRefusesACLIRepublishedUnderTheSameVersion(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	lock, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.lock.json")) //nolint:gosec // G304: a path this fixture just wrote
	if err != nil {
		t.Fatalf("read the lock: %v", err)
	}
	repinned := strings.Replace(string(lock), strings.Repeat("a", 64), strings.Repeat("9", 64), 1)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"), []byte(repinned), 0o600); err != nil {
		t.Fatalf("repin putnami.lock.json: %v", err)
	}
	_, err = imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when the lock's CLI integrity moved under an unchanged version")
	}
	for _, want := range []string{"putnami", strings.Repeat("a", 64), strings.Repeat("9", 64)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// TestImageLayersCheckRefusesAStaleExtensionPin is the warm layer's half of the
// precondition: a lock that repinned @putnami/go leaves a produced store the
// runner would still bake — valid bytes for a set of extensions this checkout
// no longer resolves. It is also the only live verification those two pin axes
// have (pin-guard.sh names this producer as their enforcement).
func TestImageLayersCheckRefusesAStaleExtensionPin(t *testing.T) {
	cases := map[string]struct {
		lock  string
		wants []string
	}{
		"the workspace repinned an extension": {
			lock: `{"cli":{"version":"0.1.0-dedb6e331","integrities":{"linux/amd64":"` + strings.Repeat("a", 64) + `"}},` +
				`"extensions":{"@putnami/go":{"version":"0.1.0-newgoext","integrities":{"linux/amd64":"` + strings.Repeat("b", 64) + `"}},` +
				`"@putnami/typescript":{"version":"0.1.0-tsext","integrities":{"linux/amd64":"` + strings.Repeat("c", 64) + `"}}}}`,
			wants: []string{"putnami-warm", "0.1.0-goext", "0.1.0-newgoext"},
		},
		"the lock lost the platform integrity": {
			lock: `{"cli":{"version":"0.1.0-dedb6e331","integrities":{"linux/amd64":"` + strings.Repeat("a", 64) + `"}},` +
				`"extensions":{"@putnami/go":{"version":"0.1.0-goext"},` +
				`"@putnami/typescript":{"version":"0.1.0-tsext","integrities":{"linux/amd64":"` + strings.Repeat("c", 64) + `"}}}}`,
			wants: []string{"putnami-warm", `records no extensions["@putnami/go"].integrities`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspaceRoot, project := imageLayersCheckFixture(t)
			if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"), []byte(tc.lock), 0o600); err != nil {
				t.Fatalf("repin putnami.lock.json: %v", err)
			}
			_, err := imageLayersCheck(t, workspaceRoot, project)
			if err == nil {
				t.Fatal("want a refusal when the workspace moved an extension pin under a produced store")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestImageLayersCheckRefusesARestoredToolFromARepublishedExtension is the
// staleness axis a restored tool has and a compiled one does not.
//
// The layer's module, version and package are untouched; what moved is the
// @putnami/go artifact the binary was TAKEN from. The framework republishes a
// release built with another Go under the same tool pins, and the produced
// golangci-lint is then a different program behind an identical build input.
// Comparing the build input alone would package it as current.
func TestImageLayersCheckRefusesARestoredToolFromARepublishedExtension(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	lock, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.lock.json")) //nolint:gosec // G304: a path this fixture just wrote
	if err != nil {
		t.Fatalf("read the lock: %v", err)
	}
	republished := strings.Repeat("7", 64)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "putnami.lock.json"),
		[]byte(strings.ReplaceAll(string(lock), imageLayersCheckGoExtensionSHA, republished)), 0o600); err != nil {
		t.Fatalf("repin putnami.lock.json: %v", err)
	}
	_, err = imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when the artifact a restored tool came from was republished")
	}
	for _, want := range []string{"staticcheck", imageLayersCheckGoExtensionSHA, republished} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// TestImageLayersCheckRefusesAnUnproducedLayer: a fresh clone, a cleaned .gen,
// or a manifest that gained a layer nobody produced. Packaging would fail on
// the missing source file — but only after the operator waited for it, and with
// a diagnostic about a path rather than about the step they skipped.
func TestImageLayersCheckRefusesAnUnproducedLayer(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	if err := os.Remove(filepath.Join(imageLayersOutputDir(project), "bun.json")); err != nil {
		t.Fatalf("remove the bun record: %v", err)
	}
	_, err := imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when a declared layer was never produced")
	}
	for _, want := range []string{"bun", "putnami cloud image-layers"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// TestImageLayersCheckRefusesAStalePin is the one this precondition exists for:
// the workspace repinned a tool and nobody re-ran the producer. The layer bytes
// are intact and hash perfectly — they are simply the previous pin's, and
// packaging them would publish a valid content tag for an image that no longer
// describes this checkout.
func TestImageLayersCheckRefusesAStalePin(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	if err := os.WriteFile(filepath.Join(workspaceRoot, "go.work"), []byte("go 1.26.1\n\ntoolchain go1.27.0\n"), 0o600); err != nil {
		t.Fatalf("repin go.work: %v", err)
	}
	_, err := imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when the workspace moved a pin under a produced layer")
	}
	if !strings.Contains(err.Error(), "1.26.1") || !strings.Contains(err.Error(), "1.27.0") {
		t.Fatalf("refusal %q does not state both the produced and the declared version", err)
	}
	if clicore.ExitCode(err) == clicore.ExitUsage {
		t.Fatal("a stale layer set is a state to repair, not a malformed invocation")
	}
}

// TestImageLayersCheckRefusesBytesThatMovedUnderTheRecord: the record is the
// producer's claim about what it wrote, and packaging hashes the FILE. A file
// edited (or half-written) after the fact would be published under a content
// key computed from bytes nobody verified.
func TestImageLayersCheckRefusesBytesThatMovedUnderTheRecord(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	tampered := filepath.Join(imageLayersOutputDir(project), "bin", "staticcheck")
	if err := os.WriteFile(tampered, []byte("not the binary that was verified\n"), 0o600); err != nil {
		t.Fatalf("tamper with the produced file: %v", err)
	}
	_, err := imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when a produced file no longer matches its record")
	}
	if !strings.Contains(err.Error(), "staticcheck") {
		t.Fatalf("refusal %q does not name the layer whose bytes moved", err)
	}
}

// TestImageLayersCheckRefusesARelocatedLayer: the manifest may move a layer's
// in-image path (uv moved once already), and a produced set that predates the
// move would install it where nothing looks for it.
func TestImageLayersCheckRefusesARelocatedLayer(t *testing.T) {
	workspaceRoot, project := imageLayersCheckFixture(t)
	moved := strings.Replace(imageLayersCheckManifest,
		`"path": "/usr/local/bin/staticcheck"`, `"path": "/usr/local/libexec/staticcheck"`, 1)
	if err := os.WriteFile(filepath.Join(project, imageLayersManifestName), []byte(moved), 0o600); err != nil {
		t.Fatalf("rewrite the manifest: %v", err)
	}
	_, err := imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when a produced layer's destination moved")
	}
	if !strings.Contains(err.Error(), "/usr/local/libexec/staticcheck") {
		t.Fatalf("refusal %q does not name the new destination", err)
	}
}

// TestImageLayersCheckReadsAProducedDirectoryLayer runs the real producer and
// then the real check over the same project: after producing, the set is
// current, and editing the declared directory makes it stale. This is the pair
// that matters for a layer whose identity is nothing but its path — produce
// records it, `--check` re-derives it, and the two share one label function
// precisely so they cannot end up with different ideas of "current".
func TestImageLayersCheckReadsAProducedDirectoryLayer(t *testing.T) {
	workspaceRoot := t.TempDir() // this producer reads no workspace source at all
	project := imageLayersProject(t, `{"layers":[{"name":"workspace","producer":"dirs","path":"/workspace"}]}`)
	if err := ImageLayers(map[string]any{"project": project}, nil, workspaceRoot, nil, imageLayersIO(new([]string))); err != nil {
		t.Fatalf("cloud image-layers: %v", err)
	}
	out, err := imageLayersCheck(t, workspaceRoot, project)
	if err != nil {
		t.Fatalf("cloud image-layers --check right after producing: %v", err)
	}
	if got := decodeCommandJSON(t, out); got["status"] != "current" {
		t.Fatalf("status = %v, want current", got["status"])
	}

	moved := `{"layers":[{"name":"workspace","producer":"dirs","path":"/srv/workspace"}]}`
	if err := os.WriteFile(filepath.Join(project, imageLayersManifestName), []byte(moved), 0o600); err != nil {
		t.Fatalf("rewrite the manifest: %v", err)
	}
	_, err = imageLayersCheck(t, workspaceRoot, project)
	if err == nil {
		t.Fatal("want a refusal when the manifest declares another directory than the produced layer carries")
	}
	for _, want := range []string{"workspace", "/workspace", "/srv/workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
}

// TestImageLayersCheckRefusesOutsideAnImageProject keeps the mode's activation
// honest: it shares the producer's manifest reader, so a directory that
// declares no layers is a caller mistake with the same diagnostic.
func TestImageLayersCheckRefusesOutsideAnImageProject(t *testing.T) {
	_, err := imageLayersCheck(t, imagePinWorkspace(t), t.TempDir())
	if err == nil {
		t.Fatal("want a usage error outside an image project")
	}
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("exit code = %d, want usage", clicore.ExitCode(err))
	}
	if !strings.Contains(err.Error(), imageLayersManifestName) {
		t.Fatalf("error %q does not name the missing manifest", err)
	}
}
