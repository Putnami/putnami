package pkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/go/extension/internal/platform"
)

// dockerBaseImagePinned is the digest-pinned distroless base. The pin — not
// the moving :nonroot tag — is what makes assembled digests reproducible
// across machines; bumping it (for base CVE fixes) is an explicit, reviewable
// change that re-addresses every image. Override per workspace with
// --docker-base-image (the override must also be digest-pinned).
const dockerBaseImagePinned = "gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240"

const dockerBuilderBaseImage = "gcr.io/distroless/static:nonroot"

// dockerParams carries the docker channel inputs resolved by the package job.
type dockerParams struct {
	Version               string
	Registry              string
	Tag                   string
	Platform              string
	WorkspaceBuilderImage string
	BaseImage             string
	BaseLayout            string
	BaseDigest            string
	Port                  string
	Load                  bool
	DryRun                bool
}

// dockerSpec describes the COPY-only Go service image: the binary at /app,
// PORT in the environment, and nothing git-derived anywhere. User and base
// env are inherited from the distroless :nonroot base. The binary
// self-reports its version via version-var ldflags / runtime/debug.ReadBuildInfo;
// the release id is injected as environment at deploy time.
func dockerSpec(baseRef, dockerPlatform, binaryAbsPath, port string) oci.Spec {
	return oci.Spec{
		BaseRef:    baseRef,
		Platform:   dockerPlatform,
		Layers:     []oci.Layer{{Files: []oci.File{{Source: binaryAbsPath, Path: "/app", Mode: 0o755}}}},
		Env:        []string{"PORT=" + port},
		Entrypoint: []string{"/app"},
	}
}

func buildDockerImage(ctx *pctx.Context, emit *jsonl.Emitter, params dockerParams, outputRoot string) bool {
	emit.PhaseStart("package-docker")

	entrypoint := platform.ReadGoEntrypoint(ctx.Project.FullPath)
	binaryNameVal := platform.DeriveBinaryName(entrypoint, ctx.Project.Name)

	// Resolve binary from cross-compile output (platform-specific subdirectory).
	// If a cmd/ layout makes the binary name differ from the project name, scan
	// that same platform directory for the emitted executable.
	platformSuffix := platform.SuffixFromDockerPlatform(params.Platform)
	binaryAbsPath := filepath.Join(ctx.OutputPath, "bin", platformSuffix, binaryNameVal)
	if _, err := os.Stat(binaryAbsPath); err != nil {
		binaryAbsPath = ""
		platformBinDir := filepath.Join(ctx.OutputPath, "bin", platformSuffix)
		if entries, dirErr := os.ReadDir(platformBinDir); dirErr == nil {
			for _, e := range entries {
				if !e.IsDir() {
					binaryAbsPath = filepath.Join(platformBinDir, e.Name())
					break
				}
			}
		}
		if binaryAbsPath == "" {
			emit.Diagnostic("error", "Go binary not found — run the cross-compile step first", "", 0)
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
	}

	// Refuse to ship a binary that embeds a different version than this run's.
	if expected := expectedEmbeddedVersion(ctx, params.Version); expected != "" {
		if err := verifyBinaryVersion(binaryAbsPath, expected); err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
	}

	imageName := strings.TrimPrefix(ctx.Project.Name, "@")
	imageName = strings.ReplaceAll(imageName, "/", "-")
	// Package owns only an execution-local candidate. Registry qualification is
	// publish identity and must not enter package output bytes.
	qualifiedImage := imageName

	// A workspace builder image means the binary must come from another
	// image at build time — only docker build can express that. Everything
	// else is assembled in-process, deterministically and daemon-free.
	if params.WorkspaceBuilderImage != "" {
		if err := validateDockerComposition(params); err != nil {
			emit.Diagnostic("error", err.Error(), "", 0)
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
		return buildDockerImageWithBuilder(ctx, emit, params, imageName, qualifiedImage, binaryAbsPath, outputRoot)
	}

	baseRef := params.BaseImage
	if baseRef == "" {
		baseRef = dockerBaseImagePinned
	}
	spec := dockerSpec(baseRef, params.Platform, binaryAbsPath, params.Port)

	imagePlan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		emit.Diagnostic("error", "Failed to plan docker content: "+err.Error(), "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}
	fullHash := imagePlan.Identity
	contentHash := fullHash[:12]
	fullImage := qualifiedImage + ":c-" + contentHash

	dockerOutputDir := filepath.Join(outputRoot, "docker")

	if params.DryRun {
		emit.Log("info", "Dry run: would assemble Docker image "+fullImage)
		if !recordDockerChannel(emit, dockerOutputDir, imageName, params.Version) {
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
		emit.PhaseEnd("package-docker", "success")
		return true
	}

	layoutDir := filepath.Join(dockerOutputDir, "oci")
	baseCacheDir := filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base")
	digest, err := oci.AssemblePlanToLayout(imagePlan, layoutDir, oci.LayoutOptions{
		BaseCacheDir:  baseCacheDir,
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
		BaseLayout:    params.BaseLayout,
		BaseDigest:    params.BaseDigest,
	})
	if err != nil {
		emit.Diagnostic("error", "Docker image assembly failed for "+fullImage+": "+err.Error(), "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}

	manifestTags := []string{fullImage}
	if params.Tag != "" {
		manifestTags = append(manifestTags, qualifiedImage+":"+params.Tag)
	}
	if params.Load {
		if !loadIntoDaemon(emit, layoutDir, fullImage, manifestTags[1:]) {
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
	}

	if !writeDockerManifest(emit, dockerOutputDir, map[string]any{
		"image":       imageName,
		"version":     params.Version,
		"contentHash": contentHash,
		"digest":      digest,
		"layout":      "oci",
		"tags":        manifestTags,
		"platform":    params.Platform,
	}) {
		return false
	}

	emit.Log("info", fmt.Sprintf("Assembled Docker image %s (%s)", fullImage, digest))
	emit.PhaseEnd("package-docker", "success")
	return true
}

func validateDockerComposition(params dockerParams) error {
	if params.WorkspaceBuilderImage != "" && (params.BaseLayout != "" || params.BaseDigest != "") {
		return fmt.Errorf("workspace-builder-image cannot compose a dockerBaseProject local OCI candidate; use native local OCI assembly or remove one of these settings")
	}
	return nil
}

// buildDockerImageWithBuilder is the docker-build escape hatch for multi-stage
// workspace-builder images. It cannot produce reproducible digests (docker
// build stamps real times into layer tars and config), so digest travel for
// these images only holds within a cache lineage.
func buildDockerImageWithBuilder(ctx *pctx.Context, emit *jsonl.Emitter, params dockerParams, imageName, qualifiedImage, binaryAbsPath, outputRoot string) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		emit.Diagnostic("error", "docker CLI is required", "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}

	// Compute the binary path relative to workspace root for the Dockerfile COPY.
	binaryRelPath, _ := filepath.Rel(ctx.WorkspaceRoot, binaryAbsPath)
	dockerfileContent := dockerfileFor(params.WorkspaceBuilderImage, binaryRelPath, params.Port)

	contentHash, err := dockerContentHash(dockerfileContent, binaryAbsPath)
	if err != nil {
		emit.Diagnostic("error", "Failed to hash docker content: "+err.Error(), "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}
	fullImage := qualifiedImage + ":c-" + contentHash

	if params.DryRun {
		emit.Log("info", "Dry run: would build Docker image "+fullImage)
		if !recordDockerChannel(emit, filepath.Join(outputRoot, "docker"), imageName, params.Version) {
			emit.PhaseEnd("package-docker", "failed")
			return false
		}
		emit.PhaseEnd("package-docker", "success")
		return true
	}

	tmpDir, _ := os.MkdirTemp("", "putnami-go-docker-")
	defer os.RemoveAll(tmpDir)

	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0o644); err != nil {
		emit.Diagnostic("error", "Failed to write Dockerfile: "+err.Error(), "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}

	// Build image (no push)
	buildArgs := []string{"build", "-t", fullImage}
	manifestTags := []string{fullImage}
	if params.Tag != "" {
		aliasTag := qualifiedImage + ":" + params.Tag
		buildArgs = append(buildArgs, "-t", aliasTag)
		manifestTags = append(manifestTags, aliasTag)
	}
	buildArgs = append(buildArgs, "-f", dockerfilePath, "--platform", params.Platform)
	buildArgs = append(buildArgs, "--build-arg", "WORKSPACE_BUILDER_IMAGE="+params.WorkspaceBuilderImage)
	buildArgs = append(buildArgs, ctx.WorkspaceRoot)

	buildCmd := exec.Command("docker", buildArgs...)
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		emit.Diagnostic("error", "Docker build failed for "+fullImage, "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}

	// Export the daemon-built image into the same self-contained local contract
	// as the native assembler. That makes a package cache restore sufficient for
	// publish on another runner; publish never has to rediscover this daemon.
	dockerOutputDir := filepath.Join(outputRoot, "docker")
	layoutDir := filepath.Join(dockerOutputDir, "oci")
	digest, err := oci.ExportDaemonImageToLayout(fullImage, layoutDir)
	if err != nil {
		emit.Diagnostic("error", "Failed to write local OCI candidate: "+err.Error(), "", 0)
		emit.PhaseEnd("package-docker", "failed")
		return false
	}
	if !writeDockerManifest(emit, dockerOutputDir, map[string]any{
		"image":       imageName,
		"version":     params.Version,
		"contentHash": contentHash,
		"digest":      digest,
		"layout":      "oci",
		"tags":        manifestTags,
		"platform":    params.Platform,
	}) {
		return false
	}

	emit.Log("info", "Built Docker image: "+fullImage)
	emit.PhaseEnd("package-docker", "success")
	return true
}

// writeDockerManifest writes the docker manifest for the publish step. The
// version is advisory (publish derives the release id from its session); the
// content hash and digest are the identities publish uses to decide whether
// the registry already has this image.
func writeDockerManifest(emit *jsonl.Emitter, dockerOutputDir string, manifest map[string]any) bool {
	os.MkdirAll(dockerOutputDir, 0o755)
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dockerOutputDir, "manifest.json"), append(manifestData, '\n'), 0o644); err != nil {
		emit.Diagnostic("warning", "Failed to write Docker manifest: "+err.Error(), "", 0)
	}
	image, _ := manifest["image"].(string)
	version, _ := manifest["version"].(string)
	return recordDockerChannel(emit, dockerOutputDir, image, version)
}

// recordDockerChannel writes the docker channel record inside the directory
// this packager owns. Both assembly paths — the in-process assembler and the
// workspace-builder escape hatch — reach it, so a restored docker/ always
// carries the fact that the docker channel was packaged.
func recordDockerChannel(emit *jsonl.Emitter, dockerOutputDir, image, version string) bool {
	if err := pkgmeta.WriteChannelRecord(dockerOutputDir, pkgmeta.ChannelRecord{
		Version:  version,
		Artifact: image,
		Channels: []string{"docker"},
	}); err != nil {
		emit.Diagnostic("error", "Failed to write channel record: "+err.Error(), "", 0)
		return false
	}
	return true
}

// loadIntoDaemon writes the assembled image as a docker tarball and loads it
// into the local daemon (opt-in via --docker-load) so it can be `docker run`
// immediately; extra alias tags are applied after the load.
func loadIntoDaemon(emit *jsonl.Emitter, layoutDir, contentRef string, aliases []string) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		emit.Diagnostic("error", "--docker-load requires the docker CLI", "", 0)
		return false
	}
	tarFile, err := os.CreateTemp("", "putnami-docker-load-*.tar")
	if err != nil {
		emit.Diagnostic("error", "Failed to stage docker tarball: "+err.Error(), "", 0)
		return false
	}
	tarPath := tarFile.Name()
	tarFile.Close()
	defer os.Remove(tarPath)

	if err := oci.WriteDockerTarball(layoutDir, contentRef, tarPath); err != nil {
		emit.Diagnostic("error", "Failed to write docker tarball: "+err.Error(), "", 0)
		return false
	}
	// The executable and subcommand are fixed; tarPath comes from CreateTemp
	// and is passed as a direct argument without a shell.
	loadCmd := exec.Command("docker", "load", "-i", tarPath) //nolint:gosec
	loadCmd.Stderr = os.Stderr
	if err := loadCmd.Run(); err != nil {
		emit.Diagnostic("error", "docker load failed for "+contentRef, "", 0)
		return false
	}
	for _, alias := range aliases {
		tagCmd := exec.Command("docker", "tag", contentRef, alias)
		tagCmd.Stderr = os.Stderr
		if err := tagCmd.Run(); err != nil {
			emit.Diagnostic("error", "docker tag failed for "+alias, "", 0)
			return false
		}
	}
	emit.Log("info", "Loaded into docker daemon: "+contentRef)
	return true
}

// dockerfileFor renders the multi-stage Dockerfile for the
// workspace-builder escape hatch. It contains no git-derived bytes.
func dockerfileFor(workspaceBuilderImage, binaryRelPath, port string) string {
	if workspaceBuilderImage != "" {
		workspaceBinaryPath := "/app/" + binaryRelPath
		return fmt.Sprintf("ARG WORKSPACE_BUILDER_IMAGE=%s\nFROM ${WORKSPACE_BUILDER_IMAGE} AS builder\nFROM %s\nENV PORT=%s\nCOPY --from=builder %s /app\nUSER nonroot:nonroot\nENTRYPOINT [\"/app\"]\n",
			workspaceBuilderImage, dockerBuilderBaseImage, port, workspaceBinaryPath)
	}
	return fmt.Sprintf("FROM %s\nENV PORT=%s\nCOPY %s /app\nUSER nonroot:nonroot\nENTRYPOINT [\"/app\"]\n",
		dockerBuilderBaseImage, port, binaryRelPath)
}

// dockerContentHash hashes the docker-build inputs: the rendered
// Dockerfile and the binary it copies.
func dockerContentHash(dockerfileContent, binaryAbsPath string) (string, error) {
	h := sha256.New()
	h.Write([]byte(dockerfileContent))
	f, err := os.Open(binaryAbsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
