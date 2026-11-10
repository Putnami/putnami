package dart

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
)

const (
	// PackageDockerJobName is the job that assembles an application image.
	PackageDockerJobName = "package-docker"
	// PublishDockerJobName is the job that pushes an application image.
	PublishDockerJobName = "publish-docker"
)

// Diagnostic codes of the package job.
const (
	// CodeImageFailed is the diagnostic of an image that cannot be assembled.
	CodeImageFailed = "dart.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "dart.base_not_pinned"
	// CodeNoEntrypoint is the diagnostic of a package whose image entrypoint
	// the job cannot choose.
	CodeNoEntrypoint = "dart.no_entrypoint"
	// CodeCrossCompileUnsupported is the diagnostic of a selected Dart that
	// cannot compile for a Linux architecture.
	CodeCrossCompileUnsupported = "dart.cross_compile_unsupported"
	// CodePlatformUnsupported is the diagnostic of an image platform that has
	// no Dart target.
	CodePlatformUnsupported = "dart.platform_unsupported"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/app"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
	// imageUser is the nonroot user of distroless, by number, so that any base
	// runs the executable as a non-root user.
	imageUser = "65532:65532"
	// glibcBase is the distroless base of an executable that links the C
	// library dynamically, pinned by digest: glibc, CA certificates, time
	// zones and the C++ runtime, and no shell.
	glibcBase = "gcr.io/distroless/cc-debian12:nonroot@sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985"
	// crossCompileSince is the first Dart release whose dart compile exe
	// takes --target-arch.
	crossCompileSince = "3.8.0"
)

// targetArchs are the dart compile exe architectures of the image platforms.
var targetArchs = map[string]string{
	"linux/amd64": "x64",
	"linux/arm64": "arm64",
}

// PublishDockerJob pushes the packaged image of an application through the
// shared publisher. A library or a Flutter package publishes nothing.
func PublishDockerJob() cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if !imageable(ctx) {
			return "SKIP", nil, nil
		}
		return dockerpublish.Publish(ctx, emit, args)
	}
}

// PackageDockerJob assembles the image of an application: its entrypoint
// compiled ahead of time for Linux with the selected dart, on a digest-pinned
// distroless base that holds glibc. It needs no Docker daemon. It compiles
// under the lock of the stack root, into a directory of its own. A library or
// a Flutter package packages nothing.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if !imageable(ctx) {
			return "SKIP", nil, nil
		}
		if err := packageImage(stack, ctx, cli.ParseFlags(args), emit); err != nil {
			return "FAILED", nil, err
		}
		return "OK", nil, nil
	}
}

// imageable reports whether the job's component may have an image: an
// application that is no Flutter package and holds a bin/*.dart.
func imageable(ctx *pctx.Context) bool {
	return ctx.Project.Type != "library" && variant(ctx) != VariantFlutter && len(executables(ctx.Project.FullPath)) > 0
}

// executables are the names of the bin/*.dart of the package at dir, without
// their extension, sorted.
func executables(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, "bin", "*.dart"))
	var out []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && !info.IsDir() {
			out = append(out, strings.TrimSuffix(filepath.Base(m), ".dart"))
		}
	}
	slices.Sort(out)
	return out
}

func packageImage(stack stacks.Stack, ctx *pctx.Context, flags map[string]string, emit *jsonl.Emitter) error {
	base := cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage"))
	if base == "" {
		base = glibcBase
	} else if _, err := oci.ParseDigestReference(base); err != nil {
		return fail(emit, CodeBaseNotPinned, fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", base, err))
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	arch, ok := targetArchs[platform]
	if !ok {
		return fail(emit, CodePlatformUnsupported, fmt.Errorf("the platform %s has no Dart target an image runs: use linux/amd64 or linux/arm64", platform))
	}
	var meta stacks.Metadata
	_ = json.Unmarshal(ctx.Project.Metadata[ExtensionName], &meta)
	entrypoint, err := imageEntrypoint(ctx.Project.FullPath, meta.Component, cli.FlagString(flags, "entrypoint", ctx.Params.String("entrypoint")))
	if err != nil {
		return fail(emit, CodeNoEntrypoint, err)
	}
	executable := strings.TrimSuffix(path.Base(entrypoint), ".dart")

	output := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, dockerChannel)
	name := stacks.ImageName(ctx.Project.Name)
	version := "0.0.0"
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	record := func() error {
		return pkgmeta.WriteChannelRecord(output, pkgmeta.ChannelRecord{Version: version, Artifact: name, Channels: []string{dockerChannel}})
	}
	dryRun := cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun"))
	staging, err := os.MkdirTemp("", "putnami-dart-image-")
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	err = stack.Locked(ctx, emit, func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
		dart := dartProgram(ctx, root)
		if err := checkCrossCompile(run, dart, root.Env, emit); err != nil || dryRun {
			return err
		}
		return compileExecutable(run, root, dart, entrypoint, arch, filepath.Join(staging, executable), emit)
	})
	if err != nil {
		return err
	}
	if dryRun {
		emit.Info("dry run: would assemble an image of " + entrypoint + " for " + platform + " on " + base)
		if err := record(); err != nil {
			return fail(emit, CodeImageFailed, err)
		}
		return nil
	}
	return assembleImage(ctx, flags, filepath.Join(staging, executable), base, platform, output, name, version, record, emit)
}

// imageEntrypoint answers the Dart file the image runs, relative to the
// package at dir: the one named, a Dart file of the package or the name of a
// bin/ executable; else bin/server.dart, which serve starts; else
// bin/<package>.dart, which dart run starts; else the package's only
// bin/*.dart.
func imageEntrypoint(dir, pkg, named string) (string, error) {
	if named != "" {
		rel := filepath.ToSlash(filepath.Clean(named))
		if !strings.HasSuffix(rel, ".dart") {
			if strings.ContainsAny(named, `:/\`) {
				return "", fmt.Errorf("the entrypoint %s is neither a Dart file nor the name of a bin/ executable of %s", named, dir)
			}
			rel = "bin/" + named + ".dart"
		}
		if path.IsAbs(rel) || filepath.IsAbs(named) || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", fmt.Errorf("the entrypoint %s is outside the package %s", named, dir)
		}
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil || info.IsDir() {
			return "", fmt.Errorf("the entrypoint %s names no Dart file of %s", named, dir)
		}
		return rel, nil
	}
	names := executables(dir)
	for _, preferred := range []string{"server", pkg} {
		if preferred != "" && slices.Contains(names, preferred) {
			return "bin/" + preferred + ".dart", nil
		}
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("%s holds no bin/*.dart", dir)
	case 1:
		return "bin/" + names[0] + ".dart", nil
	}
	return "", fmt.Errorf("%s holds several executables (%s) and no bin/server.dart: name one with --entrypoint or options.package.entrypoint", dir, strings.Join(names, ", "))
}

// releaseNumbers reads the major and minor numbers of a Dart version.
var releaseNumbers = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.`)

// checkCrossCompile fails unless the dart at program compiles for a Linux
// architecture: a release from crossCompileSince.
func checkCrossCompile(run context.Context, program string, env []string, emit *jsonl.Emitter) error {
	version, err := dartVersion(run, program, env)
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	if !atLeast(version, crossCompileSince) {
		return fail(emit, CodeCrossCompileUnsupported, fmt.Errorf("the selected Dart %s cannot compile an executable for an image: dart compile exe takes --target-arch from Dart %s; state environment.sdk ^%s in pubspec.yaml, or select a newer SDK", version, crossCompileSince, crossCompileSince))
	}
	return nil
}

// atLeast reports whether the major and minor numbers of version reach those
// of floor.
func atLeast(version, floor string) bool {
	v, f := releaseNumbers.FindStringSubmatch(version), releaseNumbers.FindStringSubmatch(floor)
	if v == nil || f == nil {
		return false
	}
	number := func(s string) int { n, _ := strconv.Atoi(s); return n }
	if number(v[1]) != number(f[1]) {
		return number(v[1]) > number(f[1])
	}
	return number(v[2]) >= number(f[2])
}

// compileExecutable compiles entrypoint, relative to the package, into the
// executable at out for Linux on arch, with the selected dart in the package
// directory.
func compileExecutable(run context.Context, root stacks.RootStep, dart, entrypoint, arch, out string, emit *jsonl.Emitter) error {
	args := []string{"compile", "exe", entrypoint, "-o", out, "--target-os", "linux", "--target-arch", arch}
	emit.Info("$ dart " + strings.Join(args, " "))
	cmd := exec.CommandContext(run, dart, args...) //nolint:gosec // the selected SDK
	cmd.Dir = root.Project
	cmd.Env = root.Env
	// The job's stdout carries its protocol.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(emit, CodeImageFailed, fmt.Errorf("dart %s in %s: %w", strings.Join(args, " "), root.Project, err))
	}
	if info, err := os.Stat(out); err != nil || info.IsDir() {
		return fail(emit, CodeImageFailed, fmt.Errorf("dart compile exe wrote no executable %s", out))
	}
	return nil
}

// assembleImage assembles the compiled executable into the OCI layout of
// output, records it, and loads it into Docker when asked.
func assembleImage(ctx *pctx.Context, flags map[string]string, executable, base, platform, output, name, version string, record func() error, emit *jsonl.Emitter) error {
	plan, err := oci.NewImagePlan(imageSpec(executable, base, platform, defaultImagePort), oci.PlanOptions{})
	if err != nil {
		return fail(emit, CodeImageFailed, fmt.Errorf("plan the image: %w", err))
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(output, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base"),
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
	})
	if err != nil {
		return fail(emit, CodeImageFailed, fmt.Errorf("assemble the image: %w", err))
	}
	contentHash := plan.Identity[:12]
	tags := []string{name + ":c-" + contentHash}
	if tag := cli.FlagString(flags, "docker-tag", ctx.Params.String("docker-tag", "dockerTag")); tag != "" {
		tags = append(tags, name+":"+tag)
	}
	if cli.FlagBool(flags, "docker-load", ctx.Params.Bool("docker-load", false, "dockerLoad")) {
		if err := stacks.LoadImage(filepath.Join(output, "oci"), tags, emit); err != nil {
			return fail(emit, CodeImageFailed, err)
		}
	}
	manifest, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: name, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: platform,
	}, "", "  ")
	if err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(manifest, '\n'), 0o644); err != nil { //nolint:gosec // an output of the job
		return fail(emit, CodeImageFailed, err)
	}
	if err := record(); err != nil {
		return fail(emit, CodeImageFailed, err)
	}
	emit.Info(fmt.Sprintf("assembled the image %s (%s)", tags[0], digest))
	return nil
}

// imageSpec describes the application's image: the executable at
// /app/<name>, its entrypoint, run as nonroot in /app with PORT set.
func imageSpec(executable, base, platform, port string) oci.Spec {
	entrypoint := imageAppDir + "/" + filepath.Base(executable)
	return oci.Spec{
		BaseRef:      base,
		Platform:     platform,
		Layers:       []oci.Layer{{Files: []oci.File{{Source: executable, Path: entrypoint, Mode: 0o755}}}},
		Env:          []string{"PORT=" + port},
		Entrypoint:   []string{entrypoint},
		WorkingDir:   imageAppDir,
		User:         imageUser,
		ExposedPorts: []string{port + "/tcp"},
	}
}

func fail(emit *jsonl.Emitter, code string, err error) error {
	emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
	return fmt.Errorf("%s: %w", code, err)
}
