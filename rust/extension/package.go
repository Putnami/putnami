package rust

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/stacks"
)

// The channels of package, as params and the publish field of a project name
// them.
const (
	channelArchives = "archives"
	channelDocker   = "docker"
)

const (
	defaultDockerPlatform = "linux/amd64"
	defaultPort           = 3000
	// linuxGlibc is the glibc of the image base. cargo zigbuild reads it from
	// the suffix of a Linux target.
	linuxGlibc = ".2.36"
	// imageBase is the digest-pinned base of an image: a Rust binary needs
	// glibc and libgcc. The pin is what makes the image digest reproducible.
	imageBase = "gcr.io/distroless/cc-debian12:nonroot@sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985"
)

// rustTargets are the Rust names of the platforms package compiles for.
var rustTargets = map[string]string{
	"linux/amd64":   "x86_64-unknown-linux-gnu",
	"linux/arm64":   "aarch64-unknown-linux-gnu",
	"darwin/amd64":  "x86_64-apple-darwin",
	"darwin/arm64":  "aarch64-apple-darwin",
	"windows/amd64": "x86_64-pc-windows-gnu",
}

// Jobs are the job handlers @putnami/rust adds to the stack core's: run and
// serve, on the core's process helpers, and the package and publish steps.
func Jobs(stack stacks.Stack) map[string]cli.JobFunc {
	return map[string]cli.JobFunc{
		RunJobName:         RunJob(stack),
		ServeJobName:       ServeJob(stack),
		"package-compile":  packageCompile(stack),
		"package-archives": packageArchives,
		"package-docker":   packageDocker,
		"package-crate":    packageCrate(stack),
		"publish-crates":   publishCrates(stack),
		"publish-docker":   publishDocker,
	}
}

// platformName is the os/arch form of a platform.
func platformName(p pkgmeta.ArchivePlatform) string { return p.GOOS + "/" + p.GOARCH }

// findPlatform resolves an os/arch name.
func findPlatform(name string) (pkgmeta.ArchivePlatform, error) {
	all := pkgmeta.ArchivePlatforms()
	for _, p := range all {
		if platformName(p) == name {
			return p, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, platformName(p))
	}
	return pkgmeta.ArchivePlatform{}, fmt.Errorf("package does not know the platform %q: use one of %s", name, strings.Join(names, ", "))
}

// hasChannel reports whether this package produces a channel: the flag, or the
// publish field of the project, which the plan unions the same way.
func hasChannel(ctx *pctx.Context, channel string) bool {
	return ctx.Params.Bool(channel, false) || slices.Contains(ctx.PublishChannels(), channel)
}

// dockerPlatform is the one Linux platform of the image.
func dockerPlatform(ctx *pctx.Context) (pkgmeta.ArchivePlatform, error) {
	name := ctx.Params.String("platform")
	if name == "" {
		name = defaultDockerPlatform
	}
	p, err := findPlatform(name)
	if err != nil {
		return pkgmeta.ArchivePlatform{}, err
	}
	if p.GOOS != "linux" {
		return pkgmeta.ArchivePlatform{}, fmt.Errorf("an image runs on Linux: --platform %s names another system", name)
	}
	return p, nil
}

// archivePlatforms are the platforms of --platforms: a comma-separated list, or
// all five when it is empty or all.
func archivePlatforms(ctx *pctx.Context) ([]pkgmeta.ArchivePlatform, error) {
	spec := strings.TrimSpace(ctx.Params.String("platforms"))
	if spec == "" || spec == "all" {
		return pkgmeta.ArchivePlatforms(), nil
	}
	var out []pkgmeta.ArchivePlatform
	for _, name := range strings.Split(spec, ",") {
		p, err := findPlatform(strings.TrimSpace(name))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// compilePlatforms are the platforms the channels of this package need.
func compilePlatforms(ctx *pctx.Context) ([]pkgmeta.ArchivePlatform, error) {
	var out []pkgmeta.ArchivePlatform
	if hasChannel(ctx, channelDocker) {
		p, err := dockerPlatform(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if hasChannel(ctx, channelArchives) {
		platforms, err := archivePlatforms(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range platforms {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// binDir is the directory of the binary of one platform.
func binDir(ctx *pctx.Context, p pkgmeta.ArchivePlatform) string {
	return filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "bin"), p.Suffix)
}

// compiledBinary is the path and the name of the binary the compile step left
// for a platform. A library has none: ok is false.
func compiledBinary(ctx *pctx.Context, p pkgmeta.ArchivePlatform) (path, name string, ok bool, err error) {
	if ctx.Project.Type == typeLibrary {
		return "", "", false, nil
	}
	dir := binDir(ctx, p)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false, fmt.Errorf("no binary for %s: the compile step wrote none: %w", platformName(p), err)
	}
	if len(entries) != 1 {
		return "", "", false, fmt.Errorf("%s holds %d files, want the one binary", dir, len(entries))
	}
	return filepath.Join(dir, entries[0].Name()), entries[0].Name(), true, nil
}

// releaseVersion is the version the orchestrator computed for the project, in
// the form a Cargo manifest and an archive name accept.
func releaseVersion(ctx *pctx.Context) (string, error) {
	version := ""
	if ctx.Version != nil {
		version = ctx.Version.Full
		if version == "" {
			version = ctx.Version.Base
		}
	}
	if version == "" {
		return "", errors.New("the release version is unknown: run this task through putnami")
	}
	return strings.TrimPrefix(version, "v"), nil
}

func packageCompile(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == typeLibrary {
			emit.Info(ctx.Project.Name + " is a library: it has no binary to compile")
			return "OK", nil, nil
		}
		return stack.RootJob(func(c context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return compile(c, ctx, root, emit)
		})(ctx, emit, args)
	}
}

func compile(c context.Context, ctx *pctx.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
	platforms, err := compilePlatforms(ctx)
	if err != nil || len(platforms) == 0 {
		return err
	}
	targets := make([]string, 0, len(platforms))
	for _, p := range platforms {
		targets = append(targets, rustTargets[platformName(p)])
	}
	tools, err := provideCompiler(c, root.Expected, targets, root.Env, emit.Info)
	if err != nil {
		return err
	}
	binary, targetDir, err := crateBinary(c, tools, root)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "bin")); err != nil {
		return err
	}
	for i, p := range platforms {
		target := targets[i]
		zigTarget := target
		if p.GOOS == "linux" {
			zigTarget += linuxGlibc
		}
		emit.Info("compiling " + root.Component + " for " + platformName(p))
		cmd := exec.CommandContext(c, tools.zigbuild, "zigbuild", "--package", root.Component, "--bin", binary, "--locked", "--release", "--target", zigTarget) //nolint:gosec // the pinned tool, with a checked target
		cmd.Dir = root.Dir
		cmd.Env = tools.env
		if err := runForwarding(cmd, emit, nil); err != nil {
			return fmt.Errorf("cargo zigbuild for %s: %w", platformName(p), err)
		}
		file := pkgmeta.ExecutableName(p.GOOS, binary)
		dest := filepath.Join(binDir(ctx, p), file)
		if err := copyExecutable(filepath.Join(targetDir, target, "release", file), dest); err != nil {
			return err
		}
	}
	return nil
}

// crateBinary is the binary of the crate, and the build directory of its
// Cargo workspace.
func crateBinary(c context.Context, tools compiler, root stacks.RootStep) (string, string, error) {
	cmd := exec.CommandContext(c, tools.cargo, "metadata", "--no-deps", "--format-version", "1") //nolint:gosec // the pinned tool
	cmd.Dir = root.Dir
	cmd.Env = tools.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("cargo metadata: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var metadata struct {
		TargetDirectory string `json:"target_directory"`
		Packages        []struct {
			Name       string `json:"name"`
			DefaultRun string `json:"default_run"`
			Targets    []struct {
				Name string   `json:"name"`
				Kind []string `json:"kind"`
			} `json:"targets"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(out, &metadata); err != nil {
		return "", "", fmt.Errorf("cargo metadata: %w", err)
	}
	for _, pkg := range metadata.Packages {
		if pkg.Name != root.Component {
			continue
		}
		var bins []string
		for _, target := range pkg.Targets {
			if slices.Contains(target.Kind, "bin") {
				bins = append(bins, target.Name)
			}
		}
		switch {
		case pkg.DefaultRun != "":
			return pkg.DefaultRun, metadata.TargetDirectory, nil
		case len(bins) == 1:
			return bins[0], metadata.TargetDirectory, nil
		case slices.Contains(bins, pkg.Name):
			return pkg.Name, metadata.TargetDirectory, nil
		case len(bins) == 0:
			return "", "", fmt.Errorf("%s has no binary target to package", pkg.Name)
		default:
			return "", "", fmt.Errorf("%s has %d binaries (%s): state package.default-run in its Cargo.toml", pkg.Name, len(bins), strings.Join(bins, ", "))
		}
	}
	return "", "", fmt.Errorf("cargo metadata names no package %s", root.Component)
}

func copyExecutable(from, to string) (err error) {
	source, err := os.Open(from) //nolint:gosec // the path the build wrote
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	dest, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755) //nolint:gosec // an executable in the command output
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dest.Close()) }()
	_, err = io.Copy(dest, source)
	return err
}

// artifactName is a project name in the form of an archive name. The image
// name is stacks.ImageName, which is also lowercase.
func artifactName(project string) string {
	return strings.ReplaceAll(strings.TrimPrefix(project, "@"), "/", "-")
}

func packageArchives(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if ctx.Project.Type == typeLibrary {
		emit.Info(ctx.Project.Name + " is a library: it has no binary to archive")
		return "OK", nil, nil
	}
	version, err := releaseVersion(ctx)
	if err != nil {
		return "FAILED", nil, err
	}
	platforms, err := archivePlatforms(ctx)
	if err != nil {
		return "FAILED", nil, err
	}
	artifact := artifactName(ctx.Project.Name)
	dir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, channelArchives)
	if err := os.RemoveAll(dir); err != nil {
		return "FAILED", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "FAILED", nil, err
	}
	for _, p := range platforms {
		binary, name, ok, err := compiledBinary(ctx, p)
		if err != nil {
			return "FAILED", nil, err
		}
		if !ok {
			continue
		}
		archive := filepath.Join(dir, artifact+"-"+p.Suffix+".tar.gz")
		if err := writeArchive(archive, binary, name); err != nil {
			return "FAILED", nil, fmt.Errorf("write %s: %w", archive, err)
		}
		emit.Info("wrote " + archive)
	}
	channels := []string{channelArchives}
	if err := pkgmeta.WriteChannelRecord(dir, pkgmeta.ChannelRecord{Version: version, Artifact: artifact, Channels: channels}); err != nil {
		return "FAILED", nil, err
	}
	err = pkgmeta.WritePackageMetadata(ctx.WorkspaceRoot, ctx.Project.Path, pkgmeta.PackageMetadata{Version: version, Artifact: artifact, Channels: channels})
	if err != nil {
		return "FAILED", nil, err
	}
	return "OK", nil, nil
}

// writeArchive writes a tar.gz that holds the binary at its root. Dates and
// owners are fixed: one binary always gives the same archive.
func writeArchive(destination, binary, name string) (err error) {
	source, err := os.Open(binary) //nolint:gosec // the path the compile step wrote
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	file, err := os.Create(destination) //nolint:gosec // the task's own output
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	gz, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	tw := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tw.Close()) }()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: info.Size(), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err = io.Copy(tw, source)
	return err
}

func packageDocker(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	if ctx.Project.Type == typeLibrary {
		emit.Info(ctx.Project.Name + " is a library: it has no program to put in an image")
		return "OK", nil, nil
	}
	data, err := assembleImage(ctx, emit)
	if err != nil {
		return "FAILED", nil, err
	}
	return "OK", data, nil
}

func assembleImage(ctx *pctx.Context, emit *jsonl.Emitter) (map[string]any, error) {
	version, err := releaseVersion(ctx)
	if err != nil {
		return nil, err
	}
	platform, err := dockerPlatform(ctx)
	if err != nil {
		return nil, err
	}
	binary, _, _, err := compiledBinary(ctx, platform)
	if err != nil {
		return nil, err
	}
	base := ctx.Params.String("docker-base-image")
	if base == "" {
		base = imageBase
	}
	port := ctx.Params.Int("port", defaultPort)
	plan, err := oci.NewImagePlan(oci.Spec{
		BaseRef:      base,
		Platform:     platformName(platform),
		Layers:       []oci.Layer{{Files: []oci.File{{Source: binary, Path: "/app", Mode: 0o755}}}},
		Env:          []string{"PORT=" + strconv.Itoa(port)},
		Entrypoint:   []string{"/app"},
		ExposedPorts: []string{strconv.Itoa(port) + "/tcp"},
	}, oci.PlanOptions{})
	if err != nil {
		return nil, fmt.Errorf("plan the image: %w", err)
	}
	contentHash := plan.Identity[:12]
	dir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, channelDocker)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(dir, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base"),
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
	})
	if err != nil {
		return nil, fmt.Errorf("assemble the image: %w", err)
	}
	image := stacks.ImageName(ctx.Project.Name)
	content := image + ":c-" + contentHash
	tags := []string{content}
	if tag := ctx.Params.String("docker-tag"); tag != "" {
		tags = append(tags, image+":"+tag)
	}
	if ctx.Params.Bool("docker-load", false) {
		if err := loadImage(ctx, filepath.Join(dir, "oci"), content, tags[1:], emit); err != nil {
			return nil, err
		}
	}
	manifest, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: image, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: platformName(platform),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(manifest, '\n'), 0o644); err != nil { //nolint:gosec // the task's own output
		return nil, err
	}
	if err := pkgmeta.WriteChannelRecord(dir, pkgmeta.ChannelRecord{Version: version, Artifact: image, Channels: []string{channelDocker}}); err != nil {
		return nil, err
	}
	emit.Info("assembled " + content)
	return map[string]any{"image": content, "contentHash": contentHash, "digest": digest}, nil
}

// loadImage puts the image in the local docker daemon, with its other tags.
func loadImage(ctx *pctx.Context, layout, ref string, aliases []string, emit *jsonl.Emitter) error {
	tarball := filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, channelDocker), "load.tar")
	defer func() { _ = os.Remove(tarball) }()
	if err := oci.WriteDockerTarball(layout, ref, tarball); err != nil {
		return fmt.Errorf("write the docker tarball: %w", err)
	}
	background := context.Background()
	run := func(args ...string) error {
		timed, cancel := context.WithTimeout(background, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(timed, "docker", args...) //nolint:gosec // fixed verbs with the task's own tags
		if err := runForwarding(cmd, emit, nil); err != nil {
			return fmt.Errorf("docker %s: %w", args[0], err)
		}
		return nil
	}
	if err := run("load", "-i", tarball); err != nil {
		return err
	}
	for _, alias := range aliases {
		if err := run("tag", ref, alias); err != nil {
			return err
		}
	}
	return nil
}
