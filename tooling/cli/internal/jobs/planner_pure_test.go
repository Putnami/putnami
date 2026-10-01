package jobs

import (
	"os"
	"path/filepath"
	"testing"

	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// --- toSet ---

func TestToSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   []string
		wantLen int
		has     []string
		notHas  []string
	}{
		{
			name:    "basic conversion",
			input:   []string{"a", "b", "c"},
			wantLen: 3,
			has:     []string{"a", "b", "c"},
			notHas:  []string{"d"},
		},
		{
			name:    "empty input",
			input:   []string{},
			wantLen: 0,
		},
		{
			name:    "deduplicates",
			input:   []string{"a", "a", "b"},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := toSet(tt.input)
			if len(s) != tt.wantLen {
				t.Errorf("toSet(%v) len = %d, want %d", tt.input, len(s), tt.wantLen)
			}
			for _, item := range tt.has {
				if !s[item] {
					t.Errorf("toSet missing item %q", item)
				}
			}
			for _, item := range tt.notHas {
				if s[item] {
					t.Errorf("toSet should not contain %q", item)
				}
			}
		})
	}
}

// --- hasActivationFiles ---

func TestHasActivationFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setupFn  func(t *testing.T) string
		patterns []string
		want     bool
	}{
		{
			name: "file exists",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644)
				return dir
			},
			patterns: []string{"package.json"},
			want:     true,
		},
		{
			name: "file absent",
			setupFn: func(t *testing.T) string {
				return t.TempDir()
			},
			patterns: []string{"package.json"},
			want:     false,
		},
		{
			name: "no patterns",
			setupFn: func(t *testing.T) string {
				return t.TempDir()
			},
			patterns: []string{},
			want:     false,
		},
		{
			name: "glob pattern",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
				return dir
			},
			patterns: []string{"*.go"},
			want:     true,
		},
		{
			name: "multiple patterns second matches",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644)
				return dir
			},
			patterns: []string{"package.json", "go.mod"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setupFn(t)
			got := hasActivationFiles(dir, tt.patterns, nil)
			if got != tt.want {
				t.Errorf("hasActivationFiles(%v) = %v, want %v", tt.patterns, got, tt.want)
			}
		})
	}
}

// TestHasActivationFiles_CacheReusesResult exercises the planCache fast
// path: a second hasActivationFiles call with the same (root, patterns)
// reuses the cached result and skips the filesystem walk. We verify by
// pre-populating the cache with a known answer that disagrees with the
// disk, then asserting the cached answer wins.
func TestHasActivationFiles_CacheReusesResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cache := newPlanCache()
	// Disk has no matching file, but pre-seed the cache with `true`.
	cache.hasFiles[dir+"\x00"+"*.go"] = true
	if !hasActivationFiles(dir, []string{"*.go"}, cache) {
		t.Error("cached answer should win over filesystem state")
	}

	// Same call with no cache must hit the filesystem and return false.
	if hasActivationFilesUncached(dir, []string{"*.go"}) {
		t.Error("expected miss on empty dir without a cached answer")
	}
}

// TestExtensionActivatesForProject_CacheReusesResult covers the higher-
// level wrapper: extensionActivatesForProject results are reused across
// matchJob calls for the same (extension, project) pair.
func TestExtensionActivatesForProject_CacheReusesResult(t *testing.T) {
	t.Parallel()
	cache := newPlanCache()
	cache.extActivates["@x/ts\x00/tmp/proj"] = true

	got := extensionActivatesForProject("@x/ts", "/tmp/proj", map[string][]*extension.JobDefinition{}, cache)
	if !got {
		t.Error("cached extActivates result should be returned")
	}
}

// --- matchDoubleStarGlob ---

func TestMatchDoubleStarGlob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setupFn func(t *testing.T) string
		pattern string
		want    bool
	}{
		{
			name: "top level match",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "test.ts"), []byte(""), 0o644)
				return dir
			},
			pattern: "**/*.ts",
			want:    true,
		},
		{
			name: "nested match",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				sub := filepath.Join(dir, "src", "components")
				os.MkdirAll(sub, 0o755)
				os.WriteFile(filepath.Join(sub, "Button.test.ts"), []byte(""), 0o644)
				return dir
			},
			pattern: "**/*.test.ts",
			want:    true,
		},
		{
			name: "no match",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "main.go"), []byte(""), 0o644)
				return dir
			},
			pattern: "**/*.ts",
			want:    false,
		},
		{
			name: "empty dir",
			setupFn: func(t *testing.T) string {
				return t.TempDir()
			},
			pattern: "**/*.go",
			want:    false,
		},
		{
			name: "skips node_modules",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				nm := filepath.Join(dir, "node_modules", "pkg")
				os.MkdirAll(nm, 0o755)
				os.WriteFile(filepath.Join(nm, "index.ts"), []byte(""), 0o644)
				return dir
			},
			pattern: "**/*.ts",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setupFn(t)
			got := matchDoubleStarGlob(dir, tt.pattern)
			if got != tt.want {
				t.Errorf("matchDoubleStarGlob(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

// --- mergeCommandParams ---

func makeMinimalWorkspace() *workspace.Workspace {
	ws := workspace.NewWorkspace("/ws", &wsproto.Config{
		Options: map[string]map[string]any{},
	}, nil)
	ws.Name = "test-ws"
	return ws
}

func TestMergeCommandParams_CLIFlagsHighestPriority(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	ws.Config.Options["build"] = map[string]any{"target": "default"}

	proj := &workspace.Project{
		Name: "my-app",
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				"build": {"target": "project-default"},
			},
		},
	}
	ext := &extension.ExtensionDescription{Name: "@test/go"}
	jobDef := &extension.JobDefinition{
		Name: "build",
		Flags: map[string]extension.FlagDefinition{
			"target": {Default: "manifest-default"},
		},
	}
	cliFlags := map[string]any{"target": "cli-value"}

	params := mergeCommandParams(ws, proj, ext, "build", jobDef, cliFlags)
	if params["target"] != "cli-value" {
		t.Errorf("CLI flags should win, got %v", params["target"])
	}
}

func TestMergeCommandParams_ManifestDefaults(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	proj := &workspace.Project{Name: "my-app"}
	ext := &extension.ExtensionDescription{Name: "@test/go"}
	jobDef := &extension.JobDefinition{
		Name: "build",
		Flags: map[string]extension.FlagDefinition{
			"verbose": {Default: false},
			"target":  {Default: "linux/amd64"},
		},
	}

	params := mergeCommandParams(ws, proj, ext, "build", jobDef, nil)
	if params["target"] != "linux/amd64" {
		t.Errorf("manifest default target = %v, want linux/amd64", params["target"])
	}
	if params["verbose"] != false {
		t.Errorf("manifest default verbose = %v, want false", params["verbose"])
	}
}

func TestMergeCommandParams_ProjectOptionsOverrideManifest(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	proj := &workspace.Project{
		Name: "my-app",
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				"build": {"target": "darwin/arm64"},
			},
		},
	}
	ext := &extension.ExtensionDescription{Name: "@test/go"}
	jobDef := &extension.JobDefinition{
		Name: "build",
		Flags: map[string]extension.FlagDefinition{
			"target": {Default: "linux/amd64"},
		},
	}

	params := mergeCommandParams(ws, proj, ext, "build", jobDef, nil)
	if params["target"] != "darwin/arm64" {
		t.Errorf("project option should override manifest default, got %v", params["target"])
	}
}

func TestMergeCommandParams_NilProjectConfig(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	proj := &workspace.Project{Name: "my-app", Config: nil}
	ext := &extension.ExtensionDescription{Name: "@test/go"}
	jobDef := &extension.JobDefinition{Name: "build"}

	// Should not panic
	params := mergeCommandParams(ws, proj, ext, "build", jobDef, map[string]any{"key": "val"})
	if params["key"] != "val" {
		t.Errorf("CLI flag should still be set, got %v", params["key"])
	}
}

func TestMergeCommandParams_PublishChannelsOverrideManifestDefaults(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	proj := &workspace.Project{
		Name:    "go.putnami.dev/app",
		Publish: []string{"go"},
		Config:  &wsproto.ProjectConfig{},
	}
	ext := &extension.ExtensionDescription{Name: "@putnami/go"}
	jobDef := &extension.JobDefinition{
		Name:   "package",
		Traits: extension.CommandTraits{InjectPublishChannels: true},
		Flags: map[string]extension.FlagDefinition{
			"go":       {Default: false},
			"archives": {Default: false},
			"docker":   {Default: false},
		},
	}

	params := mergeCommandParams(ws, proj, ext, "package", jobDef, nil)
	if params["go"] != true {
		t.Errorf("publish channel 'go' should override manifest default false, got %v", params["go"])
	}
	if params["archives"] != false {
		t.Errorf("archives should remain false (not in publish channels), got %v", params["archives"])
	}
}

func TestMergeCommandParams_ExtensionColonCommand(t *testing.T) {
	t.Parallel()
	ws := makeMinimalWorkspace()
	proj := &workspace.Project{
		Name: "my-app",
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				"@test/go:build": {"workers": 4},
			},
		},
	}
	ext := &extension.ExtensionDescription{Name: "@test/go"}
	jobDef := &extension.JobDefinition{Name: "build"}

	params := mergeCommandParams(ws, proj, ext, "build", jobDef, nil)
	if params["workers"] != 4 {
		t.Errorf("@ext:cmd option should be applied, got %v", params["workers"])
	}
}

// --- resolved run selection ---

// ResolvedRunSelection is the one place the wire block is built for a planned
// run. It sorts, derives `scoped`, and passes the resolution evidence through.
func TestResolvedRunSelection(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/libs/widget"},
		nil,
		{ID: "/apps/console"},
	}
	selection := ResolvedRunSelection(
		protocoljob.SelectionModeImpacted, "origin/main", "trunk", projects)

	if selection.Mode != protocoljob.SelectionModeImpacted {
		t.Errorf("mode = %q, want impacted", selection.Mode)
	}
	if !selection.Scoped {
		t.Error("an impacted run is scoped")
	}
	if selection.Baseline != "origin/main" || selection.BaselineSource != "trunk" {
		t.Errorf("baseline = %q/%q, want origin/main/trunk", selection.Baseline, selection.BaselineSource)
	}
	// Sorted, and a nil member locates nothing so it contributes no id.
	if len(selection.ProjectIDs) != 2 ||
		selection.ProjectIDs[0] != "/apps/console" || selection.ProjectIDs[1] != "/libs/widget" {
		t.Errorf("projects = %v, want the sorted ids", selection.ProjectIDs)
	}

	// The whole-workspace projection is the one mode that is not scoped, and an
	// empty selection is an empty array rather than a missing one — the contract
	// rejects null.
	unscoped := ResolvedRunSelection(protocoljob.SelectionModeAll, "", "", nil)
	if unscoped.Scoped {
		t.Error("the whole-workspace projection is not a narrowing")
	}
	if unscoped.ProjectIDs == nil {
		t.Error("projects must be an empty array, never null")
	}
}

// AttachRunSelection stamps EVERY node, unlike attachSelectedProjects: a
// project-scoped validator reporting on the workspace has to know the run was
// narrowed just as much as a workspace-scoped one does.
func TestAttachRunSelection_StampsEveryPlannedNode(t *testing.T) {
	t.Parallel()
	selection := &protocoljob.Selection{
		Mode: protocoljob.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/a"},
	}
	planned := []*ScheduledJob{
		{JobDef: &extension.JobDefinition{Name: "build"}},
		nil,
		{JobDef: &extension.JobDefinition{Name: "validate"}},
	}
	AttachRunSelection(planned, selection)
	for i, job := range planned {
		if job == nil {
			continue
		}
		if job.Selection != selection {
			t.Errorf("planned[%d].Selection = %+v, want the run's resolved selection", i, job.Selection)
		}
	}

	// A run that resolved nothing stamps nothing: an absent member is the honest
	// answer, and a consumer must not receive an invented selection.
	unstamped := []*ScheduledJob{{JobDef: &extension.JobDefinition{Name: "build"}}}
	AttachRunSelection(unstamped, nil)
	if unstamped[0].Selection != nil {
		t.Errorf("Selection = %+v, want nil when the run resolved none", unstamped[0].Selection)
	}
}
