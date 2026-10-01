package extension

import (
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// TestFirstPartyLongLivedServeTasksAreUncacheableProcesses is the positive
// regression test. A serve task waits on a resident child process and may
// bind a port, so replaying a cache entry can never satisfy the command. The
// expanded job is what the engine renders as NO-CACHE in a plan.
func TestFirstPartyLongLivedServeTasksAreUncacheableProcesses(t *testing.T) {
	repoRoot := findRepoRoot(t)
	fixtures := []struct {
		name string
		path string
		task string
	}{
		{name: "go", path: "go/extension/putnami.extension.json", task: "serve-run"},
		{name: "typescript", path: "typescript/extension/putnami.extension.json", task: "serve-app"},
		{name: "python", path: "python/extension/putnami.extension.json", task: "serve-run"},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(repoRoot, fixture.path)
			manifest, err := LoadManifest(path)
			if err != nil {
				t.Fatalf("LoadManifest(%s): %v", fixture.path, err)
			}
			if diags := proto.FullValidateManifest(manifest); diag.HasErrors(diags) {
				t.Fatalf("%s is not schema-valid: %v", fixture.path, diags)
			}

			resolved := Resolve(manifest, filepath.Dir(path))
			command, ok := resolved.Jobs["serve"]
			if !ok {
				t.Fatalf("%s has no serve command", fixture.path)
			}
			expanded, err := ExpandPipeline("serve", command, command.PipelineSteps, resolved, nil)
			if err != nil {
				t.Fatalf("ExpandPipeline(%s serve): %v", fixture.name, err)
			}

			expectedFound := false
			longLived := 0
			for _, step := range expanded {
				if step.Step.Task == fixture.task {
					expectedFound = true
				}
				if step.JobDef.TimeoutMs != -1 {
					continue
				}
				longLived++
				if step.Task.Cache.IsEnabled() {
					t.Errorf("%s is cache-eligible; a serve cache hit cannot start its process", step.Step.Task)
				}
				if step.Task.Declares == nil || !hasTaskEffect(step.Task.Declares.Effects, proto.EffectProcess) {
					t.Errorf("%s effects = %v, want %q", step.Step.Task, taskEffects(step.Task), proto.EffectProcess)
				}
				if step.JobDef.Cache {
					t.Errorf("%s expands as cache-eligible; the plan would render MISS instead of NO-CACHE", step.Step.Task)
				}
			}
			if !expectedFound {
				t.Fatalf("serve pipeline does not schedule task %q", fixture.task)
			}
			if longLived == 0 {
				t.Fatal("serve pipeline has no long-lived task; expected a resident server process")
			}
		})
	}
}

func hasTaskEffect(effects []string, want string) bool {
	for _, effect := range effects {
		if effect == want {
			return true
		}
	}
	return false
}

func taskEffects(task TaskDefinition) []string {
	if task.Declares == nil {
		return nil
	}
	return task.Declares.Effects
}
