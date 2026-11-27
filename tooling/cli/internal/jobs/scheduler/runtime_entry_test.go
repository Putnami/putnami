package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A task that names a runtime entry runs that entry's executable, which
// prepare wrote beside the main one; a task that names none runs the main
// executable. An entry whose executable is missing fails the task's runtime
// instead of running the main executable in its place.
func TestRuntimeEntry_ATaskRunsItsEntry(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-implementation-cache-key",
		"a-task-runs-its-runtime-entry")
	runtimeRoot := t.TempDir()
	for _, name := range []string{"putnami-go", "putnami-go-codegen"} {
		writeExecutable(t, filepath.Join(runtimeRoot, "compiled", name), "#!/bin/sh\n")
	}
	job := func(entry string) *ScheduledJob {
		return &ScheduledJob{
			Extension: &extension.ExtensionDescription{
				Name: "@putnami/go",
				Runtime: &extension.RuntimeDefinition{
					Executable: "compiled/putnami-go",
					Entries:    map[string]extension.RuntimeEntry{"codegen": {Executable: "compiled/putnami-go-codegen"}},
				},
				RuntimeExecutable: filepath.Join(runtimeRoot, "compiled", "putnami-go"),
			},
			JobDef: &extension.JobDefinition{
				Name: "build~generate", Command: "{extensionRuntime}", Args: []string{"build-generate"}, RuntimeEntry: entry,
			},
		}
	}
	if got, err := resolvedExtensionRuntime(job("codegen")); err != nil || got != filepath.Join(runtimeRoot, "compiled", "putnami-go-codegen") {
		t.Fatalf("codegen task runs %q, %v", got, err)
	}
	if got, err := resolvedExtensionRuntime(job("")); err != nil || got != filepath.Join(runtimeRoot, "compiled", "putnami-go") {
		t.Fatalf("main task runs %q, %v", got, err)
	}
	if got := jobRuntimePath(job("codegen")); got != filepath.Join(runtimeRoot, "compiled", "putnami-go-codegen") {
		t.Errorf("the codegen task's context names runtime %q", got)
	}
	if err := os.Remove(filepath.Join(runtimeRoot, "compiled", "putnami-go-codegen")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvedExtensionRuntime(job("codegen")); err == nil || !strings.Contains(err.Error(), `runtime entry "codegen" is unavailable`) {
		t.Fatalf("a missing entry executable = %v, want the entry named unavailable", err)
	}
}
