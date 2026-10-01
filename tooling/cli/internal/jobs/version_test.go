package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A project takes the version of ITS line, not one answer for the run.
func TestVersionInfoForProject_ResolvesTheProjectsLine(t *testing.T) {
	t.Parallel()
	versions := RunVersions{
		"typescript": &JobContextVersion{Base: "0.4.0", Full: "0.4.0-abc1234", Line: "typescript"},
		"go":         &JobContextVersion{Base: "1.2.0", Full: "1.2.0-abc1234", Line: "go"},
	}
	got := VersionInfoForProject(versions, &workspace.Project{Line: "go"})
	if got == nil || got.Full != "1.2.0-abc1234" {
		t.Fatalf("version = %+v, want the go line's", got)
	}
	if base := LineBaseVersion(versions, &workspace.Project{Line: "typescript"}); base != "0.4.0" {
		t.Errorf("LineBaseVersion = %q, want the typescript line's base", base)
	}
}

// A project outside every declared line falls back to the root line, which is
// the answer a workspace that declares no line gives every project.
func TestVersionInfoForProject_FallsBackToTheRootLine(t *testing.T) {
	t.Parallel()
	versions := RunVersions{"": &JobContextVersion{Base: "0.0.0", Full: "0.0.0-abc1234"}}
	got := VersionInfoForProject(versions, &workspace.Project{Line: "unknown"})
	if got == nil || got.Full != "0.0.0-abc1234" {
		t.Fatalf("version = %+v, want the root line's", got)
	}
	if base := LineBaseVersion(nil, &workspace.Project{}); base != "" {
		t.Errorf("LineBaseVersion without versions = %q, want none", base)
	}
	if info := VersionInfoForProject(nil, nil); info != nil {
		t.Errorf("VersionInfoForProject without versions = %+v, want nil", info)
	}
}

// Primary is the reporting answer for a whole run: the root line when there is
// one, else the first line in sorted order, so two runs over one workspace file
// their report the same way.
func TestRunVersionsPrimary(t *testing.T) {
	t.Parallel()
	root := &JobContextVersion{Base: "1.0.0"}
	if got := (RunVersions{"": root, "go": {Base: "2.0.0"}}).Primary(); got != root {
		t.Errorf("Primary = %+v, want the root line's", got)
	}
	first := &JobContextVersion{Base: "2.0.0", Line: "go"}
	if got := (RunVersions{"typescript": {Base: "3.0.0"}, "go": first}).Primary(); got != first {
		t.Errorf("Primary = %+v, want the first line in sorted order", got)
	}
	if got := (RunVersions{}).Primary(); got != nil {
		t.Errorf("Primary of nothing = %+v, want nil", got)
	}
}

func TestGenerateVersionFiles_WritesFullVersion(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projPath := "services/auth"
	if err := os.MkdirAll(filepath.Join(wsRoot, projPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ws := &workspace.Workspace{Name: "ws", Root: wsRoot}
	proj := &workspace.Project{Name: "auth/server", Path: projPath}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "ext"},
		JobDef:    &extension.JobDefinition{Name: "publish"},
	}

	versionInfo := rootLineVersions(&JobContextVersion{
		Base:    "0.0.0",
		Full:    "0.0.0-b49fb6f.81b85cc",
		SHA:     "b49fb6f",
		Branch:  "feature/x",
		Suffix:  "b49fb6f.81b85cc",
		IsDirty: true,
	})

	generateVersionFilesAt(ws, []*ScheduledJob{job}, versionInfo, "2026-07-20T12:00:00Z", false)

	data, err := os.ReadFile(filepath.Join(wsRoot, projPath, ".gen", "version.json"))
	if err != nil {
		t.Fatalf("read version.json: %v", err)
	}

	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse version.json: %v", err)
	}

	if got.Version != "0.0.0-b49fb6f.81b85cc" {
		t.Errorf("Version = %q, want 0.0.0-b49fb6f.81b85cc (matches docker tag)", got.Version)
	}
	if got.Suffix != "b49fb6f.81b85cc" {
		t.Errorf("Suffix = %q, want b49fb6f.81b85cc", got.Suffix)
	}
	if got.SHA != "b49fb6f" {
		t.Errorf("SHA = %q, want b49fb6f", got.SHA)
	}
	if !got.IsDirty {
		t.Error("IsDirty = false, want true")
	}
	if got.Branch != "feature/x" {
		t.Errorf("Branch = %q, want feature/x", got.Branch)
	}
	if got.Name != "auth/server" {
		t.Errorf("Name = %q, want auth/server", got.Name)
	}
}

func TestPreserveMatchingVersionFilesKeepsCachedBuildTime(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projPath := "services/auth"
	if err := os.MkdirAll(filepath.Join(wsRoot, projPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ws := &workspace.Workspace{Name: "ws", Root: wsRoot}
	job := &ScheduledJob{
		Project:   &workspace.Project{Name: "auth", Path: projPath},
		Extension: &extension.ExtensionDescription{Name: "ext"},
		JobDef:    &extension.JobDefinition{Name: "build"},
	}
	version := rootLineVersions(&JobContextVersion{SHA: "b49fb6f", Branch: "main", Suffix: "b49fb6f"})
	oldBuildTime := "2026-07-20T10:00:00Z"
	newBuildTime := "2026-07-20T12:00:00Z"

	generateVersionFilesAt(ws, []*ScheduledJob{job}, version, oldBuildTime, false)
	preserveMatchingVersionFiles(ws, []*ScheduledJob{job}, version, newBuildTime)

	path := filepath.Join(wsRoot, projPath, ".gen", "version.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.BuildTime != oldBuildTime {
		t.Fatalf("BuildTime = %q, want cached artifact time %q", got.BuildTime, oldBuildTime)
	}

	generateVersionFilesAt(ws, []*ScheduledJob{job}, version, newBuildTime, false)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.BuildTime != newBuildTime {
		t.Fatalf("BuildTime after real execution refresh = %q, want %q", got.BuildTime, newBuildTime)
	}
}

func TestGenerateVersionFiles_UsesTheProjectsLineVersion(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projPath := "packages/lib"
	if err := os.MkdirAll(filepath.Join(wsRoot, projPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ws := &workspace.Workspace{Name: "ws", Root: wsRoot}
	proj := &workspace.Project{Name: "lib", Path: projPath}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "ext"},
		JobDef:    &extension.JobDefinition{Name: "build"},
	}

	versionInfo := rootLineVersions(&JobContextVersion{
		Base: "1.2.3", Full: "1.2.3-abc1234", SHA: "abc1234", Suffix: "abc1234"})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, versionInfo, "2026-07-20T12:00:00Z", false)

	data, err := os.ReadFile(filepath.Join(wsRoot, projPath, ".gen", "version.json"))
	if err != nil {
		t.Fatalf("read version.json: %v", err)
	}

	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse version.json: %v", err)
	}

	if got.Version != "1.2.3-abc1234" {
		t.Errorf("Version = %q, want the line version 1.2.3-abc1234", got.Version)
	}
	if len(got.CapabilityPackages) != 1 || got.CapabilityPackages[0].Package != "lib" || got.CapabilityPackages[0].Version != "1.2.3" {
		t.Fatalf("CapabilityPackages = %+v, want stable self package lib@1.2.3", got.CapabilityPackages)
	}
}

func TestGenerateVersionFiles_StampsTransitiveCapabilityPackageGraph(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projects := []*workspace.Project{
		{Name: "workload", Path: "apps/workload", Dependencies: []string{"feature"}},
		{Name: "feature", Path: "libs/feature", Dependencies: []string{"framework"}},
		{Name: "framework", Path: "libs/framework"},
	}
	for _, project := range projects {
		if err := os.MkdirAll(filepath.Join(wsRoot, project.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wsRoot, project.Path, "putnami.json"), []byte(`{"name":"`+project.Name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := &workspace.Workspace{Name: "ws", Root: wsRoot, Projects: projects}
	job := &ScheduledJob{Project: projects[0], Extension: &extension.ExtensionDescription{Name: "ext"}, JobDef: &extension.JobDefinition{Name: "build"}}

	// One line, so every project of the closure is stamped at one version: a
	// project no longer declares one of its own.
	versions := rootLineVersions(&JobContextVersion{Base: "2.0.0", Full: "2.0.0-abc1234", Suffix: "abc1234"})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, versions, "2026-07-20T12:00:00Z", false)
	data, err := os.ReadFile(filepath.Join(wsRoot, projects[0].Path, ".gen", "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.CapabilityPackages) != 3 {
		t.Fatalf("CapabilityPackages = %+v, want self + transitive dependencies", got.CapabilityPackages)
	}
	want := []CapabilityPackageStamp{
		{Package: "feature", Version: "2.0.0", EvidencePath: "libs/feature/putnami.json", SourceRoot: "libs/feature", CapabilityManifestPath: "../../libs/feature/schema/capabilities.json"},
		{Package: "framework", Version: "2.0.0", EvidencePath: "libs/framework/putnami.json", SourceRoot: "libs/framework", CapabilityManifestPath: "../../libs/framework/schema/capabilities.json"},
		{Package: "workload", Version: "2.0.0", EvidencePath: "apps/workload/putnami.json", SourceRoot: "apps/workload"},
	}
	for i := range want {
		if got.CapabilityPackages[i].SourceBindingUnavailable {
			if got.CapabilityPackages[i].SourceBinding != "" {
				t.Errorf("CapabilityPackages[%d] stamped a binding while marking it unavailable", i)
			}
		} else if !strings.HasPrefix(got.CapabilityPackages[i].SourceBinding, "source-v1:sha256:") {
			t.Errorf("CapabilityPackages[%d].SourceBinding = %q, want a source-v1 binding", i, got.CapabilityPackages[i].SourceBinding)
		}
		want[i].SourceBinding = got.CapabilityPackages[i].SourceBinding
		want[i].SourceBindingUnavailable = got.CapabilityPackages[i].SourceBindingUnavailable
		if got.CapabilityPackages[i] != want[i] {
			t.Errorf("CapabilityPackages[%d] = %+v, want %+v", i, got.CapabilityPackages[i], want[i])
		}
	}
}

// TestGenerateVersionFiles_CapabilityStampsCoverWorkspaceProjectsOnly pins the
// scheduler's half of the capability ownership contract (CapabilityPackageStamp):
// a declared dependency that resolves to no workspace project — a released
// framework module such as go.putnami.dev/events, consumed from the package
// manager — is deliberately absent from the stamp, and the workspace projects
// around it still resolve exactly. Emitters resolve the absent half from the
// artifact's own package graph, so a fabricated entry here would be worse than
// no entry: it would carry a source root and a version nothing can bind.
func TestGenerateVersionFiles_CapabilityStampsCoverWorkspaceProjectsOnly(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projects := []*workspace.Project{
		{Name: "workload", Path: "apps/workload", Version: "2.0.0", Dependencies: []string{"go.putnami.dev/events", "feature"}},
		{Name: "feature", Path: "libs/feature", Version: "1.4.0", Dependencies: []string{"go.putnami.dev/app"}},
	}
	for _, project := range projects {
		if err := os.MkdirAll(filepath.Join(wsRoot, project.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wsRoot, project.Path, "putnami.json"), []byte(`{"name":"`+project.Name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := &workspace.Workspace{Name: "ws", Root: wsRoot, Projects: projects}
	job := &ScheduledJob{Project: projects[0], Extension: &extension.ExtensionDescription{Name: "ext"}, JobDef: &extension.JobDefinition{Name: "build"}}

	// One line, so every project of the closure is stamped at one version: a
	// project no longer declares one of its own.
	versions := rootLineVersions(&JobContextVersion{Base: "2.0.0", Full: "2.0.0-abc1234", Suffix: "abc1234"})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, versions, "2026-07-20T12:00:00Z", false)
	data, err := os.ReadFile(filepath.Join(wsRoot, projects[0].Path, ".gen", "version.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	stamped := make([]string, 0, len(got.CapabilityPackages))
	for _, stamp := range got.CapabilityPackages {
		stamped = append(stamped, stamp.Package)
		if stamp.SourceRoot == "" || stamp.EvidencePath == "" {
			t.Errorf("stamped package %q has no workspace source: %+v", stamp.Package, stamp)
		}
	}
	if !reflect.DeepEqual(stamped, []string{"feature", "workload"}) {
		t.Fatalf("capability packages = %v, want exactly the workspace projects", stamped)
	}
}

func TestGenerateVersionFiles_NoGitMetadata(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projPath := "pkg"
	if err := os.MkdirAll(filepath.Join(wsRoot, projPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ws := &workspace.Workspace{Name: "ws", Root: wsRoot}
	proj := &workspace.Project{Name: "pkg", Path: projPath}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "ext"},
		JobDef:    &extension.JobDefinition{Name: "build"},
	}

	generateVersionFilesAt(ws, []*ScheduledJob{job}, nil, "2026-07-20T12:00:00Z", false)

	data, err := os.ReadFile(filepath.Join(wsRoot, projPath, ".gen", "version.json"))
	if err != nil {
		t.Fatalf("read version.json: %v", err)
	}

	var got VersionInfo
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse version.json: %v", err)
	}

	if got.Version != "0.0.0" {
		t.Errorf("Version = %q, want 0.0.0 fallback when no git metadata", got.Version)
	}
	if got.Suffix != "" {
		t.Errorf("Suffix = %q, want empty when no git metadata", got.Suffix)
	}
}

// Two consecutive runs of an unchanged tree must compute the same cache key for
// a task that globs the .gen/version.json stamp — otherwise the entry a run
// stores is unreachable by the next one.
//
// This models the exact ordering the scheduler uses (scheduler.go:197 seeds the
// stamps, scheduler_exec.go computes the key, and only then refreshes the stamp
// of a project that is about to execute), starting from a project that already
// carries an earlier run's stamp. Hashing buildTime made every run key on the
// previous run's stamp while leaving the current one on disk, so a project that
// missed once missed forever.
func TestCacheKeyStableAcrossVersionFileRefresh(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.Root, "proj", "src.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	version := rootLineVersions(&JobContextVersion{SHA: "b49fb6f", Branch: "main", Suffix: "b49fb6f"})
	mkJob := func() *ScheduledJob {
		return &ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name:          "lint",
				ExtensionName: "@test/ext",
				Cache:         true,
				// TypeScript lint globs JSON, which reaches .gen/version.json.
				FilePatterns: []string{"**/*.ts", "**/*.json"},
			},
		}
	}
	localStore := store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store"))
	// A fresh CacheManager per run: each real run is a new process, so a run must
	// not inherit the previous one's memoized file hashes.
	keyFor := func(job *ScheduledJob) string {
		t.Helper()
		hash, err := computeJobCacheHash(ws, job, nil, version, store.NewCacheManager(localStore), nil)
		if err != nil {
			t.Fatalf("compute cache key: %v", err)
		}
		return hash
	}

	// An earlier run left this project's stamp behind.
	generateVersionFilesAt(ws, []*ScheduledJob{mkJob()}, version, "2026-07-20T10:00:00Z", false)

	// Run A: seed stamps, compute the key, then miss and refresh before executing.
	// The entry is stored under keyA.
	jobA := mkJob()
	preserveMatchingVersionFiles(ws, []*ScheduledJob{jobA}, version, "2026-07-20T11:00:00Z")
	keyA := keyFor(jobA)
	generateVersionFilesAt(ws, []*ScheduledJob{jobA}, version, "2026-07-20T11:00:00Z", false)

	// Run B: same tree, later wall clock. It must look up the key run A stored.
	jobB := mkJob()
	preserveMatchingVersionFiles(ws, []*ScheduledJob{jobB}, version, "2026-07-20T12:00:00Z")
	keyB := keyFor(jobB)

	if keyA != keyB {
		t.Fatalf("cache key moved between runs of an unchanged tree (%s → %s): the entry run A stored can never be hit", keyA, keyB)
	}

	// The stamp must still carry real invalidation: a new commit changes the key.
	newVersion := rootLineVersions(&JobContextVersion{SHA: "c51ac7e", Branch: "main", Suffix: "c51ac7e"})
	jobC := mkJob()
	preserveMatchingVersionFiles(ws, []*ScheduledJob{jobC}, newVersion, "2026-07-20T13:00:00Z")
	hash, err := computeJobCacheHash(ws, jobC, nil, newVersion, store.NewCacheManager(localStore), nil)
	if err != nil {
		t.Fatalf("compute cache key: %v", err)
	}
	if hash == keyA {
		t.Error("a new commit sha must still invalidate a task keyed on the version stamp")
	}
}

// TestVersionStampOwnedFieldsMatchVersionInfo keeps the closed set of
// scheduler-owned stamp keys in lockstep with the struct that produces them.
//
// The set decides two opposite things: an owned key is authoritatively
// overwritten (and REMOVED when omitempty drops it), while an unowned key is
// carried through untouched. A new VersionInfo field that is missing from the
// set would be treated as another writer's property — written once and then
// preserved forever, never updated — and a stale name left in the set would
// delete a field the extensions legitimately own.
func TestVersionStampOwnedFieldsMatchVersionInfo(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(VersionInfo{})
	fromStruct := make(map[string]bool, typ.NumField())
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("VersionInfo field %q has no JSON name; the stamp's key set cannot be derived",
				typ.Field(i).Name)
		}
		fromStruct[name] = true
	}
	if !reflect.DeepEqual(fromStruct, versionStampOwnedFields) {
		t.Fatalf("versionStampOwnedFields = %v, want the VersionInfo JSON names %v", versionStampOwnedFields, fromStruct)
	}
}

// TestGenerateVersionFiles_PreservesExtensionOwnedStampFields pins the stamp as a
// SHARED document. The TypeScript build-generate task merges `contentHash` into
// it (build.UpdateVersionContentHash) and the running app reads that value back
// to namespace its disk cache; docker publish merges a `publish` object the same
// way. A scheduler write that marshaled its struct over the whole file destroyed
// both — and since generation became content-keyed, a generation cache hit can
// re-stamp without generate ever running again to restore them.
func TestGenerateVersionFiles_PreservesExtensionOwnedStampFields(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projPath := "services/web"
	if err := os.MkdirAll(filepath.Join(wsRoot, projPath), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := &workspace.Workspace{Name: "ws", Root: wsRoot}
	job := &ScheduledJob{
		Project:   &workspace.Project{Name: "web", Path: projPath},
		Extension: &extension.ExtensionDescription{Name: "ext"},
		JobDef:    &extension.JobDefinition{Name: "build"},
	}
	stampPath := filepath.Join(wsRoot, projPath, ".gen", "version.json")

	readDoc := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(stampPath)
		if err != nil {
			t.Fatalf("read stamp: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse stamp %s: %v", data, err)
		}
		return doc
	}

	// A producing run: the scheduler stamps the identity, then the extension
	// merges in the fields it owns.
	first := rootLineVersions(&JobContextVersion{Base: "0.0.0", Full: "0.0.0-aaaa111", SHA: "aaaa111", Branch: "main", Suffix: "aaaa111"})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, first, "2026-07-30T10:00:00Z", false)
	doc := readDoc()
	doc["contentHash"] = "sha256:cafebabe"
	doc["publish"] = map[string]any{"version": "0.0.0-aaaa111", "channels": []any{"latest"}}
	merged, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stampPath, append(merged, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. A preserve-matching seed at the SAME identity must not touch the file at
	//    all, so the extension fields are trivially intact.
	preserveMatchingVersionFiles(ws, []*ScheduledJob{job}, first, "2026-07-31T09:00:00Z")
	doc = readDoc()
	if doc["contentHash"] != "sha256:cafebabe" {
		t.Fatalf("preserve-matching seed dropped contentHash: %v", doc)
	}
	if doc["buildTime"] != "2026-07-30T10:00:00Z" {
		t.Fatalf("preserve-matching seed churned buildTime: %v", doc["buildTime"])
	}

	// 2. An identity-CHANGING rewrite — the seed on a new commit, and the
	//    post-restore re-stamp — must update every owned field, drop an owned
	//    field that is now empty, and carry the unowned ones through.
	//    The new commit has no suffix, so `suffix` must VANISH rather than linger
	//    as the previous commit's identity.
	second := rootLineVersions(&JobContextVersion{Base: "0.0.0", Full: "0.0.0", SHA: "bbbb222", Branch: "release"})
	generateVersionFilesAt(ws, []*ScheduledJob{job}, second, "2026-07-31T09:00:00Z", true)
	doc = readDoc()

	if doc["contentHash"] != "sha256:cafebabe" {
		t.Errorf("contentHash did not survive the re-stamp: %v — the app's disk-cache namespace silently changes", doc)
	}
	publish, ok := doc["publish"].(map[string]any)
	if !ok || publish["version"] != "0.0.0-aaaa111" {
		t.Errorf("the publish overlay did not survive the re-stamp: %v", doc["publish"])
	}
	if doc["sha"] != "bbbb222" || doc["branch"] != "release" {
		t.Errorf("owned identity fields were not updated: sha=%v branch=%v", doc["sha"], doc["branch"])
	}
	if doc["version"] != "0.0.0" {
		t.Errorf("version = %v, want the suffix-less 0.0.0 of the new commit", doc["version"])
	}
	if doc["buildTime"] != "2026-07-31T09:00:00Z" {
		t.Errorf("buildTime = %v, want the rewriting run's", doc["buildTime"])
	}
	if _, lingers := doc["suffix"]; lingers {
		t.Errorf("the previous commit's suffix lingered after an emptied rewrite: %v", doc["suffix"])
	}

	// 3. A preserved unowned field must not, by itself, force a rewrite:
	//    versionFileMatchesBuild decodes into VersionInfo and cannot see it.
	preserveMatchingVersionFiles(ws, []*ScheduledJob{job}, second, "2026-08-01T09:00:00Z")
	if got := readDoc(); got["buildTime"] != "2026-07-31T09:00:00Z" {
		t.Errorf("a carried-through field made an identical stamp look stale: buildTime = %v", got["buildTime"])
	}
}

func TestSchedulerCapabilitySourceBindingsMemoizedAcrossFullyCachedRestamps(t *testing.T) {
	const projectCount = 22
	ws := makeExecutorTestWorkspace(t)
	runVersionsGit(t, ws.Root, "init")
	projects := make([]*workspace.Project, 0, projectCount)
	jobs := make([]*ScheduledJob, 0, projectCount)
	for i := range projectCount {
		name := fmt.Sprintf("project-%02d", i)
		project := &workspace.Project{ID: "/" + name, Name: name, Path: name}
		if i > 0 {
			project.Dependencies = []string{projects[i-1].Name}
		}
		if err := os.MkdirAll(filepath.Join(ws.Root, project.Path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFileAt(t, filepath.Join(ws.Root, project.Path, "source.go"), "package fixture\n")
		projects = append(projects, project)
		jobs = append(jobs, &ScheduledJob{
			Project:   project,
			Extension: &extension.ExtensionDescription{Name: "@test/ext"},
			JobDef:    &extension.JobDefinition{Name: "build~generate"},
		})
	}
	ws.Projects = projects

	scheduler := &Scheduler{
		ws: ws, planned: jobs, renderer: &mockRenderer{}, versionFiles: make(map[string]bool),
		capabilitySourceBindings: newCapabilitySourceBindingMemo(),
		cacheStats:               &CacheStats{},
	}
	calls := make(map[string]int)
	resolve := scheduler.capabilitySourceBindings.resolve
	scheduler.capabilitySourceBindings.resolve = func(repoRoot, projectRoot string) (string, int, error) {
		calls[filepath.Clean(projectRoot)]++
		return resolve(repoRoot, projectRoot)
	}

	// The seed plus five rounds of post-.gen-restore restamps model more than the
	// 121 generate/restamp calls in the reported all-hit session. .gen is excluded
	// from source-v1, so those restamps must all reuse the session memo.
	scheduler.prepareRun()
	entry := &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
		ID: "gen", Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject,
		Path: ".gen", State: store.TaskOutputPresent,
	}}}
	for range 5 {
		for _, job := range jobs {
			scheduler.invalidateCapabilitySourceBindingsForRestore(job, entry)
			scheduler.invalidateCapabilitySourceBindingsForRestore(job, entry)
			scheduler.restampRestoredVersionStamp(job, entry)
		}
	}

	total := 0
	for _, project := range projects {
		root := filepath.Join(ws.Root, project.Path)
		if got := calls[root]; got != 1 {
			t.Errorf("ProjectSourceBinding calls for %s = %d, want 1", project.Name, got)
		}
		for _, stamp := range readVersionStamp(t, root).CapabilityPackages {
			if stamp.SourceBindingUnavailable || !strings.HasPrefix(stamp.SourceBinding, "source-v1:sha256:") {
				t.Fatalf("invalid real source binding for %s: %+v", project.Name, stamp)
			}
		}
		total += calls[root]
	}
	if total != projectCount {
		t.Fatalf("ProjectSourceBinding calls = %d, want %d", total, projectCount)
	}
	if got := scheduler.cacheStats.Snapshot().LocalSpawnedProcesses; got != 2 {
		t.Fatalf("reported source-binding processes = %d, want 2 repository-wide ls-files calls", got)
	}
}

func TestSchedulerCapabilitySourceBindingInvalidatedAfterTrackedSidecarWrite(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	runVersionsGit(t, ws.Root, "init")
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	ws.Projects = []*workspace.Project{project}
	describe := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: "build~describe"},
	}
	sidecar := filepath.Join(ws.Root, project.Path, "schema", "openapi.json")
	writeFileAt(t, sidecar, "before describe\n")
	runVersionsGit(t, ws.Root, "add", "proj/schema/openapi.json")

	scheduler := &Scheduler{
		ws: ws, planned: []*ScheduledJob{describe}, renderer: &mockRenderer{}, versionFiles: make(map[string]bool),
		capabilitySourceBindings: newCapabilitySourceBindingMemo(),
	}
	calls := 0
	resolve := scheduler.capabilitySourceBindings.resolve
	scheduler.capabilitySourceBindings.resolve = func(repoRoot, projectRoot string) (string, int, error) {
		calls++
		return resolve(repoRoot, projectRoot)
	}

	scheduler.prepareRun()
	before := readVersionStamp(t, filepath.Join(ws.Root, project.Path)).CapabilityPackages[0].SourceBinding

	// Go describe commits source-v1-visible schema sidecars after the seed pass.
	// finalizeExecutedJob is the convergence point for singleton and batched real
	// executions, so it must evict the pre-write binding before a later cache-hit
	// restamp constructs the inventory again.
	writeFileAt(t, sidecar, "after describe\n")
	writeFileAt(t, filepath.Join(ws.Root, project.Path, "schema", "new.json"), "new untracked sidecar\n")
	var hashesMu sync.Mutex
	scheduler.finalizeExecutedJob(
		t.Context(), describe, &JobResult{Status: "success"}, false, "", "", &hashesMu, map[string]string{},
	)
	entry := &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
		ID: "gen", Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject,
		Path: ".gen", State: store.TaskOutputPresent,
	}}}
	scheduler.restampRestoredVersionStamp(describe, entry)
	after := readVersionStamp(t, filepath.Join(ws.Root, project.Path)).CapabilityPackages[0].SourceBinding

	if after == before {
		t.Fatalf("restamped source binding stayed %q after describe changed a tracked sidecar", after)
	}
	want, _, err := putnamigit.ProjectSourceBindingMeasured(ws.Root, filepath.Join(ws.Root, project.Path))
	if err != nil || after != want {
		t.Fatalf("post-execution binding = %q, want fresh enumeration %q: %v", after, want, err)
	}
	if calls != 2 {
		t.Fatalf("ProjectSourceBinding calls = %d, want seed + one post-write recomputation", calls)
	}
}

func TestSchedulerCapabilitySourceBindingSnapshotInvalidatedAroundRestore(t *testing.T) {
	for _, root := range []string{extension.OutputRootProject, extension.OutputRootWorkspace} {
		t.Run(root, func(t *testing.T) {
			ws := makeExecutorTestWorkspace(t)
			runVersionsGit(t, ws.Root, "init")
			project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
			projectRoot := filepath.Join(ws.Root, project.Path)
			writeFileAt(t, filepath.Join(projectRoot, "source.go"), "package fixture\n")
			writeFileAt(t, filepath.Join(projectRoot, "ignored.go"), "package ignored\n")
			writeFileAt(t, filepath.Join(projectRoot, ".gitignore"), "ignored.go\n")
			scheduler := &Scheduler{ws: ws, capabilitySourceBindings: newCapabilitySourceBindingMemo()}
			memo := scheduler.capabilitySourceBindings
			before, spawned := memo.sourceBinding(ws.Root, projectRoot)
			if before.err != nil || spawned != 2 {
				t.Fatalf("seed: %+v, processes=%d", before, spawned)
			}
			job := &ScheduledJob{Project: project}
			entry := &store.TaskEntry{Outputs: []store.TaskEntryOutput{{
				ID: "source", Kind: extension.OutputKindDirectory, Root: root,
				Path: ".", State: store.TaskOutputPresent,
			}}}
			scheduler.invalidateCapabilitySourceBindingsForRestore(job, entry)
			// A stamp can be computed between the pre-write invalidation and the
			// actual restore. The post-write invalidation must discard that snapshot
			// too, including its old untracked set and ignore decisions.
			if during, spawned := memo.sourceBinding(ws.Root, projectRoot); during.err != nil || during.binding != before.binding || spawned != 2 {
				t.Fatalf("during restore: %+v, processes=%d", during, spawned)
			}
			writeFileAt(t, filepath.Join(projectRoot, "new.go"), "package newfile\n")
			writeFileAt(t, filepath.Join(projectRoot, ".gitignore"), "source.go\n")
			// Becoming tracked overrides the new ignore rule, so refreshing only
			// the untracked listing would also be incorrect.
			runVersionsGit(t, ws.Root, "add", "-f", "proj/source.go")
			scheduler.invalidateCapabilitySourceBindingsForRestore(job, entry)
			after, spawned := memo.sourceBinding(ws.Root, projectRoot)
			want, _, err := putnamigit.ProjectSourceBindingMeasured(ws.Root, projectRoot)
			if after.err != nil || err != nil || after.binding != want || after.binding == before.binding || spawned != 2 {
				t.Fatalf("after restore: %+v, want=%s err=%v processes=%d", after, want, err, spawned)
			}
			// The refreshed repository enumeration is reused for another root.
			otherRoot := filepath.Join(ws.Root, "test-proj")
			if other, spawned := memo.sourceBinding(ws.Root, otherRoot); other.err != nil || spawned != 0 {
				t.Fatalf("shared refreshed snapshot: %+v, processes=%d", other, spawned)
			}
		})
	}
}

// sourceClaimWorkspace is a workspace of three projects in one dependency
// chain, each with one source file, at a root with no repository.
func sourceClaimWorkspace(t *testing.T) (*workspace.Workspace, []*workspace.Project) {
	t.Helper()
	ws := makeExecutorTestWorkspace(t)
	projects := []*workspace.Project{
		{ID: "/apps/workload", Name: "workload", Path: "apps/workload", Dependencies: []string{"feature"}},
		{ID: "/libs/feature", Name: "feature", Path: "libs/feature", Dependencies: []string{"framework"}},
		{ID: "/libs/framework", Name: "framework", Path: "libs/framework"},
	}
	for _, project := range projects {
		writeFileAt(t, filepath.Join(ws.Root, project.Path, "source.go"), "package fixture\n")
	}
	ws.Projects = projects
	return ws, projects
}

// A root Git does not manage has no source-v1 binding, so every package of the
// closure is stamped with an empty binding and the unavailable marker, and its
// source root stays stamped. Git is asked once for the whole closure.
func TestCapabilityPackageStamps_OutsideARepositoryMakeNoSourceClaim(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "no-source-claim",
		"stamps-outside-a-repository-make-no-source-claim")
	ws, projects := sourceClaimWorkspace(t)
	// Git discovery stops above the root, so the answer does not depend on
	// where the temporary directory lives.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(ws.Root))

	memo := newCapabilitySourceBindingMemo()
	resolved := 0
	resolve := memo.resolve
	memo.resolve = func(repoRoot, projectRoot string) (string, int, error) {
		resolved++
		return resolve(repoRoot, projectRoot)
	}
	probed := 0
	isUnmanaged := memo.isUnmanaged
	memo.isUnmanaged = func(repoRoot string) bool {
		probed++
		return isUnmanaged(repoRoot)
	}

	stamps := capabilityPackageStamps(ws, rootLineVersions(&JobContextVersion{Base: "0.0.0"}), projects[0], memo, nil)
	if len(stamps) != len(projects) {
		t.Fatalf("stamps = %+v, want one per project of the closure", stamps)
	}
	for _, stamp := range stamps {
		if !stamp.SourceBindingUnavailable || stamp.SourceBinding != "" {
			t.Errorf("stamp for %s = %+v, want no binding and the unavailable marker", stamp.Package, stamp)
		}
		if stamp.SourceRoot == "" {
			t.Errorf("stamp for %s lost its source root: %+v", stamp.Package, stamp)
		}
	}
	if resolved != 1 || probed != 1 {
		t.Fatalf("binding attempts = %d, repository probes = %d, want one of each for the whole closure", resolved, probed)
	}
}

// The unavailable marker means "Git does not manage this root" and nothing
// else. A binding that fails inside a repository is stamped empty with the
// marker unset, which is the shape every emitter refuses.
func TestCapabilityPackageStamps_ABindingFailureInsideARepositoryIsNotMarkedUnavailable(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "no-source-claim",
		"a-binding-failure-inside-a-repository-is-not-marked-unavailable")
	ws, projects := sourceClaimWorkspace(t)
	runVersionsGit(t, ws.Root, "init")

	memo := newCapabilitySourceBindingMemo()
	memo.resolve = func(string, string) (string, int, error) {
		return "source-v1:sha256:partial", 1, fmt.Errorf("unmerged index entry")
	}
	probed := 0
	isUnmanaged := memo.isUnmanaged
	memo.isUnmanaged = func(repoRoot string) bool {
		probed++
		return isUnmanaged(repoRoot)
	}

	stamps := capabilityPackageStamps(ws, rootLineVersions(&JobContextVersion{Base: "0.0.0"}), projects[0], memo, nil)
	if len(stamps) != len(projects) {
		t.Fatalf("stamps = %+v, want one per project of the closure", stamps)
	}
	for _, stamp := range stamps {
		if stamp.SourceBindingUnavailable || stamp.SourceBinding != "" {
			t.Errorf("stamp for %s = %+v, want an empty binding with the marker unset", stamp.Package, stamp)
		}
	}
	if probed != 1 {
		t.Fatalf("repository probes = %d, want 1 for the whole closure", probed)
	}
}

// Inside a repository the stamp carries exactly the binding Git's enumeration
// yields for the project root, the marker stays unset, and Git is never asked
// whether it manages the root.
func TestCapabilityPackageStamps_InsideARepositoryCarryTheSourceBinding(t *testing.T) {
	spectest.Proves(t, "cli/workspace-without-git", "no-source-claim",
		"bindings-inside-a-repository-are-unchanged")
	ws, projects := sourceClaimWorkspace(t)
	runVersionsGit(t, ws.Root, "init")

	memo := newCapabilitySourceBindingMemo()
	memo.isUnmanaged = func(repoRoot string) bool {
		t.Errorf("asked whether Git manages %s although every binding resolved", repoRoot)
		return false
	}

	spawned := 0
	stamps := capabilityPackageStamps(ws, rootLineVersions(&JobContextVersion{Base: "0.0.0"}), projects[0], memo, &spawned)
	if len(stamps) != len(projects) {
		t.Fatalf("stamps = %+v, want one per project of the closure", stamps)
	}
	byName := make(map[string]*workspace.Project, len(projects))
	for _, project := range projects {
		byName[project.Name] = project
	}
	for _, stamp := range stamps {
		want, _, err := putnamigit.ProjectSourceBindingMeasured(ws.Root, filepath.Join(ws.Root, byName[stamp.Package].Path))
		if err != nil {
			t.Fatal(err)
		}
		if stamp.SourceBindingUnavailable || stamp.SourceBinding != want {
			t.Errorf("stamp for %s = %+v, want the binding %s", stamp.Package, stamp, want)
		}
	}
	if spawned != 2 {
		t.Fatalf("git processes = %d, want the 2 of one repository enumeration", spawned)
	}
}

// rootLineVersions is a run's versions for a workspace made of one root line,
// which is what every fixture in this package builds.
func rootLineVersions(info *JobContextVersion) RunVersions {
	if info == nil {
		return nil
	}
	return RunVersions{"": info}
}
