package moon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
)

// CodeTasksInferred is a sync of a workspace that infers tasks from
// package.json scripts, with neither the export nor moon to list them.
const CodeTasksInferred = "moon.tasks_inferred"

// moonProgram is a moon the repository installs, as @moonrepo/cli.
const moonProgram = "node_modules/.bin/moon"

// exportArgs print the moon project graph, each project's resolved tasks
// included.
var exportArgs = []string{"project-graph", "--json"}

// exportSource names the graph the sync read when it ran moon.
var exportSource = "moon " + strings.Join(exportArgs, " ")

// lookPath finds a moon on PATH; tests replace it.
var lookPath = exec.LookPath

// resolvedGraph is where the sync reads the moon project graph from.
type resolvedGraph struct {
	// options name the export Detect reads; empty reads the one at the root,
	// else the configuration.
	options detect.Options
	// unresolved says what the configuration misses when the sync could not
	// run moon; empty otherwise.
	unresolved string
	// cleanup removes the export moon printed.
	cleanup func()
}

// resolveGraph answers the graph the sync reads at root: the export at the
// root, else, when the workspace infers tasks from package.json scripts, the
// graph moon prints, from the repository's moon or, when that one is missing
// or fails, the one on PATH. When every moon fails, the sync fails, naming
// each. Without one, or when the workspace infers no task, the sync reads
// the configuration, which then states every task.
func resolveGraph(ctx context.Context, root string) (resolvedGraph, error) {
	none := resolvedGraph{cleanup: func() {}}
	if fileExists(root, detect.DefaultGraphFile) {
		return none, nil
	}
	setting := detect.InfersTasks(root)
	if setting == "" {
		return none, nil
	}
	programs := moonPrograms(root)
	if len(programs) == 0 {
		none.unresolved = fmt.Sprintf("%s infers tasks from package.json scripts, which only moon lists, and no moon is installed: the sync read the moon configuration, which states none of them; install moon or write the export with `%s`, then run putnami projects sync", setting, detect.ExportCommand)
		return none, nil
	}
	failures := make([]string, 0, len(programs)+1)
	for _, program := range programs {
		if ctx.Err() != nil {
			break
		}
		var output bytes.Buffer
		file, cleanup, err := stacks.ExportGraph(ctx, root, profileID, program, exportArgs, detect.DefaultGraphFile, &output)
		if err == nil {
			return resolvedGraph{options: detect.Options{GraphFile: file}, cleanup: cleanup}, nil
		}
		failures = append(failures, err.Error()+tail(output.String()))
	}
	if len(programs) == 1 && programs[0] == filepath.Join(root, filepath.FromSlash(moonProgram)) {
		failures = append(failures, "no other moon is on PATH")
	}
	return resolvedGraph{}, fmt.Errorf("%s failed, so no edge is written: %s", exportSource, strings.Join(failures, "\n"))
}

// moonPrograms are the moons the sync may run at root, in order: the
// repository's own, then the one on PATH when it is another file.
func moonPrograms(root string) []string {
	var programs []string
	program := filepath.Join(root, filepath.FromSlash(moonProgram))
	if info, err := os.Stat(program); err == nil && !info.IsDir() {
		programs = append(programs, program)
	}
	if onPath, err := lookPath("moon"); err == nil && (len(programs) == 0 || !sameFile(programs[0], onPath)) {
		programs = append(programs, onPath)
	}
	return programs
}

// sameFile reports whether two paths name one file.
func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// tail is the end of a command's error output, to quote in an error.
func tail(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	const limit = 2048
	if len(output) > limit {
		output = "…" + output[len(output)-limit:]
	}
	return ":\n" + output
}
