package pkg

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/releaseset"
)

func prepareGoModule(ctx *pctx.Context, emit *jsonl.Emitter, version, outputRoot string, dryRun bool) bool {
	planned, err := releaseplan.ResolveGoProject(ctx)
	if err != nil {
		emit.Diagnostic("error", "Invalid Go release-set plan: "+err.Error(), "", 0)
		return false
	}
	emit.PhaseStart("package-go")

	projectRoot := ctx.Project.FullPath
	goModPath := filepath.Join(projectRoot, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		emit.Diagnostic("error", "go.mod not found in "+projectRoot, "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	goModData, err := os.ReadFile(goModPath)
	if err != nil {
		emit.Diagnostic("error", "Failed to read go.mod: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	// Parse module path from go.mod
	modulePath := ""
	for _, line := range strings.Split(string(goModData), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(trimmed, "module"))
			break
		}
	}
	if modulePath == "" {
		emit.Diagnostic("error", "Could not parse module path from go.mod", "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}
	if planned != nil {
		if planned.Member.Coordinate != modulePath {
			emit.Diagnostic("error", fmt.Sprintf("Release-set member %q does not match module %q", planned.Member.Coordinate, modulePath), "", 0)
			emit.PhaseEnd("package-go", "failed")
			return false
		}
		if !planned.Member.Selected {
			emit.Log("info", fmt.Sprintf("Skipping unchanged Go module %s inherited at %s", modulePath, planned.Member.Version))
			emit.PhaseEnd("package-go", "skipped")
			return true
		}
		version = planned.Member.Version
	}

	// Ensure version starts with v for Go modules
	goVersion := version
	if !strings.HasPrefix(goVersion, "v") {
		goVersion = "v" + goVersion
	}

	emit.Log("info", fmt.Sprintf("Preparing Go module %s@%s", modulePath, goVersion))

	goOutputDir := filepath.Join(outputRoot, "go")

	if dryRun {
		emit.Log("info", fmt.Sprintf("Dry run: would prepare Go module %s@%s", modulePath, goVersion))
		if !recordGoChannel(emit, goOutputDir, modulePath, goVersion) {
			emit.PhaseEnd("package-go", "failed")
			return false
		}
		emit.PhaseEnd("package-go", "success")
		return true
	}

	os.MkdirAll(goOutputDir, 0o755)

	// Copy all Go source files to staging directory
	stageDir := filepath.Join(goOutputDir, "source")
	os.RemoveAll(stageDir)
	os.MkdirAll(stageDir, 0o755)

	// Copy module source files (*.go, go.mod, go.sum, and subdirectories)
	err = filepath.Walk(projectRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(projectRoot, path)
		// Skip hidden directories, testdata, vendor
		if info.IsDir() {
			base := filepath.Base(rel)
			if base == ".putnami" || base == "vendor" || (strings.HasPrefix(base, ".") && base != ".") {
				return filepath.SkipDir
			}
			// Skip nested modules: a subdirectory carrying its own go.mod is
			// a separate Go module and must not be bundled into the parent
			// module's zip. Go's module zip validator rejects misplaced
			// go.mod files with errMisplacedModFile.
			if rel != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Copy .go files, go.mod, go.sum, tools/versions.json, LICENSE*, README*, AI.md, and doc/**/*.md.
		ext := filepath.Ext(rel)
		base := filepath.Base(rel)
		inDocDir := strings.HasPrefix(rel, "doc"+string(filepath.Separator)) || rel == "doc"
		shouldCopy := ext == ".go" || base == "go.mod" || base == "go.sum" ||
			rel == filepath.Join("tools", "versions.json") ||
			strings.HasPrefix(base, "LICENSE") || strings.HasPrefix(base, "README") ||
			base == "AI.md" || (inDocDir && ext == ".md")
		if !shouldCopy {
			return nil
		}

		destPath := filepath.Join(stageDir, rel)
		os.MkdirAll(filepath.Dir(destPath), 0o755)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destPath, data, info.Mode())
	})
	if err != nil {
		emit.Diagnostic("error", "Failed to stage module source: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	// The allowlist above only stages known file classes, so //go:embed
	// targets (arbitrary names and extensions) must be staged explicitly or
	// the published module cannot compile. A pattern with no match fails the
	// package step here, before anything reaches a registry.
	if err := stageEmbedTargets(projectRoot, stageDir); err != nil {
		emit.Diagnostic("error", "Failed to stage go:embed targets: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	// Rewrite go.mod: strip replace directives, update dependency versions
	stagedGoMod := filepath.Join(stageDir, "go.mod")
	cleanedGoMod := stripReplaceDirectives(string(goModData))
	if planned != nil {
		cleanedGoMod, err = rewritePlannedWorkspaceDeps(cleanedGoMod, planned.Member, planned.Plan)
		if err != nil {
			emit.Diagnostic("error", "Failed to resolve internal Go requirements from release set: "+err.Error(), "", 0)
			emit.PhaseEnd("package-go", "failed")
			return false
		}
	} else {
		// Legacy/full/cloudless publication keeps the uniform workspace version.
		cleanedGoMod = updateDependencyVersions(cleanedGoMod, goVersion, ctx)
	}
	if err := os.WriteFile(stagedGoMod, []byte(cleanedGoMod), 0o644); err != nil {
		emit.Diagnostic("error", "Failed to write cleaned go.mod: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	// Create GOPROXY artifacts
	zipPath := filepath.Join(goOutputDir, modulePath+"@"+goVersion+".zip")
	os.MkdirAll(filepath.Dir(zipPath), 0o755)
	if err := createModuleZip(stageDir, modulePath, goVersion, zipPath); err != nil {
		emit.Diagnostic("error", "Failed to create module zip: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}
	if err := validateModuleZip(zipPath, modulePath, goVersion); err != nil {
		emit.Diagnostic("error", "Module zip failed validation: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}
	if err := verifyZipEmbedTargets(zipPath, modulePath, goVersion); err != nil {
		emit.Diagnostic("error", "Module zip failed go:embed validation: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	modPath := filepath.Join(goOutputDir, goVersion+".mod")
	if err := os.WriteFile(modPath, []byte(cleanedGoMod), 0o644); err != nil {
		emit.Diagnostic("error", "Failed to write .mod file: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	infoContent := fmt.Sprintf(`{"Version":%q,"Time":%q}`, goVersion, time.Now().UTC().Format(time.RFC3339))
	infoPath := filepath.Join(goOutputDir, goVersion+".info")
	if err := os.WriteFile(infoPath, []byte(infoContent+"\n"), 0o644); err != nil {
		emit.Diagnostic("error", "Failed to write .info file: "+err.Error(), "", 0)
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	// Write module metadata
	goMeta := map[string]any{
		"modulePath": modulePath,
		"version":    goVersion,
		"sourceDir":  stageDir,
		"zipPath":    zipPath,
		"modPath":    modPath,
		"infoPath":   infoPath,
	}
	goMetaData, _ := json.MarshalIndent(goMeta, "", "  ")
	if err := os.WriteFile(filepath.Join(goOutputDir, "module.json"), append(goMetaData, '\n'), 0o644); err != nil {
		emit.Diagnostic("warning", "Failed to write Go module metadata: "+err.Error(), "", 0)
	}

	if !recordGoChannel(emit, goOutputDir, modulePath, goVersion) {
		emit.PhaseEnd("package-go", "failed")
		return false
	}

	emit.Log("info", fmt.Sprintf("Prepared Go module %s@%s in %s", modulePath, goVersion, goOutputDir))
	emit.PhaseEnd("package-go", "success")
	return true
}

// recordGoChannel writes the go channel record inside the directory this
// packager owns, at the module version it just staged. The record states the
// exact module version the zip beside it carries.
func recordGoChannel(emit *jsonl.Emitter, goOutputDir, modulePath, goVersion string) bool {
	if err := pkgmeta.WriteChannelRecord(goOutputDir, pkgmeta.ChannelRecord{
		Version:  goVersion,
		Artifact: modulePath,
		Channels: []string{"go"},
	}); err != nil {
		emit.Diagnostic("error", "Failed to write channel record: "+err.Error(), "", 0)
		return false
	}
	return true
}

// stripReplaceDirectives removes all replace directives from a go.mod file.
func stripReplaceDirectives(goMod string) string {
	lines := strings.Split(goMod, "\n")
	var result []string
	inReplaceBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Handle block replace: replace ( ... )
		if strings.HasPrefix(trimmed, "replace (") || trimmed == "replace (" {
			inReplaceBlock = true
			continue
		}
		if inReplaceBlock {
			if trimmed == ")" {
				inReplaceBlock = false
			}
			continue
		}

		// Handle single-line replace
		if strings.HasPrefix(trimmed, "replace ") {
			continue
		}

		result = append(result, line)
	}

	// Clean up trailing empty lines
	output := strings.Join(result, "\n")
	for strings.HasSuffix(output, "\n\n") {
		output = strings.TrimSuffix(output, "\n")
	}
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return output
}

// updateDependencyVersions replaces workspace dependency versions (v0.0.x placeholders)
// with the actual publish version for modules in the same workspace.
func updateDependencyVersions(goMod string, publishVersion string, ctx *pctx.Context) string {
	// Collect all module paths from the workspace go.work
	workspaceModules := discoverWorkspaceGoModules(ctx.WorkspaceRoot)
	return rewriteWorkspaceDeps(goMod, publishVersion, workspaceModules)
}

// rewriteWorkspaceDeps rewrites every requirement on a workspace module to the
// publish version. Both require forms are handled: entries inside a
// `require ( ... )` block (`go.putnami.dev/app v0.0.1`) and the single-line
// form (`require go.putnami.dev/app v0.0.0`). The single-line form used to be
// missed, so a published go.mod kept its unpublishable v0.0.0 placeholder and
// standalone consumers 404ed resolving it.
func rewriteWorkspaceDeps(goMod string, publishVersion string, workspaceModules []string) string {
	if len(workspaceModules) == 0 {
		return goMod
	}

	lines := strings.Split(goMod, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		entry, singleLine := strings.CutPrefix(trimmed, "require ")
		if !singleLine {
			entry = trimmed
		}
		entry = strings.TrimSpace(entry)
		replaced := false
		for _, mod := range workspaceModules {
			if !strings.HasPrefix(entry, mod+" ") {
				continue
			}
			parts := strings.Fields(entry)
			if len(parts) < 2 {
				continue
			}
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			rewritten := indent + parts[0] + " " + publishVersion
			if singleLine {
				rewritten = indent + "require " + parts[0] + " " + publishVersion
			}
			result = append(result, rewritten)
			replaced = true
			break
		}
		if !replaced {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// rewritePlannedWorkspaceDeps rewrites every internal requirement from the
// immutable full release-set plan. External requirements are untouched. A Go
// requirement cannot escape the set or disagree with the current member's
// dependency record: either condition would create a package whose go.mod and
// release-set snapshot describe different graphs.
func rewritePlannedWorkspaceDeps(goMod string, current releaseset.PlannedMember, plan *releaseset.Plan) (string, error) {
	parsed, err := modfile.Parse("go.mod", []byte(goMod), nil)
	if err != nil {
		return "", fmt.Errorf("parse staged go.mod: %w", err)
	}

	plannedDependencies := make(map[string]string)
	for _, dependency := range current.Dependencies {
		if dependency.Ecosystem == releaseplan.GoEcosystem {
			plannedDependencies[dependency.Coordinate] = dependency.Version
		}
	}
	seenInternalRequirements := make(map[string]bool, len(plannedDependencies))
	for _, requirement := range parsed.Require {
		path := requirement.Mod.Path
		if !strings.HasPrefix(path, "go.putnami.dev/") {
			continue
		}
		if err := module.CheckPath(path); err != nil {
			return "", fmt.Errorf("internal requirement %q has malformed module path: %w", path, err)
		}
		if err := module.Check(path, requirement.Mod.Version); err != nil {
			return "", fmt.Errorf("internal requirement %q has malformed version %q: %w", path, requirement.Mod.Version, err)
		}
		target, ok := plan.Member(releaseplan.GoEcosystem, path)
		if !ok {
			return "", fmt.Errorf("internal requirement %q is absent from release-set plan", path)
		}
		dependencyVersion, ok := plannedDependencies[path]
		if !ok {
			return "", fmt.Errorf("internal requirement %q is absent from release-set member %q dependencies", path, current.Coordinate)
		}
		if dependencyVersion != target.Version {
			return "", fmt.Errorf("internal requirement %q dependency version %q does not match release-set member version %q", path, dependencyVersion, target.Version)
		}
		seenInternalRequirements[path] = true
		if err := parsed.AddRequire(path, target.Version); err != nil {
			return "", fmt.Errorf("rewrite internal requirement %q to %q: %w", path, target.Version, err)
		}
	}
	for coordinate := range plannedDependencies {
		if !seenInternalRequirements[coordinate] {
			return "", fmt.Errorf("release-set member %q declares Go dependency %q absent from staged go.mod", current.Coordinate, coordinate)
		}
	}
	formatted, err := parsed.Format()
	if err != nil {
		return "", fmt.Errorf("format staged go.mod: %w", err)
	}
	return string(formatted), nil
}

// discoverWorkspaceGoModules reads go.work to find all workspace module paths.
func discoverWorkspaceGoModules(workspaceRoot string) []string {
	return toolchain.WorkspaceGoModules(workspaceRoot)
}

// validateModuleZip runs the zip we just wrote through the same
// validator `go mod download` and the module proxy use, so a malformed
// zip (nested go.mod in a non-root directory, oversized files, path
// case collisions, vendored content, etc.) fails the publish step
// rather than the next downstream `go mod download`. The previous
// nested-module incident was exactly this class of bug: caught here,
// it never reaches a tagged release.
func validateModuleZip(zipPath, modulePath, version string) error {
	if _, err := module.EscapePath(modulePath); err != nil {
		return fmt.Errorf("invalid module path %q: %w", modulePath, err)
	}
	if err := module.Check(modulePath, version); err != nil {
		return fmt.Errorf("invalid module+version %q@%q: %w", modulePath, version, err)
	}
	_, err := modzip.CheckZip(module.Version{Path: modulePath, Version: version}, zipPath)
	return err
}

// createModuleZip creates a Go module zip at outputPath.
// The zip follows the Go module zip format: all files are prefixed with modulePath@version/.
func createModuleZip(stageDir, modulePath, version, outputPath string) (resultErr error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close zip file: %w", err))
		}
	}()

	zw := zip.NewWriter(f)
	prefix := modulePath + "@" + version + "/"

	err = filepath.Walk(stageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(stageDir, path)
		// Module zips use forward slashes
		zipPath := prefix + filepath.ToSlash(rel)

		w, err := zw.Create(zipPath)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		walkErr := fmt.Errorf("walk source: %w", err)
		if closeErr := zw.Close(); closeErr != nil {
			return errors.Join(walkErr, fmt.Errorf("close partial module zip: %w", closeErr))
		}
		return walkErr
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close module zip: %w", err)
	}
	return nil
}
