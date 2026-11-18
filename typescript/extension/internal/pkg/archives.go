package pkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/sdk/extension/treearchive"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// ArchivesChannel is the publish channel of an extension archive: the one the
// archive uploader (@putnami/cloud packages publish) publishes from
// <command-output>/archives.
const ArchivesChannel = "archives"

// extensionRootToken is the placeholder a manifest writes for the directory
// the CLI installs the extension into.
const extensionRootToken = "{extensionRoot}"

// scriptExtensions are the source extensions of a script a manifest names
// without an extension, in the order Bun resolves them. The archive carries
// the bundle of that script under the name with ".js".
var scriptExtensions = []string{".ts", ".tsx", ".mts", ".js", ".mjs", ".jsx"}

// placeholderPattern matches a manifest placeholder such as {projectRoot}.
var placeholderPattern = regexp.MustCompile(`\{[A-Za-z]+\}`)

// ArchivesParams is what one extension-archive packaging run needs.
type ArchivesParams struct {
	WorkspaceRoot string
	ProjectPath   string
	// PackageName is the name the archive is published under: the manifest's
	// name.
	PackageName string
	Version     string
	Stable      bool
	// OutputRoot is the package command's output directory. The archives go
	// to <OutputRoot>/archives.
	OutputRoot string
	DryRun     bool
	// Bundle writes a self-contained bundle of the script at source to out.
	// PackageArchives bundles with Bun when it is nil.
	Bundle func(projectRoot, source, out string) error
}

// ArchivesResult describes the archives one packaging run wrote.
type ArchivesResult struct {
	Artifact string
	Version  string
	// Archives are the archive paths, one per registry platform key.
	Archives []string
	// Files are the slash-separated paths the archive holds, sorted.
	Files []string
}

// PackageArchives packages a TypeScript extension as the archive the CLI
// installs with `putnami extensions install`. The archive is platform
// independent: it is written once per registry platform key
// (pkgmeta.ArchivePlatformSuffixes), with identical bytes, into
// <OutputRoot>/archives, beside the archive channel record, and the archive
// publication manifest is written to <command-output>/metadata.json.
//
// The archive holds the version-stamped manifest, its built agent content,
// README.md, AI.md, the workspace LICENSE files, and every file the manifest
// names under {extensionRoot} (stageExtensionReferences): a script is bundled
// with every module it imports, so it runs from the CLI's artifact store,
// where no node_modules is. Packaging fails when the manifest names a file the
// archive would not hold (VerifyExtensionReferences), or when the manifest
// does not pass the contract gate of a put archive.
func PackageArchives(p ArchivesParams) (*ArchivesResult, error) {
	projectRoot := filepath.Join(p.WorkspaceRoot, p.ProjectPath)
	manifestSource := filepath.Join(projectRoot, "putnami.extension.json")
	if !project.FileExists(manifestSource) {
		return nil, fmt.Errorf("the archives channel packages an extension, and %s has no putnami.extension.json", p.ProjectPath)
	}
	artifact, err := archiveArtifactName(p.PackageName)
	if err != nil {
		return nil, err
	}
	bundle := p.Bundle
	if bundle == nil {
		bundle = bundleScript
	}

	tmp, err := scratch.New("putnami-ts-archives-")
	if err != nil {
		return nil, fmt.Errorf("create the archive stage: %w", err)
	}
	defer func() { _ = tmp.Remove() }()
	// The scratch directory holds its owner record; the stage is a directory
	// of its own below it, so the archive holds only what is staged.
	stageDir := filepath.Join(tmp.Path(), "stage")
	if err := os.Mkdir(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("create the archive stage: %w", err)
	}

	if err := stageArchiveManifest(manifestSource, stageDir, p.Version); err != nil {
		return nil, err
	}
	if _, err := agentartifact.StageExtensionContent(projectRoot, stageDir, p.PackageName, p.Version); err != nil {
		return nil, err
	}
	if err := gateAndStampManifestContract(stageDir, putArchive); err != nil {
		return nil, err
	}
	if _, err := validateStagedRuntimeExecutable(stageDir, ""); err != nil {
		return nil, err
	}
	copyReadme(projectRoot, stageDir)
	if src := filepath.Join(projectRoot, "AI.md"); project.FileExists(src) {
		if err := build.CopyFile(src, filepath.Join(stageDir, "AI.md")); err != nil {
			return nil, fmt.Errorf("stage AI.md: %w", err)
		}
	}
	copyLicenseFiles(p.WorkspaceRoot, stageDir)
	if err := stageExtensionReferences(projectRoot, stageDir, bundle); err != nil {
		return nil, err
	}
	if err := VerifyExtensionReferences(stageDir); err != nil {
		return nil, err
	}
	if err := normalizeStageModes(stageDir); err != nil {
		return nil, err
	}
	files, err := stagedFiles(stageDir)
	if err != nil {
		return nil, err
	}

	result := &ArchivesResult{Artifact: artifact, Version: p.Version, Files: files}
	outputDir := filepath.Join(p.OutputRoot, ArchivesChannel)
	for _, suffix := range pkgmeta.ArchivePlatformSuffixes() {
		result.Archives = append(result.Archives, filepath.Join(outputDir, artifact+"-"+suffix+".tar.gz"))
	}
	if p.DryRun {
		return result, nil
	}

	if err := os.RemoveAll(outputDir); err != nil {
		return nil, fmt.Errorf("clear %s: %w", outputDir, err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", outputDir, err)
	}
	for _, archive := range result.Archives {
		if err := treearchive.WriteTarGz(archive, stageDir); err != nil {
			return nil, fmt.Errorf("write %s: %w", filepath.Base(archive), err)
		}
	}
	channels := []string{ArchivesChannel}
	if err := pkgmeta.WriteChannelRecord(outputDir, pkgmeta.ChannelRecord{
		Version:  p.Version,
		Artifact: artifact,
		Stable:   p.Stable,
		Channels: channels,
	}); err != nil {
		return nil, fmt.Errorf("write the archive channel record: %w", err)
	}
	if err := pkgmeta.WritePackageMetadata(p.WorkspaceRoot, p.ProjectPath, pkgmeta.PackageMetadata{
		Version:  p.Version,
		Artifact: artifact,
		Stable:   p.Stable,
		Channels: channels,
	}); err != nil {
		return nil, fmt.Errorf("write the archive publication manifest: %w", err)
	}
	return result, nil
}

// archiveArtifactName is the file name stem of an extension's archives, the
// one the registry resolves: @putnami/web becomes putnami-web.
func archiveArtifactName(name string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(name), "@")
	artifact := strings.ReplaceAll(trimmed, "/", "-")
	if artifact == "" || strings.ContainsAny(artifact, `\ `) || strings.Contains(artifact, "..") {
		return "", fmt.Errorf("invalid extension archive name %q", name)
	}
	return artifact, nil
}

// stageArchiveManifest copies the authored manifest into stageDir with the
// version stamped.
func stageArchiveManifest(source, stageDir, version string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse %s: %w", source, err)
	}
	manifest["version"] = version
	stamped, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("stamp the version into %s: %w", source, err)
	}
	return os.WriteFile(filepath.Join(stageDir, "putnami.extension.json"), append(stamped, '\n'), 0o644)
}

// ExtensionReference is one path a manifest names under {extensionRoot}.
type ExtensionReference struct {
	// Path is the slash-separated path below the extension root.
	Path string
	// Location is where the manifest names it, such as
	// tasks.web-i18n-sync-exec.args[1].
	Location string
}

// ExtensionReferences returns every path the manifest names under
// {extensionRoot}, wherever it names one, sorted by location. A string names
// a path when it holds the placeholder followed by a slash; the path runs to
// the end of the string.
func ExtensionReferences(manifest []byte) ([]ExtensionReference, error) {
	var document any
	if err := json.Unmarshal(manifest, &document); err != nil {
		return nil, fmt.Errorf("parse the extension manifest: %w", err)
	}
	var refs []ExtensionReference
	var walk func(value any, location string) error
	walk = func(value any, location string) error {
		switch v := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if location != "" {
					next = location + "." + key
				}
				if err := walk(v[key], next); err != nil {
					return err
				}
			}
		case []any:
			for index, item := range v {
				if err := walk(item, fmt.Sprintf("%s[%d]", location, index)); err != nil {
					return err
				}
			}
		case string:
			at := strings.Index(v, extensionRootToken)
			if at < 0 {
				return nil
			}
			rest := v[at+len(extensionRootToken):]
			if rest == "" {
				return nil
			}
			if !strings.HasPrefix(rest, "/") {
				return fmt.Errorf("%s: %q names %s without a path below it", location, v, extensionRootToken)
			}
			rel := strings.TrimPrefix(rest, "/")
			if placeholderPattern.MatchString(rel) {
				return fmt.Errorf("%s: %q names a path below %s with a placeholder in it, which no archive can be checked against", location, v, extensionRootToken)
			}
			clean := path.Clean(rel)
			if rel == "" || clean == "." || !filepath.IsLocal(filepath.FromSlash(clean)) {
				return fmt.Errorf("%s: %q names a path outside %s", location, v, extensionRootToken)
			}
			refs = append(refs, ExtensionReference{Path: clean, Location: location})
		}
		return nil
	}
	if err := walk(document, ""); err != nil {
		return nil, err
	}
	return refs, nil
}

// stageExtensionReferences writes into stageDir every path the staged manifest
// names under {extensionRoot}:
//   - a path the project holds as a file or a directory is copied as it is;
//   - a path the project holds as a script without its extension (bin/i18n
//     for bin/i18n.ts) is bundled, with every module it imports, to the path
//     with ".js", which Bun resolves the same name to;
//   - a path that names a TypeScript source with its extension is refused:
//     the archive holds its bundle under another name.
func stageExtensionReferences(projectRoot, stageDir string, bundle func(projectRoot, source, out string) error) error {
	manifest, err := os.ReadFile(filepath.Join(stageDir, "putnami.extension.json"))
	if err != nil {
		return fmt.Errorf("read the staged extension manifest: %w", err)
	}
	refs, err := ExtensionReferences(manifest)
	if err != nil {
		return err
	}
	staged := map[string]bool{}
	for _, ref := range refs {
		if staged[ref.Path] {
			continue
		}
		staged[ref.Path] = true
		if err := stageExtensionReference(projectRoot, stageDir, ref, bundle); err != nil {
			return err
		}
	}
	return nil
}

func stageExtensionReference(projectRoot, stageDir string, ref ExtensionReference, bundle func(projectRoot, source, out string) error) error {
	native := filepath.FromSlash(ref.Path)
	switch ext := path.Ext(ref.Path); ext {
	case ".ts", ".tsx", ".mts", ".jsx":
		return fmt.Errorf("%s names {extensionRoot}/%s: name the script without its extension (%s), which Bun resolves to the source here and to its bundle in the archive",
			ref.Location, ref.Path, strings.TrimSuffix(ref.Path, ext))
	}
	source := filepath.Join(projectRoot, native)
	if info, err := os.Lstat(source); err == nil {
		dest := filepath.Join(stageDir, native)
		if _, err := os.Lstat(dest); err == nil {
			// The packager staged it already: the manifest, README.md, AI.md or
			// a LICENSE file.
			return nil
		}
		switch {
		case info.IsDir():
			return treearchive.CopyTree(source, dest, nil)
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			return build.CopyFile(source, dest)
		default:
			return fmt.Errorf("%s names {extensionRoot}/%s, which is neither a regular file nor a directory", ref.Location, ref.Path)
		}
	}
	for _, ext := range scriptExtensions {
		script := source + ext
		if !project.FileExists(script) {
			continue
		}
		out := filepath.Join(stageDir, native+".js")
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := bundle(projectRoot, script, out); err != nil {
			return fmt.Errorf("%s names {extensionRoot}/%s: bundle %s: %w", ref.Location, ref.Path, filepath.Base(script), err)
		}
		return nil
	}
	return fmt.Errorf("%s names {extensionRoot}/%s, which the project does not hold (no file, directory or script of that name)", ref.Location, ref.Path)
}

// VerifyExtensionReferences refuses an extension directory whose manifest
// names, under {extensionRoot}, a path the directory does not hold. A path
// resolves the way Bun runs it: the file or directory itself, or a script of
// that name with one of the script extensions.
func VerifyExtensionReferences(extensionDir string) error {
	manifest, err := os.ReadFile(filepath.Join(extensionDir, "putnami.extension.json"))
	if err != nil {
		return fmt.Errorf("read the extension manifest: %w", err)
	}
	refs, err := ExtensionReferences(manifest)
	if err != nil {
		return err
	}
	var missing []string
	for _, ref := range refs {
		if !referenceResolves(extensionDir, ref.Path) {
			missing = append(missing, fmt.Sprintf("%s names {extensionRoot}/%s", ref.Location, ref.Path))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the extension archive lacks what its manifest runs: %s", strings.Join(missing, "; "))
	}
	return nil
}

func referenceResolves(extensionDir, rel string) bool {
	target := filepath.Join(extensionDir, filepath.FromSlash(rel))
	if info, err := os.Lstat(target); err == nil {
		return info.IsDir() || info.Mode().IsRegular()
	}
	for _, ext := range scriptExtensions {
		if info, err := os.Lstat(target + ext); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// normalizeStageModes gives every staged directory 0755 and every staged file
// 0644: Bun runs the scripts, so none is executable, and the archive bytes do
// not depend on the packaging machine's umask.
func normalizeStageModes(stageDir string) error {
	return filepath.WalkDir(stageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return os.Chmod(p, 0o644)
	})
}

func stagedFiles(stageDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(stageDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(stageDir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	return files, err
}

// bundleTimeout bounds one script bundle.
const bundleTimeout = 5 * time.Minute

// execRunBun runs Bun. Tests replace it.
var execRunBun = exec.Run

// resolveBunBinary finds the Bun the bundler runs. Tests replace it.
var resolveBunBinary = toolchain.ResolveBun

// bundleScript bundles the script at source, with every module it imports, as
// one ES module for Bun at out. It runs from projectRoot, so Bun reads the
// project's tsconfig.json and resolves its dependencies there.
func bundleScript(projectRoot, source, out string) error {
	bun, err := resolveBunBinary()
	if err != nil {
		return err
	}
	args := []string{"build", "--target=bun", "--format=esm", "--env=disable", "--outfile=" + out, source}
	result, err := execRunBun(bun, args, exec.Dir(projectRoot), exec.Timeout(bundleTimeout))
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New(strings.TrimSpace(result.Stderr + "\n" + result.Stdout))
	}
	if !project.FileExists(out) {
		return fmt.Errorf("bun wrote no %s", filepath.Base(out))
	}
	return nil
}
