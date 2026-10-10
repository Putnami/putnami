// Package build compiles Go projects using `go build` with full flag support.
// Emits JSONL events compatible with the Putnami orchestrator.
package build

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/releaseversion"
	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Invocation intent, bound by the PHASE the manifest step selected.
//
// Intent is the second half of the nature+intent contract: a project's nature
// says what evidence a compile owes, and the invocation's intent says which
// platforms that evidence must cover.
//
//   - `compile` is ORDINARY VALIDATION (`build`). It compiles for the host and
//     nothing else, because no step of `build` consumes the binaries and four
//     cross-compiles of one module share no GOCACHE objects.
//   - `cross-compile` is DISTRIBUTION (`package`/release). It owes the full
//     archive matrix, and channel scoping owns narrowing it per channel.
const (
	phaseTidy         = "tidy"
	phaseCompile      = "compile"
	phaseCrossCompile = "cross-compile"
)

// projectTypeLibrary is the resolved classification of a module that contains
// no `package main`. An
// EMPTY type is an application, matching the orchestrator's own default and the
// SDK's infraagg.IsWorkload, so an unclassified project keeps the behavior it
// had before classification existed.
const projectTypeLibrary = "library"

// Run executes the build job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)

	phase := cli.FlagString(flags, "phase", "")
	skipDeps := cli.FlagBool(flags, "skip-deps", false)
	target := cli.FlagString(flags, "target", "")
	if target == "" {
		target = cli.FlagString(flags, "t", "")
	}
	if target == "" {
		target = ctx.Params.String("target")
	}
	platformsSpec, platformsErr := platformsParam(ctx)
	if platformsErr != nil {
		emit.Diagnostic("error", platformsErr.Error(), "", 0)
		return "FAILED", nil, nil
	}
	outputPath := cli.FlagString(flags, "output_path", "")
	if outputPath == "" {
		outputPath = cli.FlagString(flags, "o", "")
	}
	// -ldflags is resolved here rather than in the shared configuration: it
	// reaches the LINK action alone, so the describe build leaves it out and
	// still shares every compile action with this one. It is read from the
	// parameter bag for the same reason as everything below — a task's argv is
	// fixed by its manifest entry, so `--ldflags` and an `options` layer both
	// arrive as parameters. Both tasks that link already declare it as a
	// cache-key input.
	ldflags := cli.FlagString(flags, "ldflags", ctx.Params.String("ldflags"))
	// Everything else that decides WHICH PROGRAM this builds is resolved by the
	// shared host-build resolver, so the describe step compiles the same
	// program and Go's build cache serves this one.
	hostBuild := toolchain.ResolveHostBuild(ctx, flags)
	cgo := hostBuild.CGO
	entrypoint := cli.FlagString(flags, "entrypoint", "")
	if entrypoint == "" {
		entrypoint = ctx.Params.String("entrypoint")
	}
	versionVar := cli.FlagString(flags, "version-var", "")
	if versionVar == "" {
		versionVar = ctx.Params.String("version-var", "versionVar")
	}
	artifactVersion := releaseversion.Select(ctx.Version, ctx.Workspace.Version)

	// Auto-inject version via -X ldflags when version-var is configured
	if versionVar != "" && artifactVersion != "" {
		xFlag := "-X " + versionVar + "=" + artifactVersion
		if ldflags == "" {
			ldflags = xFlag
		} else {
			ldflags = ldflags + " " + xFlag
		}
	}

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}

	// --- Phase: tidy ---
	if phase == "" || phase == phaseTidy {
		if skipDeps {
			if phase == phaseTidy {
				return "SKIP", nil, nil
			}
		} else {
			emit.PhaseStart("setup")
			tidyResult := "OK"
			tidyPhaseStatus := "success"
			// The environment a `go mod tidy` runs with, resolved once so the
			// guard below reads the GOPROXY the command itself would obey.
			//
			// `go mod tidy` resolves modules: an inherited GOWORK=off (leaked
			// from a parent that built the CLI standalone) disables workspace
			// resolution, so each `require go.putnami.dev/<mod> v0.0.0`
			// placeholder escapes to the private GOPROXY as a doomed 404. Strip
			// the leaked entry and re-point GOWORK at the governing go.work so
			// framework deps resolve through the workspace. This is the
			// dominant cold-build / `putnami upgrade` path.
			tidyEnv := toolchain.WorkspaceBuildEnv(os.Environ(), moduleRoot(ctx), goBinary)
			if toolchain.ModuleDownloadsDisabled(tidyEnv) {
				// Module downloads are disabled by configuration, so tidy is a
				// no-op this task reports as done rather than as work it still
				// owes. `go mod tidy` resolves the FULL module graph — the test
				// dependencies of every imported package, and a fresh version
				// query for every requirement it has to add — which is wider
				// than the build list `putnami install` warms, so it has
				// nothing to resolve against here and no attempt can change
				// that. GOPROXY is one of this task's declared cache-key
				// inputs, so the entry this outcome writes answers offline runs
				// of the same tree and never a run that can resolve modules.
				emit.Log("info", tidyOfflineMessage)
			} else {
				// go mod tidy rewrites go.mod/go.sum — the very files that key
				// the build-tidy cache. Snapshot them so a failed tidy can be
				// rolled back and never churns its own cache-key inputs. A
				// successful tidy keeps its output untouched.
				snap := snapshotModFiles(ctx.Project.FullPath)
				cmd := exec.Command(goBinary, "mod", "tidy")
				cmd.Dir = ctx.Project.FullPath
				cmd.Env = tidyEnv
				// Tee tidy's combined output: os.Stderr keeps the raw toolchain
				// output debuggable, the buffer feeds the failure diagnostic below.
				var tidyOut bytes.Buffer
				tee := io.MultiWriter(os.Stderr, &tidyOut)
				cmd.Stdout = tee
				cmd.Stderr = tee
				if err := cmd.Run(); err != nil {
					// A tidy that RAN and failed degrades to an uncacheable SKIP
					// that re-runs its network round trips on every build, so
					// surface it as a diagnostic (visible at default verbosity),
					// not a warn log that default verbosity suppresses.
					emit.Diagnostic("warning", tidyFailureMessage(ctx.WorkspaceRoot, ctx.Project.FullPath, tidyOut.String()), "", 0)
					// Report a phase-only tidy failure as SKIP: dependents may
					// still run, but the restored modfile key must not cache as OK.
					tidyResult = "SKIP"
					tidyPhaseStatus = "skipped"
					restored, restoreErr := restoreModFiles(ctx.Project.FullPath, snap)
					if restoreErr != nil {
						emit.Log("warn", "restore go.mod/go.sum after failed tidy: "+restoreErr.Error())
					}
					if len(restored) > 0 {
						emit.Log("debug", "failed go mod tidy had written "+strings.Join(restored, ", ")+"; restored pre-tidy content")
					}
				} else if added := tidyAddedWorkspaceRequirements(snap, ctx.Project.FullPath, toolchain.WorkspaceGoModules(ctx.WorkspaceRoot)); len(added) > 0 {
					// A successful tidy that grew the committed require set is a
					// drift the release-set plan cannot see: the probe derived the
					// member's dependencies from the go.mod that was checked in,
					// so `package~go` refuses a module whose tidied go.mod names
					// a workspace module the plan does not. Say so here, at the
					// step that made the change, not at publish time.
					emit.Diagnostic("warning", tidyDriftMessage(ctx.Project.FullPath, added), "go.mod", 0)
				}
			}
			emit.PhaseEnd("setup", tidyPhaseStatus)
			if phase == phaseTidy {
				return tidyResult, nil, nil
			}
		}
		if phase == phaseTidy {
			return "OK", nil, nil
		}
	}

	// Common build flags (shared between compile and cross-compile phases)
	commonFlags := hostBuild.BuildArgs(ldflags)

	// --- Phase: cross-compile (distribution intent) ---
	// Compiles the platforms the invocation's PACKAGE CHANNELS consume: the
	// image's single platform for docker, the declared release matrix for
	// archives, nothing for channels that carry no binary.
	// Every input to that decision is a plan-time parameter or a declared
	// project fact the cache key already covers — see selectDistributionPlatforms.
	if phase == phaseCrossCompile {
		distribution, err := selectDistributionPlatforms(ctx, platformsSpec, target)
		if err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			return "FAILED", nil, nil
		}
		return runCrossCompile(ctx, emit, goBinary, entrypoint, cgo, commonFlags, distribution)
	}

	// --- Phase: compile (ordinary validation intent) ---
	//
	// The platform set is decided HERE, from plan-time inputs only, and the
	// default is the host alone. Nature and intent are INDEPENDENT: the platform
	// set says which platforms the evidence must cover, the project's nature says
	// what that evidence is. Resolving the set before dispatching on nature is
	// what keeps them from being conflated.
	requested, err := requestedPlatforms(platformsSpec, target)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	resolved := requested
	if len(resolved) == 0 {
		resolved = []platform.Target{platform.HostTarget()}
	}

	// A library owns no `package main`: its build evidence is that its packages
	// COMPILE, not that a binary exists. `go build ./...` is that evidence, and
	// it replaces the fallback that used to link the root non-main package once
	// per archive platform.
	//
	// This comes BEFORE the explicit-request branch because an explicit
	// `platforms` request does not turn a library into something that can produce
	// a binary. Routing it to the cross-compile path instead would revive exactly
	// a phantom build, and quietly: `go build -o <file> .`
	// on a non-main package EXITS 0 and writes a package archive, so the opt-in
	// would emit a file that is not an executable, for the root package only —
	// weaker evidence than `./...`, wearing a binary's name. Per-platform
	// checking is the whole point of the opt-in for a library (build tags, CGO,
	// platform-specific sources), so the request still decides the set.
	if ctx.Project.Type == projectTypeLibrary {
		return runCompileCheck(ctx, emit, goBinary, commonFlags, hostBuild, resolved)
	}

	if len(requested) > 0 {
		// An explicit platform request is an opt-in to cross-compilation, and it
		// keeps the shape it has always had under `build --target`: the
		// distribution-flavored flags (-trimpath, CGO off unless asked for) and
		// the platform-partitioned bin/<suffix>/ tree the package channels index.
		crossFlags := commonFlags
		if !hostBuild.Trimpath {
			crossFlags = append(append([]string(nil), commonFlags...), "-trimpath")
		}
		crossCGO := cgo
		if crossCGO == "" {
			crossCGO = "false"
		}
		return runCrossCompile(ctx, emit, goBinary, entrypoint, crossCGO, crossFlags, requested)
	}

	host := resolved[0]

	os.MkdirAll(ctx.OutputPath, 0o755)

	binExt := ""
	if host.GOOS == "windows" {
		binExt = ".exe"
	}

	// Determine packages to compile.
	// Priority: explicit entrypoint > auto-detect cmd/*/ > root package "."
	targets := resolveCompileTargets(ctx, entrypoint, outputPath, binExt)

	// Build environment: the host build's own, which resolves the workspace
	// go.work explicitly so an inherited GOWORK=off (leaked from a parent that
	// built standalone) can't drop the workload's workspace replaces and break
	// compilation, and settles CGO_ENABLED from the `cgo` parameter. GOOS/GOARCH
	// are left alone: this path is the host build, and every non-host platform
	// went through the explicit request above.
	buildEnv := hostBuild.Env(os.Environ(), moduleRoot(ctx), goBinary)

	// Compile each target
	emit.PhaseStart("compile")

	binaryPaths := make([]string, 0, len(targets))
	failed := false

	for _, t := range targets {
		buildArgs := make([]string, 0, len(commonFlags)+4)
		buildArgs = append(buildArgs, "build", "-o", t.binaryPath)
		buildArgs = append(buildArgs, commonFlags...)
		buildArgs = append(buildArgs, t.pkg)

		emit.Log("debug", "Building "+t.pkg+"...")

		cmd := exec.Command(goBinary, buildArgs...)
		cmd.Dir = ctx.Project.FullPath
		cmd.Env = buildEnv

		output, err := cmd.CombinedOutput()

		if err != nil {
			parse.GoBuildErrors(string(output), ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
			failed = true
			continue
		}

		if info, statErr := os.Stat(t.binaryPath); statErr == nil {
			emit.Metric("binary-size", info.Size(), "bytes")
		}
		// Compiled binaries are first-class artifacts (runtime protocol
		// kind "binary"), not just paths buried in result data.
		emit.Artifact(filepath.Base(t.binaryPath), filepath.Base(t.binaryPath), "binary", t.binaryPath)
		binaryPaths = append(binaryPaths, t.binaryPath)
	}

	if failed {
		emit.PhaseEnd("compile", "failed")
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("compile", "success")

	// Post-compile install: copy binary to a well-known location if configured.
	// Only applies to single-target builds.
	if len(binaryPaths) == 1 {
		installPath := cli.FlagString(flags, "install", "")
		if installPath == "" {
			installPath = ctx.Params.String("install")
		}
		if installPath != "" {
			resolved := installPath
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(ctx.WorkspaceRoot, installPath)
			}
			os.MkdirAll(filepath.Dir(resolved), 0o755)
			if cpErr := copyFile(binaryPaths[0], resolved); cpErr != nil {
				emit.Log("warn", "Install failed: "+cpErr.Error())
			} else {
				emit.Log("info", "Installed to "+resolved)
			}
		}
	}

	return "OK", map[string]any{
		"binaryPaths": binaryPaths,
		"platforms":   platformNames([]platform.Target{host}),
	}, nil
}

// runCrossCompile compiles all entrypoints once per resolved platform.
// Binaries are written to {outputPath}/bin/{platform-suffix}/{binaryName}.
//
// The platform set is a PARAMETER, never a decision made here: it is resolved
// from plan-time inputs by the caller so the set that produced a cache entry is
// the set its key was computed from.
func runCrossCompile(ctx *pctx.Context, emit *jsonl.Emitter, goBinary, entrypoint, cgo string, commonFlags []string, platforms []platform.Target) (string, map[string]any, error) {
	os.MkdirAll(ctx.OutputPath, 0o755)

	// Detect entrypoints (packages to compile).
	targets := resolveCompileTargets(ctx, entrypoint, "", "")

	emit.PhaseStart("cross-compile")

	// Declared extra executables ship beside the entrypoint in every platform
	// archive, so they are compiled for every platform too.
	executables, err := platform.ReadGoExecutables(ctx.Project.FullPath)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		emit.PhaseEnd("cross-compile", "failed")
		return "FAILED", nil, nil
	}
	for _, e := range executables {
		for _, t := range targets {
			if filepath.Base(t.binaryPath) == e.Name {
				emit.Diagnostic("error", fmt.Sprintf(`options["@putnami/go"].executables: %q collides with the entrypoint binary`, e.Name), "", 0)
				emit.PhaseEnd("cross-compile", "failed")
				return "FAILED", nil, nil
			}
		}
	}
	for _, e := range executables {
		targets = append(targets, compileTarget{pkg: e.Package, binaryPath: e.Name})
	}

	platformBinaries := make(map[string][]string)
	failed := false

	// Resolve the workspace go.work once, over an inherited GOWORK=off.
	baseEnv := toolchain.WorkspaceBuildEnv(os.Environ(), moduleRoot(ctx), goBinary)

	for i, p := range platforms {
		emit.Progress(i+1, len(platforms), fmt.Sprintf("Compiling for %s/%s", p.GOOS, p.GOARCH))

		platformBinDir := filepath.Join(ctx.OutputPath, "bin", p.Suffix)
		os.MkdirAll(platformBinDir, 0o755)

		// Build environment for this platform (fresh copy: setEnv mutates in place).
		platformEnv := append([]string(nil), baseEnv...)
		platformEnv = setEnv(platformEnv, "GOOS", p.GOOS)
		platformEnv = setEnv(platformEnv, "GOARCH", p.GOARCH)
		if cgo == "true" {
			platformEnv = setEnv(platformEnv, "CGO_ENABLED", "1")
		} else {
			platformEnv = setEnv(platformEnv, "CGO_ENABLED", "0")
		}

		binExt := ""
		if p.GOOS == "windows" {
			binExt = ".exe"
		}

		for _, t := range targets {
			binaryName := filepath.Base(t.binaryPath) + binExt
			binaryPath := filepath.Join(platformBinDir, binaryName)

			// Build to a unique temp file inside the destination directory, then
			// rename over the final path. `go build -o` only renames atomically
			// when its work dir shares a filesystem with the output; a cross-device
			// build falls back to an in-place O_TRUNC copy, so a second run exec'ing
			// binaryPath can observe a torn Mach-O/ELF (SIGKILL/SIGBUS). An
			// intra-directory rename() publishes the binary atomically: a concurrent
			// reader/exec sees either the old inode or the fully-written new one. The
			// unique temp name lets two concurrent cross-compiles for the same
			// platform build without colliding on a shared temp path.
			binaryPath, ok := func() (string, bool) {
				tmpFile, err := os.CreateTemp(platformBinDir, binaryName+".tmp-*")
				if err != nil {
					emit.Diagnostic("error", fmt.Sprintf("Failed to stage temp output for %s/%s: %s", p.GOOS, p.GOARCH, err.Error()), "", 0)
					return "", false
				}
				tmpPath := tmpFile.Name()
				tmpFile.Close()
				renamed := false
				defer func() {
					// Best-effort cleanup; no-ops once rename has consumed tmpPath.
					if !renamed {
						os.Remove(tmpPath)
					}
				}()

				buildArgs := []string{"build", "-o", tmpPath}
				buildArgs = append(buildArgs, commonFlags...)
				buildArgs = append(buildArgs, t.pkg)

				emit.Log("debug", fmt.Sprintf("Building %s for %s/%s...", t.pkg, p.GOOS, p.GOARCH))

				// Fresh Cmd per attempt: the shared GOCACHE is concurrently
				// deletable and the transient ENOENT class is retried.
				output, err := toolchain.RunWithGoCacheRetry(func() ([]byte, error) {
					cmd := exec.Command(goBinary, buildArgs...)
					cmd.Dir = ctx.Project.FullPath
					cmd.Env = platformEnv
					return cmd.CombinedOutput()
				})
				if err != nil {
					parse.GoBuildErrors(string(output), ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
					emit.Diagnostic("error", fmt.Sprintf("Failed to compile %s for %s/%s: %s", t.pkg, p.GOOS, p.GOARCH, strings.TrimSpace(string(output))), "", 0)
					return "", false
				}

				if err := os.Rename(tmpPath, binaryPath); err != nil {
					emit.Diagnostic("error", fmt.Sprintf("Failed to finalize %s for %s/%s: %s", binaryName, p.GOOS, p.GOARCH, err.Error()), "", 0)
					return "", false
				}
				renamed = true
				return binaryPath, true
			}()
			if !ok {
				failed = true
				continue
			}

			if info, statErr := os.Stat(binaryPath); statErr == nil {
				emit.Metric("binary-size-"+p.Suffix, info.Size(), "bytes")
			}
			platformBinaries[p.Suffix] = append(platformBinaries[p.Suffix], binaryPath)
		}
	}

	if failed {
		emit.PhaseEnd("cross-compile", "failed")
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("cross-compile", "success")
	return "OK", map[string]any{
		"platformBinaries": platformBinaries,
		"platforms":        platformNames(platforms),
	}, nil
}

// runCompileCheck proves that a library's packages compile, for each resolved
// platform, and emits nothing.
//
// `go build ./...` on a module with no `package main` writes no artifact by
// construction: the go tool discards the built objects and reports only the
// diagnostics. That is exactly the evidence an ordinary `build` owes a library —
// and it is one compile per platform instead of the four link attempts the
// root-package fallback used to make against a package that can never produce a
// binary.
func runCompileCheck(
	ctx *pctx.Context,
	emit *jsonl.Emitter,
	goBinary string,
	commonFlags []string,
	hostBuild toolchain.HostBuild,
	platforms []platform.Target,
) (string, map[string]any, error) {
	emit.PhaseStart("compile")

	// The same host build environment the binary path uses, so a library's
	// compile check and an application's compile resolve CGO_ENABLED — and the
	// `-race` rule that depends on it — identically. GOOS/GOARCH are applied on
	// top, per requested platform.
	baseEnv := hostBuild.Env(os.Environ(), moduleRoot(ctx), goBinary)
	failed := false

	for i, p := range platforms {
		emit.Progress(i+1, len(platforms), fmt.Sprintf("Checking %s/%s", p.GOOS, p.GOARCH))

		env := append([]string(nil), baseEnv...)
		env = setEnv(env, "GOOS", p.GOOS)
		env = setEnv(env, "GOARCH", p.GOARCH)

		buildArgs := append([]string{"build"}, commonFlags...)
		buildArgs = append(buildArgs, "./...")

		emit.Log("debug", fmt.Sprintf("Compile-checking ./... for %s/%s...", p.GOOS, p.GOARCH))

		cmd := exec.Command(goBinary, buildArgs...)
		cmd.Dir = ctx.Project.FullPath
		cmd.Env = env

		if output, err := cmd.CombinedOutput(); err != nil {
			parse.GoBuildErrors(string(output), ctx.WorkspaceRoot, ctx.Project.FullPath, emit)
			failed = true
		}
	}

	if failed {
		emit.PhaseEnd("compile", "failed")
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("compile", "success")
	return "OK", map[string]any{
		"compileChecked": true,
		"binaryPaths":    []string{},
		"platforms":      platformNames(platforms),
	}, nil
}

// requestedPlatforms resolves an EXPLICIT platform request under the ordinary
// validation intent, and returns nil when there is none.
//
// Both inputs are plan-time parameters resolved by the orchestrator — the
// `platforms` project option (or `--platforms`) and `--target` — and both are
// declared `from: "params"` task inputs, so the set a compile produced is the
// set its cache key was computed from. Nothing here reads putnami.json, the
// environment, or the machine: a platform set discovered inside the task would
// be invisible to the key, and a stored verdict would then be served for a
// platform set it was never built for.
//
// `--target` names ONE platform and wins over the `platforms` set, because it is
// the narrower, per-invocation request.
func requestedPlatforms(spec []string, target string) ([]platform.Target, error) {
	if target != "" {
		return platform.TargetsFor(target)
	}
	return platform.ParsePlatformSpec(spec)
}

// selectDistributionPlatforms resolves the DISTRIBUTION intent's platform set,
// scoped to the package channels this invocation will produce.
//
// Every input is plan-time and keyed: `--target` and `--platforms`/`platforms`
// are declared `from: "params"` inputs, the image `--platform` is a declared
// parameter too, and the channel set unions those same parameters with the
// project's `publish` declaration — which lives in putnami.json, a declared
// `from: "project"` file input, so changing it moves the key. Nothing here is
// discovered at execution time; a channel set the key could not see would let a
// four-platform entry answer a one-platform request, or the reverse.
func selectDistributionPlatforms(ctx *pctx.Context, spec []string, target string) ([]platform.Target, error) {
	return platform.DistributionTargets(platform.DistributionRequest{
		Channels:       packageChannels(ctx),
		Declared:       spec,
		DockerPlatform: ctx.Params.String("platform"),
		Target:         target,
	})
}

// packageChannels resolves which channels this `package` invocation produces:
// the UNION of the project's declared publish channels and the channel
// parameters the invocation set.
//
// The union is the channel set the plan schedules: `putnami package --docker`
// on a project declaring `publish: ["archives"]` runs BOTH packagers.
//
// The declaration reaches this task as a project fact (ctx.Project.Publish).
// Both halves are plan-time and both are in the cache key: the parameters
// directly, the declaration through putnami.json.
//
// An explicitly FALSE channel parameter does not un-declare a published
// channel here.
func packageChannels(ctx *pctx.Context) platform.PackageChannels {
	channels := platform.PackageChannelsFor(ctx.PublishChannels())
	if ctx.Params.Bool(platform.ChannelArchives, false) {
		channels.Archives = true
	}
	if ctx.Params.Bool(platform.ChannelTemplateArchives, false, "templateArchives") {
		channels.TemplateArchives = true
	}
	if ctx.Params.Bool(platform.ChannelDocker, false) {
		channels.Docker = true
	}
	if ctx.Params.Bool(platform.ChannelGoModule, false) {
		channels.GoModule = true
	}
	return channels
}

// platformsParam reads the resolved `platforms` job parameter from ctx.Params —
// the orchestrator's merged, plan-time parameter map — and never from the
// project's config file, which is what keeps it a cache-key input. The decoding
// itself is platform.PlatformsSpec, shared with the package job so one spec
// cannot resolve to two different matrices.
func platformsParam(ctx *pctx.Context) ([]string, error) {
	return platform.PlatformsSpec(ctx.Params["platforms"])
}

// platformNames renders a resolved platform set for result data, so a reader of
// a build result can see which platforms it actually covers.
func platformNames(platforms []platform.Target) []string {
	names := make([]string, 0, len(platforms))
	for _, p := range platforms {
		names = append(names, p.GOOS+"/"+p.GOARCH)
	}
	return names
}

// compileTarget describes a Go package to compile and its output binary path.
type compileTarget struct {
	pkg        string // Go package path (e.g., "./cmd/build", ".")
	binaryPath string // output binary path (without platform prefix for cross-compile)
}

// resolveCompileTargets determines which Go packages to compile and their output binary paths.
// Priority: explicit entrypoint > auto-detect cmd/*/ > root package "."
func resolveCompileTargets(ctx *pctx.Context, entrypoint, outputPath, binExt string) []compileTarget {
	var targets []compileTarget

	switch {
	case outputPath != "":
		// Explicit output path: single target
		bp := outputPath
		if !filepath.IsAbs(bp) {
			bp = filepath.Join(ctx.OutputPath, bp)
		}
		pkg := "."
		if entrypoint != "" {
			pkg = entrypoint
		}
		targets = append(targets, compileTarget{pkg: pkg, binaryPath: bp})
	case entrypoint != "":
		// Explicit entrypoint: single target
		binDir := filepath.Join(ctx.OutputPath, "bin")
		os.MkdirAll(binDir, 0o755)
		binaryName := platform.DeriveBinaryName(entrypoint, ctx.Project.Name)
		targets = append(targets, compileTarget{
			pkg:        entrypoint,
			binaryPath: filepath.Join(binDir, binaryName+binExt),
		})
	default:
		// Auto-detect: scan for cmd/*/main.go
		cmdDir := filepath.Join(ctx.Project.FullPath, "cmd")
		if entries, err := os.ReadDir(cmdDir); err == nil {
			binDir := filepath.Join(ctx.OutputPath, "bin")
			os.MkdirAll(binDir, 0o755)
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				mainFile := filepath.Join(cmdDir, entry.Name(), "main.go")
				if _, err := os.Stat(mainFile); err == nil {
					targets = append(targets, compileTarget{
						pkg:        "./cmd/" + entry.Name(),
						binaryPath: filepath.Join(binDir, entry.Name()+binExt),
					})
				}
			}
		}

		// Fallback: compile root package
		if len(targets) == 0 {
			binDir := filepath.Join(ctx.OutputPath, "bin")
			os.MkdirAll(binDir, 0o755)
			binaryName := platform.DeriveBinaryName(".", ctx.Project.Name)
			targets = append(targets, compileTarget{
				pkg:        ".",
				binaryPath: filepath.Join(binDir, binaryName+binExt),
			})
		}
	}
	return targets
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// tidyOfflineMessage explains the one outcome in which the tidy phase runs no
// command. It is a log rather than a diagnostic: a run configured to resolve no
// module is doing what it was asked, on every project it selects.
const tidyOfflineMessage = "module downloads are disabled (GOPROXY=off): go mod tidy is not run and go.mod/go.sum are left as committed"

// tidyFailureMessage builds the warning diagnostic for a failed `go mod tidy`.
// Module-mode tidy ignores go.work, so a go.mod missing a relative replace for
// a workspace dependency chases the proxy for a placeholder version that only
// exists in this repo; when the tidy output names such modules the message
// points at `putnami projects sync`, which maintains the replace closure.
// Other failures carry the trimmed tidy output.
func tidyFailureMessage(workspaceRoot, projectDir, output string) string {
	module := toolchain.GoModModulePath(filepath.Join(projectDir, "go.mod"))
	if module == "" {
		module = filepath.Base(projectDir)
	}
	missing := unresolvableWorkspaceModules(output, toolchain.WorkspaceGoModules(workspaceRoot), module)
	if len(missing) > 0 {
		return fmt.Sprintf("go mod tidy failed for %s: workspace deps [%s] unresolvable offline — run `putnami projects sync` to add missing replaces",
			module, strings.Join(missing, ", "))
	}
	msg := "go mod tidy failed for " + module + ", continuing"
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		msg += ": " + trimmed
	}
	return msg
}

// unresolvableWorkspaceModules returns the workspace module paths (excluding
// self, the module being tidied) that the tidy output reports as unresolvable,
// using the same "path:"/"path@" classification as the CLI's offline tidy
// guard (TestWorkspaceGoModulesTidyOffline).
func unresolvableWorkspaceModules(output string, workspaceModules []string, self string) []string {
	var missing []string
	for _, mod := range workspaceModules {
		if mod == self {
			continue
		}
		if strings.Contains(output, mod+":") || strings.Contains(output, mod+"@") {
			missing = append(missing, mod)
		}
	}
	return missing
}

// tidyAddedWorkspaceRequirements names the workspace modules a successful
// `go mod tidy` newly required in the project's go.mod, sorted. Test-only
// imports count: tidy requires what `_test.go` files import, so a module whose
// tests reach a sibling for the first time gains a requirement the committed
// go.mod never carried. The release-set probe derives a member's dependencies
// from the committed go.mod, so such a requirement is invisible to the plan
// until the file is committed. Nil when the snapshot did not observe go.mod,
// either file fails to parse, or nothing was added.
func tidyAddedWorkspaceRequirements(snap modSnapshot, dir string, workspaceModules []string) []string {
	before, ok := snap.files["go.mod"]
	if !ok || !before.exists {
		return nil
	}
	after, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil || bytes.Equal(before.content, after) {
		return nil
	}
	beforeFile, err := modfile.ParseLax("go.mod", before.content, nil)
	if err != nil {
		return nil
	}
	afterFile, err := modfile.ParseLax("go.mod", after, nil)
	if err != nil {
		return nil
	}
	known := make(map[string]bool, len(beforeFile.Require))
	for _, require := range beforeFile.Require {
		known[require.Mod.Path] = true
	}
	workspace := make(map[string]bool, len(workspaceModules))
	for _, module := range workspaceModules {
		workspace[module] = true
	}
	var added []string
	for _, require := range afterFile.Require {
		path := require.Mod.Path
		if known[path] || !workspace[path] {
			continue
		}
		known[path] = true
		added = append(added, path)
	}
	sort.Strings(added)
	return added
}

// tidyDriftMessage names the remedy for a tidy that grew the require set:
// commit go.mod, then run `putnami projects sync` so every dependent's
// replace closure follows. Without both, the release-set plan built from the
// committed tree does not carry the new edge and `package~go` refuses the
// module.
func tidyDriftMessage(projectDir string, added []string) string {
	module := toolchain.GoModModulePath(filepath.Join(projectDir, "go.mod"))
	if module == "" {
		module = filepath.Base(projectDir)
	}
	return fmt.Sprintf("go mod tidy added workspace requirement(s) [%s] to %s: commit go.mod and run `putnami projects sync`, or the release-set plan built from the committed tree will not carry them and `package~go` refuses the module",
		strings.Join(added, ", "), module)
}

// modFileNames are the module files `go mod tidy` may rewrite. They are also
// the cache-key inputs of the build-tidy task, so a failed tidy must never
// leave them churned.
var modFileNames = []string{"go.mod", "go.sum"}

// modFileState records one module file at snapshot time. Absent (exists=false)
// is distinct from present-but-empty so restore can delete a file that a
// failed tidy newly created.
type modFileState struct {
	exists  bool
	content []byte
}

// modSnapshot captures go.mod/go.sum content before a mutating command runs.
// A file that could not be read for a reason other than non-existence has no
// entry at all: restore must leave such a file untouched rather than guess.
type modSnapshot struct {
	files map[string]modFileState
}

// snapshotModFiles reads go.mod and go.sum in dir. A missing file is recorded
// as absent; snapshotting never fails the build.
func snapshotModFiles(dir string) modSnapshot {
	snap := modSnapshot{files: make(map[string]modFileState, len(modFileNames))}
	for _, name := range modFileNames {
		content, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case err == nil:
			snap.files[name] = modFileState{exists: true, content: content}
		case os.IsNotExist(err):
			snap.files[name] = modFileState{exists: false}
		default:
			// Unreadable but possibly present: skip it so restore never
			// deletes or truncates a file we could not observe.
		}
	}
	return snap
}

// restoreModFiles rewrites go.mod/go.sum in dir back to their snapshotted
// bytes and removes files that were absent at snapshot time. It returns the
// names of files it actually changed and the first error encountered; errors
// never abort the remaining restores.
func restoreModFiles(dir string, snap modSnapshot) ([]string, error) {
	var changed []string
	var firstErr error
	for _, name := range modFileNames {
		state, ok := snap.files[name]
		if !ok {
			continue // not observed at snapshot time: leave untouched
		}
		path := filepath.Join(dir, name)
		current, readErr := os.ReadFile(path)
		currentExists := readErr == nil

		if !state.exists {
			if !currentExists {
				continue
			}
			if err := os.Remove(path); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			changed = append(changed, name)
			continue
		}

		if currentExists && bytes.Equal(current, state.content) {
			continue
		}
		// Preserve the current file mode when the file still exists;
		// otherwise fall back to the conventional modfile mode.
		mode := os.FileMode(0o644)
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(path, state.content, mode); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		changed = append(changed, name)
	}
	return changed, firstErr
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// moduleRoot resolves the module root directory for ctx: Project.FullPath when
// set, else WorkspaceRoot/Project.Path. It is the directory each spawned `go`
// subprocess resolves its GOWORK against, so a leaked GOWORK=off cannot disable
// workspace resolution and 404-storm the proxy with v0.0.0 placeholders.
func moduleRoot(ctx *pctx.Context) string {
	if ctx.Project.FullPath != "" {
		return ctx.Project.FullPath
	}
	return filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
}
