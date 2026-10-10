package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// A reader that does not know the releaseBaseline runtime input would key the
// task without it, so a manifest that declares it needs the contract that
// names it, whatever other additive vocabulary it also uses.
func TestReleaseBaselineInputRequiresAdditiveCLIContract(t *testing.T) {
	manifest := []byte(`{"name":"@test/go","cliContract":6,"commands":{"validate":{"run":[{"id":"api","task":"api"}]}},"tasks":{"api":{"kind":"command","command":"echo","inputs":{"sources":{"from":"project","files":["**/*.go","go-embed:build"]},"releaseBaseline":{"from":"runtime"}}}}}`)
	if _, err := NegotiateManifest("go.json", manifest); err == nil || !strings.Contains(err.Error(), "requires 7") {
		t.Fatalf("contract-6 stamp accepted the releaseBaseline input: %v", err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"cliContract":6`, `"cliContract":7`, 1))
	m, err := NegotiateManifest("go.json", manifest)
	if err != nil {
		t.Fatalf("contract-7 stamp refused: %v", err)
	}
	if got := RequiredCLIContract(m); got != protocolcli.ReleaseBaselineInputContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.ReleaseBaselineInputContract)
	}
	m.AgentContent = &AgentContentContribution{}
	if got := RequiredCLIContract(m); got != protocolcli.ReleaseBaselineInputContract {
		t.Fatalf("combined additive floor = %d, want %d", got, protocolcli.ReleaseBaselineInputContract)
	}
}

// From contract 7 a `git:` input keys each candidate's executable bit and
// refuses an unmerged one; an older reader keys bytes alone. A manifest that
// declares one on a task needs rung 7, so the older reader refuses it instead
// of replaying a verdict across a chmod.
func TestAGitInputRequiresTheContractThatKeysTheMode(t *testing.T) {
	manifest := []byte(`{"name":"@test/sdd","cliContract":6,"commands":{"validate":{"run":[{"id":"features","task":"features"}]}},"tasks":{"features":{"kind":"command","command":"echo","inputs":{"repository":{"from":"workspace","files":["git:**"]}}}}}`)
	if _, err := NegotiateManifest("sdd.json", manifest); err == nil || !strings.Contains(err.Error(), "requires 7") {
		t.Fatalf("contract-6 stamp accepted a git: input: %v", err)
	}
	manifest = []byte(strings.Replace(string(manifest), `"cliContract":6`, `"cliContract":7`, 1))
	m, err := NegotiateManifest("sdd.json", manifest)
	if err != nil {
		t.Fatalf("contract-7 stamp refused: %v", err)
	}
	if got := RequiredCLIContract(m); got != protocolcli.GitInputModeContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.GitInputModeContract)
	}

	for name, task := range map[string]TaskDefinition{
		"project port":         {Inputs: map[string]TaskInputPort{"r": {From: TaskInputFromProject, Files: []string{"git:**"}}}},
		"closure port":         {Inputs: map[string]TaskInputPort{"r": {From: TaskInputFromClosure, Files: []string{"git:doc/**"}}}},
		"cache key files":      {Cache: &TaskCachePolicy{Key: &TaskCacheKey{Files: []string{"git:**"}}}},
		"cache key workspace":  {Cache: &TaskCachePolicy{Key: &TaskCacheKey{WorkspaceFiles: []string{"git:**"}}}},
		"cache key closure":    {Cache: &TaskCachePolicy{Key: &TaskCacheKey{ClosureFiles: []string{"git:**"}}}},
		"beside an embed rung": {Inputs: map[string]TaskInputPort{"r": {From: TaskInputFromProject, Files: []string{"go-embed:build", "git:**"}}}},
	} {
		if got := RequiredCLIContract(&Manifest{Tasks: map[string]TaskDefinition{"t": task}}); got != protocolcli.GitInputModeContract {
			t.Errorf("%s: RequiredCLIContract = %d, want %d", name, got, protocolcli.GitInputModeContract)
		}
	}
	for name, task := range map[string]TaskDefinition{
		"an exclusion only": {Inputs: map[string]TaskInputPort{"r": {From: TaskInputFromProject, Files: []string{"**/*.md", "!git:docs/**"}}}},
		"a path with git":   {Inputs: map[string]TaskInputPort{"r": {From: TaskInputFromProject, Files: []string{"docs/git:**"}}}},
	} {
		if got := RequiredCLIContract(&Manifest{Tasks: map[string]TaskDefinition{"t": task}}); got != protocolcli.CurrentContract {
			t.Errorf("%s: RequiredCLIContract = %d, want the base contract %d", name, got, protocolcli.CurrentContract)
		}
	}
}

// Every task counts: a task that needs a lower rung, met first, cannot mask a
// task that needs the release-baseline rung.
func TestRequiredCLIContractTakesTheHighestRungAcrossTasks(t *testing.T) {
	embed := TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
		"sources": {From: TaskInputFromProject, Files: []string{"go-embed:build"}},
	}}
	plain := TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
		"sources": {From: TaskInputFromProject, Files: []string{"**/*.go"}},
	}}
	byPort := TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
		RuntimeInputReleaseBaseline: {From: TaskInputFromRuntime},
	}}
	byCacheKey := TaskDefinition{Kind: "command", Command: "echo",
		Cache: &TaskCachePolicy{Key: &TaskCacheKey{Runtime: []string{RuntimeInputReleaseBaseline}}}}
	sameNameOtherOrigin := TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
		RuntimeInputReleaseBaseline: {From: TaskInputFromParams},
	}}

	for _, test := range []struct {
		name  string
		tasks map[string]TaskDefinition
		want  int
	}{
		{"none", map[string]TaskDefinition{"a": plain}, protocolcli.CurrentContract},
		{"embed only", map[string]TaskDefinition{"a": plain, "b": embed}, protocolcli.GoEmbedInputsContract},
		{"input port", map[string]TaskDefinition{"a": embed, "b": byPort}, protocolcli.ReleaseBaselineInputContract},
		{"cache key runtime", map[string]TaskDefinition{"a": embed, "b": byCacheKey}, protocolcli.ReleaseBaselineInputContract},
		{"same name, params origin", map[string]TaskDefinition{"a": sameNameOtherOrigin}, protocolcli.CurrentContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Map iteration order is random; repeat so an early return on a
			// lower rung would show.
			for range 32 {
				if got := RequiredCLIContract(&Manifest{Tasks: test.tasks}); got != test.want {
					t.Fatalf("RequiredCLIContract = %d, want %d", got, test.want)
				}
			}
		})
	}
	if got := RequiredCLIContract(nil); got != protocolcli.CurrentContract {
		t.Fatalf("RequiredCLIContract(nil) = %d, want %d", got, protocolcli.CurrentContract)
	}
}
