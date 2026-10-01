package pkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/sdk/extension/agentartifact"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/scratch"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/toolchain"
)

// releaseTargets is the RELEASE matrix for this invocation: the platforms the
// archive set carries.
//
// It reads the same plan-time `platforms` parameter the cross-compile step
// resolves its target set from, through the same decoder, so the archives that
// get staged and the binaries that get built are the same set by construction.
// A project that declares nothing gets the whole archive matrix (only
// declared targets, and no widening).
func releaseTargets(ctx *pctx.Context) ([]platform.Target, error) {
	spec, err := platform.PlatformsSpec(ctx.Params["platforms"])
	if err != nil {
		return nil, err
	}
	return platform.DeclaredTargets(spec)
}

func createExtensionArchives(ctx *pctx.Context, emit *jsonl.Emitter, packageName, version, outputRoot string, dryRun bool) bool {
	emit.PhaseStart("package-archives")

	projectRoot := ctx.Project.FullPath
	artifact, err := toResolverArtifactName(packageName)
	if err != nil {
		emit.Diagnostic("error", "Could not resolve artifact name from '"+packageName+"'", "", 0)
		emit.PhaseEnd("package-archives", "failed")
		return false
	}

	releasePlatforms, err := releaseTargets(ctx)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		emit.PhaseEnd("package-archives", "failed")
		return false
	}

	if dryRun {
		emit.Log("info", fmt.Sprintf("Dry run: would create archives for %s v%s", artifact, version))
		emit.PhaseEnd("package-archives", "success")
		return true
	}

	archivesOutputDir := filepath.Join(outputRoot, "archives")
	os.MkdirAll(archivesOutputDir, 0o755)

	tmp, err := scratch.New("putnami-go-pkg-")
	if err != nil {
		emit.Diagnostic("error", "Failed to create temp directory: "+err.Error(), "", 0)
		emit.PhaseEnd("package-archives", "failed")
		return false
	}
	defer func() { _ = tmp.Remove() }()
	tmpDir := tmp.Path()

	baseStageDir := filepath.Join(tmpDir, "base-stage")
	os.MkdirAll(baseStageDir, 0o755)

	// Stage common files (include whatever exists)
	for _, rel := range []string{"putnami.extension.json", "package.json", "bin", "templates", "tools", "README.md", "AI.md", "LICENSE.md", "config"} {
		if err := copyRel(projectRoot, baseStageDir, rel); err != nil && !os.IsNotExist(err) {
			emit.Diagnostic("warning", "Failed to stage "+rel+": "+err.Error(), "", 0)
		}
	}

	// Bundle Go framework AI.md files into framework-docs/ so they travel
	// with the extension archive. Agents in consumer workspaces can then find
	// API references without needing access to the original framework source.
	stageFrameworkDocs(ctx.WorkspaceRoot, baseStageDir, emit)

	_, statErr := os.Stat(filepath.Join(baseStageDir, "putnami.extension.json"))
	isExtension := statErr == nil
	if isExtension {
		stampManifestVersion(baseStageDir, version)
		// Agent content is built once, into the shared base stage, before any
		// platform copies it: every platform archive carries the same content
		// bytes, and the contract gate below verifies them against the digest
		// the staged manifest now binds.
		if _, err := agentartifact.StageExtensionContent(projectRoot, baseStageDir, packageName, version); err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-archives", "failed")
			return false
		}
	}

	// Detect Go command entrypoint to determine binary name.
	entrypoint := platform.ReadGoEntrypoint(projectRoot)
	binaryName := platform.DeriveBinaryName(entrypoint, packageName)
	hasBinary := false
	if entrypoint != "" {
		entrypointAbs := filepath.Join(projectRoot, entrypoint)
		if platform.HasMainGo(entrypointAbs) {
			hasBinary = true
		}
	}

	// Extra executables declared beside the runtime. They are staged
	// and checked on every platform archive, the runtime's own rule.
	var executables []platform.Executable
	if hasBinary {
		executables, err = platform.ReadGoExecutables(projectRoot)
		if err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-archives", "failed")
			return false
		}
	}

	// Without a binary, every platform archives the one shared base stage. Gate
	// it once, before any worker starts, so the workers only ever read it. The
	// shared stage carries no platform binary, so its declared runtime is
	// checked under the declared name, with no platform suffix.
	var baseExecutables []string
	if !hasBinary && isExtension {
		if err := gateAndStampManifestContract(baseStageDir); err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-archives", "failed")
			return false
		}
		runtimeExecutable, err := validateStagedRuntimeExecutable(baseStageDir, "")
		if err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-archives", "failed")
			return false
		}
		if runtimeExecutable != "" {
			baseExecutables = []string{runtimeExecutable}
		}
	}

	// Stage pre-built binaries from the cross-compile step and create archives.
	// Binaries are at {OutputPath}/bin/{platform-suffix}/{binaryName}, with
	// ".exe" appended for a Windows platform.
	workers, goMaxProcs := archiveParallelism(len(releasePlatforms), os.Getenv)
	emit.Log("info", fmt.Sprintf(
		"Packaging %d platforms, %d at the same time (GOMAXPROCS=%d for each pinned tool build)",
		len(releasePlatforms), workers, goMaxProcs))
	run := &platformPackaging{
		emit:              emit,
		binDir:            filepath.Join(ctx.OutputPath, "bin"),
		tmpDir:            tmpDir,
		baseStageDir:      baseStageDir,
		archivesOutputDir: archivesOutputDir,
		artifact:          artifact,
		binaryName:        binaryName,
		executables:       executables,
		baseExecutables:   baseExecutables,
		expectedVersion:   expectedEmbeddedVersion(ctx, version),
		hasBinary:         hasBinary,
		isExtension:       isExtension,
		goMaxProcs:        goMaxProcs,
	}
	// One lock keeps the packaged count and its progress event in step, so the
	// counter a consumer reads never goes backwards.
	var progressMu sync.Mutex
	packaged := 0
	errs := packageAllPlatforms(releasePlatforms, workers,
		func(p platform.Target) error { return packagePlatformStep(run, p) },
		func(p platform.Target) {
			progressMu.Lock()
			defer progressMu.Unlock()
			packaged++
			emit.Progress(packaged, len(releasePlatforms), "Packaged "+p.Suffix)
		})
	for _, err := range errs {
		if err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-archives", "failed")
			return false
		}
	}

	emit.Log("info", fmt.Sprintf("Created %d archive packages in %s", len(releasePlatforms), archivesOutputDir))
	emit.PhaseEnd("package-archives", "success")
	return true
}

// cpuBudgetEnv carries the CPU grant the scheduler gives each job. The CLI
// exports it to every job it runs (applyCPUBudget in
// tooling/cli/internal/jobs/runner.go).
const cpuBudgetEnv = "PUTNAMI_CPU_BUDGET"

// archiveParallelism returns how many platforms package at the same time and
// how many cores each pinned tool build may use.
//
// The grant is PUTNAMI_CPU_BUDGET. When it is missing or not a positive
// integer (a run by hand, or a CLI that does not export it), the grant is
// GOMAXPROCS. Workers never exceed the grant or the platform count. Each
// `go install` gets grant/workers cores, at least one, so the builds that run
// at the same time together stay within the grant instead of each taking all
// of it. With a grant of 1 the run is serial, as it was before.
func archiveParallelism(platforms int, getenv func(string) string) (workers, goMaxProcs int) {
	grant := runtime.GOMAXPROCS(0)
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(cpuBudgetEnv))); err == nil && n > 0 {
		grant = n
	}
	workers = max(1, min(platforms, grant))
	return workers, max(1, grant/workers)
}

// packageAllPlatforms runs step for every platform, with at most workers of
// them running at the same time, and returns each platform's error at the
// platform's index. It returns only after every step it started has returned.
//
// Platforms are dispatched in declared order: a platform takes a worker slot
// only after every platform before it took one, and a dispatched platform
// always runs. With one worker this is the serial loop it replaced. After a
// failure, no platform that was not dispatched yet is dispatched, the same stop
// the serial loop made. The caller reports the first error in platform order,
// and that error never depends on which platform finished first: every
// platform before a failed one was already dispatched, so the lowest failing
// index always ran.
//
// packaged runs after each platform that succeeds. It can run from several
// workers at the same time.
func packageAllPlatforms(
	platforms []platform.Target,
	workers int,
	step func(platform.Target) error,
	packaged func(platform.Target),
) []error {
	errs := make([]error, len(platforms))
	slots := make(chan struct{}, max(1, workers))
	var failed atomic.Bool
	var wg sync.WaitGroup
	for i, p := range platforms {
		slots <- struct{}{}
		// The slot was freed by a finished step, and a failed step records
		// its failure before it frees the slot.
		if failed.Load() {
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := step(p); err != nil {
				errs[i] = err
				failed.Store(true)
				return
			}
			packaged(p)
		}()
	}
	wg.Wait()
	return errs
}

// platformPackaging is what every platform of one packaging run shares. The
// workers only read it. Each platform writes only to its own stage directory,
// its own temporary GOPATH, and its own archive. The emitter is safe to share:
// each event is one Write of one complete line to os.Stdout, and *os.File
// serializes concurrent writes.
type platformPackaging struct {
	emit              *jsonl.Emitter
	binDir            string
	tmpDir            string
	baseStageDir      string
	archivesOutputDir string
	artifact          string
	binaryName        string
	// executables are the extra programs staged beside binaryName in
	// compiled/, each one built by the cross-compile step.
	executables []platform.Executable
	// baseExecutables are the archive paths the shared base stage declares
	// executable, used when the project has no binary.
	baseExecutables []string
	expectedVersion string
	hasBinary       bool
	isExtension     bool
	// goMaxProcs is each pinned tool build's share of the task's CPU grant.
	goMaxProcs int
}

// packagePlatformStep is the per-platform step the worker pool runs. It is a
// variable so a test can observe the scheduling without building anything.
var packagePlatformStep = packagePlatform

// packagePlatform stages and archives one platform. It runs the same checks,
// in the same order, as the serial loop it came from, and returns the message
// that loop reported instead of reporting it.
//
// Every program the stage carries is named for the platform
// (pkgmeta.ExecutableName: compiled/<name>.exe in a Windows archive), and the
// archive marks each one executable because the project declares it, whatever
// mode the file has on the packaging machine's disk. A Windows stage also
// passes checkWindowsArchive: no symbolic link, and a runtime under its
// Windows name.
func packagePlatform(run *platformPackaging, p platform.Target) error {
	run.emit.Log("info", "Packaging "+p.Suffix)

	stageDir := run.baseStageDir
	executables := run.baseExecutables
	if run.hasBinary {
		// Verify the pre-built binary exists
		binaryFile := pkgmeta.ExecutableName(p.GOOS, run.binaryName)
		preBuiltBinary := filepath.Join(run.binDir, p.Suffix, binaryFile)
		if _, err := os.Stat(preBuiltBinary); err != nil {
			return errors.New("Pre-built binary not found at " + preBuiltBinary + " — cross-compile step may have failed")
		}

		// Refuse to ship a binary that embeds a different version than this
		// run's.
		if run.expectedVersion != "" {
			if err := verifyBinaryVersion(preBuiltBinary, run.expectedVersion); err != nil {
				return err
			}
		}

		stageDir = filepath.Join(run.tmpDir, "platform-"+p.Suffix)
		if err := copyTree(run.baseStageDir, stageDir); err != nil {
			return errors.New("Failed to copy stage directory for " + p.Suffix + ": " + err.Error())
		}

		// Copy the pre-built binary into compiled/ in the stage dir
		compiledDest := filepath.Join(stageDir, "compiled")
		if err := os.MkdirAll(compiledDest, 0o755); err != nil {
			return errors.New("Failed to create compiled directory: " + err.Error())
		}
		if err := copyRel(filepath.Dir(preBuiltBinary), compiledDest, binaryFile); err != nil {
			return errors.New("Failed to copy binary " + binaryFile + ": " + err.Error())
		}
		executables = []string{"compiled/" + binaryFile}
		for _, e := range run.executables {
			staged, err := stageDeclaredExecutable(filepath.Join(run.binDir, p.Suffix), compiledDest, p.GOOS, e)
			if err != nil {
				return err
			}
			executables = append(executables, "compiled/"+staged)
		}
		// The Go extension's archive also carries the pinned development
		// tools built for this platform, so `putnami install` on a consumer
		// machine copies them instead of compiling them. Every other
		// project skips this: the marker is the staged pin manifest, not
		// the package name.
		if stagesPinnedTools(stageDir) {
			tools, err := stagePinnedTools(run.emit, toolchain.CurrentGoBinary(), p, stageDir, run.goMaxProcs)
			if err != nil {
				return err
			}
			executables = append(executables, tools...)
		}
		if run.isExtension {
			if err := gateAndStampManifestContract(stageDir); err != nil {
				return err
			}
			runtimeExecutable, err := validateStagedRuntimeExecutable(stageDir, p.GOOS)
			if err != nil {
				return err
			}
			if runtimeExecutable != "" {
				executables = append(executables, runtimeExecutable)
			}
		}
	}
	if p.GOOS == "windows" {
		var err error
		if executables, err = checkWindowsArchive(run, stageDir, p, executables); err != nil {
			return err
		}
	}

	archivePath := filepath.Join(run.archivesOutputDir, run.artifact+"-"+p.Suffix+".tar.gz")
	if err := writeReproducibleArchive(archivePath, stageDir, executables); err != nil {
		return errors.New("Failed to create archive for " + p.Suffix)
	}
	return nil
}

// stageDeclaredExecutable copies one declared executable from the platform's
// cross-compile output into compiled/, under its file name for goos, checks
// that the staged copy is a regular file, and returns that file name. The
// archive marks it executable because it is declared. A missing one fails the
// platform: an archive without it installs cleanly and then fails in every
// consumer workspace at the first task that runs it.
func stageDeclaredExecutable(platformBinDir, compiledDest, goos string, e platform.Executable) (string, error) {
	name := pkgmeta.ExecutableName(goos, e.Name)
	source := filepath.Join(platformBinDir, name)
	if _, err := os.Stat(source); err != nil {
		return "", fmt.Errorf("declared executable %s (%s) not found at %s: cross-compile step may have failed", e.Name, e.Package, source)
	}
	if err := copyRel(platformBinDir, compiledDest, name); err != nil {
		return "", fmt.Errorf("failed to copy declared executable %s: %w", e.Name, err)
	}
	info, err := os.Lstat(filepath.Join(compiledDest, name))
	if err != nil {
		return "", fmt.Errorf("staged executable compiled/%s is unavailable: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("staged executable compiled/%s is not a regular file", name)
	}
	return name, nil
}

// createTemplateArchives creates platform archives for template projects.
// Templates contain no binaries, so all platform archives are identical.
//
// It is NOT scoped by the declared platform matrix: every platform archive name
// the resolver indexes gets an archive.
func createTemplateArchives(ctx *pctx.Context, emit *jsonl.Emitter, packageName, version, outputRoot string, dryRun bool) bool {
	emit.PhaseStart("package-template-archives")

	projectRoot := ctx.Project.FullPath
	artifact, err := toResolverArtifactName(packageName)
	if err != nil {
		emit.Diagnostic("error", "Could not resolve artifact name from '"+packageName+"'", "", 0)
		emit.PhaseEnd("package-template-archives", "failed")
		return false
	}

	if dryRun {
		emit.Log("info", fmt.Sprintf("Dry run: would create template archives for %s v%s", artifact, version))
		emit.PhaseEnd("package-template-archives", "success")
		return true
	}

	archivesOutputDir := filepath.Join(outputRoot, "archives")
	os.MkdirAll(archivesOutputDir, 0o755)

	tmp, err := scratch.New("putnami-tpl-pkg-")
	if err != nil {
		emit.Diagnostic("error", "Failed to create temp directory: "+err.Error(), "", 0)
		emit.PhaseEnd("package-template-archives", "failed")
		return false
	}
	defer func() { _ = tmp.Remove() }()
	tmpDir := tmp.Path()

	stageDir := filepath.Join(tmpDir, "stage")
	os.MkdirAll(stageDir, 0o755)

	// Stage template files
	for _, rel := range []string{"putnami.template.json", "README.md", "LICENSE.md"} {
		if err := copyRel(projectRoot, stageDir, rel); err != nil && !os.IsNotExist(err) {
			emit.Diagnostic("warning", "Failed to stage "+rel+": "+err.Error(), "", 0)
		}
	}

	// Stage all other template content (directories that aren't dotfiles or node_modules)
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		emit.Diagnostic("error", "Failed to read project directory: "+err.Error(), "", 0)
		emit.PhaseEnd("package-template-archives", "failed")
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if name[0] == '.' || name == "node_modules" || name == "putnami.template.json" || name == "README.md" || name == "LICENSE.md" {
			continue
		}
		if err := copyRel(projectRoot, stageDir, name); err != nil && !os.IsNotExist(err) {
			emit.Diagnostic("warning", "Failed to stage "+name+": "+err.Error(), "", 0)
		}
	}

	// Verify template manifest was staged
	if _, err := os.Stat(filepath.Join(stageDir, "putnami.template.json")); os.IsNotExist(err) {
		emit.Diagnostic("error", "No putnami.template.json found in project", "", 0)
		emit.PhaseEnd("package-template-archives", "failed")
		return false
	}

	// Stamp version in template manifest
	stampTemplateManifestVersion(stageDir, version)

	if err := refuseTemplateArchiveLinks(stageDir); err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		emit.PhaseEnd("package-template-archives", "failed")
		return false
	}

	// Templates are platform-independent — create identical archives for all platforms
	for i, p := range platform.ArchivePlatforms {
		emit.Progress(i+1, len(platform.ArchivePlatforms), fmt.Sprintf("Packaging %s", p.Suffix))

		archiveName := artifact + "-" + p.Suffix + ".tar.gz"
		archivePath := filepath.Join(archivesOutputDir, archiveName)
		if err := writeReproducibleArchive(archivePath, stageDir, nil); err != nil {
			emit.Diagnostic("error", "Failed to create archive for "+p.Suffix, "", 0)
			emit.PhaseEnd("package-template-archives", "failed")
			return false
		}
	}

	emit.Log("info", fmt.Sprintf("Created %d template archive packages in %s", len(platform.ArchivePlatforms), archivesOutputDir))
	emit.PhaseEnd("package-template-archives", "success")
	return true
}

// refuseTemplateArchiveLinks fails when the template stage holds a symbolic
// link, and names the first one in walk order. The CLI installs the same
// template archive on every OS, and a Windows CLI creates no link from an
// archive, so a link would break the template on Windows only. The rule holds
// for every platform archive, as they are identical.
func refuseTemplateArchiveLinks(stageDir string) error {
	rel, target, err := firstStagedLink(stageDir)
	if err != nil || rel == "" {
		return err
	}
	return fmt.Errorf(
		"template archive entry %s is a symbolic link to %s: a template archive holds no link, because "+
			"the CLI installs the same template archive on every OS and a Windows CLI creates no link from an archive; "+
			"ship a regular file or a directory at that path",
		rel, target)
}

// recordArchiveChannels writes the two documents the archive packager owns: its
// channel record inside archives/, and the archive publication manifest at
// <command-output>/metadata.json.
//
// One writer, one statement. Both archive channels are produced by this same
// task into the same directory it owns, so the record names every archive
// channel this invocation produced rather than each function racing to be last.
// Both documents are declared outputs of package-archives, so a cache restore
// reproduces them with the archives they describe.
//
// The manifest is not an index: the archive uploader (@putnami/cloud
// publish-archives) reads exactly its five fields to decide what to upload and
// under which version. `template` stays false here on purpose — this extension
// writes one archive PER platform even for a template, so the uploader must key
// each blob by its own platform instead of fanning one out to all of them.
func recordArchiveChannels(
	emit *jsonl.Emitter,
	ctx *pctx.Context,
	archivesOutputDir, artifact, version string,
	channels []string,
) bool {
	if err := pkgmeta.WriteChannelRecord(archivesOutputDir, pkgmeta.ChannelRecord{
		Version:  version,
		Artifact: artifact,
		Channels: channels,
	}); err != nil {
		emit.Diagnostic("error", "Failed to write channel record: "+err.Error(), "", 0)
		return false
	}
	if err := pkgmeta.WritePackageMetadata(ctx.WorkspaceRoot, ctx.Project.Path, pkgmeta.PackageMetadata{
		Version:  version,
		Artifact: artifact,
		Channels: channels,
	}); err != nil {
		emit.Diagnostic("error", "Failed to write the archive publication manifest: "+err.Error(), "", 0)
		return false
	}
	return true
}

func stampTemplateManifestVersion(stageDir, version string) {
	manifestPath := filepath.Join(stageDir, "putnami.template.json")
	data, _ := os.ReadFile(manifestPath)
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return
	}
	manifest["version"] = version
	rewritten, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, append(rewritten, '\n'), 0o644); err != nil {
		slog.Warn("failed to stamp template manifest version", slog.String("path", manifestPath), slog.Any("error", err))
	}
}

func stampManifestVersion(stageDir, version string) {
	manifestPath := filepath.Join(stageDir, "putnami.extension.json")
	data, _ := os.ReadFile(manifestPath)
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return
	}
	manifest["version"] = version
	rewritten, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, append(rewritten, '\n'), 0o644); err != nil {
		slog.Warn("failed to stamp manifest version", slog.String("path", manifestPath), slog.Any("error", err))
	}
}

// stageFrameworkDocs copies Go framework AI.md files into framework-docs/
// in the staging directory. This bundles API documentation into the extension
// archive so agents in consumer workspaces can discover framework APIs.
//
// The function looks for go/framework/*/AI.md relative to the workspace root
// and stages each module as framework-docs/{module}/AI.md.
func stageFrameworkDocs(workspaceRoot, stageDir string, emit *jsonl.Emitter) {
	frameworkRoot := filepath.Join(workspaceRoot, "go", "framework")
	entries, err := os.ReadDir(frameworkRoot)
	if err != nil {
		return // no framework directory — nothing to bundle
	}

	docsDir := filepath.Join(stageDir, "framework-docs")
	staged := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		moduleName := entry.Name()
		data, err := os.ReadFile(filepath.Join(frameworkRoot, moduleName, "AI.md"))
		if err != nil {
			continue // no AI.md for this module
		}

		destDir := filepath.Join(docsDir, moduleName)
		os.MkdirAll(destDir, 0o755)
		if err := os.WriteFile(filepath.Join(destDir, "AI.md"), data, 0o644); err != nil {
			emit.Diagnostic("warning", "Failed to stage framework doc for "+moduleName+": "+err.Error(), "", 0)
			continue
		}
		staged++
	}
	if staged > 0 {
		emit.Log("info", fmt.Sprintf("Bundled %d Go framework AI.md files into extension archive", staged))
	}
}

func copyRel(src, dest, rel string) error {
	srcPath := filepath.Join(src, rel)
	destPath := filepath.Join(dest, rel)
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := copyTree(srcPath, destPath); err != nil {
			return fmt.Errorf("copy directory %s: %w", rel, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("create parent directory for %s: %w", rel, err)
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if err := os.WriteFile(destPath, data, info.Mode()); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}
