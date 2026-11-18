package pkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/treearchive"
)

// ExtensionRootPlaceholder is the placeholder a manifest's tasks and hooks use
// for the directory the extension is installed in.
const ExtensionRootPlaceholder = "{extensionRoot}"

// ArchivesChannel is the publish channel an extension archive is recorded on:
// the one the archive uploader (@putnami/cloud packages publish) reads.
const ArchivesChannel = "archives"

// archiveDocs are the project files an extension archive carries when they
// exist, beside its manifest and agent content.
var archiveDocs = []string{"README.md", "AI.md", "LICENSE.md"}

// bunResolveOrder is the order in which `bun run <name>` tries extensions for
// a name that is not a file, as Bun 1.4 resolves it. The packager stages an
// extensionless entry point from the first one the project holds, so the
// archive runs the source `bun run` would have run.
var bunResolveOrder = []string{".tsx", ".jsx", ".mts", ".ts", ".mjs", ".js", ".cts", ".cjs"}

// bundledSourceExtensions are the source extensions an entry point is bundled
// from: TypeScript and JSX sources, which need their imports inlined to run
// from the archive.
var bundledSourceExtensions = []string{".tsx", ".jsx", ".mts", ".ts", ".cts"}

// languageRunHooks are the hooks the TypeScript extension discovers in the
// node_modules of each project that depends on a framework package, and runs
// from the npm package that project installed. An archive leaves them out:
// the CLI would otherwise run the archived copy before each of the
// extension's own jobs, a second generator at another version (ADR 0012).
var languageRunHooks = []string{"preBuild", "configExtract"}

// ExtensionArchiveResult describes the archives one packaging wrote.
type ExtensionArchiveResult struct {
	// Name is the extension name the manifest declares.
	Name string
	// Artifact is the name the archive files carry: @scope/name becomes
	// scope-name.
	Artifact string
	// Version is the version stamped into the manifest and recorded.
	Version string
	// Entries are the paths under {extensionRoot} the manifest names, sorted.
	Entries []string
	// LeftOutHooks are the hooks of the source manifest the archive leaves
	// out (languageRunHooks), sorted.
	LeftOutHooks []string
	// Archives are the archive paths written, one per registry platform key.
	// A dry run writes none and lists the names it would write.
	Archives []string
}

// ExtensionArchiveOptions configure PackageExtensionArchives.
type ExtensionArchiveOptions struct {
	// Bun is the bun executable the entry points are bundled with.
	Bun string
	// ProjectRoot is the project's directory.
	ProjectRoot string
	// WorkspaceRoot is the workspace's directory, whose LICENSE files the
	// archive carries, as the npm package does. Empty stages none.
	WorkspaceRoot string
	// OutputDir is the archives directory the step owns:
	// <command-output>/archives.
	OutputDir string
	// Version is the version the archive is published under.
	Version string
	// DryRun stages and checks the archive but writes no file.
	DryRun bool
}

// errNoExecutableSurface reports a project whose manifest declares no task,
// command or hook: there is nothing for this packager to ship.
var errNoExecutableSurface = errors.New("the extension manifest declares no task, command or hook")

// errNoManifest reports a project without putnami.extension.json.
var errNoManifest = errors.New("the project has no putnami.extension.json")

// IsNoExtensionSurface reports whether err means the project has no extension
// manifest with tasks, commands or hooks, which the package step skips. Any
// other error, a missing file of the extension included, fails the step.
func IsNoExtensionSurface(err error) bool {
	return errors.Is(err, errNoExecutableSurface) || errors.Is(err, errNoManifest)
}

// archiveManifest is the part of an extension manifest this packager reads.
type archiveManifest struct {
	Name     string                     `json:"name"`
	Commands map[string]json.RawMessage `json:"commands"`
	Tasks    map[string]archiveCommand  `json:"tasks"`
	Hooks    map[string]archiveCommand  `json:"hooks"`
}

// archiveCommand is the part of a task or hook that names files.
type archiveCommand struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// PackageExtensionArchives packages a TypeScript extension as the archive the
// CLI installs from the registry (ADR 0012).
//
// The stage holds the manifest with the version stamped and without the hooks
// the TypeScript extension runs from node_modules (ArchivedManifest), the
// project's README.md, AI.md and LICENSE.md, its agent content, and every
// entry point a task or kept hook names under {extensionRoot}: a TypeScript
// source is bundled with its imports into the same name with the .js
// extension, so it runs with Bun and no node_modules; any other file or
// directory is copied as is. Packaging fails,
// before it writes any archive, when a name the manifest uses does not resolve
// in the stage the way `bun run` resolves it. The manifest is then gated and
// stamped in its archive form, and the same reproducible archive is written
// under each registry platform key, with the channel record and the
// publication metadata the archive uploader reads.
func PackageExtensionArchives(opts ExtensionArchiveOptions) (*ExtensionArchiveResult, error) {
	manifestPath := filepath.Join(opts.ProjectRoot, "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoManifest
	}
	if err != nil {
		return nil, err
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestPath, err)
	}
	if len(manifest.Commands) == 0 && len(manifest.Tasks) == 0 && len(manifest.Hooks) == 0 {
		return nil, errNoExecutableSurface
	}
	if manifest.Name == "" {
		return nil, fmt.Errorf("%s names no extension: an archive is published under the manifest's name", manifestPath)
	}
	archived, err := ArchivedManifest(data, opts.Version)
	if err != nil {
		return nil, err
	}
	entries, err := ExtensionRootEntries(archived)
	if err != nil {
		return nil, err
	}

	stage, err := os.MkdirTemp("", "putnami-ts-archive-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := os.WriteFile(filepath.Join(stage, "putnami.extension.json"), archived, 0o644); err != nil {
		return nil, err
	}
	// The workspace's LICENSE files first, so the project's own LICENSE.md,
	// staged next, takes precedence over the workspace's file of that name.
	if opts.WorkspaceRoot != "" {
		copyLicenseFiles(opts.WorkspaceRoot, stage)
	}
	for _, doc := range archiveDocs {
		if err := copyIfPresent(filepath.Join(opts.ProjectRoot, doc), filepath.Join(stage, doc)); err != nil {
			return nil, err
		}
	}
	if _, err := agentartifact.StageExtensionContent(opts.ProjectRoot, stage, manifest.Name, opts.Version); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := stageEntry(opts.Bun, opts.ProjectRoot, stage, entry); err != nil {
			return nil, err
		}
	}
	if missing := unresolvedEntries(stage, entries); len(missing) > 0 {
		return nil, fmt.Errorf("the extension manifest names %s under %s, which the archive does not hold: name a project file or directory, or a TypeScript entry point without its extension",
			strings.Join(missing, ", "), ExtensionRootPlaceholder)
	}
	if err := gateAndStampManifestContract(stage, putArchive); err != nil {
		return nil, err
	}

	result := &ExtensionArchiveResult{
		Name:     manifest.Name,
		Artifact: archiveArtifactName(manifest.Name),
		Version:  opts.Version,
		Entries:  entries,
	}
	for _, name := range languageRunHooks {
		if _, ok := manifest.Hooks[name]; ok {
			result.LeftOutHooks = append(result.LeftOutHooks, name)
		}
	}
	sort.Strings(result.LeftOutHooks)
	for _, platform := range pkgmeta.ArchivePlatformSuffixes() {
		result.Archives = append(result.Archives, filepath.Join(opts.OutputDir, result.Artifact+"-"+platform+".tar.gz"))
	}
	if opts.DryRun {
		return result, nil
	}
	if err := writePlatformArchives(stage, opts.OutputDir, result.Archives); err != nil {
		return nil, err
	}
	return result, nil
}

// ExtensionRootEntries returns the paths, relative to the extension root and
// slash-separated, that the manifest's tasks and hooks name in their command
// or arguments through {extensionRoot}/. A name that leaves the extension root
// is an error.
func ExtensionRootEntries(manifestData []byte) ([]string, error) {
	var manifest archiveManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse the extension manifest: %w", err)
	}
	seen := map[string]bool{}
	var entries []string
	collect := func(owner string, command archiveCommand) error {
		for _, token := range append([]string{command.Command}, command.Args...) {
			index := strings.Index(token, ExtensionRootPlaceholder+"/")
			if index < 0 {
				continue
			}
			rel := token[index+len(ExtensionRootPlaceholder)+1:]
			clean := path.Clean(rel)
			if rel == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
				return fmt.Errorf("%s names %q, which is not a file under %s", owner, token, ExtensionRootPlaceholder)
			}
			if !seen[clean] {
				seen[clean] = true
				entries = append(entries, clean)
			}
		}
		return nil
	}
	for _, name := range sortedKeys(manifest.Tasks) {
		if err := collect("task "+name, manifest.Tasks[name]); err != nil {
			return nil, err
		}
	}
	for _, name := range sortedKeys(manifest.Hooks) {
		if err := collect("hook "+name, manifest.Hooks[name]); err != nil {
			return nil, err
		}
	}
	sort.Strings(entries)
	return entries, nil
}

// stageEntry stages one entry point the manifest names, resolved in Bun's
// order. A TypeScript source, named exactly or without its extension, is
// bundled into the entry's name without its extension plus .js; any other file
// or directory is copied under its own name. An entry the project does not
// hold stages nothing, and the archive check reports it.
func stageEntry(bun, projectRoot, stage, entry string) error {
	source := filepath.Join(projectRoot, filepath.FromSlash(entry))
	if isDirectory(source) {
		destination := filepath.Join(stage, filepath.FromSlash(entry))
		if isDirectory(destination) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := treearchive.CopyTree(source, destination, nil); err != nil {
			return fmt.Errorf("stage %s: %w", entry, err)
		}
		return nil
	}
	if isRegularFile(source) {
		if slices.Contains(bundledSourceExtensions, path.Ext(entry)) {
			return bundleEntry(bun, projectRoot, source, stage, strings.TrimSuffix(entry, path.Ext(entry))+".js")
		}
		return copyIfPresent(source, filepath.Join(stage, filepath.FromSlash(entry)))
	}
	for _, ext := range bunResolveOrder {
		if !isRegularFile(source + ext) {
			continue
		}
		if slices.Contains(bundledSourceExtensions, ext) {
			return bundleEntry(bun, projectRoot, source+ext, stage, entry+".js")
		}
		return copyIfPresent(source+ext, filepath.Join(stage, filepath.FromSlash(entry+ext)))
	}
	return nil
}

// bundleEntry bundles the TypeScript source with its imports into the stage
// at out, for Bun.
func bundleEntry(bun, projectRoot, source, stage, out string) error {
	target := filepath.Join(stage, filepath.FromSlash(out))
	if bun == "" {
		return fmt.Errorf("bundle %s: no bun to bundle with; run putnami install", out)
	}
	args := []string{"build", "--target", "bun", "--outfile", target, source}
	result, err := exec.Run(bun, args, exec.Dir(projectRoot), exec.Timeout(5*time.Minute))
	if err != nil {
		return fmt.Errorf("bundle %s: %w", out, err)
	}
	if !result.Success {
		return fmt.Errorf("bundle %s: %s", out, strings.TrimSpace(result.Stderr))
	}
	if !isRegularFile(target) {
		return fmt.Errorf("bundle %s: bun wrote no %s", out, out)
	}
	return nil
}

// unresolvedEntries returns the entries `bun run` would not find in the stage:
// not the file itself, and not the name with one of the extensions Bun tries.
func unresolvedEntries(stage string, entries []string) []string {
	var missing []string
	for _, entry := range entries {
		if !ResolvesForBunRun(stage, entry) {
			missing = append(missing, entry)
		}
	}
	return missing
}

// ResolvesForBunRun reports whether the entry exists under root as the
// installed extension uses it: the file or directory itself, or, for `bun
// run`, the entry with an extension Bun tries.
func ResolvesForBunRun(root, entry string) bool {
	file := filepath.Join(root, filepath.FromSlash(entry))
	if isRegularFile(file) || isDirectory(file) {
		return true
	}
	for _, ext := range bunResolveOrder {
		if isRegularFile(file + ext) {
			return true
		}
	}
	return false
}

// ArchivedManifest returns the manifest an archive carries: the source
// manifest with the archive's version, and without the hooks the TypeScript
// extension runs from node_modules (languageRunHooks). A hooks object left
// empty is removed. The source manifest is never touched.
func ArchivedManifest(data []byte, version string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse the extension manifest: %w", err)
	}
	raw["version"] = version
	if hooks, ok := raw["hooks"].(map[string]any); ok {
		for _, name := range languageRunHooks {
			delete(hooks, name)
		}
		if len(hooks) == 0 {
			delete(raw, "hooks")
		}
	}
	rewritten, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(rewritten, '\n'), nil
}

// writePlatformArchives packs the stage once and writes the same bytes under
// each archive path: the archive holds no platform executable, and the CLI's
// installer asks the registry for its own platform's key.
func writePlatformArchives(stage, outputDir string, archives []string) error {
	if err := os.MkdirAll(filepath.Dir(outputDir), 0o755); err != nil {
		return err
	}
	// The archives are written beside the output directory and replace it
	// only once every one is on disk: a failed run leaves the previous
	// archives and the metadata that describes them as they were.
	pending, err := os.MkdirTemp(filepath.Dir(outputDir), ".archives-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(pending) }()
	packed := filepath.Join(pending, "pack.tar.gz")
	if err := treearchive.WriteTarGz(packed, stage); err != nil {
		return fmt.Errorf("write the extension archive: %w", err)
	}
	for _, archive := range archives {
		if err := copyIfPresent(packed, filepath.Join(pending, filepath.Base(archive))); err != nil {
			return err
		}
	}
	if err := os.Remove(packed); err != nil {
		return err
	}
	// MkdirTemp creates the directory 0700; the archives directory every
	// packager writes is 0755.
	if err := os.Chmod(pending, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(outputDir); err != nil {
		return err
	}
	return os.Rename(pending, outputDir)
}

// archiveArtifactName is the name archive files carry, the CLI's encoding of
// an extension name: @putnami/web becomes putnami-web.
func archiveArtifactName(name string) string {
	trimmed := strings.TrimPrefix(name, "@")
	return strings.Replace(trimmed, "/", "-", 1)
}

func isDirectory(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func isRegularFile(file string) bool {
	info, err := os.Stat(file)
	return err == nil && info.Mode().IsRegular()
}

// copyIfPresent copies a regular file, creating the destination's directory.
// A source that does not exist copies nothing.
func copyIfPresent(source, destination string) (err error) {
	in, err := os.Open(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
