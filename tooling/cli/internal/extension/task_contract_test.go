package extension

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The CLI's half of extension task-contract v3 is RECOGNITION, not behavior: a
// manifest that declares outputs, effects, or source mutation must load and
// resolve exactly like the v2 manifest it would otherwise be, with the
// declarations intact for the capture work that consumes them later. These
// tests pin both halves — the declarations survive, and nothing else moves.

const v3Manifest = `{
	"name": "@putnami/v3",
	"version": "1.0.0",
	"cliContract": 4,
	"commands": {
		"build": {
			"run": [{"id": "generate", "task": "build-generate"}]
		},
		"lint": {
			"run": [{"id": "fix", "task": "lint-fix"}]
		}
	},
	"tasks": {
		"build-generate": {
			"kind": "command",
			"command": "node",
			"args": ["generate.js"],
			"writes": ["gen"],
			"cache": {"enabled": true},
			"declares": {
				"outputs": {
					"gen": {"kind": "directory", "root": "project", "path": ".gen"},
					"coverage": {
						"kind": "file",
						"root": "command-output",
						"path": "lcov.info",
						"optionalEmpty": true
					}
				},
				"effects": ["toolchain-cache"]
			}
		},
		"lint-fix": {
			"kind": "command",
			"command": "node",
			"args": ["lint.js", "--fix"],
			"writes": ["sources"],
			"cache": {"enabled": true, "noOutput": true},
			"declares": {"mutatesSources": true}
		}
	}
}`

// v2Manifest is v3Manifest with every declaration removed. Resolving both must
// produce identical planner input apart from the declarations themselves.
const v2Manifest = `{
	"name": "@putnami/v3",
	"version": "1.0.0",
	"cliContract": 4,
	"commands": {
		"build": {
			"run": [{"id": "generate", "task": "build-generate"}]
		},
		"lint": {
			"run": [{"id": "fix", "task": "lint-fix"}]
		}
	},
	"tasks": {
		"build-generate": {
			"kind": "command",
			"command": "node",
			"args": ["generate.js"],
			"writes": ["gen"],
			"cache": {"enabled": true}
		},
		"lint-fix": {
			"kind": "command",
			"command": "node",
			"args": ["lint.js", "--fix"],
			"writes": ["sources"],
			"cache": {"enabled": true, "noOutput": true}
		}
	}
}`

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "putnami.extension.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifest_RecognizesTaskContractV3(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, v3Manifest))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got := ManifestProtocolVersion(m); got != 3 {
		t.Fatalf("ManifestProtocolVersion = %d, want 3", got)
	}

	declares := m.Tasks["build-generate"].Declares
	if declares == nil {
		t.Fatal("build-generate lost its declaration")
	}
	gen := declares.Outputs["gen"]
	if gen.Kind != OutputKindDirectory || gen.Path != ".gen" || gen.EffectiveRoot() != OutputRootProject {
		t.Errorf("gen output = %+v, want a project-rooted .gen directory", gen)
	}
	coverage := declares.Outputs["coverage"]
	if coverage.Kind != OutputKindFile || coverage.EffectiveRoot() != OutputRootCommandOutput || !coverage.OptionalEmpty {
		t.Errorf("coverage output = %+v, want an optional-empty command-output file", coverage)
	}
	if !reflect.DeepEqual(declares.Effects, []string{"toolchain-cache"}) {
		t.Errorf("effects = %v, want [toolchain-cache]", declares.Effects)
	}
	if !m.Tasks["lint-fix"].Declares.MutatesSources {
		t.Error("lint-fix lost its source-mutation declaration")
	}
	if !m.Tasks["lint-fix"].UsesTaskContractV3() || m.Tasks["lint-fix"].Cache.NoOutput != true {
		t.Error("v3 recognition must not disturb the existing cache policy")
	}
}

// TestResolve_V3DeclarationsAreInertToday pins the additive promise at the CLI
// boundary: resolving a v3 manifest yields the same planner input as the same
// manifest without declarations, so nothing that RUNS today changes behavior.
// One deliberate exception was carved out of that promise: the
// task-contract digest. Declaring outputs IS a contract change, and cache key
// v5 must miss on it (binding invariant 6) — so the digest must
// move with the declaration while every other member of the resolved job
// stays identical. Execution behavior remains inert until B4 consumes the
// declarations.
func TestResolve_V3DeclarationsAreInertToday(t *testing.T) {
	v3, err := LoadManifest(writeManifest(t, v3Manifest))
	if err != nil {
		t.Fatalf("LoadManifest v3: %v", err)
	}
	v2, err := LoadManifest(writeManifest(t, v2Manifest))
	if err != nil {
		t.Fatalf("LoadManifest v2: %v", err)
	}

	v3Desc := Resolve(v3, "/ext")
	v2Desc := Resolve(v2, "/ext")

	neutralizeDigests := func(jobs map[string]*JobDefinition) map[string]*JobDefinition {
		out := make(map[string]*JobDefinition, len(jobs))
		for name, job := range jobs {
			copied := *job
			copied.ContractDigest = ""
			out[name] = &copied
		}
		return out
	}
	if !reflect.DeepEqual(neutralizeDigests(v3Desc.Jobs), neutralizeDigests(v2Desc.Jobs)) {
		t.Errorf("v3 declarations changed the resolved jobs beyond the contract digest:\n v3: %+v\n v2: %+v", v3Desc.Jobs, v2Desc.Jobs)
	}
	digestMoved := false
	for name, v3Job := range v3Desc.Jobs {
		if v2Job, ok := v2Desc.Jobs[name]; ok && v2Job.ContractDigest != v3Job.ContractDigest {
			digestMoved = true
			break
		}
	}
	if !digestMoved {
		t.Error("declaring outputs moved no job's contract digest — cache key v5 would reuse pre-declaration entries for a changed contract")
	}

	// The tasks differ by exactly the declarations, which Resolve carries
	// through untouched for the declared-output capture that consumes them.
	if v3Desc.Tasks["build-generate"].Declares == nil {
		t.Error("Resolve dropped the v3 declaration")
	}
	for name, task := range v2Desc.Tasks {
		if task.Declares != nil {
			t.Errorf("task %q gained a declaration it never had", name)
		}
		stripped := v3Desc.Tasks[name]
		stripped.Declares = nil
		if !reflect.DeepEqual(stripped, task) {
			t.Errorf("task %q differs beyond its declaration:\n v3: %+v\n v2: %+v", name, stripped, task)
		}
	}
}

// TestOutputsOverlap_ReExported pins that the CLI names the protocol's
// ownership predicate rather than growing a second definition of "same output".
func TestOutputsOverlap_ReExported(t *testing.T) {
	subtree := OutputRef{Root: OutputRootProject, Path: ".gen"}
	nested := OutputRef{Root: OutputRootProject, Path: ".gen/api/openapi.json"}
	if !OutputsOverlap(subtree, nested) {
		t.Error("a file inside a declared subtree must overlap it")
	}
	if OutputsOverlap(subtree, OutputRef{Root: OutputRootWorkspace, Path: ".gen"}) {
		t.Error("root-relative paths under different roots are not comparable")
	}
}
