package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

type entriesManifest struct {
	Runtime struct {
		Executable string `json:"executable"`
		Entries    map[string]struct {
			Executable string `json:"executable"`
		} `json:"entries"`
	} `json:"runtime"`
	Tasks map[string]struct {
		Command      string   `json:"command"`
		Args         []string `json:"args"`
		RuntimeEntry string   `json:"runtimeEntry"`
	} `json:"tasks"`
}

func readEntriesManifest(t *testing.T) entriesManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m entriesManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The codegen entry runs exactly the jobs of the tasks that name it, and the
// main executable runs every other job: each task finds its handler in the
// executable it runs. Both halves are in the untagged build this test sees, so
// the check is on the two tables bin/prepare selects with its build tags.
func TestRuntimeEntries_EachTaskRunsAnExecutableThatHoldsItsJob(t *testing.T) {
	m := readEntriesManifest(t)
	if _, ok := m.Runtime.Entries["codegen"]; !ok {
		t.Fatal("the manifest declares no codegen runtime entry")
	}
	var codegenJobs []string
	for name, task := range m.Tasks {
		if task.Command != "{extensionRuntime}" || len(task.Args) == 0 {
			continue
		}
		job := task.Args[0]
		switch task.RuntimeEntry {
		case "codegen":
			codegenJobs = append(codegenJobs, job)
			if _, ok := codegenHandlers()[job]; !ok {
				t.Errorf("task %s runs the codegen entry, which has no job %q", name, job)
			}
		case "":
			if _, ok := codegenHandlers()[job]; ok {
				t.Errorf("task %s runs the main executable, which leaves out codegen job %q", name, job)
			}
		default:
			t.Errorf("task %s names runtime entry %q, which this runtime does not build", name, task.RuntimeEntry)
		}
	}
	sort.Strings(codegenJobs)
	codegenJobs = slices.Compact(codegenJobs)
	var handlers []string
	for job := range codegenHandlers() {
		handlers = append(handlers, job)
		if _, ok := generalHandlers()[job]; ok {
			t.Errorf("job %q is in both halves of the dispatch table", job)
		}
	}
	sort.Strings(handlers)
	if !slices.Equal(codegenJobs, handlers) {
		t.Errorf("codegen jobs the manifest runs = %v, codegen handlers = %v", codegenJobs, handlers)
	}
}

// The archive builds every runtime entry as a declared executable of
// options["@putnami/go"].executables, under the entry's file name, with the
// build tag that selects its half of the dispatch table.
func TestRuntimeEntries_TheArchiveBuildsEveryEntry(t *testing.T) {
	m := readEntriesManifest(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var project struct {
		Options map[string]struct {
			Executables []struct {
				Name    string   `json:"name"`
				Package string   `json:"package"`
				Tags    []string `json:"tags"`
			} `json:"executables"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &project); err != nil {
		t.Fatal(err)
	}
	for name, entry := range m.Runtime.Entries {
		if filepath.Dir(filepath.FromSlash(entry.Executable)) != "compiled" {
			t.Errorf("entry %s executable %q is not under compiled/, where the archive stages declared executables", name, entry.Executable)
		}
		file := filepath.Base(entry.Executable)
		found := false
		for _, e := range project.Options["@putnami/go"].Executables {
			if e.Name == file {
				found = true
				if e.Package != "./cmd/putnami-go" || !slices.Contains(e.Tags, "putnami_runtime_"+name) {
					t.Errorf("declared executable %s = %+v, want package ./cmd/putnami-go with tag putnami_runtime_%s", file, e, name)
				}
			}
		}
		if !found {
			t.Errorf("runtime entry %s (%s) is not a declared executable of the archive", name, file)
		}
	}
}
