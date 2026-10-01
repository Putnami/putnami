package changeplan

import (
	"encoding/json"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestBuildCanonicalDigestIsStable(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	first, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go", "libs/lib/lib.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/lib:build", Key: "key-lib"}, {TaskKey: "/app:lint", Key: "key-app"}},
	}))
	if err != nil {
		t.Fatalf("Build first: %v", err)
	}
	second, err := Build(changePlanInput(app, lib, []string{"libs/lib/lib.go", "apps/app/main.go", "apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app"}, {TaskKey: "/lib:build", Key: "key-lib"}},
	}))
	if err != nil {
		t.Fatalf("Build second: %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("equivalent inputs emitted different documents:\n%s\n%s", firstJSON, secondJSON)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest differs: %s != %s", first.Digest, second.Digest)
	}
	if err := ciproto.ValidateChangePlan(first); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	recomputed, err := ciproto.RecomputeChangePlanDigest(first)
	if err != nil {
		t.Fatalf("RecomputeChangePlanDigest: %v", err)
	}
	if recomputed != first.Digest {
		t.Fatalf("recomputed digest = %s, want %s", recomputed, first.Digest)
	}
}

func TestDigestExcludesAdvisoryCachePresence(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "libs/lib"}
	cold, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app", Present: false}},
	}))
	if err != nil {
		t.Fatalf("Build cold: %v", err)
	}
	warm, err := Build(changePlanInput(app, lib, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{
		Status:  "available",
		Entries: []ciproto.ChangePlanCacheEntry{{TaskKey: "/app:lint", Key: "key-app", Present: true}},
	}))
	if err != nil {
		t.Fatalf("Build warm: %v", err)
	}
	if cold.Digest != warm.Digest {
		t.Fatalf("cache presence changed digest: %s != %s", cold.Digest, warm.Digest)
	}
	if err := ciproto.ValidateChangePlan(cold); err != nil {
		t.Fatalf("Validate cold: %v", err)
	}
	if err := ciproto.ValidateChangePlan(warm); err != nil {
		t.Fatalf("Validate warm: %v", err)
	}
}

func TestBuildDoesNotSerializeInvocationDetails(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	job := testJob(app, "lint~secret", 0)
	job.JobDef.Command = "private-command"
	job.JobDef.Args = []string{"--token", "super-secret-token"}
	job.JobDef.Cwd = "private-working-directory"
	job.JobDef.Env = map[string]string{"API_TOKEN": "super-secret-token"}
	document, err := Build(Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Repository:       ciproto.ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     []string{"apps/app/main.go"},
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{app},
		Planned:          []*jobs.ScheduledJob{job},
		Cache:            ciproto.ChangePlanCache{Status: "disabled"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	for _, forbidden := range []string{"private-command", "super-secret-token", "private-working-directory", "API_TOKEN"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("document leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildRejectsTaskWithoutHardDeadline(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "apps/app"}
	in := changePlanInput(app, app, []string{"apps/app/main.go"}, ciproto.ChangePlanCache{Status: "disabled"})
	in.Planned = []*jobs.ScheduledJob{testJob(app, "lint~unbounded", -1)}
	if _, err := Build(in); err == nil || !strings.Contains(err.Error(), "no hard deadline") {
		t.Fatalf("Build error = %v, want hard-deadline rejection", err)
	}
}

func changePlanInput(app, lib *workspace.Project, changed []string, cache ciproto.ChangePlanCache) Input {
	return Input{
		Generator:        ciproto.ChangePlanGenerator{Name: "putnami", Version: "test"},
		Repository:       ciproto.ChangePlanRepository{Remote: "origin", URL: "https://example.test/org/repo.git"},
		BaseSHA:          strings.Repeat("a", 40),
		HeadSHA:          strings.Repeat("b", 40),
		ChangedFiles:     changed,
		DirectProjects:   []*workspace.Project{app},
		ImpactedProjects: []*workspace.Project{lib, app},
		Planned:          []*jobs.ScheduledJob{testJob(lib, "build", 12_000), testJob(app, "lint", 0)},
		Cache:            cache,
	}
}

func testJob(project *workspace.Project, name string, timeout int) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.0.0"},
		JobDef: &extension.JobDefinition{
			Name:           name,
			CommandName:    strings.Split(name, "~")[0],
			TimeoutMs:      timeout,
			ContractDigest: "tc1:test-contract",
			Reads:          []extension.ResourceRef{{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}},
		},
	}
}
