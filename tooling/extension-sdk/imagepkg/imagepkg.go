// Package imagepkg implements first-class image projects. It turns an authored,
// COPY-only OCI specification into a local, content-addressed OCI candidate.
// Registry target resolution, credentials, publication, and remote verification
// belong exclusively to dockerpublish.
package imagepkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
)

// Config is options.package.image. Sources are project-relative regular files;
// layer order and runtime config order are intentional OCI inputs.
type Config struct {
	Base string `json:"base"`
	// Registry is accepted for compatibility with older project documents but
	// is intentionally not consumed by package. Docker publish reads it only as
	// a lower-precedence compatibility target after the publish command's own
	// registry option; it never enters candidate identity or package I/O.
	Registry     string        `json:"registry"`
	Platform     string        `json:"platform"`
	Layers       []LayerConfig `json:"layers"`
	Env          []string      `json:"env,omitempty"`
	Entrypoint   []string      `json:"entrypoint,omitempty"`
	WorkingDir   string        `json:"workingDir,omitempty"`
	User         string        `json:"user,omitempty"`
	ExposedPorts []string      `json:"exposedPorts,omitempty"`
}

// LayerConfig declares either a project-relative tar layer or a set of files
// to place in one deterministic layer.
type LayerConfig struct {
	Files   []FileConfig `json:"files,omitempty"`
	Tarball string       `json:"tarball,omitempty"`
}

// FileConfig maps one project-relative regular file to an absolute image path.
type FileConfig struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Mode   int64  `json:"mode,omitempty"`
}

// BaseArtifact is a digest-pinned input to local image assembly. Layout is set
// for a dockerBaseProject dependency and empty for a literal registry input.
// Reference is an identity input to the image plan, never publication evidence.
type BaseArtifact struct {
	Reference string
	Digest    string
	Layout    string
}

// Package resolves, content-addresses, and locally packages one image project.
func Package(ctx *pctx.Context, emit *jsonl.Emitter) (string, map[string]any, error) {
	if ctx == nil || ctx.Project.Path == "" {
		return "SKIP", nil, nil
	}
	if ctx.Project.Type != "image" {
		return "FAILED", nil, fmt.Errorf("package-image requires project type %q, got %q", "image", ctx.Project.Type)
	}

	cfg, err := configFromParams(ctx.Params)
	if err != nil {
		return "FAILED", nil, err
	}
	platform := ctx.Params.String("platform")
	if platform == "" {
		platform = cfg.Platform
	}
	if platform == "" {
		platform = "linux/amd64"
	}

	literalBase := ctx.Params.String("docker-base-image", "dockerBaseImage")
	if literalBase != "" && cfg.Base != "" && literalBase != cfg.Base {
		return "FAILED", nil, fmt.Errorf("image base is declared twice with different values")
	}
	if literalBase == "" {
		literalBase = cfg.Base
	}
	base, err := ResolveBaseArtifact(ctx, literalBase,
		ctx.Params.String("docker-base-project", "dockerBaseProject"), platform)
	if err != nil {
		return "FAILED", nil, err
	}

	spec, err := resolveSpec(ctx.Project.FullPath, base.Reference, platform, cfg)
	if err != nil {
		return "FAILED", nil, err
	}
	imagePlan, err := oci.NewImagePlan(spec, oci.PlanOptions{})
	if err != nil {
		return "FAILED", nil, fmt.Errorf("planning image project inputs: %w", err)
	}
	fullHash := imagePlan.Identity
	contentTag := "c-" + fullHash

	imageName := normalizeImageName(ctx.Project.Name)
	contentRef := imageName + ":" + contentTag
	if ctx.Params.Bool("dry-run", false, "dryRun") {
		emit.Info("Dry run: would assemble local image candidate " + contentRef)
		return "OK", map[string]any{"image": contentRef, "contentHash": fullHash, "version": contentTag, "dryRun": true}, nil
	}

	dockerDir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "docker")
	layout := "oci"
	layoutDir := filepath.Join(dockerDir, layout)
	baseCacheDir := filepath.Join(ctx.WorkspaceRoot, ".putnami", "cache", "oci-base")
	var baseKeychain = oci.NewRegistryKeychain("", "")
	baseHint := ""
	if base.Layout == "" {
		baseDigestRef, parseErr := oci.ParseDigestReference(base.Reference)
		if parseErr != nil {
			return "FAILED", nil, fmt.Errorf("resolving base registry credentials: %w", parseErr)
		}
		baseHost := baseDigestRef.Context().RegistryStr()
		baseToken, hint := registrycred.ResolveToken(baseHost)
		baseKeychain = oci.NewRegistryKeychain(baseHost, baseToken)
		baseHint = hint
	}
	digest, err := oci.AssemblePlanToLayout(imagePlan, layoutDir, oci.LayoutOptions{
		BaseCacheDir:  baseCacheDir,
		LayerCacheDir: oci.LayerCacheRootFromEnv(),
		Keychain:      baseKeychain,
		BaseLayout:    base.Layout,
		BaseDigest:    base.Digest,
	})
	if err != nil {
		err = oci.WrapAuthError(err, baseHint)
		return "FAILED", nil, fmt.Errorf("assembling image project: %w", err)
	}

	if _, err := oci.ParseDigestReference("putnami.local/" + imageName + "@" + digest); err != nil {
		return "FAILED", nil, fmt.Errorf("assembled non-immutable image digest %q: %w", digest, err)
	}
	manifest := pkgmeta.DockerManifest{
		Image: imageName, Tags: []string{contentRef}, Version: contentTag,
		ContentHash: fullHash, Digest: digest, Layout: layout,
		Platform: platform,
	}
	if err := writeJSONAtomic(filepath.Join(dockerDir, "manifest.json"), manifest); err != nil {
		return "FAILED", nil, fmt.Errorf("writing image manifest: %w", err)
	}
	// The channel record goes INSIDE the docker directory this task owns, so it
	// is captured and restored by the same declared output as the layout it
	// describes. Previously this wrote the shared package index instead, and
	// it OVERWROTE it — the one writer of three that did not merge.
	if err := pkgmeta.WriteChannelRecord(dockerDir, pkgmeta.ChannelRecord{
		Version: contentTag, Artifact: imageName, Channels: []string{"docker"},
	}); err != nil {
		return "FAILED", nil, fmt.Errorf("writing channel record: %w", err)
	}

	emit.ArtifactWithData("docker", contentRef, "package", layoutDir, map[string]any{
		"registry": "docker", "contentHash": fullHash, "candidateDigest": digest,
		"platform": platform, "artifactLocation": layout,
	})
	return "OK", map[string]any{
		"image": contentRef, "contentHash": fullHash, "version": contentTag,
		"candidateDigest": digest, "platform": platform, "layout": layout,
	}, nil
}

// ResolveBaseArtifact resolves either a literal pinned base or a local project
// candidate in the orchestrator-provided dependency closure. A project path
// merely present on disk but absent from the graph is deliberately rejected.
func ResolveBaseArtifact(ctx *pctx.Context, literal, projectRef, platform string) (BaseArtifact, error) {
	literal = strings.TrimSpace(literal)
	projectRef = strings.TrimSpace(projectRef)
	if literal != "" && projectRef != "" {
		return BaseArtifact{}, fmt.Errorf("dockerBaseImage and dockerBaseProject are mutually exclusive")
	}
	if projectRef == "" {
		if literal == "" {
			return BaseArtifact{}, fmt.Errorf("image base is required")
		}
		if _, err := oci.ParseDigestReference(literal); err != nil {
			return BaseArtifact{}, err
		}
		return BaseArtifact{Reference: literal}, nil
	}
	if ctx == nil {
		return BaseArtifact{}, fmt.Errorf("cannot resolve dockerBaseProject without job context")
	}

	var base *pctx.ProjectRef
	for i := range ctx.Project.DependencyClosure {
		ref := &ctx.Project.DependencyClosure[i]
		id := ref.ID
		if id == "" && ref.Path != "" {
			id = "/" + strings.Trim(ref.Path, "/")
		}
		if projectRef == id || projectRef == ref.Name {
			base = ref
			break
		}
	}
	if base == nil || base.Path == ctx.Project.Path {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q is not a distinct project in %s's dependency closure", projectRef, ctx.Project.Name)
	}
	manifest, err := pkgmeta.ReadDockerManifest(ctx.WorkspaceRoot, base.Path)
	if err != nil {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q has no packaged image artifact: %w", projectRef, err)
	}
	if manifest.Layout == "" {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q has no local OCI layout", projectRef)
	}
	localRef := "putnami.local/" + normalizeImageName(manifest.Image) + "@" + manifest.Digest
	digestRef, err := oci.ParseDigestReference(localRef)
	if err != nil {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q candidate digest: %w", projectRef, err)
	}
	if manifest.Digest == "" || digestRef.DigestStr() != manifest.Digest {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q has invalid candidate digest metadata", projectRef)
	}
	if manifest.Platform != "" && platform != "" && manifest.Platform != platform {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q targets %s, not %s", projectRef, manifest.Platform, platform)
	}
	layout := filepath.Clean(manifest.Layout)
	if filepath.IsAbs(layout) || layout == "." || layout == ".." || strings.HasPrefix(layout, ".."+string(filepath.Separator)) {
		return BaseArtifact{}, fmt.Errorf("dockerBaseProject %q has invalid local OCI layout %q", projectRef, manifest.Layout)
	}
	layout = filepath.Join(pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, base.Path, "docker"), layout)
	return BaseArtifact{Reference: localRef, Digest: manifest.Digest, Layout: layout}, nil
}

// ResolveBaseReference is the compatibility projection for callers that need
// only the digest-pinned plan identity. New packagers should consume the full
// BaseArtifact so a project dependency stays local.
func ResolveBaseReference(ctx *pctx.Context, literal, projectRef, platform string) (string, error) {
	artifact, err := ResolveBaseArtifact(ctx, literal, projectRef, platform)
	return artifact.Reference, err
}

func configFromParams(params pctx.Params) (Config, error) {
	raw, ok := params["image"]
	if !ok || len(raw) == 0 {
		return Config{}, fmt.Errorf("image project requires options.package.image")
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing options.package.image: %w", err)
	}
	return cfg, nil
}

func resolveSpec(projectRoot, baseRef, platform string, cfg Config) (oci.Spec, error) {
	if projectRoot == "" {
		return oci.Spec{}, fmt.Errorf("project full path is required")
	}
	layers := make([]oci.Layer, 0, len(cfg.Layers))
	for i, input := range cfg.Layers {
		if input.Tarball != "" && len(input.Files) > 0 {
			return oci.Spec{}, fmt.Errorf("image layer %d declares both files and tarball", i)
		}
		if input.Tarball != "" {
			source, err := resolveProjectFile(projectRoot, input.Tarball)
			if err != nil {
				return oci.Spec{}, fmt.Errorf("image layer %d tarball: %w", i, err)
			}
			layers = append(layers, oci.Layer{Tarball: source})
			continue
		}
		if len(input.Files) == 0 {
			return oci.Spec{}, fmt.Errorf("image layer %d is empty", i)
		}
		files := make([]oci.File, 0, len(input.Files))
		seen := map[string]bool{}
		for _, inputFile := range input.Files {
			destination := path.Clean(inputFile.Path)
			if !strings.HasPrefix(inputFile.Path, "/") || destination == "/" || destination != inputFile.Path || seen[destination] {
				return oci.Spec{}, fmt.Errorf("image layer %d has invalid or duplicate destination %q", i, inputFile.Path)
			}
			seen[destination] = true
			source, err := resolveProjectFile(projectRoot, inputFile.Source)
			if err != nil {
				return oci.Spec{}, fmt.Errorf("image file %q: %w", inputFile.Source, err)
			}
			mode := inputFile.Mode
			if mode == 0 {
				mode = 0o644
			}
			if mode < 0 || mode > 0o7777 {
				return oci.Spec{}, fmt.Errorf("image file %q has invalid mode %d", inputFile.Source, mode)
			}
			files = append(files, oci.File{Source: source, Path: destination, Mode: mode})
		}
		// Tar emission sorts paths; canonicalize the spec the same way so a
		// declaration-only reorder cannot mint a different version for one image.
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		layers = append(layers, oci.Layer{Files: files})
	}
	ports := append([]string(nil), cfg.ExposedPorts...)
	sort.Strings(ports)
	ports = unique(ports)
	return oci.Spec{
		BaseRef: baseRef, Platform: platform, Layers: layers,
		Env: append([]string(nil), cfg.Env...), Entrypoint: append([]string(nil), cfg.Entrypoint...),
		WorkingDir: cfg.WorkingDir, User: cfg.User, ExposedPorts: ports,
	}, nil
}

func resolveProjectFile(root, source string) (string, error) {
	if source == "" || filepath.IsAbs(source) {
		return "", fmt.Errorf("source must be a project-relative regular file")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(rootReal, filepath.Clean(source))
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("source escapes the project root")
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file")
	}
	return real, nil
}

func normalizeImageName(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	return strings.ToLower(strings.ReplaceAll(name, "/", "-"))
}

func unique(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func writeJSONAtomic(filename string, value any) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".putnami-image-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filename)
}
