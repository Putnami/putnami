package nx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	detect "go.putnami.dev/tooling/observe/detect/nx"
)

// CodeGraphStatic is a sync that read nx.json and the project files because
// neither the graph export nor the repository's Nx was there: it misses what
// Nx infers.
const CodeGraphStatic = "nx.graph_static"

// nxProgram is the repository's own Nx, the one its lockfile pins.
const nxProgram = "node_modules/.bin/nx"

// graphCommand names the graph the sync read when it ran the repository's
// Nx.
const graphCommand = "nx graph"

// exportCommand writes the graph export the sync reads at the root.
const exportCommand = "nx graph --file=" + detect.DefaultGraphFile

// staticWarning says what a sync that read the configuration alone misses.
var staticWarning = fmt.Sprintf("no %s at the workspace root and no %s: the sync read nx.json and the project files, which miss the edges Nx infers from imports and the targets its plugins infer; install the dependencies or write the export with `%s`, then run putnami projects sync",
	detect.DefaultGraphFile, nxProgram, exportCommand)

// resolvedGraph is where the sync reads the Nx project graph from: the export
// at the root, else the graph the repository's Nx resolves, else nx.json and
// the project files.
type resolvedGraph struct {
	// options name the export Detect reads; empty reads the one at the root.
	options detect.Options
	// source names the graph in the sync's messages.
	source string
	// static reports a read of the configuration alone.
	static bool
	// cleanup removes the graph the repository's Nx wrote.
	cleanup func()
}

// resolveGraph answers the graph the sync reads at root. With no export at the
// root, it runs nx graph with the repository's Nx, whose graph holds the
// edges Nx infers from imports and the targets its plugins infer; an Nx that
// fails fails the sync.
func resolveGraph(ctx context.Context, root string) (resolvedGraph, error) {
	none := func() {}
	if exists(root, detect.DefaultGraphFile) {
		return resolvedGraph{source: detect.DefaultGraphFile, cleanup: none}, nil
	}
	program := filepath.Join(root, filepath.FromSlash(nxProgram))
	if info, err := os.Stat(program); err != nil || info.IsDir() {
		return resolvedGraph{source: "nx.json", static: true, cleanup: none}, nil
	}
	dir, err := os.MkdirTemp("", "putnami-nx-graph-")
	if err != nil {
		return resolvedGraph{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, detect.DefaultGraphFile)
	cmd := exec.CommandContext(ctx, program, "graph", "--file="+file) //nolint:gosec // the repository's own Nx
	cmd.Dir = root
	// Nx prints its progress on stdout, which carries the job's events.
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		cleanup()
		return resolvedGraph{}, fmt.Errorf("%s --file=%s failed, so no edge is written: %w%s", graphCommand, detect.DefaultGraphFile, err, tail(output.String()))
	}
	return resolvedGraph{options: detect.Options{GraphFile: file}, source: graphCommand, cleanup: cleanup}, nil
}

// tail is the end of a command's output, to quote in an error.
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
