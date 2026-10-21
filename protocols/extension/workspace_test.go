package extension

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

func adapterManifest(adapter WorkspaceAdapter, tasks ...string) *Manifest {
	m := &Manifest{Workspace: &adapter, Tasks: map[string]TaskDefinition{}}
	for _, name := range tasks {
		m.Tasks[name] = TaskDefinition{Kind: "command", Command: "echo"}
	}
	return m
}

func TestValidateWorkspaceAdapter_AcceptedShapes(t *testing.T) {
	cases := map[string]*Manifest{
		"markers are inputs": adapterManifest(WorkspaceAdapter{
			Markers: []string{"package.json"},
			Inputs:  []string{"bun.lock", "package.json", "tsconfig*.json"},
		}),
		"several markers": adapterManifest(WorkspaceAdapter{
			Markers: []string{"go.mod", "putnami.json"},
			Inputs:  []string{"go.mod", "go.sum", "putnami.json"},
		}),
		"glob marker": adapterManifest(WorkspaceAdapter{
			Markers: []string{"*.csproj"},
			Inputs:  []string{"*.csproj"},
		}),
		"nested marker path": adapterManifest(WorkspaceAdapter{
			Markers: []string{"src/pyproject.toml"},
			Inputs:  []string{"src/pyproject.toml"},
		}),
		"excludes and a sync task": adapterManifest(WorkspaceAdapter{
			Markers:  []string{"package.json"},
			Inputs:   []string{"package.json"},
			Excludes: []string{"dist", "node_modules"},
			SyncTask: "workspace-sync",
		}, "workspace-sync"),
		"install inputs and a language": adapterManifest(WorkspaceAdapter{
			Markers:       []string{"package.json"},
			Inputs:        []string{"package.json"},
			InstallInputs: []string{"bun.lock", "bun.lockb", ".tool-versions"},
			Language:      "typescript",
		}),
		"a language with digits and hyphens": adapterManifest(WorkspaceAdapter{
			Markers:  []string{"Cargo.toml"},
			Inputs:   []string{"Cargo.toml"},
			Language: "c-sharp2",
		}),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if diags := ValidateWorkspaceAdapter(m); len(diags) != 0 {
				t.Fatalf("expected no diagnostics, got %v", diags)
			}
		})
	}
}

// TestValidateWorkspaceAdapter_Rules pins one diagnostic code per rule so
// slices C3a/C3b key on the code rather than on message text.
func TestValidateWorkspaceAdapter_Rules(t *testing.T) {
	cases := []struct {
		name     string
		manifest *Manifest
		code     string
		field    string
	}{
		{
			name:     "markers are required",
			manifest: adapterManifest(WorkspaceAdapter{Inputs: []string{"go.mod"}}),
			code:     "required-field",
			field:    "workspace.markers",
		},
		{
			name:     "inputs are required",
			manifest: adapterManifest(WorkspaceAdapter{Markers: []string{"go.mod"}}),
			code:     "required-field",
			field:    "workspace.inputs",
		},
		{
			name: "a marker must be a metadata input",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"go.mod"},
				Inputs:  []string{"go.sum"},
			}),
			code:  "marker-not-input",
			field: "workspace.markers[0]",
		},
		{
			name: "a marker must not escape the candidate directory",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"../go.mod"},
				Inputs:  []string{"go.mod"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.markers[0]",
		},
		{
			name: "a marker must not be absolute",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"/etc/hosts"},
				Inputs:  []string{"/etc/hosts"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.markers[0]",
		},
		{
			name: "a marker must not be drive-qualified",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"C:/workspace/go.mod"},
				Inputs:  []string{"C:/workspace/go.mod"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.markers[0]",
		},
		{
			name: "an input must not use a template variable",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"go.mod"},
				Inputs:  []string{"go.mod", "{projectRoot}/go.sum"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.inputs[1]",
		},
		{
			name: "an input must use valid glob syntax",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"go.mod"},
				Inputs:  []string{"go.mod", "src/[abc"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.inputs[1]",
		},
		{
			name: "an input must not be declared twice",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers: []string{"go.mod"},
				Inputs:  []string{"go.mod", "./go.mod"},
			}),
			code:  "duplicate-value",
			field: "workspace.inputs[1]",
		},
		{
			name: "an exclude must not escape the candidate directory",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers:  []string{"go.mod"},
				Inputs:   []string{"go.mod"},
				Excludes: []string{"../vendor"},
			}),
			code:  "invalid-workspace-path",
			field: "workspace.excludes[0]",
		},
		{
			name: "the sync task must exist",
			manifest: adapterManifest(WorkspaceAdapter{
				Markers:  []string{"go.mod"},
				Inputs:   []string{"go.mod"},
				SyncTask: "missing",
			}),
			code:  "unresolved-sync-task",
			field: "workspace.syncTask",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateWorkspaceAdapter(tc.manifest)
			if !diag.HasErrors(diags) {
				t.Fatalf("expected an error diagnostic, got %v", diags)
			}
			assertDiag(t, diags, tc.code, tc.field)
		})
	}
}

// TestValidateWorkspaceAdapter_InstallInputsAndLanguage pins the two rules of
// the install inputs and the language: an install input is a file name at the
// workspace root, declared once, and a language is a lowercase id.
func TestValidateWorkspaceAdapter_InstallInputsAndLanguage(t *testing.T) {
	for name, value := range map[string]string{
		"nested":            "web/bun.lock",
		"dot-slash":         "./bun.lock",
		"star glob":         "*.lock",
		"question glob":     "uv.loc?",
		"class glob":        "bun.lock[b]",
		"template variable": "{workspaceRoot}.lock",
		"empty":             "",
		"blank":             "   ",
		"parent":            "..",
		"root":              ".",
		"backslash":         `web\bun.lock`,
		"drive":             "C:bun.lock",
	} {
		t.Run("install input "+name, func(t *testing.T) {
			diags := ValidateWorkspaceAdapter(adapterManifest(WorkspaceAdapter{
				Markers:       []string{"package.json"},
				Inputs:        []string{"package.json"},
				InstallInputs: []string{value},
			}))
			if !diag.HasErrors(diags) {
				t.Fatalf("install input %q accepted: %v", value, diags)
			}
			assertDiag(t, diags, "invalid-workspace-path", "workspace.installInputs[0]")
		})
	}

	t.Run("install input declared twice", func(t *testing.T) {
		diags := ValidateWorkspaceAdapter(adapterManifest(WorkspaceAdapter{
			Markers:       []string{"pyproject.toml"},
			Inputs:        []string{"pyproject.toml"},
			InstallInputs: []string{"uv.lock", " uv.lock "},
		}))
		if !diag.HasErrors(diags) {
			t.Fatalf("a duplicate install input was accepted: %v", diags)
		}
		assertDiag(t, diags, "duplicate-value", "workspace.installInputs[1]")
	})

	for _, language := range []string{"Go", "1go", "go_lang", "-go", "type script"} {
		t.Run("language "+language, func(t *testing.T) {
			diags := ValidateWorkspaceAdapter(adapterManifest(WorkspaceAdapter{
				Markers:  []string{"go.mod"},
				Inputs:   []string{"go.mod"},
				Language: language,
			}))
			if !diag.HasErrors(diags) {
				t.Fatalf("language %q accepted: %v", language, diags)
			}
			assertDiag(t, diags, "invalid-value", "workspace.language")
		})
	}
}

// TestValidateWorkspaceAdapter_CoreOwnedExclusions pins that restating a
// core-owned exclusion is reported as having no effect — and reported as a
// WARNING, because the adapter is not wrong, only redundant.
func TestValidateWorkspaceAdapter_CoreOwnedExclusions(t *testing.T) {
	for _, name := range AlwaysExcludedDirs {
		t.Run(name, func(t *testing.T) {
			m := adapterManifest(WorkspaceAdapter{
				Markers:  []string{"go.mod"},
				Inputs:   []string{"go.mod"},
				Excludes: []string{name},
			})
			diags := ValidateWorkspaceAdapter(m)
			if diag.HasErrors(diags) {
				t.Fatalf("a redundant exclusion is not an error: %v", diags)
			}
			assertDiag(t, diags, "redundant-exclude", "workspace.excludes[0]")
		})
	}

	if !IsAlwaysExcludedDir(".git") || !IsAlwaysExcludedDir(".putnami") {
		t.Error("core must always exclude .git and .putnami")
	}
	if IsAlwaysExcludedDir("node_modules") || IsAlwaysExcludedDir("") {
		t.Error("IsAlwaysExcludedDir accepted a directory core does not own")
	}
	for i := 1; i < len(AlwaysExcludedDirs); i++ {
		if AlwaysExcludedDirs[i-1] >= AlwaysExcludedDirs[i] {
			t.Errorf("AlwaysExcludedDirs is not in canonical (sorted) order: %v", AlwaysExcludedDirs)
		}
	}
}

// TestWorkspaceAdapterKeysOnPaths is the paths-not-modules invariant stated as
// a test: every member of the adapter is a path pattern resolved against a
// CANDIDATE DIRECTORY, so nothing in the contract can express "the module that
// owns this project". A project root that is not a module root is therefore a
// layout choice, not a protocol break.
func TestWorkspaceAdapterKeysOnPaths(t *testing.T) {
	// Two projects nested under one Go module: the module root holds the go.mod
	// and each project directory holds only its own putnami.json. A contract
	// that assumed project root == module root could not describe this at all;
	// a path-keyed one describes it by naming the paths it actually reads.
	m := adapterManifest(WorkspaceAdapter{
		Markers: []string{"putnami.json"},
		Inputs:  []string{"go.mod", "putnami.json"},
	})
	if diags := ValidateWorkspaceAdapter(m); len(diags) != 0 {
		t.Fatalf("a project that is not a module root must be declarable: %v", diags)
	}

	// The inverse layout — the marker IS the module manifest — is equally
	// declarable, and neither spelling is privileged by the contract.
	m = adapterManifest(WorkspaceAdapter{
		Markers: []string{"go.mod"},
		Inputs:  []string{"go.mod", "go.sum"},
	})
	if diags := ValidateWorkspaceAdapter(m); len(diags) != 0 {
		t.Fatalf("a project that IS a module root must be declarable: %v", diags)
	}

	// Every declared member goes through the same path normalizer, which is
	// what makes the invariant checkable instead of aspirational.
	adapter := WorkspaceAdapter{
		Markers:  []string{"pkg/go.mod"},
		Inputs:   []string{"pkg/go.mod"},
		Excludes: []string{"pkg/vendor"},
	}
	for _, pattern := range append(append(append([]string{}, adapter.Markers...), adapter.Inputs...), adapter.Excludes...) {
		if _, err := NormalizeInputPattern(pattern); err != nil {
			t.Errorf("adapter member %q is not a plain relative path pattern: %v", pattern, err)
		}
	}
}

func TestWorkspaceAdapterAccessors(t *testing.T) {
	var nilManifest *Manifest
	if nilManifest.DeclaresWorkspaceAdapter() {
		t.Error("a nil manifest declares no adapter")
	}
	if (&Manifest{}).DeclaresWorkspaceAdapter() {
		t.Error("a manifest without a workspace section declares no adapter")
	}
	if !loadFixtureManifest(t, lifecycleFixturePath).DeclaresWorkspaceAdapter() {
		t.Error("the lifecycle fixture declares an adapter")
	}
}

// TestNormalizeWorkspaceAdapter_Canonical pins the canonical form the workspace
// snapshot digest is taken over: every pattern set is a SET, so it is cleaned
// and sorted and no longer depends on authoring order.
func TestNormalizeWorkspaceAdapter_Canonical(t *testing.T) {
	build := func() *Manifest {
		return adapterManifest(WorkspaceAdapter{
			Markers:       []string{"./package.json"},
			Inputs:        []string{"tsconfig*.json", "./package.json", "bun.lock"},
			Excludes:      []string{"node_modules", "./dist/"},
			InstallInputs: []string{"bun.lockb", " bun.lock "},
			Language:      "typescript",
		})
	}
	m := build()
	NormalizeWorkspaceAdapter(m)

	if got, want := m.Workspace.InstallInputs, []string{"bun.lock", "bun.lockb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("install inputs = %v, want %v", got, want)
	}
	if m.Workspace.Language != "typescript" {
		t.Errorf("language = %q, want it unchanged", m.Workspace.Language)
	}

	if got, want := m.Workspace.Markers, []string{"package.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("markers = %v, want %v", got, want)
	}
	if got, want := m.Workspace.Inputs, []string{"bun.lock", "package.json", "tsconfig*.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inputs = %v, want %v", got, want)
	}
	if got, want := m.Workspace.Excludes, []string{"dist", "node_modules"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excludes = %v, want %v", got, want)
	}

	canonical, err := json.Marshal(m.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		other := build()
		NormalizeWorkspaceAdapter(other)
		got, err := json.Marshal(other.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(canonical) {
			t.Fatalf("iteration %d: serialization is not canonical\n want %s\n  got %s", i, canonical, got)
		}
	}
}

// TestNormalizeWorkspaceAdapter_KeepsAnInvalidInstallInput pins that
// normalization never rewrites an install input it cannot read: validation
// must still see the authored value.
func TestNormalizeWorkspaceAdapter_KeepsAnInvalidInstallInput(t *testing.T) {
	m := adapterManifest(WorkspaceAdapter{
		Markers:       []string{"package.json"},
		Inputs:        []string{"package.json"},
		InstallInputs: []string{"web/bun.lock", "bun.lock"},
	})
	authored := m.Workspace.InstallInputs
	NormalizeWorkspaceAdapter(m)
	if got, want := m.Workspace.InstallInputs, []string{"bun.lock", "web/bun.lock"}; !reflect.DeepEqual(got, want) {
		t.Errorf("install inputs = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(authored, []string{"web/bun.lock", "bun.lock"}) {
		t.Errorf("normalization wrote through the authored slice: %v", authored)
	}
	assertDiag(t, ValidateWorkspaceAdapter(m), "invalid-workspace-path", "workspace.installInputs[1]")
}

// TestWorkspaceAdapterDeclarationsNeedNoContractRung pins that the install
// inputs and the language are additive: they raise no load floor, so the
// packager stamps such a manifest at the base contract, and the load path
// reads it there. A reader without the members drops a declaration it has no
// use for and plans nothing differently.
func TestWorkspaceAdapterDeclarationsNeedNoContractRung(t *testing.T) {
	data := []byte(fmt.Sprintf(`{
		"name": "@test/go",
		"cliContract": %d,
		"workspace": {
			"markers": ["go.mod"],
			"inputs": ["go.mod", "go.work"],
			"installInputs": ["go.work"],
			"language": "go"
		},
		"commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
		"tasks": {"build": {"kind": "command", "command": "echo"}}
	}`, protocolcli.CurrentContract))
	m, err := NegotiateManifest("go.json", data)
	if err != nil {
		t.Fatalf("a base-contract manifest with the declarations did not load: %v", err)
	}
	if got := RequiredLoadContract(m); got != protocolcli.CurrentContract {
		t.Fatalf("the declarations raised the load floor to %d, want %d", got, protocolcli.CurrentContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
		t.Fatalf("the declarations raised the stamp to %d, want %d", got, protocolcli.CurrentContract)
	}
	if !reflect.DeepEqual(m.Workspace.InstallInputs, []string{"go.work"}) || m.Workspace.Language != "go" {
		t.Fatalf("loaded adapter = %+v", m.Workspace)
	}
}

// TestWorkspaceSectionMakesAManifestV3 pins that the adapter vocabulary is
// self-identifying, exactly as the runtime section and the task contract are.
func TestWorkspaceSectionMakesAManifestV3(t *testing.T) {
	m := &Manifest{
		Tasks: map[string]TaskDefinition{"build": {Kind: "command", Command: "echo"}},
		Workspace: &WorkspaceAdapter{
			Markers: []string{"go.mod"},
			Inputs:  []string{"go.mod"},
		},
	}
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV3 {
		t.Errorf("a manifest with a workspace adapter reports v%d, want v%d", got, ProtocolVersionV3)
	}
	m.Workspace = nil
	if got := ManifestProtocolVersion(m); got != ProtocolVersionV2 {
		t.Errorf("without the section the same manifest reports v%d, want v%d", got, ProtocolVersionV2)
	}
}
