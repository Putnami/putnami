package java

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

const (
	// CodeImageFailed is the diagnostic of an image that cannot be assembled.
	CodeImageFailed = "java.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "java.base_not_pinned"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/app"
	imageUser        = "1000:1000"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
)

// jreBases are the Eclipse Temurin JRE images of the JDKs the extension pins,
// by Java major, pinned by the digest of their multi-platform index.
var jreBases = map[string]string{
	"17": "eclipse-temurin:17.0.21_9-jre@sha256:2a996a534890f1bee252fab23a0c01e773e9e1dc0e4700e8f0f6a2697c6cbf76",
	"21": "eclipse-temurin:21.0.13_9-jre@sha256:0b99b3376fbb50dbf9c0af8535fdd5b8eb0d3fdfc8b1e4967f383b6919f6901a",
	"25": "eclipse-temurin:25.0.5_8-jre@sha256:ecdc0adb37bde1a281c4d245a3cd535004b1fc8322d560f5b09463b248c81df0",
}

// PublishDockerJob pushes the packaged image through the shared publisher.
func PublishDockerJob() cli.JobFunc {
	return dockerpublish.Publish
}

// PackageDockerJob assembles the image of an application: its runnable form,
// built by its own build, on a digest-pinned Temurin JRE. It needs no Docker
// daemon. A library packages nothing.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == TypeLibrary {
			return "SKIP", nil, nil
		}
		if err := assembleImage(ctx, stack, cli.ParseFlags(args), emit); err != nil {
			return "FAILED", nil, err
		}
		return "OK", nil, nil
	}
}

func assembleImage(ctx *pctx.Context, stack stacks.Stack, flags map[string]string, emit *jsonl.Emitter) error {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	output := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "package", dockerChannel)
	name := stacks.ImageName(ctx.Project.Name)
	version := "0.0.0"
	if ctx.Version != nil && ctx.Version.Full != "" {
		version = ctx.Version.Full
	}
	record := func() error {
		return pkgmeta.WriteChannelRecord(output, pkgmeta.ChannelRecord{Version: version, Artifact: name, Channels: []string{dockerChannel}})
	}
	named := cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage"))
	if cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun")) {
		emit.Info("dry run: would assemble an image of " + name)
		if err := record(); err != nil {
			return fail(CodeImageFailed, err)
		}
		return nil
	}
	root, app, cleanup, err := application(ctx, stack, emit)
	defer cleanup()
	if err != nil {
		return err
	}
	base, err := baseImage(context.Background(), root, named)
	if err != nil {
		return fail(CodeBaseNotPinned, err)
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	spec, err := imageSpec(app, base, platform, defaultImagePort)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	plan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		return fail(CodeImageFailed, fmt.Errorf("plan the image: %w", err))
	}
	digest, err := oci.AssemblePlanToLayout(plan, filepath.Join(output, "oci"), oci.LayoutOptions{
		BaseCacheDir:  filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base"),
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
		if err := oci.LoadDocker(filepath.Join(output, "oci"), tags, emit); err != nil {
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
	emit.Info(fmt.Sprintf("assembled the image %s (%s) on %s", tags[0], digest, base))
	return nil
}

// javacVersion matches the major of javac -version: 21 of "javac 21.0.13",
// 8 of "javac 1.8.0_392".
var javacVersion = regexp.MustCompile(`javac (?:1\.)?([0-9]+)`)

// baseImage answers the named base, which must carry a digest, or the Temurin
// JRE of the Java major the task selected.
func baseImage(ctx context.Context, root stacks.RootStep, named string) (string, error) {
	if named != "" {
		if _, err := oci.ParseDigestReference(named); err != nil {
			return "", fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", named, err)
		}
		return named, nil
	}
	cmd := exec.CommandContext(ctx, root.Program("javac"), "-version") //nolint:gosec // the selected JDK
	cmd.Dir = root.Project
	cmd.Env = root.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read the Java version: %w", err)
	}
	m := javacVersion.FindStringSubmatch(out.String())
	if m == nil {
		return "", fmt.Errorf("javac -version printed no version: %q", strings.TrimSpace(out.String()))
	}
	base, ok := jreBases[m[1]]
	if !ok {
		return "", fmt.Errorf("no Temurin JRE image is pinned for Java %s: name a digest-pinned base with --docker-base-image", m[1])
	}
	return base, nil
}

// imageSpec describes the application's image: the runnable form under /app.
func imageSpec(app runnable, base, platform, port string) (oci.Spec, error) {
	var files []oci.File
	var entrypoint []string
	switch app.kind {
	case installDist:
		listed, err := imageFiles(app.path)
		if err != nil {
			return oci.Spec{}, err
		}
		files = listed
		entrypoint = []string{imageAppDir + "/bin/" + filepath.Base(app.command[0])}
	default:
		files = []oci.File{{Source: app.path, Path: imageAppDir + "/app.jar", Mode: 0o644}}
		// The base image's PATH names its java, a Temurin JRE or another base.
		entrypoint = []string{"java", "-jar", imageAppDir + "/app.jar"}
	}
	return oci.Spec{
		BaseRef:      base,
		Platform:     platform,
		Layers:       []oci.Layer{{Files: files}},
		Env:          []string{"PORT=" + port, "SERVER_PORT=" + port},
		Entrypoint:   entrypoint,
		WorkingDir:   imageAppDir,
		User:         imageUser,
		ExposedPorts: []string{port + "/tcp"},
	}, nil
}

// imageFiles lists the files of an installDist directory under /app, with
// their executable bit. A symbolic link is an error: the image's cache key
// does not cover what a link points to.
func imageFiles(dir string) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A Windows launcher has no use in a Linux image.
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".bat") {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link, which an image cannot hold", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, oci.File{Source: path, Path: imageAppDir + "/" + filepath.ToSlash(rel), Mode: mode})
		return nil
	})
	slices.SortFunc(files, func(a, b oci.File) int { return strings.Compare(a.Path, b.Path) })
	return files, err
}
