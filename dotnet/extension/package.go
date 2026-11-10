package dotnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/observe/detect/dotnet"
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
	CodeImageFailed = "dotnet.image_failed"
	// CodeBaseNotPinned is the diagnostic of a base image with no digest.
	CodeBaseNotPinned = "dotnet.base_not_pinned"
)

const (
	dockerChannel    = "docker"
	imageAppDir      = "/app"
	imageUser        = "app"
	defaultImagePort = "8080"
	defaultPlatform  = "linux/amd64"
)

// runtimeBases are the official .NET images, pinned by digest, by runtime
// version: aspnet for a web application, runtime for any other.
var runtimeBases = map[string]map[string]string{
	"aspnet": {
		"8.0":  "mcr.microsoft.com/dotnet/aspnet:8.0@sha256:b14a41431d252c640aae69b52320e512050fc53bafcb5012b97ccb9ca173c2d5",
		"9.0":  "mcr.microsoft.com/dotnet/aspnet:9.0@sha256:c5d96442d3fc5b9a85380922a174692c8a9899bc1efa5d1bf5a540dcd2079f5c",
		"10.0": "mcr.microsoft.com/dotnet/aspnet:10.0@sha256:1a48394b6f29d3a32c79d9edecd4abda0ecbe8776d567dbfc4590ed6979c740c",
	},
	"runtime": {
		"8.0":  "mcr.microsoft.com/dotnet/runtime:8.0@sha256:09f71a9459e260fe4bab0528f0bd9bfbfed7c8330bed3aa5b665281760205bec",
		"9.0":  "mcr.microsoft.com/dotnet/runtime:9.0@sha256:8e09d303d50457f544b0f14bc5f448cb0b218a7ba92d09cd3c079a382913f292",
		"10.0": "mcr.microsoft.com/dotnet/runtime:10.0@sha256:4f1f30cf0bb4ab0c8b2daa84613051683456f8df4a9bf7c812e20d74962992ac",
	},
}

// runtimeArchitectures are the .NET names of the image platforms.
var runtimeArchitectures = map[string]string{
	"linux/amd64": "linux-x64",
	"linux/arm64": "linux-arm64",
}

// PublishDockerJob pushes the packaged image through the shared publisher.
func PublishDockerJob() cli.JobFunc {
	return dockerpublish.Publish
}

// PackageDockerJob assembles the image of an application: the output of
// dotnet publish under /app, on a digest-pinned official .NET image. It needs
// no Docker daemon.
func PackageDockerJob(stack stacks.Stack) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
		if ctx.Project.Type == dotnet.TypeLibrary {
			return "SKIP", nil, nil
		}
		manifest, err := projectFile(ctx, emit)
		if err != nil {
			return "FAILED", nil, err
		}
		flags := cli.ParseFlags(args)
		// dotnet publish restores and builds the referenced projects in
		// place: it runs under the stack root's lock, as every .NET task.
		return stack.RootJob(func(run context.Context, root stacks.RootStep, emit *jsonl.Emitter) error {
			return assembleImage(run, ctx, root, manifest, flags, emit)
		})(ctx, emit, args)
	}
}

// projectProperties are the evaluated MSBuild properties an image needs.
type projectProperties struct {
	TargetFramework  string
	TargetFrameworks string
	AssemblyName     string
	Web              bool
}

func assembleImage(run context.Context, ctx *pctx.Context, root stacks.RootStep, manifest string, flags map[string]string, emit *jsonl.Emitter) error {
	fail := func(code string, err error) error {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, code)
		return fmt.Errorf("%s: %w", code, err)
	}
	platform := cli.FlagString(flags, "platform", ctx.Params.String("platform"))
	if platform == "" {
		platform = defaultPlatform
	}
	rid, ok := runtimeArchitectures[platform]
	if !ok {
		return fail(CodeImageFailed, fmt.Errorf("an image targets linux/amd64 or linux/arm64, not %s", platform))
	}
	props, err := readProperties(run, root, manifest)
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	framework, err := targetFramework(props, cli.FlagString(flags, "framework", ctx.Params.String("framework")))
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	named := cli.FlagString(flags, "docker-base-image", ctx.Params.String("docker-base-image", "dockerBaseImage"))
	base, err := baseImage(props.Web, framework, named)
	if err != nil {
		return fail(CodeBaseNotPinned, err)
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
	if cli.FlagBool(flags, "dry-run", ctx.Params.Bool("dry-run", false, "dryRun")) {
		message := "dry run: would assemble an image of " + name + " on " + base
		if named == "" && !props.Web {
			message += ", or on the aspnet image if the published application needs ASP.NET Core"
		}
		emit.Info(message)
		if err := record(); err != nil {
			return fail(CodeImageFailed, err)
		}
		return nil
	}
	staging, err := os.MkdirTemp("", "putnami-dotnet-image-")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	published := filepath.Join(staging, "app")
	argv := []string{"publish", manifest, "--configuration", "Release", "--framework", framework, "--runtime", rid, "--self-contained", "false", "-p:UseAppHost=false", "--output", published}
	emit.Info("$ dotnet " + strings.Join(argv, " "))
	cmd := exec.CommandContext(run, root.Program("dotnet"), argv...) //nolint:gosec // the selected SDK
	cmd.Dir = root.Project
	cmd.Env = quietEnv(root.Env)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(CodeImageFailed, fmt.Errorf("dotnet publish: %w", err))
	}
	// The frameworks the published application runs on decide the base.
	if props.Web, err = needsASPNET(published, props.AssemblyName); err != nil {
		return fail(CodeImageFailed, err)
	}
	if base, err = baseImage(props.Web, framework, named); err != nil {
		return fail(CodeBaseNotPinned, err)
	}
	spec, err := imageSpec(published, props, base, platform)
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
		if err := loadImage(filepath.Join(output, "oci"), tags); err != nil {
			return fail(CodeImageFailed, err)
		}
	}
	manifestDoc, err := json.MarshalIndent(pkgmeta.DockerManifest{
		Image: name, Tags: tags, Version: version, ContentHash: contentHash, Digest: digest, Layout: "oci", Platform: spec.Platform,
	}, "", "  ")
	if err != nil {
		return fail(CodeImageFailed, err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), append(manifestDoc, '\n'), 0o644); err != nil { //nolint:gosec // an output of the job
		return fail(CodeImageFailed, err)
	}
	if err := record(); err != nil {
		return fail(CodeImageFailed, err)
	}
	emit.Info(fmt.Sprintf("assembled the image %s (%s)", tags[0], digest))
	return nil
}

// readProperties evaluates the project's target frameworks, assembly name
// and web SDK with MSBuild.
func readProperties(run context.Context, root stacks.RootStep, manifest string) (projectProperties, error) {
	cmd := exec.CommandContext(run, root.Program("dotnet"), "msbuild", manifest, //nolint:gosec // the selected SDK
		"-getProperty:TargetFramework", "-getProperty:TargetFrameworks", "-getProperty:AssemblyName", "-getProperty:UsingMicrosoftNETSdkWeb")
	cmd.Dir = root.Project
	cmd.Env = quietEnv(root.Env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return projectProperties{}, fmt.Errorf("read the properties of %s: %w: %s", manifest, err, strings.TrimSpace(stderr.String()))
	}
	// A first run of an SDK may print a banner before the document.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var doc struct {
		Properties map[string]string `json:"Properties"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		return projectProperties{}, fmt.Errorf("read the properties of %s: %w", manifest, err)
	}
	p := doc.Properties
	return projectProperties{
		TargetFramework:  strings.TrimSpace(p["TargetFramework"]),
		TargetFrameworks: strings.TrimSpace(p["TargetFrameworks"]),
		AssemblyName:     strings.TrimSpace(p["AssemblyName"]),
		Web:              strings.EqualFold(strings.TrimSpace(p["UsingMicrosoftNETSdkWeb"]), "true"),
	}, nil
}

// quietEnv is env with the SDK's first-run banner and telemetry off.
func quietEnv(env []string) []string {
	return append(slices.Clip(env), "DOTNET_NOLOGO=1", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1")
}

// needsASPNET reports whether the published application runs on ASP.NET
// Core, from the frameworks its runtimeconfig.json names. A Windows desktop
// framework has no Linux image.
func needsASPNET(published, assembly string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(published, assembly+".runtimeconfig.json")) //nolint:gosec // an output of dotnet publish
	if err != nil {
		return false, fmt.Errorf("dotnet publish wrote no %s.runtimeconfig.json: %w", assembly, err)
	}
	var doc struct {
		RuntimeOptions struct {
			Framework  *struct{ Name string }  `json:"framework"`
			Frameworks []struct{ Name string } `json:"frameworks"`
		} `json:"runtimeOptions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("%s.runtimeconfig.json: %w", assembly, err)
	}
	frameworks := doc.RuntimeOptions.Frameworks
	if f := doc.RuntimeOptions.Framework; f != nil {
		frameworks = append(frameworks, *f)
	}
	web := false
	for _, f := range frameworks {
		switch f.Name {
		case "Microsoft.AspNetCore.App":
			web = true
		case "Microsoft.WindowsDesktop.App":
			return false, errors.New("the application runs on the Windows desktop framework, which has no Linux image")
		}
	}
	return web, nil
}

// targetFramework is the framework an image runs: the framework flag, else
// the project's single target framework.
func targetFramework(p projectProperties, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if p.TargetFramework != "" {
		return p.TargetFramework, nil
	}
	var frameworks []string
	for _, f := range strings.Split(p.TargetFrameworks, ";") {
		if f = strings.TrimSpace(f); f != "" {
			frameworks = append(frameworks, f)
		}
	}
	switch len(frameworks) {
	case 1:
		return frameworks[0], nil
	case 0:
		return "", errors.New("the project states no target framework")
	}
	return "", fmt.Errorf("the project targets %s: name the one the image runs with --framework", strings.Join(frameworks, ", "))
}

// baseImage answers the named base, which must carry a digest, or the
// official image of the framework's runtime.
func baseImage(web bool, framework, named string) (string, error) {
	if named != "" {
		if _, err := oci.ParseDigestReference(named); err != nil {
			return "", fmt.Errorf("the base image %s has no valid digest: name it as image@sha256:<digest>: %w", named, err)
		}
		return named, nil
	}
	version, ok := strings.CutPrefix(framework, "net")
	if !ok || strings.Contains(version, "-") || !strings.Contains(version, ".") {
		return "", fmt.Errorf("no official image is pinned for %s: name a digest-pinned base with --docker-base-image", framework)
	}
	kind := "runtime"
	if web {
		kind = "aspnet"
	}
	base, ok := runtimeBases[kind][version]
	if !ok {
		return "", fmt.Errorf("no official %s image is pinned for .NET %s: name a digest-pinned base with --docker-base-image", kind, version)
	}
	return base, nil
}

// imageSpec describes the application's image: the published files of its
// dependencies, then its own, under /app.
func imageSpec(published string, p projectProperties, base, platform string) (oci.Spec, error) {
	own := func(rel string) bool {
		name := filepath.Base(rel)
		return strings.HasPrefix(name, p.AssemblyName+".") || strings.HasPrefix(name, "appsettings") ||
			name == "web.config" || strings.HasPrefix(rel, "wwwroot"+string(filepath.Separator))
	}
	// The SDK copies a web project's JSON files into its output: Putnami's
	// project file and the SDK pin are no files of the application.
	shipped := func(rel string) bool {
		rel = filepath.ToSlash(rel)
		return rel != "putnami.json" && rel != "global.json" && rel != "schema/http-routes.json"
	}
	dependencies, err := imageFiles(published, func(rel string) bool { return shipped(rel) && !own(rel) })
	if err != nil {
		return oci.Spec{}, err
	}
	files, err := imageFiles(published, func(rel string) bool { return shipped(rel) && own(rel) })
	if err != nil {
		return oci.Spec{}, err
	}
	if p.AssemblyName == "" {
		return oci.Spec{}, errors.New("the project states no assembly name")
	}
	if _, err := os.Stat(filepath.Join(published, p.AssemblyName+".dll")); err != nil {
		return oci.Spec{}, fmt.Errorf("dotnet publish wrote no %s.dll", p.AssemblyName)
	}
	spec := oci.Spec{
		BaseRef:    base,
		Platform:   platform,
		Layers:     []oci.Layer{{Files: dependencies}, {Files: files}},
		Entrypoint: []string{"dotnet", imageAppDir + "/" + p.AssemblyName + ".dll"},
		WorkingDir: imageAppDir,
		User:       imageUser,
	}
	if p.Web {
		spec.Env = []string{"ASPNETCORE_HTTP_PORTS=" + defaultImagePort, "PORT=" + defaultImagePort}
		spec.ExposedPorts = []string{defaultImagePort + "/tcp"}
	}
	return spec, nil
}

// imageFiles lists the published files whose path keep accepts.
func imageFiles(published string, keep func(rel string) bool) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(published, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(published, path)
		if err != nil || rel == "." || entry.IsDir() || !keep(rel) {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a link to a directory, which an image cannot hold", rel)
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, oci.File{Source: path, Path: imageAppDir + "/" + filepath.ToSlash(rel), Mode: mode})
		return nil
	})
	return files, err
}

// loadImage loads the assembled image into the local Docker daemon under its
// tags.
func loadImage(layout string, tags []string) error {
	archive, err := os.CreateTemp("", "putnami-dotnet-image-*.tar")
	if err != nil {
		return err
	}
	_ = archive.Close()
	defer func() { _ = os.Remove(archive.Name()) }()
	if err := oci.WriteDockerTarball(layout, tags[0], archive.Name()); err != nil {
		return err
	}
	load := exec.Command("docker", "load", "-i", archive.Name()) //nolint:gosec // fixed program, a path from CreateTemp
	load.Stderr = os.Stderr
	if err := load.Run(); err != nil {
		return errors.New("docker load failed for " + tags[0])
	}
	for _, alias := range tags[1:] {
		tag := exec.Command("docker", "tag", tags[0], alias) //nolint:gosec // fixed program
		tag.Stderr = os.Stderr
		if err := tag.Run(); err != nil {
			return errors.New("docker tag failed for " + alias)
		}
	}
	return nil
}
