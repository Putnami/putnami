package jobs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/gitcandidate"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const (
	// CodeImageFailed is the diagnostic of an image that cannot be assembled.
	CodeImageFailed = "python.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest, or of
	// a Python minor no official base is pinned for.
	CodeBaseNotPinned = "python.base_not_pinned"
	// CodeNoEntrypoint is the diagnostic of an application whose entrypoint
	// file does not exist.
	CodeNoEntrypoint = "python.no_entrypoint"
	// CodePathDependencyOutsideClosure is the diagnostic of a path dependency
	// the project's dependency closure does not hold, so the image's cache key
	// would not cover it.
	CodePathDependencyOutsideClosure = "python.path_dependency_outside_closure"
)

const (
	// DockerChannel is the publish channel of an application image.
	DockerChannel = "docker"
	// ImageAppDir is where an image holds the application files.
	ImageAppDir = "/app"
	// ImageSitePackages is where an image holds the installed dependencies.
	ImageSitePackages = "/opt/python/site-packages"
	// imageUser is the unprivileged user an image runs as: nobody, by number,
	// so a named base needs no user of that name.
	imageUser        = "65534:65534"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
	// defaultEntrypoint is the entrypoint an image runs when none is
	// configured, as serve and run do.
	defaultEntrypoint = "src/main.py"
)

// slimBases are the official Python slim images, pinned by digest, by Python
// minor version.
var slimBases = map[string]string{
	"3.11": "python:3.11.18-slim@sha256:07d4824036af339fa1b6ef769cc99d9c10a141769538b52b3b597c84f7a7b535",
	"3.12": "python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b",
	"3.13": "python:3.13.18-slim@sha256:e48f59cdba69a3cbd9ed2cf0f348d90d45a8d350d98134cf1070bc106ee9ed90",
	"3.14": "python:3.14.10-slim@sha256:9aaa474097d094040649f2162a627c0049b408f52138d980916788d80f04802f",
}

// wheelPlatforms are uv's target platforms by image platform. A target of
// uv's names a glibc that runs manylinux_2_28 wheels.
var wheelPlatforms = map[string]string{
	"linux/amd64": "x86_64-unknown-linux-gnu",
	"linux/arm64": "aarch64-unknown-linux-gnu",
}

// generatedNames are the directories no image and no staged build holds, at
// any depth: installed environments and caches.
var generatedNames = []string{
	".git", ".mypy_cache", ".putnami", ".pytest_cache", ".ruff_cache", ".tox", ".venv",
	"__pycache__", "node_modules",
}

// topLevelGenerated are the directories no image and no staged build holds at
// the top of a tree: Putnami's generated files, build outputs and a virtual
// environment. Deeper, a directory of that name can be a package.
var topLevelGenerated = []string{".gen", "build", "dist", "venv"}

// testDirectories are the top-level directories of a project that only tests
// read.
var testDirectories = []string{"test", "tests"}

// unwalkedNames are the directories the ordinary walk of a cache input skips at
// any depth below a project. A file under one of them is keyed only through
// the project's Git candidates, so an image ships it only when it is one.
var unwalkedNames = []string{"dist", "out", "vendor"}

// PublishDocker pushes the packaged image through the shared publisher.
func PublishDocker(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return dockerpublish.Publish(ctx, emit, args)
}

// PackageDocker assembles the image of an application: a layer of its locked
// production dependencies, installed as wheels for the image's platform, and a
// layer of its files, on a digest-pinned Python base. It needs no Docker
// daemon and runs nothing inside the image.
func PackageDocker(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" || ctx.Project.Type == "library" {
		return "SKIP", nil, nil
	}
	if err := assembleImage(ctx, cli.ParseFlags(args), emit); err != nil {
		return "FAILED", nil, err
	}
	return "OK", nil, nil
}

func assembleImage(ctx *pctx.Context, flags map[string]string, emit *jsonl.Emitter) error {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	wsRoot := ctx.WorkspaceRoot
	projectRoot := ctx.Project.FullPath
	if _, err := os.Stat(filepath.Join(wsRoot, "uv.lock")); err != nil {
		return fail(CodeImageFailed, fmt.Errorf("%s has no uv.lock: an image installs the locked dependencies; run `putnami install`", wsRoot))
	}
	entrypoint, err := imageEntrypoint(ctx, flags)
	if err != nil {
		return fail(CodeNoEntrypoint, err)
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	wheelPlatform, ok := wheelPlatforms[platform]
	if !ok {
		return fail(CodeImageFailed, fmt.Errorf("an image of platform %s cannot be packaged: use linux/amd64 or linux/arm64", platform))
	}
	uv, err := resolveUV(wsRoot)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	env := MakeEnv(wsRoot, projectRoot, nil)
	interpreter, err := pythonVersion(uv, wsRoot, env)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	base, err := baseImage(cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage")), minorOf(interpreter))
	if err != nil {
		return fail(CodeBaseNotPinned, err)
	}

	output := pkgmeta.PackageOutputDir(wsRoot, ctx.Project.Path, DockerChannel)
	name := ImageName(ctx.Project.Name)
	version := "0.0.0"
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	record := func() error {
		return pkgmeta.WriteChannelRecord(output, pkgmeta.ChannelRecord{Version: version, Artifact: name, Channels: []string{DockerChannel}})
	}
	if cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun")) {
		emit.Info("dry run: would assemble an image of " + name + " on " + base)
		if err := record(); err != nil {
			return fail(CodeImageFailed, err)
		}
		return nil
	}

	workspace, err := os.MkdirTemp("", "putnami-python-image-")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	sitePackages := filepath.Join(workspace, "site-packages")
	if err := installDependencies(ctx, uv, env, wheelPlatform, imagePythonVersion(base, interpreter), workspace, sitePackages, emit); err != nil {
		code := CodeImageFailed
		if errors.Is(err, errOutsideClosure) {
			code = CodePathDependencyOutsideClosure
		}
		return fail(code, err)
	}

	spec, err := imageSpec(sitePackages, projectRoot, base, platform, entrypoint)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	plan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		return fail(CodeImageFailed, fmt.Errorf("plan the image: %w", err))
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(output, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(wsRoot, ".putnami", "cache", "oci-base"),
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
	})
	if err != nil {
		return fail(CodeImageFailed, fmt.Errorf("assemble the image: %w", err))
	}
	contentHash := plan.Identity[:12]
	tags := []string{name + ":c-" + contentHash}
	if tag := cli.FlagString(flags, "docker-tag", ctx.Params.String("docker-tag", "dockerTag")); tag != "" {
		tags = append(tags, name+":"+tag)
	}
	if cli.FlagBool(flags, "docker-load", ctx.Params.Bool("docker-load", false, "dockerLoad")) {
		if err := loadImage(filepath.Join(output, "oci"), tags); err != nil {
			return fail(CodeImageFailed, err)
		}
	}
	manifest, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: name, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: spec.Platform,
	}, "", "  ")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(manifest, '\n'), 0o644); err != nil { //nolint:gosec // an output of the job
		return fail(CodeImageFailed, err)
	}
	if err := record(); err != nil {
		return fail(CodeImageFailed, err)
	}
	emit.Info(fmt.Sprintf("assembled the image %s (%s)", tags[0], digest))
	return nil
}

// ImageName derives an image name from a project name: one lowercase path
// segment.
func ImageName(project string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(project, "@"), "/", "-"))
}

// imageEntrypoint answers the project-relative entrypoint the image runs: the
// entrypoint option of package, which the workspace and the project set like
// any option, else src/main.py. It must be a file of the project.
func imageEntrypoint(ctx *pctx.Context, flags map[string]string) (string, error) {
	entrypoint := cli.FlagString(flags, "entrypoint", ctx.Params.String("entrypoint"))
	if entrypoint == "" {
		entrypoint = defaultEntrypoint
	}
	clean := path.Clean(filepath.ToSlash(entrypoint))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("the entrypoint %s is outside the project", entrypoint)
	}
	if info, err := os.Stat(filepath.Join(ctx.Project.FullPath, filepath.FromSlash(clean))); err != nil || info.IsDir() {
		return "", fmt.Errorf("%s has no file %s: name the entrypoint the image runs with --entrypoint", ctx.Project.FullPath, clean)
	}
	return clean, nil
}

// pythonVersion answers the version of the interpreter uv selects at the
// workspace root, the one the tests run on.
func pythonVersion(uv, wsRoot string, env []string) (string, error) {
	find := exec.Command(uv, "python", "find") //nolint:gosec // the resolved uv
	find.Dir = wsRoot
	find.Env = env
	found, err := find.Output()
	if err != nil {
		return "", fmt.Errorf("uv python find: %w%s", err, stderrOf(err))
	}
	interpreter := strings.TrimSpace(string(found))
	version := exec.Command(interpreter, "-c", "import platform; print(platform.python_version())") //nolint:gosec // the interpreter uv found
	version.Dir = wsRoot
	version.Env = env
	answer, err := version.Output()
	if err != nil {
		return "", fmt.Errorf("read the version of %s: %w%s", interpreter, err, stderrOf(err))
	}
	return strings.TrimSpace(string(answer)), nil
}

// minorOf answers the minor of a Python version: 3.13 for 3.13.7.
func minorOf(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// imagePythonVersion answers the Python version the image runs, which the
// dependencies' markers are evaluated against: the release of an official base,
// else the interpreter's, which a named base runs the minor of.
func imagePythonVersion(base, interpreter string) string {
	for _, official := range slimBases {
		if official == base {
			tag, _, _ := strings.Cut(strings.TrimPrefix(base, "python:"), "-slim@")
			return tag
		}
	}
	return interpreter
}

// baseImage answers the named base, which must carry a digest, or the official
// slim image of the Python minor.
func baseImage(named, minor string) (string, error) {
	if named != "" {
		if _, err := oci.ParseDigestReference(named); err != nil {
			return "", fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", named, err)
		}
		return named, nil
	}
	base, ok := slimBases[minor]
	if !ok {
		return "", fmt.Errorf("no official image is pinned for Python %s: name a digest-pinned base with --docker-base-image", minor)
	}
	return base, nil
}

// errOutsideClosure marks a path dependency outside the dependency closure.
var errOutsideClosure = errors.New("outside the dependency closure")

// installDependencies installs the production dependencies of the project into
// sitePackages: the registry ones as wheels checked against their locked
// hashes, then the local ones uv export lists, the project and its path
// dependencies, built from a staged copy. Every package is selected for the
// image's platform and Python version.
func installDependencies(ctx *pctx.Context, uv string, env []string, wheelPlatform, version, workspace, sitePackages string, emit *jsonl.Emitter) error {
	wsRoot := ctx.WorkspaceRoot
	export := exec.Command(uv, "export", "--frozen", "--no-dev", "--no-editable", "--no-header", "--no-annotate", //nolint:gosec // the resolved uv
		"--package", ResolvePackageName(ctx))
	export.Dir = wsRoot
	export.Env = env
	exported, err := export.Output()
	if err != nil {
		return fmt.Errorf("uv export: %w%s", err, stderrOf(err))
	}
	registry, locals := splitRequirements(string(exported))
	staged, err := stageLocalRequirements(ctx, locals, filepath.Join(workspace, "staged"))
	if err != nil {
		return err
	}
	target := []string{"pip", "install", "--target", sitePackages, "--python-platform", wheelPlatform, "--python-version", version, "--no-deps"}
	if strings.TrimSpace(registry) != "" {
		requirements := filepath.Join(workspace, "requirements.txt")
		if err := os.WriteFile(requirements, []byte(registry), 0o600); err != nil {
			return err
		}
		emit.Info("$ uv pip install --only-binary :all: --require-hashes for " + wheelPlatform + ", Python " + version)
		install := slices.Concat(target, []string{"--only-binary", ":all:", "--require-hashes", "-r", requirements})
		if err := runUV(uv, wsRoot, env, install); err != nil {
			return err
		}
	}
	if len(staged) > 0 {
		requirements := filepath.Join(workspace, "local-requirements.txt")
		if err := os.WriteFile(requirements, []byte(strings.Join(staged, "\n")+"\n"), 0o600); err != nil {
			return err
		}
		emit.Info(fmt.Sprintf("$ uv pip install %d local packages from a staged copy", len(staged)))
		if err := runUV(uv, wsRoot, env, slices.Concat(target, []string{"-r", requirements})); err != nil {
			return err
		}
	}
	if err := dropRunRecords(sitePackages); err != nil {
		return err
	}
	// Console scripts name the interpreter of this machine, which an image
	// does not have.
	return os.RemoveAll(filepath.Join(sitePackages, "bin"))
}

// runRecords are the files an install writes in the metadata of a package
// installed from a path: the PEP 610 record, which names the staging
// directory, and uv's cache record, which holds the time of the build. Both
// differ on each run, so an image holds neither: each one is removed with its
// line in the package's RECORD.
var runRecords = []string{"direct_url.json", "uv_cache.json"}

// dropRunRecords removes the run records of the installed packages.
func dropRunRecords(sitePackages string) error {
	listings, err := filepath.Glob(filepath.Join(sitePackages, "*.dist-info"))
	if err != nil {
		return err
	}
	for _, metadata := range listings {
		dropped := map[string]bool{}
		for _, name := range runRecords {
			err := os.Remove(filepath.Join(metadata, name))
			if err == nil {
				dropped[filepath.Base(metadata)+"/"+name] = true
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if len(dropped) == 0 {
			continue
		}
		listing := filepath.Join(metadata, "RECORD")
		data, err := os.ReadFile(listing) //nolint:gosec // a staged install
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var kept []string
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if file, _, _ := strings.Cut(line, ","); !dropped[file] {
				kept = append(kept, line)
			}
		}
		if err := os.WriteFile(listing, []byte(strings.Join(kept, "")), 0o644); err != nil { //nolint:gosec // a staged install
			return err
		}
	}
	return nil
}

// localRequirement is a requirement uv export names by path: the
// workspace-relative directory, and the environment marker it carries.
type localRequirement struct {
	Path   string
	Marker string
}

// splitRequirements splits a uv export into the requirements a registry
// serves, with their hashes, and the local ones.
func splitRequirements(exported string) (string, []localRequirement) {
	var registry strings.Builder
	var locals []localRequirement
	local := false
	scanner := bufio.NewScanner(strings.NewReader(exported))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			requirement, marker, _ := strings.Cut(strings.TrimSpace(strings.TrimSuffix(trimmed, "\\")), " ;")
			requirement = strings.TrimSpace(requirement)
			local = strings.HasPrefix(requirement, ".") || strings.HasPrefix(requirement, "/") || strings.HasPrefix(requirement, "file:")
			if local {
				locals = append(locals, localRequirement{Path: strings.TrimPrefix(requirement, "file://"), Marker: strings.TrimSpace(marker)})
				continue
			}
		}
		if !local {
			registry.WriteString(line)
			registry.WriteByte('\n')
		}
	}
	return registry.String(), locals
}

// stageLocalRequirements copies each local requirement to staging, at its
// workspace-relative path, and answers the requirement lines that install the
// staged copies, markers kept. A local requirement must be the project or a
// project of its dependency closure, so the image's cache key covers it.
func stageLocalRequirements(ctx *pctx.Context, locals []localRequirement, staging string) ([]string, error) {
	wsRoot := ctx.WorkspaceRoot
	closure := map[string]bool{path.Clean(filepath.ToSlash(ctx.Project.Path)): true}
	for _, project := range ctx.Project.DependencyClosure {
		closure[path.Clean(filepath.ToSlash(project.Path))] = true
	}
	var lines []string
	staged := map[string]bool{}
	for _, dependency := range locals {
		absolute := dependency.Path
		if !filepath.IsAbs(absolute) {
			absolute = filepath.Join(wsRoot, filepath.FromSlash(dependency.Path))
		}
		rel, err := filepath.Rel(wsRoot, absolute)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("the path dependency %s is outside the workspace", dependency.Path)
		}
		rel = path.Clean(filepath.ToSlash(rel))
		if !closure[rel] {
			return nil, fmt.Errorf("the path dependency %s is %w of %s: add /%s to the dependencies of its putnami.json", rel, errOutsideClosure, ctx.Project.Name, rel)
		}
		target := filepath.Join(staging, filepath.FromSlash(rel))
		if !staged[rel] {
			if err := copyTree(filepath.Join(wsRoot, filepath.FromSlash(rel)), target); err != nil {
				return nil, err
			}
			staged[rel] = true
		}
		line := target
		if dependency.Marker != "" {
			line += " ; " + dependency.Marker
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// runUV runs uv at the workspace root.
func runUV(uv, wsRoot string, env, args []string) error {
	command := exec.Command(uv, args...) //nolint:gosec // the resolved uv
	command.Dir = wsRoot
	command.Env = env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Stdout = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("uv %s: %w\n%s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// stderrOf answers the standard error an exec failure captured.
func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "\n" + strings.TrimSpace(string(exit.Stderr))
	}
	return ""
}

// treeFilter decides which files of one tree an image ships and a staged build
// reads: the same files for both, and only files the cache key of the tree
// reads.
type treeFilter struct {
	// candidates are the Git candidates of the tree, nil outside a Git work
	// tree.
	candidates map[string]bool
}

// newTreeFilter reads the Git candidates of the tree at root.
func newTreeFilter(root string) (treeFilter, error) {
	tree, err := gitcandidate.Open(root)
	if err != nil || tree == nil {
		return treeFilter{}, err
	}
	candidates := make(map[string]bool, len(tree.Paths()))
	for _, candidate := range tree.Paths() {
		candidates[candidate] = true
	}
	return treeFilter{candidates: candidates}, nil
}

// skip reports whether a walk of the tree leaves out an entry: generated
// directories, what only tests read, files that hold secrets, and a file under
// a directory the key's walk skips that is no Git candidate.
func (f treeFilter) skip(rel string, entry fs.DirEntry) bool {
	name := entry.Name()
	top := !strings.Contains(rel, "/")
	if entry.IsDir() {
		return slices.Contains(generatedNames, name) || (top && slices.Contains(topLevelGenerated, name)) ||
			(top && slices.Contains(testDirectories, name))
	}
	if SecretFile(name) || TestFile(name) {
		return true
	}
	directories := strings.Split(rel, "/")
	for _, directory := range directories[:len(directories)-1] {
		if slices.Contains(unwalkedNames, directory) {
			return !f.candidates[rel]
		}
	}
	return false
}

// SecretFile reports the files no image holds: environment files, which hold
// the secrets of a local run, and the credentials of a package index.
func SecretFile(name string) bool {
	switch name {
	case ".env", ".envrc", ".netrc", ".pypirc":
		return true
	}
	return strings.HasPrefix(name, ".env.")
}

// TestFile reports the files pytest collects or configures.
func TestFile(name string) bool {
	return name == "conftest.py" || (strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py")) || strings.HasSuffix(name, "_test.py")
}

// copyTree copies a tree without the entries its filter leaves out.
func copyTree(source, target string) error {
	filter, err := newTreeFilter(source)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, file)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		if filter.skip(filepath.ToSlash(rel), entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		return copyFile(file, destination)
	})
}

// copyFile copies a file's content and its executable bit. A symbolic link is
// an error: the image's cache key does not cover what a link points to.
func copyFile(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link, which an image cannot hold: replace it with the file", source)
	}
	data, err := os.ReadFile(source) //nolint:gosec // inside the workspace
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, fileMode(info)) //nolint:gosec // a staged copy
}

// fileMode answers the mode an image gives a file: the executable bit is the
// only part of a local mode that Git keeps.
func fileMode(info fs.FileInfo) fs.FileMode {
	if info.Mode()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// imageSpec describes the application's image: a layer of the installed
// dependencies and a layer of the application files.
func imageSpec(sitePackages, projectRoot, base, platform, entrypoint string) (oci.Spec, error) {
	dependencies, err := treeFiles(sitePackages, ImageSitePackages, func(string, fs.DirEntry) bool { return false })
	if err != nil {
		return oci.Spec{}, err
	}
	filter, err := newTreeFilter(projectRoot)
	if err != nil {
		return oci.Spec{}, err
	}
	files, err := treeFiles(projectRoot, ImageAppDir, filter.skip)
	if err != nil {
		return oci.Spec{}, err
	}
	return oci.Spec{
		BaseRef:  base,
		Platform: platform,
		Layers:   []oci.Layer{{Files: dependencies}, {Files: files}},
		// The application is read-only for the user that runs it, so Python
		// writes no bytecode next to it.
		Env: []string{
			"PORT=" + defaultImagePort, "PYTHONPATH=" + ImageSitePackages,
			"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1",
		},
		Entrypoint:   []string{"python", ImageAppDir + "/" + entrypoint},
		WorkingDir:   ImageAppDir,
		User:         imageUser,
		ExposedPorts: []string{defaultImagePort + "/tcp"},
	}, nil
}

// treeFiles lists the files of a tree under prefix in the image, without the
// entries skip leaves out. A tree that does not exist holds no file.
func treeFiles(root, prefix string, skip func(rel string, entry fs.DirEntry) bool) ([]oci.File, error) {
	var files []oci.File
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link, which an image cannot hold: replace it with the file", file)
		}
		files = append(files, oci.File{Source: file, Path: prefix + "/" + rel, Mode: int64(fileMode(info))})
		return nil
	})
	return files, err
}

// loadImage loads the assembled image into the local Docker daemon under its
// tags.
func loadImage(layout string, tags []string) error {
	archive, err := os.CreateTemp("", "putnami-python-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := oci.WriteDockerTarball(layout, tags[0], archive.Name()); err != nil {
		return err
	}
	load := exec.Command("docker", "load", "-i", archive.Name()) //nolint:gosec // fixed program, a path from CreateTemp
	load.Stderr = os.Stderr
	if err := load.Run(); err != nil {
		return errors.New("docker load failed for " + tags[0])
	}
	for _, alias := range tags[1:] {
		tag := exec.Command("docker", "tag", tags[0], alias) //nolint:gosec // fixed program
		tag.Stderr = os.Stderr
		if err := tag.Run(); err != nil {
			return errors.New("docker tag failed for " + alias)
		}
	}
	return nil
}
