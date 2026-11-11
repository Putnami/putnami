package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/project"
)

// NodeBundleFile names the record a Node bundle directory carries: the
// bundle-relative path of the module node starts.
const NodeBundleFile = "putnami-node.json"

// NodeBundle is the record of a Node bundle.
type NodeBundle struct {
	// Entrypoint is the slash-separated path, relative to the bundle
	// directory, of the module node starts.
	Entrypoint string `json:"entrypoint"`
	// Inputs are the absolute paths of the files bun read to write the
	// bundle, from its metafile. The record does not carry them.
	Inputs []string `json:"-"`
}

// buildNodeBundleArgs builds the bun build args of a Node bundle.
func buildNodeBundleArgs(projectPath, outDir, metafile, entrypoint string, externals []string) []string {
	args := []string{"build", "--target", "node", "--format", "esm",
		"--outdir", outDir, "--root", projectPath, "--metafile=" + metafile,
		// The bundle reads process.env at run time, NODE_ENV included.
		"--env=disable",
		// A linked map is the one node --enable-source-maps finds.
		"--sourcemap=linked"}
	for _, name := range externals {
		args = append(args, "--external", name, "--external", name+"/*")
	}
	return append(args, filepath.Join(projectPath, filepath.FromSlash(entrypoint)))
}

// RunNodeBundle writes the Node bundle of the project's entrypoint into
// outDir, which it empties first: the entrypoint as an ES module with the
// project's own modules bundled, its packages (project.NodeExternals) left as
// imports, a linked source map, a package.json that makes its .js files ES
// modules, and the NodeBundleFile record. entrypoint is relative to
// projectPath. A bundle bun refuses returns its errors, and nil error.
func RunNodeBundle(bunBin, projectPath, outDir, entrypoint string) (*NodeBundle, []string, error) {
	if err := os.RemoveAll(outDir); err != nil {
		return nil, nil, fmt.Errorf("cleaning node bundle output %s: %w", outDir, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating node bundle output %s: %w", outDir, err)
	}
	entrypoint = path.Clean(filepath.ToSlash(strings.TrimPrefix(entrypoint, "./")))
	metaDir, err := os.MkdirTemp("", "putnami-ts-node-bundle-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(metaDir) }()
	metafile := filepath.Join(metaDir, "metafile.json")
	// bun reads the project's tsconfig.json itself.
	args := buildNodeBundleArgs(projectPath, outDir, metafile, entrypoint, project.NodeExternals(projectPath))
	result, err := execRunFunc(bunBin, args, exec.Dir(projectPath), exec.Timeout(5*time.Minute))
	if err != nil {
		return nil, []string{fmt.Sprintf("Node bundle of %s failed: %v", entrypoint, err)}, nil
	}
	if !result.Success {
		return nil, []string{fmt.Sprintf("Node bundle of %s failed: %s", entrypoint, strings.TrimSpace(result.Stderr))}, nil
	}

	bundle := &NodeBundle{Entrypoint: strings.TrimSuffix(entrypoint, path.Ext(entrypoint)) + ".js"}
	if !project.FileExists(filepath.Join(outDir, filepath.FromSlash(bundle.Entrypoint))) {
		return nil, []string{fmt.Sprintf("Node bundle of %s wrote no %s", entrypoint, bundle.Entrypoint)}, nil
	}
	inputs, err := metafileInputs(metafile, projectPath)
	if err != nil {
		return nil, nil, err
	}
	bundle.Inputs = inputs
	if err := os.WriteFile(filepath.Join(outDir, "package.json"), []byte("{\n  \"type\": \"module\"\n}\n"), 0o644); err != nil {
		return nil, nil, fmt.Errorf("writing node bundle package.json: %w", err)
	}
	record, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, NodeBundleFile), append(record, '\n'), 0o644); err != nil {
		return nil, nil, fmt.Errorf("writing %s: %w", NodeBundleFile, err)
	}
	return bundle, nil, nil
}

// ReadNodeBundle reads the record of the Node bundle in dir.
func ReadNodeBundle(dir string) (*NodeBundle, error) {
	data, err := os.ReadFile(filepath.Join(dir, NodeBundleFile))
	if err != nil {
		return nil, err
	}
	var bundle NodeBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("%s: %w", NodeBundleFile, err)
	}
	if bundle.Entrypoint == "" {
		return nil, fmt.Errorf("%s names no entrypoint", NodeBundleFile)
	}
	return &bundle, nil
}

// metafileInputs returns the absolute, sorted paths of the inputs a bun
// metafile records, which bun writes relative to the directory it ran in.
func metafileInputs(metafile, dir string) ([]string, error) {
	data, err := os.ReadFile(metafile)
	if err != nil {
		return nil, fmt.Errorf("reading the node bundle metafile: %w", err)
	}
	var meta struct {
		Inputs map[string]json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decoding the node bundle metafile: %w", err)
	}
	inputs := make([]string, 0, len(meta.Inputs))
	for input := range meta.Inputs {
		path := filepath.FromSlash(input)
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		inputs = append(inputs, filepath.Clean(path))
	}
	sort.Strings(inputs)
	return inputs, nil
}
