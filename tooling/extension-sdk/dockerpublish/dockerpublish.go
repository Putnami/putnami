// Package dockerpublish is the shared, ecosystem-agnostic docker publish
// orchestration. Both the TypeScript and Go extensions' `publish --docker` call
// Publish: docker packaging output is the same format regardless of the source
// language, so the orchestration (read the package manifest, resolve the session
// version, push the content once via the oci primitives, assign refs, record the
// digest into version.json) lives here once instead of being duplicated per
// ecosystem.
package dockerpublish

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"

	extproto "go.putnami.dev/protocol/extension"
	runtime "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/sdk/extension/releaseset"
)

// Publish publishes a pre-built Docker image (produced by `package --docker`): it
// ensures the content-addressed tag exists in the registry (one layer upload per
// unique content, ever) and then assigns the session's version to it. Package
// produces content; publish assigns identity — so unchanged content keeps an
// identical digest across release ids and digest-pinning deployers see no change.
//
// The version is the ONLY tag this publisher writes. A channel is a release-set
// decision, applied once by `channel set` after every member of the set has a
// verified digest; a publisher that also moved channel tags would move them
// member by member, so a consumer reading a channel mid-publish would see a set
// that was never released.
//
// The standard floor pushes via the docker default keychain to any OCI registry
// with no cloud installed; when @putnami/cloud is present, registrycred resolves a
// fresh per-host bearer and the oci tag-digest fast path is used when the registry
// advertises it.
func Publish(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if skipIfNoProject(ctx, emit) {
		return "SKIP", nil, nil
	}
	// Publication evidence belongs to this task and this attempt. Clear only its
	// typed record before any dry-run or validation exit so an earlier verified
	// publication can never survive as evidence for a later non-publication.
	if err := clearPublishedImageManifest(ctx); err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}

	flags := cli.ParseFlags(args)
	common := resolveCommonFlags(flags, ctx.Params)
	dockerRegistry, err := resolveDockerRegistry(flags, ctx)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}

	projectPath := ctx.Project.Path
	wsRoot := ctx.WorkspaceRoot

	// The Docker manifest is package's typed, cache-restorable output and the
	// current selected publish-docker task is its execution plan. Shared package
	// metadata is only a merge-point convenience: a sibling package step may
	// recreate it without a docker channel after this candidate was restored.
	// Selection prevents an unrequested publish task from reaching this function,
	// so the typed current candidate is authoritative here and the shared index
	// is not read at all.
	dockerManifest, err := pkgmeta.ReadDockerManifest(wsRoot, projectPath)
	if err != nil {
		emit.Diagnostic("error", "Docker manifest not found: "+err.Error(), "", 0)
		return "FAILED", nil, fmt.Errorf("read docker manifest: %w", err)
	}
	if len(dockerManifest.Tags) == 0 {
		emit.Diagnostic("error", "Docker manifest has no tags", "", 0)
		return "FAILED", nil, fmt.Errorf("docker manifest has no tags")
	}
	if ctx.Project.Type == "image" {
		return publishImmutableImageProject(ctx, emit, common.DryRun, dockerRegistry, dockerManifest)
	}

	imageName := dockerManifest.Image
	localRef := dockerManifest.Tags[0]

	// Identity comes from the session, not from the cached package manifest: the
	// manifest version is stamped when the package job actually runs, so a
	// cache-restored package would otherwise publish a release id from an earlier
	// session.
	version := sessionVersion(ctx, dockerManifest.Version)

	registry := dockerRegistry
	regHost := oci.RegistryHost(registry)

	// A native publication run routes every managed-registry request through the
	// invocation's loopback broker (PUTNAMI_REGISTRY_OCI_URL). The broker holds
	// the run's upstream credential; this process never asks oci.putnami.dev for
	// a token itself. An empty registry pushes nothing, so it consults no route.
	route, err := resolveDockerRoute(registry, dockerManifest)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, err
	}

	// Resolve the credential @putnami/cloud holds for the registry host and build a
	// keychain that injects it as a Bearer for that host only; every other registry
	// (GCP, ghcr, …) keeps its native docker-config / gcloud / ADC credentials via
	// DefaultKeychain. An empty token leaves DefaultKeychain in charge — the
	// standard floor. credHint carries the cloud's own guidance, surfaced only if
	// the registry then rejects the credentials (see oci.WrapAuthError). Under a
	// private broker the seam is asked about the broker host, which is how the
	// cloud knows to hand back the run's capability instead of a user session;
	// the keychain still binds that bearer to the logical registry host.
	credTok, credHint := resolveManagedDockerCredential(route.credentialHost)
	keychain := oci.NewRegistryKeychain(regHost, credTok)

	contentTag := dockerContentTag(dockerManifest, localRef)
	contentRef := oci.BuildRef(registry, imageName, contentTag)
	versionRef := oci.BuildRef(registry, imageName, version)
	qualifiedImage := strings.TrimSuffix(versionRef, ":"+version)
	allRefs := []string{versionRef}

	contentDigest := dockerManifest.Digest
	srcRef := contentRef
	if contentDigest != "" {
		srcRef = qualifiedImage + "@" + contentDigest
	}
	// The broker admits manifest writes at a digest, at the plan's version, or at
	// a released channel — never at the content tag. Content is addressed by its
	// digest there; the version is the only tag this publisher writes.
	pushRef := contentRef
	digestRef := contentRef
	sessionTags := []string{contentTag, version}
	if route.private() {
		pushRef = srcRef
		digestRef = srcRef
		sessionTags = []string{version}
	}

	emitDockerPlan(emit, imageName, version, registry, contentTag, allRefs)

	if common.DryRun {
		emit.Summary(fmt.Sprintf("Dry run: would push %d refs for %s", len(allRefs), imageName))
		for _, ref := range allRefs {
			emit.Info("  - " + ref)
		}
		emitPublishedDocker(emit, qualifiedImage, version, runtime.PublishRecord{DryRun: true})
		return "OK", map[string]any{"dryRun": true, "imageName": imageName, "refs": allRefs, "contentTag": contentTag}, nil
	}

	// One push per unique content, ever: if the content is already in the registry
	// (manifest HEAD, no layer transfer), this session uploads nothing and only
	// assigns references. This HEAD is the publication's only manifest existence
	// check: the push sends none, and a digest-addressed publication does not ask
	// for its digest again (provenContentDigest).
	contentExists := false
	cacheOutcome := ""
	publishTimings := &runtime.PublishTimings{}
	if registry != "" {
		lookupStart := time.Now()
		_, headErr := crane.Head(srcRef, route.craneOptions(keychain)...)
		publishTimings.CacheLookupMs = time.Since(lookupStart).Milliseconds()
		if headErr == nil {
			contentExists = true
			cacheOutcome = "hit"
		} else if oci.IsAuthError(headErr) {
			authErr := oci.WrapAuthError(headErr, credHint)
			emit.Diagnostic("error", authErr.Error(), "", 0)
			return "FAILED", nil, authErr
		} else {
			cacheOutcome = "miss"
		}
	}
	contentStatus := "pushed"
	if contentExists {
		contentStatus = "retagged"
	}

	emit.PhaseStart("docker-push")

	dockerDir := pkgmeta.PackageOutputDir(wsRoot, projectPath, "docker")
	pushStart := time.Now()
	pushedDigest, err := oci.PushContentWithDigest(emit, wsRoot, dockerDir, dockerManifest.Layout, localRef, pushRef, contentExists, keychain, credHint, route.pushOptions()...)
	if err != nil {
		return "FAILED", nil, err
	}
	if !contentExists {
		pushDuration := time.Since(pushStart).Milliseconds()
		publishTimings.CacheTransferMs = pushDuration
		publishTimings.RegistryPushMs = pushDuration
	}

	referencesStart := time.Now()
	if err := oci.ApplySessionRefs(emit, registry, imageName, srcRef, contentDigest, sessionTags, keychain, credHint, route.pushOptions()...); err != nil {
		return "FAILED", nil, err
	}
	publishTimings.ReferencePublishMs = time.Since(referencesStart).Milliseconds()

	digestStart := time.Now()
	imageDigest := provenContentDigest(registry, contentDigest, contentExists, pushedDigest)
	if imageDigest == "" {
		imageDigest = verifiedImmutableDigest(emit, registry, digestRef, keychain, route.pushOptions()...)
	}
	publishTimings.DigestResolveMs = time.Since(digestStart).Milliseconds()
	digestVerified := imageDigest != ""
	immutableRef := ""
	if digestVerified && registry != "" {
		immutableRef = qualifiedImage + "@" + imageDigest
		if err := writePublishedImageManifest(ctx, dockerManifest, qualifiedImage, immutableRef, imageDigest, regHost); err != nil {
			emit.PhaseEnd("docker-push", "failed")
			return "FAILED", nil, err
		}
	}
	emit.PhaseEnd("docker-push", "success")

	// Field names use snake_case to match the putnami/cloud deploy API contract
	// (image_digest), the canonical consumer of these fields.
	versionFields := map[string]any{"image": versionRef}
	if digestVerified {
		versionFields["image_digest"] = imageDigest
	}
	if pub := publishContextFields(version); len(pub) > 0 {
		versionFields["publish"] = pub
	}
	if err := mergeVersionJSON(wsRoot, projectPath, versionFields); err != nil {
		emit.Info(fmt.Sprintf("Could not update version.json: %v", err))
	}

	if contentExists {
		emit.Summary(fmt.Sprintf("Content %s unchanged in registry, retagged %s for %s", contentTag, version, imageName))
	} else {
		emit.Summary(fmt.Sprintf("Pushed new content %s and applied %s for %s", contentTag, version, imageName))
	}
	emitPublishedDocker(emit, qualifiedImage, version, runtime.PublishRecord{
		TargetRegistry: registry,
		ImmutableRef:   immutableRef,
		ContentStatus:  contentStatus,
		CacheOutcome:   cacheOutcome,
		ImageDigest:    imageDigest,
		DigestVerified: digestVerified,
		DigestReused:   contentExists && digestVerified,
		PublishTimings: publishTimings,
	})
	// One member per artifact, on the pushed and on the reused path alike: the
	// release set is assembled from what EXISTS in the registry under a verified
	// digest, not from what this session happened to upload.
	if digestVerified {
		emitPublishedMember(emit, qualifiedImage, registry, version, imageDigest)
	}

	result := map[string]any{
		"imageName":     imageName,
		"version":       version,
		"image":         versionRef,
		"refs":          allRefs,
		"contentTag":    contentTag,
		"contentStatus": contentStatus,
	}
	if digestVerified {
		result["image_digest"] = imageDigest
	}
	return "OK", result, nil
}

// resolveDockerRegistry keeps every output-target decision in publish. Image
// packaging parses its spec for local assembly but never consumes Registry,
// reads DOCKER_REGISTRY, or performs output-registry work.
//
// The chain is: the `--docker-registry` argument, then the workspace's
// `registries.oci` entry, then DOCKER_REGISTRY, then nothing — which the image
// project branch resolves to the managed target. The registries entry is the
// single declared source for an endpoint, so there is no publish-option spelling
// of it to read from the parameter bag any more.
func resolveDockerRegistry(flags map[string]string, ctx *pctx.Context) (string, error) {
	if explicit := cli.FlagString(flags, "docker-registry", ""); explicit != "" {
		return explicit, nil
	}
	declared, err := ociRegistryFromRegistries(ctx.Params)
	if err != nil {
		return "", err
	}
	if declared != "" {
		return declared, nil
	}
	if ctx.Project.Type == "image" {
		if raw, ok := ctx.Params["image"]; ok && len(raw) > 0 {
			var compatibility struct {
				Registry string `json:"registry"`
			}
			if err := json.Unmarshal(raw, &compatibility); err != nil {
				return "", fmt.Errorf("parsing options.package.image for publish registry compatibility: %w", err)
			}
			if compatibility.Registry != "" {
				return compatibility.Registry, nil
			}
		}
	}
	return os.Getenv("DOCKER_REGISTRY"), nil
}

// ociRegistryFromRegistries reads `registries.oci.publish` out of the job
// context's registries member — the project's effective registry endpoints, one
// entry per ecosystem, whose shape the ecosystem profile owns. An absent member
// or an absent oci entry is not an error: a workspace may publish images with
// DOCKER_REGISTRY alone.
func ociRegistryFromRegistries(params pctx.Params) (string, error) {
	raw, ok := params["registries"]
	if !ok || len(raw) == 0 {
		return "", nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", fmt.Errorf("parsing the registries job-context member: %w", err)
	}
	entry, ok := entries["oci"]
	if !ok || len(entry) == 0 {
		return "", nil
	}
	var profile struct {
		Publish string `json:"publish"`
	}
	if err := json.Unmarshal(entry, &profile); err != nil {
		return "", fmt.Errorf("parsing the registries.oci entry: %w", err)
	}
	return strings.TrimSpace(profile.Publish), nil
}

const managedOCIRegistry = "oci.putnami.dev"

// resolveManagedDockerCredential resolves the bearer for a docker publish.
//
// The seam takes a HOST and nothing else. The cloud decides what authority the
// credential carries for that registry; the framework does not describe the
// package or the action it is about to perform, and could not do so without
// re-importing the recipe model this seam exists to keep out.
//
// An empty return is not an error: it is the documented "no cloud credential,
// use native" signal, and it leaves DefaultKeychain in charge — the standard
// floor for GCP/ghcr/etc. via native credentials.
func resolveManagedDockerCredential(regHost string) (token, hint string) {
	return registrycred.ResolveToken(regHost)
}

type imagePublishTarget struct {
	RegistryPrefix string
	Host           string
	Repository     string
	Managed        bool
}

// resolveImagePublishTarget is the single authority for first-class image
// publication coordinates. A validated native plan supplies the managed
// namespace for the task's typed project identity; standalone publication uses
// the workspace identity. Repository parameters cannot redirect either target.
func resolveImagePublishTarget(ctx *pctx.Context, configuredRegistry, image string) (imagePublishTarget, error) {
	configuredRegistry = strings.TrimRight(strings.TrimSpace(configuredRegistry), "/")
	image = strings.TrimSpace(image)
	if image == "" {
		return imagePublishTarget{}, fmt.Errorf("image project candidate has no image name")
	}
	if configuredRegistry != "" && strings.Contains(configuredRegistry, "://") {
		return imagePublishTarget{}, fmt.Errorf("docker registry must not include a URL scheme")
	}

	configuredHost := oci.RegistryHost(configuredRegistry)
	managed := configuredRegistry == "" || configuredHost == managedOCIRegistry
	if !managed {
		return imagePublishTarget{
			RegistryPrefix: configuredRegistry,
			Host:           configuredHost,
			Repository:     strings.TrimSuffix(oci.BuildRef(configuredRegistry, image, "candidate"), ":candidate"),
		}, nil
	}
	if ctx == nil {
		return imagePublishTarget{}, fmt.Errorf("managed OCI target requires job context")
	}
	projectImage := normalizeTargetComponent(ctx.Project.Name)
	if image != projectImage {
		return imagePublishTarget{}, fmt.Errorf("packaged image name %q does not match project identity %q", image, projectImage)
	}
	namespace := normalizeTargetComponent(ctx.Workspace.Name)
	plan, err := releaseset.FromContext(ctx)
	if err != nil {
		return imagePublishTarget{}, err
	}
	if plan != nil {
		if ctx.Identity == nil || ctx.Identity.Project.ID == "" {
			return imagePublishTarget{}, fmt.Errorf("image release-set publication requires the task's typed project identity")
		}
		selected := false
		for _, member := range plan.SelectedMembers() {
			if member.Ecosystem != "oci" || member.ProjectID != ctx.Identity.Project.ID {
				continue
			}
			prefix, matches := strings.CutSuffix(member.Coordinate, "/"+image)
			if !matches || prefix == "" {
				continue
			}
			if selected {
				return imagePublishTarget{}, fmt.Errorf("image %q has ambiguous selected OCI members for project %q", image, ctx.Identity.Project.ID)
			}
			selected = true
			namespace = prefix
		}
		if !selected {
			return imagePublishTarget{}, fmt.Errorf("image %q has no selected OCI member for project %q", image, ctx.Identity.Project.ID)
		}
	}
	if namespace == "" || projectImage == "" {
		return imagePublishTarget{}, fmt.Errorf("managed OCI target requires workspace and project identity")
	}
	prefix := managedOCIRegistry + "/" + namespace
	if configuredRegistry != "" && configuredRegistry != managedOCIRegistry && configuredRegistry != prefix {
		return imagePublishTarget{}, fmt.Errorf("managed OCI registry override %q does not match canonical target %q", configuredRegistry, prefix)
	}
	return imagePublishTarget{
		RegistryPrefix: prefix,
		Host:           managedOCIRegistry,
		Repository:     prefix + "/" + projectImage,
		Managed:        true,
	}, nil
}

func normalizeTargetComponent(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "@")
	value = strings.Trim(value, "/")
	return strings.ToLower(strings.ReplaceAll(value, "/", "-"))
}

// publishImmutableImageProject publishes and verifies the first-class local
// candidate without creating release, branch, channel, or session tags.
func publishImmutableImageProject(ctx *pctx.Context, emit *jsonl.Emitter, dryRun bool, configuredRegistry string, manifest *pkgmeta.DockerManifest) (string, map[string]any, error) {
	if manifest.Digest == "" || manifest.Layout == "" {
		return "FAILED", nil, fmt.Errorf("image project manifest has no local OCI candidate")
	}
	if _, err := oci.ParseDigestReference("putnami.local/" + manifest.Image + "@" + manifest.Digest); err != nil {
		return "FAILED", nil, fmt.Errorf("image project candidate digest: %w", err)
	}
	if len(manifest.ContentHash) != 64 || manifest.Version != "c-"+manifest.ContentHash {
		return "FAILED", nil, fmt.Errorf("image project version must be the complete deterministic content hash")
	}
	if len(manifest.Tags) == 0 || manifest.Tags[0] != manifest.Image+":"+manifest.Version {
		return "FAILED", nil, fmt.Errorf("image project local candidate ref does not match its content identity")
	}
	target, err := resolveImagePublishTarget(ctx, configuredRegistry, manifest.Image)
	if err != nil {
		return "FAILED", nil, err
	}
	memberVersion, err := imagePublicationVersion(ctx, target, manifest.Version)
	if err != nil {
		return "FAILED", nil, err
	}
	privateTransport, credentialHost, err := privateOCITransportFor(target)
	if err != nil {
		return "FAILED", nil, err
	}
	contentRef := target.Repository + ":" + manifest.Version
	immutableRef := target.Repository + "@" + manifest.Digest
	pushRef := contentRef
	if privateTransport != nil {
		// Native admission permits the verified digest, not the package's local
		// content tag. The planned version identifies the release member without
		// creating a session or channel tag for an immutable image project.
		pushRef = immutableRef
	}

	if dryRun {
		emit.Summary("Dry run: would publish and verify immutable image " + immutableRef)
		return "OK", map[string]any{
			"dryRun": true, "image": immutableRef, "image_digest": manifest.Digest,
			"contentHash": manifest.ContentHash, "version": manifest.Version,
		}, nil
	}

	token, hint := registrycred.ResolveToken(credentialHost)
	if target.Managed && token == "" {
		// The managed registry rejects an anonymous push, so fail fast with the
		// cloud's own words rather than letting crane report an opaque 401.
		if hint == "" {
			hint = "managed OCI registry authorization unavailable"
		}
		return "FAILED", nil, fmt.Errorf("resolve managed OCI registry token: %s", hint)
	}
	// Authentication is resolved for the selected loopback broker, while the
	// keychain still binds that bearer to the immutable logical registry host.
	// The request therefore carries the credential only on a canonical managed
	// OCI request before the per-call transport maps it to loopback.
	keychain := oci.NewRegistryKeychain(target.Host, token)
	headOptions := []crane.Option{crane.WithAuthFromKeychain(keychain)}
	var pushOptions []oci.PushOptions
	if privateTransport != nil {
		headOptions = append(headOptions, crane.WithTransport(privateTransport))
		pushOptions = append(pushOptions, oci.PushOptions{Transport: privateTransport})
	}
	// The lookup is the publication's only manifest HEAD. On a hit it verifies the
	// digest: go-containerregistry rejects a HEAD by digest unless the registry
	// answers with that digest. On a miss the push verifies it: the registry
	// accepts a manifest PUT of the layout bytes, and the push returns their
	// SHA-256.
	lookupStart := time.Now()
	descriptor, headErr := crane.Head(immutableRef, headOptions...)
	lookupDuration := time.Since(lookupStart).Milliseconds()
	cacheOutcome := "hit"
	contentStatus := "reused"
	registryDigest := ""
	if headErr == nil {
		registryDigest = descriptor.Digest.String()
	} else {
		if oci.IsAuthError(headErr) {
			return "FAILED", nil, oci.WrapAuthError(headErr, hint)
		}
		cacheOutcome = "miss"
		contentStatus = "pushed"
		dockerDir := pkgmeta.PackageOutputDir(ctx.WorkspaceRoot, ctx.Project.Path, "docker")
		emit.PhaseStart("docker-push")
		registryDigest, err = oci.PushContentWithDigest(emit, ctx.WorkspaceRoot, dockerDir, manifest.Layout, manifest.Tags[0], pushRef, false, keychain, hint, pushOptions...)
		if err != nil {
			return "FAILED", nil, err
		}
	}
	if registryDigest != manifest.Digest {
		if cacheOutcome == "miss" {
			emit.PhaseEnd("docker-push", "failed")
		}
		return "FAILED", nil, fmt.Errorf("registry digest %s does not match packaged digest %s", registryDigest, manifest.Digest)
	}
	if cacheOutcome == "miss" {
		emit.PhaseEnd("docker-push", "success")
	}
	if err := writePublishedImageManifest(ctx, manifest, target.Repository, immutableRef, manifest.Digest, target.Host); err != nil {
		return "FAILED", nil, err
	}

	if err := mergeVersionJSON(ctx.WorkspaceRoot, ctx.Project.Path, map[string]any{
		"image": immutableRef, "image_digest": manifest.Digest,
	}); err != nil {
		emit.Info(fmt.Sprintf("Could not update version.json: %v", err))
	}
	timings := &runtime.PublishTimings{CacheLookupMs: lookupDuration}
	emitPublishedDocker(emit, target.Repository, manifest.Version, runtime.PublishRecord{
		TargetRegistry: target.Host, ImmutableRef: immutableRef, ContentStatus: contentStatus, CacheOutcome: cacheOutcome,
		ImageDigest: manifest.Digest, DigestVerified: true, DigestReused: cacheOutcome == "hit",
		PublishTimings: timings,
	})
	emitPublishedMember(emit, target.Repository, target.Host, memberVersion, manifest.Digest)
	emit.Summary("Verified immutable image " + immutableRef + "; no release or channel tags applied")
	return "OK", map[string]any{
		"imageName": manifest.Image, "image": immutableRef, "image_digest": manifest.Digest,
		"contentHash": manifest.ContentHash, "version": manifest.Version,
		"refs": []string{immutableRef}, "channels": []string{}, "contentStatus": contentStatus,
		"cacheOutcome": cacheOutcome, "digestReused": cacheOutcome == "hit",
	}, nil
}

// imagePublicationVersion binds native release evidence to the coordinator's
// selected member. Standalone publication retains the local content identity.
func imagePublicationVersion(ctx *pctx.Context, target imagePublishTarget, fallback string) (string, error) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil {
		return "", err
	}
	if plan == nil {
		return fallback, nil
	}
	coordinate := strings.TrimPrefix(target.Repository, target.Host+"/")
	member, ok := plan.Member("oci", coordinate)
	if !ok || !member.Selected {
		return "", fmt.Errorf("image %q is not selected in the release-set plan", coordinate)
	}
	if ctx.Identity == nil || ctx.Identity.Project.ID == "" {
		return "", fmt.Errorf("image release-set publication requires the task's typed project identity")
	}
	projectID := ctx.Identity.Project.ID
	if member.ProjectID != projectID {
		return "", fmt.Errorf("image release-set member %q belongs to project %q, not %q", coordinate, member.ProjectID, projectID)
	}
	return member.Version, nil
}

func clearPublishedImageManifest(ctx *pctx.Context) error {
	outputDir := ctx.OutputPath
	if outputDir == "" {
		outputDir = filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "publish")
	}
	path := pkgmeta.PublishedImageManifestPath(outputDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing prior published image evidence %s: %w", path, err)
	}
	return nil
}

func writePublishedImageManifest(ctx *pctx.Context, candidate *pkgmeta.DockerManifest, repository, immutableRef, digest, targetRegistry string) error {
	outputDir := ctx.OutputPath
	if outputDir == "" {
		outputDir = filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", ctx.Project.Path, "publish")
	}
	if err := pkgmeta.WritePublishedImageManifest(outputDir, pkgmeta.PublishedImageManifest{
		Image:           repository,
		ImmutableRef:    immutableRef,
		Digest:          digest,
		CandidateDigest: candidate.Digest,
		ContentHash:     candidate.ContentHash,
		Platform:        candidate.Platform,
		TargetRegistry:  targetRegistry,
		Verified:        true,
	}); err != nil {
		return fmt.Errorf("writing published image evidence: %w", err)
	}
	return nil
}

// provenContentDigest returns the packaged digest when this publication has
// already proven it against the registry, or "" when the registry must still be
// asked. The proof is one of two exchanges publish made anyway:
//
//   - a hit: the cache lookup was a HEAD by this digest, and go-containerregistry
//     rejects a HEAD by digest unless the registry answers with that digest;
//   - a miss: the registry accepted a manifest PUT of the layout bytes, whose
//     SHA-256 is pushedDigest, and it equals the packaged digest.
//
// Either way the version was then assigned from that digest, so it is the
// digest the version resolves to. A second manifest HEAD would add nothing.
func provenContentDigest(registry, contentDigest string, contentExists bool, pushedDigest string) string {
	if registry == "" || !isImmutableDigest(contentDigest) {
		return ""
	}
	if contentExists || pushedDigest == contentDigest {
		return contentDigest
	}
	return ""
}

// verifiedImmutableDigest returns only a SHA-256 OCI digest. A package layout
// already knows its reproducible digest (see provenContentDigest); when it does
// not, ask the registry for the content ref after references have been applied.
// A mutable tag is never returned as a substitute, so downstream deployers can
// treat a non-empty value as content-addressed provenance.
func verifiedImmutableDigest(emit *jsonl.Emitter, registry, contentRef string, keychain authn.Keychain, options ...oci.PushOptions) string {
	if registry == "" {
		return ""
	}
	resolved := oci.ResolveDigest(emit, registry, contentRef, keychain, options...)
	if isImmutableDigest(resolved) {
		return resolved
	}
	return ""
}

// isImmutableDigest accepts the OCI digest spelling a deployer can pin. Keep
// the check local and strict: image names and tags are never valid values here.
func isImmutableDigest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+64 {
		return false
	}
	for _, b := range digest[len(prefix):] {
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// sessionVersion resolves the version identity the image is published under. The
// release id comes from the orchestrator's session version so every artifact in a
// publish session shares one version regardless of when the cached package was
// produced. Without an orchestrator version (direct extension invocation), the
// advisory manifest version is the fallback.
//
// There is one version per line and the CLI derives it from git, so a publisher
// no longer chooses between a base and a full spelling of it.
func sessionVersion(ctx *pctx.Context, manifestVersion string) string {
	if ctx.Version != nil && ctx.Version.Full != "" {
		return ctx.Version.Full
	}
	return manifestVersion
}

// dockerContentTag resolves the content-addressed immutable tag the package step
// gave the image. Manifests produced before content-addressing carry no content
// hash; for those the local ref's tag is used as-is, which restores the
// pre-digest-travel push behavior until the project is repackaged.
func dockerContentTag(m *pkgmeta.DockerManifest, localRef string) string {
	if m.ContentHash != "" {
		return "c-" + m.ContentHash
	}
	if i := strings.LastIndex(localRef, ":"); i >= 0 && !strings.Contains(localRef[i+1:], "/") {
		return localRef[i+1:]
	}
	return "latest"
}

// emitDockerPlan emits the pre-push informational lines describing the image,
// registry, content tag, and the session tags that will be applied.
func emitDockerPlan(emit *jsonl.Emitter, imageName, version, registry, contentTag string, allRefs []string) {
	emit.Info(fmt.Sprintf("Publishing Docker image: %s:%s", imageName, version))
	emit.Info(fmt.Sprintf("Registry: %s", orDefault(registry, "(none - local only)")))
	emit.Info(fmt.Sprintf("Content tag: %s", contentTag))
	refTags := make([]string, len(allRefs))
	for i, r := range allRefs {
		parts := strings.SplitN(r, ":", 2)
		if len(parts) == 2 {
			refTags[i] = parts[1]
		} else {
			refTags[i] = r
		}
	}
	emit.Info(fmt.Sprintf("Tags: %s", strings.Join(refTags, ", ")))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// emitPublishedDocker records a published Docker artifact as a session event.
// The pre-existing contentStatus remains intact for human renderers; the
// additive PublishRecord fields are the machine-facing phase facts.
func emitPublishedDocker(emit *jsonl.Emitter, name, version string, record runtime.PublishRecord) {
	data := map[string]any{"registry": "docker", "version": version}
	if record.TargetRegistry != "" {
		data["targetRegistry"] = record.TargetRegistry
	}
	if record.ContentStatus != "" {
		data["contentStatus"] = record.ContentStatus
	}
	if record.CacheOutcome != "" {
		data["cacheOutcome"] = record.CacheOutcome
	}
	if record.ImageDigest != "" {
		data["imageDigest"] = record.ImageDigest
	}
	if record.ImmutableRef != "" {
		data["immutableRef"] = record.ImmutableRef
	}
	if record.DigestVerified {
		data["digestVerified"] = true
	}
	if record.DigestReused {
		data["digestReused"] = true
	}
	if record.PublishTimings != nil {
		data["publishTimings"] = record.PublishTimings
	}
	if record.DryRun {
		data["dryRun"] = true
	}
	// Keep the artifact's legacy empty path intact. Machine consumers read the
	// additive imageDigest field; changing path would make an old renderer treat
	// an OCI reference as a local filesystem path.
	emit.ArtifactWithData("docker", name, "published", "", data)
}

// emitPublishedMember reports the image to the release set being assembled: one
// member, at its ecosystem coordinate, with the digest a deployer can pin.
//
// The coordinate is the full repository path WITHOUT the registry host. The
// namespace remains part of the native identity: authorization and channel-tag
// projection must address the same repository that received the image.
// `published` above stays as it is: it is the human-and-renderer event, this one
// is the release-set fact.
func emitPublishedMember(emit *jsonl.Emitter, qualifiedImage, registry, version, artifactDigest string) {
	coordinate := strings.TrimPrefix(qualifiedImage, oci.RegistryHost(registry)+"/")
	if coordinate == "" {
		return
	}
	emit.ArtifactWithData("oci", qualifiedImage, extproto.PublishedMemberEventKind, "", map[string]any{
		"ecosystem":      "oci",
		"coordinate":     coordinate,
		"version":        version,
		"artifactDigest": artifactDigest,
	})
}

// publishContextFields builds the resolved publish context recorded under the
// "publish" key of .gen/version.json so extension publish hooks sharing the run
// graph can match the image's version instead of re-deriving it. Empty inputs
// are omitted.
func publishContextFields(version string) map[string]any {
	fields := map[string]any{}
	if version != "" {
		fields["version"] = version
	}
	return fields
}

// mergeVersionJSON merges fields into the project's .gen/version.json, creating
// it if missing. The scheduler writes the base record before any job runs;
// publish overlays channel-specific fields without disturbing it.
func mergeVersionJSON(wsRoot, projectPath string, fields map[string]any) error {
	if projectPath == "" {
		return fmt.Errorf("project path is required")
	}
	genDir := filepath.Join(wsRoot, projectPath, ".gen")
	versionPath := filepath.Join(genDir, "version.json")

	info := map[string]any{}
	switch data, err := os.ReadFile(versionPath); {
	case err == nil:
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("parsing %s: %w", versionPath, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// no existing file — create a fresh one below
	default:
		return fmt.Errorf("reading %s: %w", versionPath, err)
	}

	maps.Copy(info, fields)

	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", genDir, err)
	}
	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling version.json: %w", err)
	}
	if err := os.WriteFile(versionPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", versionPath, err)
	}
	return nil
}
