// Package pkg implements the package job (npm, docker, archives).
package pkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/catalog"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/project"
)

// NpmResult holds the output of npm packaging.
//
// There is no tag: a dist-tag IS a channel, and packaging records no channel.
// Which channels a publication advances is the release set's decision, taken
// once for every member after all of them have a verified digest.
type NpmResult struct {
	Version    string `json:"version"`
	PackageDir string `json:"packageDir"`
	Stable     bool   `json:"stable"`
}

// NpmReleaseSetPlan is the npm projection of the provider-neutral sparse
// release plan. Versions contains the exact next-snapshot version for every
// npm coordinate, including unchanged members inherited from the base set.
// A nil plan preserves the legacy full/cloudless packaging path.
type NpmReleaseSetPlan struct {
	Coordinate string
	Version    string
	Versions   map[string]string
}

// PackageNpm prepares an npm package from build output.
// Copies transpiled output, merges types, rewrites package.json for publishing.
func PackageNpm(workspaceRoot string, projectPath string, projectName string, stable bool, versionInfo *git.VersionInfo, wsVersion string) (*NpmResult, error) {
	return packageNpm(workspaceRoot, projectPath, stable, versionInfo, wsVersion, nil)
}

// PackageNpmWithReleaseSet prepares one selected npm member using the exact
// mixed-version snapshot supplied by the release-set planner.
func PackageNpmWithReleaseSet(workspaceRoot string, projectPath string, projectName string, stable bool, versionInfo *git.VersionInfo, wsVersion string, plan *NpmReleaseSetPlan) (*NpmResult, error) {
	if plan == nil || plan.Coordinate == "" || plan.Version == "" || len(plan.Versions) == 0 {
		return nil, fmt.Errorf("invalid npm release-set plan")
	}
	if plan.Coordinate != projectName {
		return nil, fmt.Errorf("npm release-set member %q does not match project %q", plan.Coordinate, projectName)
	}
	if version, ok := plan.Versions[projectName]; !ok || version != plan.Version {
		return nil, fmt.Errorf("npm release-set plan is missing the selected member %q", projectName)
	}
	return packageNpm(workspaceRoot, projectPath, stable, versionInfo, wsVersion, plan)
}

func packageNpm(workspaceRoot string, projectPath string, stable bool, versionInfo *git.VersionInfo, wsVersion string, releaseSet *NpmReleaseSetPlan) (*NpmResult, error) {
	buildOutputRoot := findBuildOutput(workspaceRoot, projectPath)
	if buildOutputRoot == "" {
		return nil, nil
	}

	libOutputPath := filepath.Join(buildOutputRoot, "lib")
	if !project.FileExists(libOutputPath) {
		return nil, fmt.Errorf("build transpile output not found at %s", libOutputPath)
	}

	// Validate lib output is non-empty
	entries, _ := os.ReadDir(libOutputPath)
	if len(entries) == 0 {
		return nil, fmt.Errorf("build transpile output is empty at %s", libOutputPath)
	}

	// Create package output directory
	packageDir := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package", "npm")
	os.RemoveAll(packageDir)
	os.MkdirAll(packageDir, 0755)

	// Copy lib output (transpiled JS)
	if err := build.CopyDir(libOutputPath, packageDir); err != nil {
		return nil, fmt.Errorf("copying lib output: %w", err)
	}

	// Copy package.json from project root and prepare for publishing
	projRoot := filepath.Join(workspaceRoot, projectPath)
	srcPkgJSON := filepath.Join(projRoot, "package.json")
	if !project.FileExists(srcPkgJSON) {
		return nil, fmt.Errorf("package.json not found at %s", srcPkgJSON)
	}
	if err := build.CopyFile(srcPkgJSON, filepath.Join(packageDir, "package.json")); err != nil {
		return nil, fmt.Errorf("copying package.json: %w", err)
	}

	// Merge types (copy .d.ts files, filter out package.json, merge types field)
	typesOutputPath := filepath.Join(buildOutputRoot, "types")
	if project.FileExists(typesOutputPath) {
		// Copy types output excluding package.json
		if err := filepath.WalkDir(typesOutputPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(typesOutputPath, path)
			if err != nil {
				return err
			}
			if filepath.Base(rel) == "package.json" {
				return nil
			}
			destPath := filepath.Join(packageDir, rel)
			if d.IsDir() {
				return os.MkdirAll(destPath, 0755)
			}
			return build.CopyFile(path, destPath)
		}); err != nil {
			return nil, fmt.Errorf("copying type declarations: %w", err)
		}

		// Merge types field from types/package.json into main package.json
		typesPkgJSON := filepath.Join(typesOutputPath, "package.json")
		if project.FileExists(typesPkgJSON) {
			publishPkg := project.ReadPackageJSONSafe(filepath.Join(packageDir, "package.json"))
			typesPkg := project.ReadPackageJSONSafe(typesPkgJSON)
			if publishPkg != nil && typesPkg != nil && typesPkg.Types != "" {
				publishPkg.Types = typesPkg.Types
				if err := project.WritePackageJSON(filepath.Join(packageDir, "package.json"), publishPkg); err != nil {
					return nil, fmt.Errorf("writing package.json with types: %w", err)
				}
			}
		}
	}

	// Read and validate package.json
	pkg := project.ReadPackageJSONSafe(filepath.Join(packageDir, "package.json"))
	if pkg == nil || pkg.Name == "" || pkg.Version == "" {
		return nil, fmt.Errorf("invalid package.json — missing name or version")
	}
	if releaseSet != nil && pkg.Name != releaseSet.Coordinate {
		_ = os.RemoveAll(packageDir)
		return nil, fmt.Errorf("staged npm package %q does not match release-set coordinate %q", pkg.Name, releaseSet.Coordinate)
	}

	// Inherit shared fields from workspace root package.json, and read any Bun
	// catalog declared there so catalog: dependency specifiers resolve to
	// concrete versions at publish time.
	wsPkg := project.ReadPackageJSONSafe(filepath.Join(workspaceRoot, "package.json"))
	var catalogs *catalog.Catalogs
	if wsPkg != nil {
		inheritWorkspaceFields(pkg, wsPkg)
		catalogs = catalog.Parse(wsPkg.Raw)
	}

	wsProjects := loadWorkspaceProjectNames(workspaceRoot)

	// Compute version suffix for pre-release builds (e.g. "abc1234" or "abc1234-d5e6f7a")
	versionSuffix := ""
	if versionInfo != nil {
		versionSuffix = versionInfo.Suffix
		if versionSuffix == "" {
			versionSuffix = versionInfo.SHA
		}
	}

	// A sparse plan owns the candidate version. Without one, retain the legacy
	// workspace/git-derived version behavior exactly.
	if releaseSet != nil {
		pkg.Version = releaseSet.Version
	} else if wsVersion != "" {
		pkg.Version = wsVersion
	}

	// Resolve the version this package ships under.
	var version string
	if releaseSet != nil {
		version = releaseSet.Version
		if err := resolveSparsePublishDeps(pkg, wsProjects, catalogs, releaseSet.Versions); err != nil {
			_ = os.RemoveAll(packageDir)
			return nil, err
		}
		setGitInfoReleaseSet(pkg, versionInfo, stable)
	} else if stable {
		if wsVersion == "" {
			return nil, fmt.Errorf("could not determine base version from workspace config")
		}
		version = wsVersion

		// For stable releases, strip pre-release suffixes from workspace deps
		if err := resolvePublishDeps(pkg, wsProjects, catalogs, "", true, wsVersion); err != nil {
			return nil, err
		}

		// Set gitInfo release flag
		setGitInfoRelease(pkg, versionInfo, true)
	} else {
		// Apply version suffix for pre-release
		if versionSuffix != "" && pkg.Version != "" {
			pkg.Version = pkg.Version + "-" + versionSuffix
		}
		version = pkg.Version

		// Resolve workspace:*/catalog: dependencies with suffix
		if err := resolvePublishDeps(pkg, wsProjects, catalogs, versionSuffix, false, wsVersion); err != nil {
			return nil, err
		}

		setGitInfoRelease(pkg, versionInfo, false)
	}

	// Strip dev-only fields
	pkg.DevDependencies = nil
	deleteRawField(pkg, "devDependencies")
	deleteRawField(pkg, "private")
	deleteRawField(pkg, "scripts")
	deleteRawField(pkg, "overrides")
	deleteRawField(pkg, "resolutions")

	// Rewrite .ts/.tsx paths to .js in exports and main fields.
	// The build step transpiles TypeScript to JavaScript, but the
	// package.json still references .ts paths. Published packages
	// only contain .js files, so exports must match.
	rewriteTSPathsToJS(pkg)

	// Declare type entry points explicitly so downstream consumers resolve
	// framework .d.ts offline without relying on TypeScript's fragile
	// ".js → .d.ts sibling" heuristic. Only .d.ts files actually emitted under
	// packageDir are referenced; a "types" condition is placed first as
	// TypeScript requires. Runs after path rewriting so exports are already .js.
	injectExportTypes(pkg, packageDir)

	// Write updated package.json
	if err := project.WritePackageJSON(filepath.Join(packageDir, "package.json"), pkg); err != nil {
		return nil, fmt.Errorf("writing final package.json: %w", err)
	}

	// Copy README from project root
	copyReadme(projRoot, packageDir)

	// Copy AI.md and doc/ from project root
	copyDocFiles(projRoot, packageDir)

	// Copy LICENSE files from workspace root
	copyLicenseFiles(workspaceRoot, packageDir)

	// Copy hook files: putnami.extension.json (with version stamped) and bin/*.ts
	// Framework packages (e.g. @putnami/application, @putnami/web) declare
	// preBuild hooks that the generate phase discovers at build time.
	// For CLI-surface manifests this also runs the package-time contract gate:
	// a non-conforming manifest fails packaging instead of being
	// skipped by every consumer's CLI at load time.
	if err := copyHookFiles(projRoot, packageDir, version); err != nil {
		return nil, err
	}

	// npm archives encode filesystem modes. Canonicalize the complete staged
	// tree after every writer has finished so caller umask cannot change the
	// immutable artifact digest. Executables come only from package.json bin
	// entries and build outputs that start with a "#!" line, never from disk
	// permission bits. Windows keeps no executable bit, so there the managed
	// publish gives the packed archive the same modes (NormalizeNPMArchive).
	if err := normalizeNPMPackageModes(packageDir, libOutputPath, pkg); err != nil {
		return nil, fmt.Errorf("normalize npm package modes: %w", err)
	}

	// Record the channel inside the npm directory this step owns, so a cache
	// restore of that directory restores the record with it.
	if err := WritePackageMetadata(packageDir, "npm", version, stable); err != nil {
		return nil, fmt.Errorf("writing package metadata: %w", err)
	}

	return &NpmResult{
		Version:    version,
		PackageDir: packageDir,
		Stable:     stable,
	}, nil
}

// normalizeNPMPackageModes gives every staged directory 0755 and every staged
// file 0644, or 0755 when it is executable. A file is executable when a
// package.json bin entry names it, or when it is a build output that starts
// with a "#!" line: the bundler marks exactly those outputs executable on Unix.
// The decision reads content, not permission bits. On Windows, os.Chmod keeps
// no executable bit, so the staged tree cannot carry the decision:
// NormalizeNPMArchive makes it again for the packed archive.
func normalizeNPMPackageModes(packageDir, libOutputPath string, pkg *project.PackageJSON) error {
	executables, err := npmExecutables(libOutputPath, pkg)
	if err != nil {
		return err
	}

	return filepath.WalkDir(packageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(packageDir, path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if _, executable := executables[filepath.ToSlash(filepath.Clean(rel))]; executable {
			mode = 0o755
		}
		return os.Chmod(path, mode)
	})
}

// npmExecutables returns the files of an npm package that are executable, as
// slash-separated paths relative to the package root: the build outputs under
// libOutputPath that start with a "#!" line, and the files the package.json
// bin entries name. The staged package mirrors libOutputPath at its root.
func npmExecutables(libOutputPath string, pkg *project.PackageJSON) (map[string]struct{}, error) {
	executables := make(map[string]struct{})
	if err := filepath.WalkDir(libOutputPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		shebang, err := startsWithShebang(path)
		if err != nil {
			return err
		}
		if shebang {
			rel, err := filepath.Rel(libOutputPath, path)
			if err != nil {
				return err
			}
			executables[filepath.ToSlash(filepath.Clean(rel))] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, binPath := range npmBinPaths(pkg) {
		rel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(binPath, "./")))
		if rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			executables[filepath.ToSlash(rel)] = struct{}{}
		}
	}
	return executables, nil
}

// startsWithShebang reports whether the file at path starts with "#!".
func startsWithShebang(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	head := make([]byte, 2)
	n, err := io.ReadFull(file, head)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n == 2 && head[0] == '#' && head[1] == '!', nil
}

func npmBinPaths(pkg *project.PackageJSON) []string {
	if pkg == nil || len(pkg.Bin) == 0 {
		return nil
	}
	var single string
	if json.Unmarshal(pkg.Bin, &single) == nil && single != "" {
		return []string{single}
	}
	var entries map[string]string
	if json.Unmarshal(pkg.Bin, &entries) != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, path := range entries {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
