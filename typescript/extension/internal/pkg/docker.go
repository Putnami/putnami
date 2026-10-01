package pkg

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/project"
)

// dockerBaseImagePinned is the digest-pinned distroless base (cc variant:
// bun-compiled binaries need glibc and libstdc++). The pin — not the moving
// :nonroot tag — is what makes assembled digests reproducible across
// machines; bumping it (for base CVE fixes) is an explicit, reviewable change
// that re-addresses every image. Override per workspace with
// --docker-base-image (the override must also be digest-pinned).
const dockerBaseImagePinned = "gcr.io/distroless/cc-debian12:nonroot@sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985"

// DockerParams holds docker packaging parameters.
type DockerParams struct {
	Tag                   string
	Platform              string
	Port                  int
	WorkspaceBuilderImage string // accepted for flag compatibility; unused (Go-only feature)
	BaseImage             string
	BaseLayout            string
	BaseDigest            string
	Load                  bool
	DryRun                bool
}

// PackageDocker assembles a Docker image from the compile output — in
// process, deterministically, and without a docker daemon. The image is
// written as a self-contained OCI layout; publish pushes it from there.
func PackageDocker(workspaceRoot, projectName, projectPath string, params DockerParams, versionInfo *git.VersionInfo, wsVersion string) (map[string]any, error) {
	compileOutput := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "build", "compile")
	if !project.FileExists(compileOutput) {
		compileOutput = filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package", "compile")
	}
	if !project.FileExists(compileOutput) {
		return nil, fmt.Errorf("compile output not found for %s", projectName)
	}

	// Resolve target platform (default: linux/amd64)
	platform := params.Platform
	if platform == "" {
		platform = "linux/amd64"
	}

	// Find the correct binary for the target platform
	binaryName, err := findBinaryForPlatform(compileOutput, platform)
	if err != nil {
		return nil, fmt.Errorf("no binary found for %s: %w", projectName, err)
	}

	// Copy .gen/public into the compile output so it ships with the image. When
	// the project declares generate assets that target public/ (e.g. a docs site
	// staging docs under .gen/public/docs), .gen/public is a required output of
	// the generate step — a missing one at package time means that output was lost
	// (a generate cache hit that did not rematerialize .gen, a skipped generate),
	// so fail loudly instead of silently shipping an asset-less image that 404s in
	// production.
	hasAssets := false
	genPublicSrc := filepath.Join(workspaceRoot, projectPath, ".gen", "public")
	if project.FileExists(genPublicSrc) {
		genPublicDst := filepath.Join(compileOutput, ".gen", "public")
		if err := build.CopyDir(genPublicSrc, genPublicDst); err != nil {
			return nil, fmt.Errorf("failed to copy .gen/public: %w", err)
		}
		hasAssets = true
	} else if projectExpectsPublicAssets(filepath.Join(workspaceRoot, projectPath)) {
		return nil, fmt.Errorf("%s declares generate assets under public/ but %s is missing — the generate output was not materialized; re-run with --no-cache to repair a poisoned generate cache entry", projectName, genPublicSrc)
	}

	// Compute the publish version from workspace version + git suffix. It is
	// advisory only: the image identity is the content hash, and publish
	// derives the release id from its own session.
	publishVersion := resolveDockerVersion(wsVersion, versionInfo)

	baseRef := params.BaseImage
	if baseRef == "" {
		baseRef = dockerBaseImagePinned
	}
	stampPath := filepath.Join(compileOutput, contentStampFile)
	spec, err := dockerSpec(baseRef, platform, compileOutput, binaryName, projectName, stampPath, params.Port, hasAssets)
	if err != nil {
		return nil, err
	}

	// Plan everything that determines the image content once. The stamp's
	// recipe is part of the identity, while its bytes are bound after the hash
	// exists and verified as they flow into the layer.
	imagePlan, err := oci.NewImagePlan(spec, oci.PlanOptions{DerivedFiles: map[string]string{
		stampImagePath: contentStampPlanIdentity + ":" + projectName,
	}})
	if err != nil {
		return nil, fmt.Errorf("planning docker content: %w", err)
	}
	fullHash := imagePlan.Identity
	contentHash := fullHash[:12]

	// Stage the content stamp. It derives from the content hash, so identical
	// inputs produce a byte-identical image and the digest can travel across
	// commits.
	if err := writeContentStamp(stampPath, projectName, contentHash); err != nil {
		return nil, fmt.Errorf("writing content stamp: %w", err)
	}

	// The image is tagged with the content-addressed immutable tag; publish
	// assigns the session version and channels in the registry. An explicit
	// --docker-tag is applied as an extra local alias.
	// Flatten the project name into a single OCI repository path component:
	// drop the npm-scope "@" and replace "/" with "-". A multi-segment name
	// (e.g. "surfaces/admin/workloads/console") would otherwise push to a
	// multi-segment OCI repo path the registry does not route, and the deploy
	// side resolves images by the dashified name. Mirrors the Go extension.
	imageName := strings.TrimPrefix(projectName, "@")
	imageName = strings.ReplaceAll(imageName, "/", "-")
	fullTag := imageName + ":" + contentTagFor(contentHash)

	if params.DryRun {
		return map[string]any{
			"image":       fullTag,
			"contentHash": contentHash,
			"dryRun":      true,
		}, nil
	}

	dockerOutputDir := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package", "docker")
	layoutDir := filepath.Join(dockerOutputDir, "oci")
	baseCacheDir := filepath.Join(workspaceRoot, ".putnami", "cache", "oci-base")
	digest, err := oci.AssemblePlanToLayout(imagePlan, layoutDir, oci.LayoutOptions{
		BaseCacheDir:  baseCacheDir,
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
		BaseLayout:    params.BaseLayout,
		BaseDigest:    params.BaseDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("assembling docker image: %w", err)
	}

	manifestTags := []string{fullTag}
	if params.Tag != "" {
		manifestTags = append(manifestTags, imageName+":"+params.Tag)
	}
	if params.Load {
		if err := loadIntoDaemon(layoutDir, fullTag, manifestTags[1:]); err != nil {
			return nil, err
		}
	}

	if err := WritePackageMetadata(dockerOutputDir, "docker", publishVersion, false); err != nil {
		return nil, fmt.Errorf("writing package metadata: %w", err)
	}

	// Write docker manifest for the publish step. The version is advisory
	// (publish derives the release id from its session); the content hash and
	// digest are the identities publish uses to decide whether the registry
	// already has this image.
	manifest := map[string]any{
		"image":       imageName,
		"version":     publishVersion,
		"contentHash": contentHash,
		"digest":      digest,
		"layout":      "oci",
		"tags":        manifestTags,
		"platform":    platform,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling docker manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dockerOutputDir, "manifest.json"), append(manifestData, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("writing docker manifest: %w", err)
	}

	return map[string]any{
		"image":       fullTag,
		"contentHash": contentHash,
		"digest":      digest,
	}, nil
}

// stampImagePath is where the content stamp lands inside the image —
// /app/.gen/version.json, where @putnami/utils getBuildInfo() resolves it at
// runtime (PWD=/app).
const stampImagePath = "/app/.gen/version.json"

// contentStampPlanIdentity versions the deterministic recipe whose output
// embeds the image plan hash. The project name is appended by the caller
// because it also shapes the staged bytes.
const contentStampPlanIdentity = "putnami/typescript/content-stamp/v1"

// dockerSpec describes the COPY-only TS service image: the bun-compiled
// binary, the staged .gen assets, and the content stamp — with nothing
// git-derived anywhere. The release id is injected as environment at deploy
// time and getBuildInfo() overlays it on the stamp. User and base env are
// inherited from the distroless :nonroot base.
func dockerSpec(baseRef, platform, compileOutput, binaryName, appName, stampPath string, port int, hasAssets bool) (oci.Spec, error) {
	if port <= 0 {
		port = 3000
	}
	layers := []oci.Layer{
		{Files: []oci.File{{Source: filepath.Join(compileOutput, binaryName), Path: "/app/" + appName, Mode: 0o755}}},
	}
	if hasAssets {
		assets, err := assetFiles(filepath.Join(compileOutput, ".gen"))
		if err != nil {
			return oci.Spec{}, fmt.Errorf("staging assets: %w", err)
		}
		layers = append(layers, oci.Layer{Files: assets})
	}
	layers = append(layers, oci.Layer{Files: []oci.File{{Source: stampPath, Path: stampImagePath, Mode: 0o644}}})

	return oci.Spec{
		BaseRef:  baseRef,
		Platform: platform,
		Layers:   layers,
		Env: []string{
			"NODE_ENV=production",
			fmt.Sprintf("PORT=%d", port),
			"PWD=/app",
		},
		Entrypoint:   []string{"/app/" + appName},
		WorkingDir:   "/app",
		ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
	}, nil
}

// assetFiles lists the staged .gen tree as image files under /app/.gen,
// mirroring the docker-build era `COPY .gen /app/.gen`.
func assetFiles(genDir string) ([]oci.File, error) {
	var files []oci.File
	err := filepath.WalkDir(genDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(genDir, p)
		if err != nil {
			return err
		}
		files = append(files, oci.File{
			Source: p,
			Path:   path.Join("/app/.gen", filepath.ToSlash(rel)),
			Mode:   0o644,
		})
		return nil
	})
	return files, err
}

// projectExpectsPublicAssets reports whether the project's generate.assets
// declare any target under public/ — the signal that the packaged image is meant
// to ship public/ assets (docs, static files staged into .gen/public during
// generate). It is what turns a missing .gen/public at package time into a hard
// error rather than a silently asset-less image; projects with no such
// declaration are unaffected.
func projectExpectsPublicAssets(projectDir string) bool {
	rcPath := filepath.Join(projectDir, "putnami.json")
	if !project.FileExists(rcPath) {
		rcPath = filepath.Join(projectDir, ".putnamirc.json")
	}
	for _, a := range project.GetGenerateAssets(rcPath) {
		to := filepath.ToSlash(filepath.Clean(a.To))
		if to == "public" || strings.HasPrefix(to, "public/") {
			return true
		}
	}
	return false
}

// loadIntoDaemon writes the assembled image as a docker tarball and loads it
// into the local daemon (opt-in via --docker-load) so it can be `docker run`
// immediately; extra alias tags are applied after the load.
func loadIntoDaemon(layoutDir, contentRef string, aliases []string) error {
	tarFile, err := os.CreateTemp("", "putnami-docker-load-*.tar")
	if err != nil {
		return fmt.Errorf("staging docker tarball: %w", err)
	}
	tarPath := tarFile.Name()
	tarFile.Close()
	defer os.Remove(tarPath)

	if err := oci.WriteDockerTarball(layoutDir, contentRef, tarPath); err != nil {
		return fmt.Errorf("writing docker tarball: %w", err)
	}
	// `docker load` can move a large image into the daemon, so give it a
	// generous bounded deadline; without it a wedged daemon would hang the job
	// (main wires no global context into the dispatcher).
	result, err := exec.Run("docker", []string{"load", "-i", tarPath}, exec.Timeout(5*time.Minute))
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("docker load failed: %s", strings.TrimSpace(result.Stderr))
	}
	for _, alias := range aliases {
		// `docker tag` is a fast metadata op; a short bounded timeout is enough.
		result, err := exec.Run("docker", []string{"tag", contentRef, alias}, exec.Timeout(30*time.Second))
		if err != nil {
			return err
		}
		if !result.Success {
			return fmt.Errorf("docker tag %s failed: %s", alias, strings.TrimSpace(result.Stderr))
		}
	}
	return nil
}

// findBinaryForPlatform locates the compiled binary matching the Docker platform.
// Platform format is "os/arch" (e.g. "linux/amd64"). If empty, defaults to "linux/amd64".
//
// The suffix comes from the SAME mapping the compile step targets: since the docker channel now compiles that one target and no
// others, the name searched for here and the name written there must be derived
// once, not spelled twice.
func findBinaryForPlatform(compileOutput, platform string) (string, error) {
	target, err := build.ImageCompileTarget(platform)
	if err != nil {
		return "", err
	}
	suffix := build.TargetSuffix(target)

	entries, err := os.ReadDir(compileOutput)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no binary with suffix %q found in %s", suffix, compileOutput)
}

// resolveDockerVersion computes the Docker image version from workspace version
// and git info, matching the npm version scheme (e.g. "0.1.0-abc1234-d5e6f7a").
func resolveDockerVersion(wsVersion string, versionInfo *git.VersionInfo) string {
	base := wsVersion
	if base == "" {
		base = "0.0.0"
	}
	if versionInfo != nil {
		suffix := versionInfo.Suffix
		if suffix == "" {
			suffix = versionInfo.SHA
		}
		if suffix != "" {
			return base + "-" + suffix
		}
	}
	return base
}

// contentStampFile is the staging name of the build-info stamp inside the
// compile output.
const contentStampFile = "putnami-buildinfo.json"

// contentStamp is the build-info subset baked into the image. It carries no
// git-derived bytes — no version, revision, or branch — so identical content
// inputs always produce a byte-identical image and the digest can travel
// across commits. The release id is injected as environment at deploy time
// and getBuildInfo() overlays it on this stamp.
type contentStamp struct {
	Name        string `json:"name"`
	ContentHash string `json:"contentHash"`
}

// writeContentStamp stages the content stamp file for the image assembly.
func writeContentStamp(path, projectName, contentHash string) error {
	data, err := json.MarshalIndent(contentStamp{Name: projectName, ContentHash: contentHash}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// contentTagFor renders the content-addressed immutable image tag.
func contentTagFor(contentHash string) string {
	return "c-" + contentHash
}
