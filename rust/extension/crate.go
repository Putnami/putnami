package rust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
)

const channelCrates = "crates"

// stagedSkips are the directories the staged copy leaves out, at any depth.
var stagedSkips = map[string]bool{"target": true, ".git": true, ".putnami": true, "node_modules": true}

// stagedWorkspace is the copy of the workspace a crate stages: the stack root
// and the path dependencies it reaches, at their workspace-relative paths.
func stagedWorkspace(ctx *pctx.Context) string {
	return filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, channelCrates), "workspace")
}

// stagedRoot is the copy of the stack root inside stagedWorkspace.
func stagedRoot(ctx *pctx.Context, rootDir string) (string, error) {
	rel, err := filepath.Rel(ctx.WorkspaceRoot, rootDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(stagedWorkspace(ctx), rel), nil
}

func packageCrate(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		return crateRootJob(stack, ctx, emit, args, func(c context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return stageCrate(c, ctx, root, emit)
		})
	}
}

// crateRootJob runs step at the crate's stack root. A Cargo workspace that is
// no crate of its own has nothing to stage or send: it selects no toolchain.
func crateRootJob(stack stacks.Stack, ctx *pctx.Context, emit *jsonl.Emitter, args []string, step func(context.Context, stacks.RootStep, *jsonl.Emitter) error) (string, map[string]any, error) {
	var meta stacks.Metadata
	if json.Unmarshal(ctx.Project.Metadata[ExtensionName], &meta) == nil && meta.Component == "" && meta.Root != "" {
		emit.Info(ctx.Project.Path + " is a Cargo workspace with no crate: it has no crate to stage or publish")
		return "OK", nil, nil
	}
	return stack.RootJob(step)(ctx, emit, args)
}

// stageCrate copies the stack root, writes the release version into the copy
// and refreshes the copy's lockfile. The checkout never changes.
func stageCrate(c context.Context, ctx *pctx.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	version, err := releaseVersion(ctx)
	if err != nil {
		return err
	}
	dir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, channelCrates)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	staged, err := stagedRoot(ctx, root.Dir)
	if err != nil {
		return err
	}
	if err := stageWithDependencies(ctx.WorkspaceRoot, root.Dir, stagedWorkspace(ctx), version); err != nil {
		return fmt.Errorf("stage the crate: %w", err)
	}
	cmd := exec.CommandContext(c, root.Program("cargo"), "update", "--workspace") //nolint:gosec // the selected toolchain
	cmd.Dir = staged
	cmd.Env = root.Env
	if err := runForwarding(cmd, emit, nil); err != nil {
		return fmt.Errorf("cargo update --workspace: %w", err)
	}
	emit.Info("staged " + root.Component + " at version " + version)
	return pkgmeta.WriteChannelRecord(dir, pkgmeta.ChannelRecord{Version: version, Artifact: root.Component, Channels: []string{channelCrates}})
}

// stageWithDependencies copies the stack root under dest, then every directory
// a path dependency reaches outside what is copied: cargo reads them all, and
// a sibling crate of a stack root is not inside it.
func stageWithDependencies(workspace, stackRoot, dest, version string) error {
	var copied []string
	var stage func(string) error
	stage = func(dir string) error {
		rel, err := filepath.Rel(workspace, dir)
		if err != nil {
			return err
		}
		if err := copyStackRoot(dir, filepath.Join(dest, rel), version); err != nil {
			return err
		}
		copied = append(copied, dir)
		return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() && path != dir && stagedSkips[entry.Name()] {
				return filepath.SkipDir
			}
			if entry.IsDir() || entry.Name() != "Cargo.toml" {
				return nil
			}
			data, err := os.ReadFile(path) //nolint:gosec // a manifest of the workspace
			if err != nil {
				return err
			}
			for _, dep := range pathDependencies(string(data)) {
				target := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(dep)))
				inside, err := filepath.Rel(workspace, target)
				if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
					return fmt.Errorf("%s depends on %s, outside the Putnami workspace", path, dep)
				}
				if !slices.ContainsFunc(copied, func(c string) bool { return target == c || strings.HasPrefix(target, c+string(filepath.Separator)) }) {
					if err := stage(target); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}
	return stage(stackRoot)
}

// pathDependencies are the path values of a manifest's dependency tables,
// dev-dependencies, [patch] and [replace] included: cargo reads those too.
func pathDependencies(content string) []string {
	var out []string
	section := ""
	for _, line := range strings.Split(content, "\n") {
		if match := sectionLine.FindStringSubmatch(line); match != nil {
			section = strings.TrimSpace(match[1])
			continue
		}
		if !anyDependencySection.MatchString(section) && !strings.HasPrefix(section, "patch.") && section != "replace" {
			continue
		}
		if match := pathValue.FindStringSubmatch(line); match != nil {
			out = append(out, match[1]+match[2])
		}
	}
	return out
}

// copyStackRoot copies what cargo would package from: inside a Git repository,
// the files Git tracks or does not ignore. The copy has no .git, so a file that
// .gitignore hides would otherwise reach the registry.
func copyStackRoot(from, to, version string) error {
	files, tracked, err := gitFiles(from)
	if err != nil {
		return err
	}
	if tracked {
		for _, rel := range files {
			if skippedPath(rel) {
				continue
			}
			path := filepath.Join(from, filepath.FromSlash(rel))
			info, err := os.Lstat(path)
			// A tracked file deleted in the checkout, or a submodule.
			if err != nil || info.IsDir() {
				continue
			}
			if err := stageFile(path, filepath.Join(to, filepath.FromSlash(rel)), info, version); err != nil {
				return err
			}
		}
		return nil
	}
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != from && stagedSkips[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return stageFile(path, filepath.Join(to, rel), info, version)
	})
}

// gitFiles lists, relative to dir, the files Git tracks or does not ignore,
// and those of its submodules. tracked is false outside a repository.
func gitFiles(dir string) (files []string, tracked bool, err error) {
	files, tracked, err = gitList(dir, "--cached", "--others", "--exclude-standard")
	if err != nil || !tracked {
		return nil, false, err
	}
	// --recurse-submodules refuses --others, so it takes its own call.
	inSubmodules, _, err := gitList(dir, "--cached", "--recurse-submodules")
	if err != nil {
		return nil, false, err
	}
	for _, name := range inSubmodules {
		if !slices.Contains(files, name) {
			files = append(files, name)
		}
	}
	return files, true, nil
}

// notARepository is how Git reports a directory outside any repository. Every
// other failure stops the copy: a walk would stage the files .gitignore hides.
const notARepository = "not a git repository (or any of the parent directories)"

func gitList(dir string, selection ...string) (files []string, tracked bool, err error) {
	args := append(append([]string{"ls-files", "-z"}, selection...), "--", ".")
	cmd := exec.Command("git", args...) //nolint:gosec // fixed arguments
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(stderr.String(), notARepository) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list the files to stage in %s: git: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, true, nil
}

// skippedPath reports whether a slash-separated relative path crosses a
// directory the copy leaves out.
func skippedPath(rel string) bool {
	parts := strings.Split(rel, "/")
	return slices.ContainsFunc(parts[:len(parts)-1], func(part string) bool { return stagedSkips[part] })
}

func stageFile(path, dest string, info fs.FileInfo, version string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return err
		}
		return os.Symlink(link, dest)
	case !info.Mode().IsRegular():
		return nil
	case filepath.Base(path) == "Cargo.toml":
		data, err := os.ReadFile(path) //nolint:gosec // a manifest of the stack root
		if err != nil {
			return err
		}
		return os.WriteFile(dest, []byte(stampManifest(string(data), version)), info.Mode().Perm()) //nolint:gosec // the staged copy
	default:
		return copyFile(path, dest, info.Mode().Perm())
	}
}

func copyFile(from, to string, mode fs.FileMode) (err error) {
	source, err := os.Open(from) //nolint:gosec // a file of the stack root
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	dest, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // the staged copy
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dest.Close()) }()
	_, err = io.Copy(dest, source)
	return err
}

var (
	sectionLine   = regexp.MustCompile(`^\s*\[\[?([^\[\]]+)\]\]?`)
	versionLine   = regexp.MustCompile(`^(\s*version\s*=\s*)(?:"[^"]*"|'[^']*')`)
	versionTable  = regexp.MustCompile(`^\s*version\s*=\s*\{`)
	versionKey    = regexp.MustCompile(`\bversion\s*=\s*(?:"[^"]*"|'[^']*')`)
	pathKey       = regexp.MustCompile(`\bpath\s*=`)
	pathLine      = regexp.MustCompile(`^\s*path\s*=`)
	pathValue     = regexp.MustCompile(`\bpath\s*=\s*(?:"([^"]+)"|'([^']+)')`)
	dottedPath    = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)\.path\s*=`)
	dottedVersion = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)\.version\s*=\s*(?:"[^"]*"|'[^']*')`)
	// anyDependencySection names any table of dependencies, or of one
	// dependency.
	anyDependencySection = regexp.MustCompile(`(^|\.)(dev[-_]|build[-_])?dependencies(\.|$)`)
	dependencyName       = `(?:build[-_]dependencies|dependencies)`
	// dependencyTable names a table of normal or build dependencies, and
	// dependencyEntry the table of one of them. Dev-dependencies stay as they
	// are: cargo drops a path dev-dependency it cannot resolve.
	dependencyTable = regexp.MustCompile(`^(?:workspace\.|target\..*\.)?` + dependencyName + `$`)
	dependencyEntry = regexp.MustCompile(`^(?:workspace\.|target\..*\.)?` + dependencyName + `\.[^.]+$`)
)

// stampManifest writes the release version into a Cargo.toml, line by line so
// that everything else stays as written: the version of [package] and
// [workspace.package], and an exact version on each normal path dependency,
// which cargo requires of a published crate.
func stampManifest(content, version string) string {
	lines := strings.Split(content, "\n")
	pin := `version = "=` + version + `"`
	out := make([]string, 0, len(lines)+2)
	var section string
	// sectionStart is the index in out just after the header of section.
	sectionStart := 0
	hasPath, hasVersion := false, false
	// entryVersion is the index in out of the version line of a dependency's
	// table, or -1. dottedPaths and dottedVersions index the b.path and b.version
	// lines of a dependencies table, by dependency name.
	entryVersion := -1
	dottedPaths, dottedVersions := map[string]int{}, map[string]int{}
	closeSection := func() {
		switch {
		case section == "package" && !hasVersion:
			out = slices.Insert(out, sectionStart, `version = "`+version+`"`)
		case dependencyEntry.MatchString(section) && hasPath && entryVersion >= 0:
			out[entryVersion] = versionLine.ReplaceAllString(out[entryVersion], `${1}"=`+version+`"`)
		case dependencyEntry.MatchString(section) && hasPath:
			out = slices.Insert(out, sectionStart, pin)
		}
		for name, at := range dottedVersions {
			if _, isPath := dottedPaths[name]; isPath {
				out[at] = dottedVersion.ReplaceAllString(out[at], "${1}"+name+`.version = "=`+version+`"`)
			}
		}
		// From the last line up, so an insertion leaves the earlier indexes valid.
		names := make([]string, 0, len(dottedPaths))
		for name := range dottedPaths {
			if _, hasOwn := dottedVersions[name]; !hasOwn {
				names = append(names, name)
			}
		}
		sort.Slice(names, func(i, j int) bool { return dottedPaths[names[i]] > dottedPaths[names[j]] })
		for _, name := range names {
			at := dottedPaths[name]
			indent := dottedPath.FindStringSubmatch(out[at])[1]
			out = slices.Insert(out, at+1, indent+name+`.version = "=`+version+`"`)
		}
	}
	for _, line := range lines {
		if match := sectionLine.FindStringSubmatch(line); match != nil {
			closeSection()
			section = strings.ReplaceAll(strings.TrimSpace(match[1]), " ", "")
			out = append(out, line)
			sectionStart = len(out)
			hasPath, hasVersion, entryVersion = false, false, -1
			dottedPaths, dottedVersions = map[string]int{}, map[string]int{}
			continue
		}
		switch {
		case section == "package" || section == "workspace.package":
			if versionLine.MatchString(line) {
				line = versionLine.ReplaceAllString(line, `${1}"`+version+`"`)
				hasVersion = true
			} else if strings.HasPrefix(strings.TrimSpace(line), "version.") || versionTable.MatchString(line) {
				// version.workspace = true: the workspace states it.
				hasVersion = true
			}
		case dependencyTable.MatchString(section):
			if match := dottedPath.FindStringSubmatch(line); match != nil {
				dottedPaths[match[2]] = len(out)
			} else if match := dottedVersion.FindStringSubmatch(line); match != nil {
				dottedVersions[match[2]] = len(out)
			} else {
				line = stampInline(line, pin)
			}
		case dependencyEntry.MatchString(section):
			if versionLine.MatchString(line) {
				entryVersion = len(out)
			}
			if pathLine.MatchString(line) {
				hasPath = true
			}
		}
		out = append(out, line)
	}
	closeSection()
	return strings.Join(out, "\n")
}

// stampInline gives the path dependency of an inline table an exact version.
// Any other line stays as written.
func stampInline(line, pin string) string {
	open := strings.Index(line, "{")
	end := strings.LastIndex(line, "}")
	if open < 0 || end < open {
		return line
	}
	body := line[open+1 : end]
	if !pathKey.MatchString(body) {
		return line
	}
	if versionKey.MatchString(body) {
		body = versionKey.ReplaceAllString(body, pin)
	} else {
		body = " " + pin + "," + body
	}
	return line[:open+1] + body + line[end:]
}

// alreadyPublished is cargo's refusal of a version the registry holds, and
// nothing else that may say a file already exists.
var alreadyPublished = regexp.MustCompile(`crate \S+ already exists on |crate version .* is already uploaded`)

func publishCrates(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		return crateRootJob(stack, ctx, emit, args, func(c context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return publishCrate(c, ctx, root, emit)
		})
	}
}

// publishCrate sends the staged crate. A version the registry already holds is
// reused. A dry run names the crate and its path dependencies together: cargo
// resolves a dependency that is not on the registry yet only in one invocation.
func publishCrate(c context.Context, ctx *pctx.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	index, err := pkgmeta.ReadChannelIndex(ctx.WorkspaceRoot, ctx.Project.Path)
	if err != nil || !index.HasChannel(channelCrates) {
		return fmt.Errorf("%s has no staged crate: run putnami package --crates first", ctx.Project.Name)
	}
	staged, err := stagedRoot(ctx, root.Dir)
	if err != nil {
		return err
	}
	// The staged copy differs from any checkout by design.
	args := []string{"publish", "--locked", "--allow-dirty"}
	if registry := ctx.Params.String("crates-registry", "cratesRegistry"); registry != "" {
		args = append(args, "--registry", registry)
	}
	cargo := root.Program("cargo")
	env := root.Env
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")
	packages := []string{root.Component}
	if dryRun {
		var outside []string
		if packages, outside, err = withPathDependencies(c, cargo, staged, root); err != nil {
			return err
		}
		if len(outside) > 0 {
			return listCrate(c, cargo, staged, env, root, outside, emit)
		}
		args = append(args, "--dry-run")
		// Cargo takes several --package from 1.90: the machine's may be older.
		if len(packages) > 1 && os.Getenv(extensionproto.OfflineDependenciesEnv) != "1" {
			provided, err := Provide(c, root.Expected, emit.Info)
			if err != nil {
				return fmt.Errorf("install the Rust of the dry run: %w", err)
			}
			bin := filepath.Dir(provided.Executable)
			cargo = filepath.Join(bin, "cargo"+stacks.ExecutableSuffix(hostPlatform))
			env = prependPath(append(withoutRustupEnv(env), provided.Env...), bin)
		}
	}
	for _, name := range packages {
		args = append(args, "--package", name)
	}
	cmd := exec.CommandContext(c, cargo, args...) //nolint:gosec // the selected toolchain, with fixed verbs
	cmd.Dir = staged
	cmd.Env = env
	var stderr strings.Builder
	runErr := runForwarding(cmd, emit, &stderr)
	if runErr == nil {
		return nil
	}
	if alreadyPublished.MatchString(stderr.String()) {
		emit.Info("the registry already holds " + root.Component + ": reusing it")
		return nil
	}
	return fmt.Errorf("cargo publish %s: %w", root.Component, runErr)
}

// listCrate is the dry run of a crate whose path dependency is outside its
// Cargo workspace: cargo can only build the crate against the registry, which
// does not hold that dependency yet, so the dry run checks the file list.
func listCrate(c context.Context, cargo, staged string, env []string, root stacks.RootStep, outside []string, emit *jsonl.Emitter) error {
	emit.Diagnostic("warning", "the dry run of "+root.Component+" checks its files only: "+strings.Join(outside, ", ")+" is outside its Cargo workspace and cargo cannot build against it before it is published", "", 0)
	cmd := exec.CommandContext(c, cargo, "package", "--list", "--allow-dirty", "--package", root.Component) //nolint:gosec // the selected toolchain, with fixed verbs
	cmd.Dir = staged
	cmd.Env = env
	if err := runForwarding(cmd, emit, nil); err != nil {
		return fmt.Errorf("cargo package --list %s: %w", root.Component, err)
	}
	return nil
}

// withPathDependencies is the crate and every path dependency it reaches
// through normal and build edges, sorted after the crate. A dependency that is
// no member of the crate's Cargo workspace, which cargo cannot name in one
// invocation with it, comes back in outside instead.
func withPathDependencies(c context.Context, cargo, staged string, root stacks.RootStep) (packages, outside []string, err error) {
	cmd := exec.CommandContext(c, cargo, "metadata", "--no-deps", "--format-version", "1") //nolint:gosec // the selected toolchain
	cmd.Dir = staged
	cmd.Env = root.Env
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("cargo metadata: %w", err)
	}
	var metadata struct {
		Packages []struct {
			Name         string `json:"name"`
			Dependencies []struct {
				Name string  `json:"name"`
				Kind *string `json:"kind"`
				Path string  `json:"path"`
			} `json:"dependencies"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(out, &metadata); err != nil {
		return nil, nil, fmt.Errorf("cargo metadata: %w", err)
	}
	members := map[string]bool{}
	edges := map[string][]string{}
	for _, pkg := range metadata.Packages {
		members[pkg.Name] = true
		for _, dep := range pkg.Dependencies {
			if dep.Path != "" && (dep.Kind == nil || *dep.Kind == "build") {
				edges[pkg.Name] = append(edges[pkg.Name], dep.Name)
			}
		}
	}
	seen := map[string]bool{root.Component: true}
	queue := []string{root.Component}
	var reached []string
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, dep := range edges[name] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if !members[dep] {
				outside = append(outside, dep)
				continue
			}
			reached = append(reached, dep)
			queue = append(queue, dep)
		}
	}
	sort.Strings(reached)
	sort.Strings(outside)
	return append([]string{root.Component}, reached...), outside, nil
}

// publishDocker pushes the assembled image with the shared publisher. A
// library has no image.
func publishDocker(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Type == typeLibrary {
		emit.Info(ctx.Project.Name + " is a library: it has no image to push")
		return "OK", nil, nil
	}
	return dockerpublish.Publish(ctx, emit, args)
}
