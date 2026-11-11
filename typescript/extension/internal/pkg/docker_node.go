package pkg

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
)

// nodeBaseImages are the digest-pinned distroless Node bases (nonroot), one
// per supported Node major. As for the Bun base, the pin and not the moving
// tag makes assembled digests reproducible; a bump is an explicit, reviewable
// change that re-addresses every Node image.
var nodeBaseImages = map[int]string{
	20: "gcr.io/distroless/nodejs20-debian12:nonroot@sha256:2cd820156cf039c8b54ae2d2a97e424b6729070714de8707a6b79f20d56f6a9a",
	22: "gcr.io/distroless/nodejs22-debian12:nonroot@sha256:13593b7570658e8477de39e2f4a1dd25db2f836d68a0ba771251572d23bb4f8e",
	24: "gcr.io/distroless/nodejs24-debian12:nonroot@sha256:14d42e2511532589a7c7e01a753667a74fcc96266e137e8125006b87b0c32d0a",
}

// nodeImageBinary is the node program of the distroless Node bases.
const nodeImageBinary = "/nodejs/bin/node"

// nodeVersionFiles are the files, besides package.json engines.node, that
// declare a project's Node version, in the order they are read.
var nodeVersionFiles = []string{".nvmrc", ".node-version"}

// majorPattern finds the first release number of a version declaration.
var majorPattern = regexp.MustCompile(`\d+`)

// NodeBaseImage returns the pinned base of the Node major the project at
// projectDir declares: package.json engines.node, else .nvmrc or
// .node-version in the project, then at the workspace root. It fails with
// typescript.node_version_undeclared when the project declares none, when a
// declaration names no major, and when the major has no pinned base.
func NodeBaseImage(projectDir, workspaceRoot string) (string, error) {
	declaration, source := declaredNodeVersion(projectDir, workspaceRoot)
	if source == "" {
		return "", errs.New(errs.CodeNodeVersionUndeclared,
			"the project declares no Node version: set engines.node in its package.json, or write the version in .nvmrc or .node-version").
			WithCategory(errs.CategoryUser)
	}
	match := majorPattern.FindString(declaration)
	if match == "" {
		return "", errs.New(errs.CodeNodeVersionUndeclared, fmt.Sprintf(
			"%s declares Node %q, which names no major: write a release such as 22", source, declaration)).
			WithCategory(errs.CategoryUser)
	}
	major, err := strconv.Atoi(match)
	if err != nil {
		return "", err
	}
	base, ok := nodeBaseImages[major]
	if !ok {
		return "", errs.New(errs.CodeNodeVersionUndeclared, fmt.Sprintf(
			"%s declares Node %d, which has no pinned base image: declare one of Node %s, or pass --docker-base-image", source, major, pinnedMajors())).
			WithCategory(errs.CategoryUser)
	}
	return base, nil
}

// declaredNodeVersion returns the Node version the project declares and
// where, or an empty source when it declares none.
func declaredNodeVersion(projectDir, workspaceRoot string) (string, string) {
	if data, err := os.ReadFile(filepath.Join(projectDir, "package.json")); err == nil {
		var pkg struct {
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
		}
		if json.Unmarshal(data, &pkg) == nil && strings.TrimSpace(pkg.Engines.Node) != "" {
			return strings.TrimSpace(pkg.Engines.Node), "package.json engines.node"
		}
	}
	dirs := []string{projectDir}
	if workspaceRoot != "" && filepath.Clean(workspaceRoot) != filepath.Clean(projectDir) {
		dirs = append(dirs, workspaceRoot)
	}
	for _, dir := range dirs {
		for _, name := range nodeVersionFiles {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if version := strings.TrimSpace(string(data)); version != "" {
				return version, name
			}
		}
	}
	return "", ""
}

// pinnedMajors words the majors that have a pinned base, such as "20, 22 or 24".
func pinnedMajors() string {
	majors := make([]int, 0, len(nodeBaseImages))
	for major := range nodeBaseImages {
		majors = append(majors, major)
	}
	sort.Ints(majors)
	words := make([]string, len(majors))
	for i, major := range majors {
		words[i] = strconv.Itoa(major)
	}
	if len(words) == 1 {
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// nodePackage is one package of a Node image's dependency closure.
type nodePackage struct {
	// Dir is the real path of the installed package.
	Dir string
	// Path is its slash-separated directory under /app, such as
	// node_modules/express or node_modules/a/node_modules/b.
	Path string
}

// nodeDependencyClosure resolves the production dependencies of the project
// at projectDir from its installed node_modules: its dependencies and
// optionalDependencies, then each package's dependencies,
// optionalDependencies and installed peerDependencies, each resolved from
// the directory of the package that depends on it, and placed where Node
// finds it from there (placeNodePackage). A dependency linked to a workspace
// project is bundled, not shipped, and its own dependencies ship where the
// bundle resolves them, as the project's do. A dependency that is not
// installed fails with
// typescript.dependency_not_installed; an optional or peer one is skipped.
func nodeDependencyClosure(projectDir string) ([]nodePackage, error) {
	pkg := project.ReadPackageJSONSafe(filepath.Join(projectDir, "package.json"))
	if pkg == nil {
		return nil, nil
	}
	type request struct {
		name, fromDir, parentPath string
		required                  bool
	}
	requests := func(p *project.PackageJSON, fromDir, parentPath string, peers bool) []request {
		var out []request
		add := func(deps map[string]string, required bool) {
			names := make([]string, 0, len(deps))
			for name := range deps {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				out = append(out, request{name: name, fromDir: fromDir, parentPath: parentPath, required: required})
			}
		}
		required := map[string]string{}
		for name, v := range p.Dependencies {
			if _, optional := p.OptionalDependencies[name]; !optional {
				required[name] = v
			}
		}
		add(required, true)
		add(p.OptionalDependencies, false)
		if peers {
			add(p.PeerDependencies, false)
		}
		return out
	}

	queue := requests(pkg, projectDir, "", false)
	byPath := map[string]string{}
	linked := map[string]bool{}
	var closure []nodePackage
	for len(queue) > 0 {
		req := queue[0]
		queue = queue[1:]
		dir, ok := project.ResolvePackageDir(req.fromDir, req.name)
		if !ok {
			if req.required {
				return nil, errs.New(errs.CodeDependencyNotInstalled, fmt.Sprintf(
					"the dependency %s is not installed in %s: run putnami install", req.name, req.fromDir)).
					WithCategory(errs.CategoryUser)
			}
			continue
		}
		if !project.InstalledFromRegistry(dir) {
			// The bundle includes a workspace project; the packages it
			// imports ship where the bundle resolves them.
			if !linked[dir] {
				linked[dir] = true
				if child := project.ReadPackageJSONSafe(filepath.Join(dir, "package.json")); child != nil {
					queue = append(queue, requests(child, dir, req.parentPath, false)...)
				}
			}
			continue
		}
		dest, placed := placeNodePackage(byPath, req.parentPath, req.name, dir)
		if placed {
			continue
		}
		if held, taken := byPath[dest]; taken && held != dir {
			return nil, fmt.Errorf("two installed versions of %s claim %s: %s and %s", req.name, dest, held, dir)
		}
		byPath[dest] = dir
		closure = append(closure, nodePackage{Dir: dir, Path: dest})
		if child := project.ReadPackageJSONSafe(filepath.Join(dir, "package.json")); child != nil {
			queue = append(queue, requests(child, dir, dest, true)...)
		}
	}
	sort.Slice(closure, func(i, j int) bool { return closure[i].Path < closure[j].Path })
	return closure, nil
}

// placeNodePackage decides where the package name, installed at dir, goes
// for the package at parentPath ("" for the project), as Node resolves it from
// there: parentPath/node_modules/<name>, then the same under each enclosing
// package, then node_modules/<name>. When the first of those slots that holds
// a package holds dir, the package is placed already and placeNodePackage
// reports true. Otherwise it returns node_modules/<name> when no slot holds a
// package, and parentPath/node_modules/<name> when a slot holds another one.
func placeNodePackage(byPath map[string]string, parentPath, name, dir string) (string, bool) {
	var slots []string
	for p := parentPath; p != ""; {
		slots = append(slots, p+"/node_modules/"+name)
		idx := strings.LastIndex(p, "/node_modules/")
		if idx < 0 {
			break
		}
		p = p[:idx]
	}
	top := "node_modules/" + name
	slots = append(slots, top)
	for _, slot := range slots {
		held, taken := byPath[slot]
		if !taken {
			continue
		}
		if held == dir {
			return slot, true
		}
		return slots[0], false
	}
	return top, false
}

// nodePackageFiles lists the files of one package of the closure as image
// files under /app/<Path>, without its own node_modules: the closure places
// every dependency itself. A file the package links to is shipped with the
// content it links to.
func nodePackageFiles(p nodePackage) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(p.Dir, func(source string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if source != p.Dir && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Stat(source)
		if err != nil || info.IsDir() {
			// A dangling link or a link to a directory ships nothing.
			return nil
		}
		rel, err := filepath.Rel(p.Dir, source)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, oci.File{Source: source, Path: path.Join("/app", p.Path, filepath.ToSlash(rel)), Mode: mode})
		return nil
	})
	return files, err
}

// nodeBundleFiles lists the Node bundle of compileOutput as image files under
// /app: every file but the staged assets and the content stamp, which their
// own layers carry.
func nodeBundleFiles(compileOutput string) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(compileOutput, func(source string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(compileOutput, source)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == contentStampFile {
			return nil
		}
		files = append(files, oci.File{Source: source, Path: path.Join("/app", rel), Mode: 0o644})
		return nil
	})
	return files, err
}

// nodeDockerSpec describes the image of a workload on the node runtime: the
// production dependencies, the Node bundle, the staged .gen assets and the
// content stamp on a distroless Node base, started with node
// --enable-source-maps on the bundle.
func nodeDockerSpec(baseRef, platform, projectDir, compileOutput, stampPath string, port int, hasAssets bool) (oci.Spec, error) {
	if port <= 0 {
		port = 3000
	}
	bundle, err := build.ReadNodeBundle(compileOutput)
	if err != nil {
		return oci.Spec{}, fmt.Errorf("the compile output holds no Node bundle: %w", err)
	}
	closure, err := nodeDependencyClosure(projectDir)
	if err != nil {
		return oci.Spec{}, err
	}
	var layers []oci.Layer
	var dependencies []oci.File
	for _, p := range closure {
		files, err := nodePackageFiles(p)
		if err != nil {
			return oci.Spec{}, fmt.Errorf("staging %s: %w", p.Path, err)
		}
		dependencies = append(dependencies, files...)
	}
	if len(dependencies) > 0 {
		layers = append(layers, oci.Layer{Files: dependencies})
	}
	app, err := nodeBundleFiles(compileOutput)
	if err != nil {
		return oci.Spec{}, fmt.Errorf("staging the Node bundle: %w", err)
	}
	layers = append(layers, oci.Layer{Files: app})
	if hasAssets {
		assets, err := assetFiles(filepath.Join(compileOutput, ".gen"))
		if err != nil {
			return oci.Spec{}, fmt.Errorf("staging assets: %w", err)
		}
		layers = append(layers, oci.Layer{Files: assets})
	}
	layers = append(layers, oci.Layer{Files: []oci.File{{Source: stampPath, Path: stampImagePath, Mode: 0o644}}})

	return oci.Spec{
		BaseRef:  baseRef,
		Platform: platform,
		Layers:   layers,
		Env: []string{
			"NODE_ENV=production",
			fmt.Sprintf("PORT=%d", port),
			"PWD=/app",
		},
		Entrypoint:   []string{nodeImageBinary, "--enable-source-maps", path.Join("/app", bundle.Entrypoint)},
		WorkingDir:   "/app",
		ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
	}, nil
}
