package distributioncli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
)

// archiveManifestMediaType is the manifest media type put-server stores for an
// archives publish. put-server stores it verbatim (no allowlist); the resolve /
// download path reads the `{"artifacts":{...}}` payload shape it wraps.
const archiveManifestMediaType = "application/vnd.putnami.archive+json"

// archivePlatforms is the platform set a template (platform-independent) archive
// fans its single blob out to, matching the framework's publisher.
var archivePlatforms = []string{
	"darwin-arm64", "darwin-x64",
	"linux-arm64", "linux-x64",
	"windows-arm64", "windows-x64",
}

// packageMetadata mirrors the subset of .putnami/out/<project>/package/
// metadata.json (written by the framework's archive packaging step) that the
// archives publisher reads. The build step owns the file's full shape; we only
// depend on these fields.
type packageMetadata struct {
	Version  string   `json:"version"`
	Artifact string   `json:"artifact"`
	Channels []string `json:"channels"`
	Template bool     `json:"template,omitempty"`
}

func (m *packageMetadata) hasChannel(channel string) bool {
	return slices.Contains(m.Channels, channel)
}

// archiveBlob maps a platform to an uploaded blob in the archive manifest
// payload (`{"artifacts":{"<os>-<arch>":{digest,size}}}`).
type archiveBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// ArchivePublishResult is the published reference the command reports.
type ArchivePublishResult struct {
	Package        string                 `json:"package"`
	Version        string                 `json:"version"`
	ArtifactDigest string                 `json:"artifact_digest"`
	Platforms      map[string]string      `json:"platforms"`
	Artifacts      map[string]archiveBlob `json:"artifacts"`
}

// PublishArchives backs `putnami cloud packages publish`. It mirrors
// publish-migration: discover the build artifact — here the per-platform
// archives the framework's `package --archives` step writes under
// .putnami/out/<project>/package/archives plus its metadata.json — then upload
// each as a content-addressed blob and publish one immutable archive manifest.
// Release-set v2 owns channel selection; this publisher never moves a package
// channel or records release membership.
//
// This owns the Putnami-specific archive publishing; the framework's
// platform/ci `publish-archives` does not carry it.
func PublishArchives(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	_, err := PublishArchivesWithResult(params, args, workspaceRoot, env, ioctx)
	return err
}

// PublishArchivesWithResult publishes the same immutable artifacts as
// PublishArchives and returns the exact typed identity only after the registry
// accepted or byte-for-byte confirmed the version. Skip and dry-run outcomes
// return nil so an event aggregator cannot mistake a plan for a published fact.
func PublishArchivesWithResult(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*ArchivePublishResult, error) {
	clicore.AdoptPositionalApp(params, args)
	if err := rejectRetiredArchivePublishControls(params); err != nil {
		return nil, err
	}
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return nil, err
	}
	ifPresent := clicore.Truthy(clicore.Param(params, "if-present", "ifPresent"))

	projectPath, err := projectRelPath(workspaceRoot, app)
	if err != nil {
		return nil, err
	}

	skip := func(reason, human string) (*ArchivePublishResult, error) {
		writePublishResult(map[string]any{"status": "skipped", "app": app, "reason": reason}, params, ioctx, human)
		return nil, nil
	}

	meta, found, err := readPackageMetadataFor(workspaceRoot, projectPath)
	if err != nil {
		return nil, err
	}
	if !found {
		// --if-present backs the publish-verb task: a project may expose a
		// Docker image or migration bundle but no archives, and those must
		// no-op rather than fail the whole publish.
		if ifPresent {
			return skip("no package metadata for "+app, fmt.Sprintf("No package metadata for %s — skipping archives publish.", app))
		}
		return nil, clicore.NewError(
			fmt.Sprintf("no package metadata for %q (looked for %s). Run `putnami package --archives %s` before publishing.",
				app, metadataPath(workspaceRoot, projectPath), app),
			clicore.ExitUsage,
		)
	}
	// A project that produces no archives channel is a normal skip — most
	// workloads ship a Docker image, not extension archives.
	if !meta.hasChannel("archives") && !meta.hasChannel("template-archives") {
		return skip("no archives channel for "+app, fmt.Sprintf("No archives channel for %s — skipping archives publish.", app))
	}

	archivesDir := archivesOutputDir(workspaceRoot, projectPath)
	if override := clicore.StringParam(params, "archives-from", "archivesFrom"); override != "" {
		// Resolve a relative override against the workspace root (matching
		// --schema-from on publish-config) rather than the process cwd.
		archivesDir = override
		if !filepath.IsAbs(archivesDir) {
			archivesDir = filepath.Join(realWorkspaceRoot(workspaceRoot), archivesDir)
		}
	}
	files := listArchiveFiles(archivesDir)
	if len(files) == 0 {
		if ifPresent {
			return skip("no archive files at "+archivesDir, fmt.Sprintf("No archive files for %s — skipping archives publish.", app))
		}
		return nil, clicore.NewError(fmt.Sprintf("no archive files (*.tar.gz) in %s — run `putnami package --archives %s` first", archivesDir, app), clicore.ExitUsage)
	}

	artifact := archiveArtifactName(params, workspaceRoot, projectPath, meta)
	if artifact == "" {
		return nil, clicore.NewError("package metadata has no artifact name — rebuild with `putnami package --archives`", clicore.ExitUsage)
	}
	coordinate := ArchivePackageCoordinate(artifact)
	ns, pkg, _ := strings.Cut(coordinate, "/")

	publishVersion := meta.Version
	if publishVersion == "" {
		return nil, clicore.NewError("package metadata has no version — rebuild with `putnami package --archives`", clicore.ExitUsage)
	}
	planned, err := validateArchiveReleaseSetPlan(params, projectPath, coordinate, publishVersion)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}

	if clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		writePublishResult(map[string]any{
			"status":      "dry-run",
			"app":         app,
			"package":     ns + "/" + pkg,
			"version":     publishVersion,
			"files":       files,
			"archivesDir": archivesDir,
		}, params, ioctx, fmt.Sprintf("Dry run: %d archive(s) ready for immutable publish at %s/%s@%s.", len(files), ns, pkg, publishVersion))
		return nil, nil
	}

	opts := archivePublishOptions{
		Namespace:    ns,
		Package:      pkg,
		Version:      publishVersion,
		ArchivesDir:  archivesDir,
		Files:        files,
		Artifact:     artifact,
		MetaArtifact: meta.Artifact,
		BinaryName:   clicore.StringParam(params, "binary-name", "binaryName"),
		Template:     meta.Template,
	}
	if outbox := PublicationOutbox(env); outbox != "" {
		// publication-v1: pack the planned member and report no published
		// result; the engine uploads it and emits the member itself.
		if planned == nil {
			return nil, OutboxWithoutPlanError("archive publication")
		}
		packed, err := packArchivesIntoOutbox(outbox, planned.ProjectID, opts)
		if err != nil {
			return nil, err
		}
		writePublishResult(map[string]any{
			"status": "packed", "package": packed.Package, "version": packed.Version,
			"artifact_digest": packed.ArtifactDigest, "platforms": packed.Platforms,
		}, params, ioctx, fmt.Sprintf("Packed %d archive(s) for immutable version %s@%s into the publication outbox.", len(files), packed.Package, packed.Version))
		return nil, nil
	}

	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	if credential.Endpoint.Registry != RegistryPut || strings.TrimSpace(credential.Endpoint.URL) == "" {
		return nil, clicore.NewError("archive credential resolved an invalid Put endpoint", clicore.ExitAuth)
	}

	res, err := publishArchivesToRegistry(context.Background(), ioctx.Client, credential, opts)
	if err != nil {
		return nil, err
	}

	writePublishResult(res, params, ioctx,
		fmt.Sprintf("Published %d archive(s) for immutable version %s@%s to %s.", len(files), res.Package, res.Version, credential.Endpoint.URL))
	return &res, nil
}

func rejectRetiredArchivePublishControls(params map[string]any) error {
	for _, flag := range []struct {
		name    string
		aliases []string
	}{
		{name: "stable"},
		{name: "archive-owner-workspace", aliases: []string{"archiveOwnerWorkspace"}},
	} {
		keys := append([]string{flag.name}, flag.aliases...)
		if clicore.Param(params, keys...) != nil {
			return clicore.NewError("--"+flag.name+" is unavailable for archive publishing; the release-set provider owns channel selection and native registry authority", clicore.ExitUsage)
		}
	}
	return nil
}

// archivePublishOptions are the inputs to publishArchivesToRegistry.
type archivePublishOptions struct {
	Namespace string
	Package   string
	Version   string
	// ArchivesDir holds the *.tar.gz files named in Files.
	ArchivesDir string
	Files       []string
	// Artifact is the (possibly --binary-name-overridden) artifact name used to
	// derive the platform key from each filename. MetaArtifact is the original
	// metadata.json artifact; when they differ the on-disk filenames are renamed
	// for display/platform parsing.
	Artifact     string
	MetaArtifact string
	// BinaryName is the raw --binary-name value, used only on the putnami/cli
	// path to locate the executable inside each archive (cliArchiveBinaryEntry).
	BinaryName string
	Template   bool
}

// publishArchivesToRegistry uploads each archive (or, for the putnami/cli
// package, the raw executable extracted from it) as a content-addressed blob,
// then publishes one immutable version without a channel. Pure of CLI plumbing
// so it is testable against an httptest server.
func publishArchivesToRegistry(ctx context.Context, client *http.Client, credential resolvedRegistryToken, opts archivePublishOptions) (ArchivePublishResult, error) {
	if err := requireCLIArchiveBinaryName(opts); err != nil {
		return ArchivePublishResult{}, err
	}
	if strings.TrimSpace(credential.Endpoint.URL) == "" || strings.TrimSpace(credential.Token) == "" {
		return ArchivePublishResult{}, clicore.NewError("archive publish credentials are incomplete", clicore.ExitAuth)
	}
	publisher, err := newPutPublisher(client, credential.Endpoint.URL, credential.Token)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	artifacts, err := collectArchiveArtifacts(opts, func(blobPath, contentType string) (string, int64, error) {
		return uploadArchiveBlobFile(ctx, publisher, opts.Namespace, opts.Package, blobPath, contentType)
	})
	if err != nil {
		return ArchivePublishResult{}, err
	}

	artifactDigest, err := publishArchiveManifest(ctx, publisher, opts.Namespace, opts.Package, opts.Version, artifacts)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	return archivePublishResult(opts, artifactDigest, artifacts), nil
}

// packArchivesIntoOutbox packs the blobs and the manifest
// publishArchivesToRegistry would publish into the publication outbox, as the
// archive member projectID owns in the plan. The blobs are the same files with
// the same media types, and the manifest is the same payload, so the artifact
// digest equals the direct path's.
func packArchivesIntoOutbox(outbox, projectID string, opts archivePublishOptions) (ArchivePublishResult, error) {
	if err := requireCLIArchiveBinaryName(opts); err != nil {
		return ArchivePublishResult{}, err
	}
	packer, err := newPutOutboxPacker(outbox)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	artifacts, err := collectArchiveArtifacts(opts, packer.addBlobFile)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	payload, err := archiveManifestPayload(artifacts)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	member, err := packer.commit(distributionproto.KindArchive, extensionproto.OutboxEcosystemArchive, opts.Namespace+"/"+opts.Package, opts.Version, projectID,
		archiveManifestMediaType, payload)
	if err != nil {
		return ArchivePublishResult{}, err
	}
	return archivePublishResult(opts, member.Put.Manifest.Digest, artifacts), nil
}

// isCLIArchive reports the putnami/cli package. Its artifact consumers
// (install.sh, install.ps1, `putnami upgrade`) expect a raw executable from
// the download route, not the extension archive, so its blobs are the
// executables extracted from each archive (cliArchiveBinaryEntry).
func isCLIArchive(opts archivePublishOptions) bool {
	return opts.Namespace == "putnami" && opts.Package == "cli"
}

func requireCLIArchiveBinaryName(opts archivePublishOptions) error {
	if isCLIArchive(opts) && opts.BinaryName == "" {
		return clicore.NewError("publishing putnami/cli requires --binary-name to locate compiled/<binary> inside each archive", clicore.ExitUsage)
	}
	return nil
}

// archiveBlobStore stores one blob file and returns the digest and size Put
// records for it: an upload, or a copy into the publication outbox.
type archiveBlobStore func(blobPath, contentType string) (digest string, size int64, err error)

// collectArchiveArtifacts stores each archive (or, for the putnami/cli
// package, the raw executable extracted from it) as a content-addressed blob
// and returns the manifest's platform map.
func collectArchiveArtifacts(opts archivePublishOptions, store archiveBlobStore) (map[string]archiveBlob, error) {
	isCLI := isCLIArchive(opts)
	renameArchives := opts.MetaArtifact != "" && opts.Artifact != opts.MetaArtifact

	artifacts := make(map[string]archiveBlob)
	var cleanup []func()
	defer func() {
		for _, c := range cleanup {
			c()
		}
	}()

	for _, f := range opts.Files {
		displayName := f
		if renameArchives {
			displayName = strings.Replace(f, opts.MetaArtifact, opts.Artifact, 1)
		}

		// The file name is the only record of an archive's platform: it picks
		// the manifest key and, for the CLI, the executable's name.
		platform := extractPlatform(displayName, opts.Artifact)
		blobPath := filepath.Join(opts.ArchivesDir, f)
		contentType := "application/gzip"
		if isCLI {
			binPath, err := extractFileFromTarGz(blobPath, cliArchiveBinaryEntry(platform, opts.BinaryName))
			if err != nil {
				return nil, clicore.NewError(fmt.Sprintf("extract CLI binary from %s: %s", f, err.Error()), clicore.ExitAPI)
			}
			cleanup = append(cleanup, func() { _ = os.Remove(binPath) })
			if err := validateCLIBinaryVersion(binPath, opts.Version); err != nil {
				return nil, clicore.NewError(fmt.Sprintf("validate CLI binary from %s: %s", f, err.Error()), clicore.ExitAPI)
			}
			blobPath = binPath
			contentType = "application/octet-stream"
		}

		digest, size, err := store(blobPath, contentType)
		if err != nil {
			return nil, err
		}

		if opts.Template {
			// Templates are platform-independent: fan the single blob out to all platforms.
			for _, p := range archivePlatforms {
				artifacts[p] = archiveBlob{Digest: digest, Size: size}
			}
			continue
		}
		if platform != "" {
			artifacts[platform] = archiveBlob{Digest: digest, Size: size}
		}
	}

	if len(artifacts) == 0 {
		return nil, clicore.NewError("no archive artifacts resolved to a platform — check the archive filenames against the artifact name", clicore.ExitAPI)
	}
	return artifacts, nil
}

func archivePublishResult(opts archivePublishOptions, artifactDigest string, artifacts map[string]archiveBlob) ArchivePublishResult {
	platforms := make(map[string]string, len(artifacts))
	for platform, artifact := range artifacts {
		platforms[strings.Replace(platform, "-", "/", 1)] = artifact.Digest
	}
	return ArchivePublishResult{
		Package:        opts.Namespace + "/" + opts.Package,
		Version:        opts.Version,
		ArtifactDigest: artifactDigest,
		Platforms:      platforms,
		Artifacts:      artifacts,
	}
}

// archiveBlobUploadAttempts bounds the retries of one blob upload. Three
// attempts with a one-second pause absorb a single gateway reset without
// hiding a registry that is actually down.
const archiveBlobUploadAttempts = 3

// archiveBlobUploadRetryPause is the wait between two upload attempts.
var archiveBlobUploadRetryPause = time.Second

// transientUploadStatus reports whether a status is a gateway-side fault the
// caller should retry: the request never reached put-server's business logic.
func transientUploadStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryableUpload reports whether a failed upload attempt may be sent again:
// a gateway-side 502/503/504, or no answer at all. A refusal put-server wrote
// and an answer outside its contract are final.
func retryableUpload(err error) bool {
	if status := clicore.ServiceStatus(err); status != 0 {
		return transientUploadStatus(status)
	}
	return !invalidPutAnswer(err)
}

// uploadArchiveBlobFile streams a file to /put/{ns}/{pkg}/blobs and returns its
// digest and size. It streams from the open file (with Content-Length) rather
// than buffering, so a large CLI binary never lands in memory. put-server
// deduplicates by digest, so re-uploading identical content is a safe no-op —
// which is also what makes a retry after a gateway reset safe.
func uploadArchiveBlobFile(ctx context.Context, publisher *putPublisher, ns, pkg, filePath, contentType string) (digest string, size int64, err error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", 0, clicore.NewError("stat "+filePath+": "+err.Error(), clicore.ExitAPI)
	}

	var (
		blob    *putserverclient.Blob
		refusal *putRefusal
	)
	for attempt := 1; ; attempt++ {
		// A streamed body is sent once, so every attempt streams from a fresh
		// handle.
		f, err := os.Open(filePath) //nolint:gosec // G304: filePath is a build-output archive selected by the publisher
		if err != nil {
			return "", 0, clicore.NewError("open "+filePath+": "+err.Error(), clicore.ExitAPI)
		}
		var callCtx context.Context
		callCtx, refusal = publisher.call(withUploadLength(ctx, info.Size()))
		blob, err = publisher.put.CreatePutBlobs(callCtx, putserverclient.CreatePutBlobsInput{
			Path:        putserverclient.CreatePutBlobsPath{Namespace: ns, Package: pkg},
			ContentType: contentType,
			Body:        f,
		})
		_ = f.Close()
		if err == nil {
			break
		}
		if attempt >= archiveBlobUploadAttempts || !retryableUpload(err) {
			if invalidPutAnswer(err) {
				return "", 0, clicore.NewError("upload archive blob: invalid response (no digest)", clicore.ExitAPI)
			}
			return "", 0, putLegError("upload archive blob", refusal, err)
		}
		// The broker or its upstream reset the connection (502/503/504, or no
		// response at all). The upload is idempotent by digest, so pause and
		// send the same bytes again rather than fail the whole publish.
		select {
		case <-ctx.Done():
			return "", 0, clicore.NewError("upload archive blob: "+ctx.Err().Error(), clicore.ExitAPI)
		case <-time.After(archiveBlobUploadRetryPause):
		}
	}
	if blob == nil || stringValue(blob.Digest) == "" {
		return "", 0, clicore.NewError("upload archive blob: invalid response (no digest)", clicore.ExitAPI)
	}
	var stored int64
	if blob.Size != nil {
		stored = *blob.Size
	}
	return *blob.Digest, stored, nil
}

// publishArchiveManifest publishes the version without moving a channel and
// returns the exact digest of the registry manifest payload. A 409 is accepted
// only after an authenticated readback proves the immutable version carries the
// same media type and byte-identical payload.
func publishArchiveManifest(ctx context.Context, publisher *putPublisher, ns, pkg, version string, artifacts map[string]archiveBlob) (string, error) {
	payload, err := archiveManifestPayload(artifacts)
	if err != nil {
		return "", err
	}
	return publishImmutablePutManifest(ctx, publisher, ns, pkg, version, archiveManifestMediaType, payload, "publish archives")
}

// archiveManifestPayload is the archive manifest payload,
// {"artifacts":{"<os>-<arch>":{"digest","size"}}}, in the canonical form Put
// stores byte for byte.
func archiveManifestPayload(artifacts map[string]archiveBlob) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{"artifacts": artifacts})
	if err != nil {
		return nil, clicore.NewError("marshal archive payload: "+err.Error(), clicore.ExitAPI)
	}
	return payload, nil
}

// ---------------------------------------------------------------------------
// Artifact discovery
// ---------------------------------------------------------------------------

// packageDir is .putnami/out/<projectPath>/package, where the framework's
// packaging step writes metadata.json and the per-channel output dirs.
func packageDir(workspaceRoot, projectPath string) string {
	return filepath.Join(realWorkspaceRoot(workspaceRoot), ".putnami", "out", filepath.FromSlash(projectPath), "package")
}

func metadataPath(workspaceRoot, projectPath string) string {
	return filepath.Join(packageDir(workspaceRoot, projectPath), "metadata.json")
}

func archivesOutputDir(workspaceRoot, projectPath string) string {
	return filepath.Join(packageDir(workspaceRoot, projectPath), "archives")
}

// realWorkspaceRoot resolves Conductor's branch-named symlink aliases to the
// real worktree so the .putnami/out path matches the build's output location.
func realWorkspaceRoot(workspaceRoot string) string {
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		return resolved
	}
	return workspaceRoot
}

// projectRelPath resolves an app token to its workspace-relative project path
// (slash-separated), the key the package output dir is addressed by.
func projectRelPath(workspaceRoot, app string) (string, error) {
	appDir, err := clicore.FindAppDir(workspaceRoot, app)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realWorkspaceRoot(workspaceRoot), appDir)
	if err != nil {
		return "", clicore.NewError("resolve project path for "+app+": "+err.Error(), clicore.ExitUsage)
	}
	return filepath.ToSlash(rel), nil
}

func readPackageMetadataFor(workspaceRoot, projectPath string) (*packageMetadata, bool, error) {
	data, err := os.ReadFile(metadataPath(workspaceRoot, projectPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, clicore.NewError("read package metadata: "+err.Error(), clicore.ExitUsage)
	}
	var m packageMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, clicore.NewError("parse package metadata: "+err.Error(), clicore.ExitUsage)
	}
	return &m, true, nil
}

func archiveArtifactName(params map[string]any, workspaceRoot, projectPath string, meta *packageMetadata) string {
	if override := clicore.StringParam(params, "binary-name", "binaryName"); override != "" {
		return override
	}
	if name, ok := extensionManifestName(workspaceRoot, projectPath); ok {
		return name
	}
	return meta.Artifact
}

func extensionManifestName(workspaceRoot, projectPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(realWorkspaceRoot(workspaceRoot), filepath.FromSlash(projectPath), "putnami.extension.json"))
	if err != nil {
		return "", false
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", false
	}
	name := strings.TrimSpace(manifest.Name)
	return name, name != ""
}

// listArchiveFiles returns the sorted *.tar.gz file names in dir, or nil when
// the directory is absent (treated as "no archives" rather than an error so the
// caller can soft-skip under --if-present).
func listArchiveFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// extractFileFromTarGz writes the regular file at targetPath inside a .tar.gz
// archive to a new temp file, returning its path. The caller removes the temp
// file. A leading "./" on the tar entry name is ignored when matching.
func extractFileFromTarGz(archivePath, targetPath string) (string, error) {
	f, err := os.Open(archivePath) //nolint:gosec // G304: archivePath is a build-output archive selected by the publisher
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gunzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	const maxBinarySize = 200 * 1024 * 1024
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if strings.TrimPrefix(hdr.Name, "./") != targetPath {
			continue
		}
		if hdr.Size > maxBinarySize {
			return "", fmt.Errorf("%s exceeds %dMB size limit", targetPath, maxBinarySize/(1024*1024))
		}
		out, err := os.CreateTemp("", "putnami-cloud-publish-bin-*")
		if err != nil {
			return "", fmt.Errorf("create temp file: %w", err)
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxBinarySize+1)) //nolint:gosec // G110: bounded by LimitReader + post-check
		_ = out.Close()
		if err != nil {
			_ = os.Remove(out.Name())
			return "", fmt.Errorf("extract %s: %w", targetPath, err)
		}
		if n > maxBinarySize {
			_ = os.Remove(out.Name())
			return "", fmt.Errorf("%s exceeded %dMB during extraction", targetPath, maxBinarySize/(1024*1024))
		}
		return out.Name(), nil
	}
	return "", fmt.Errorf("%s not found in %s", targetPath, archivePath)
}

var semverTokenRE = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?`)

func validateCLIBinaryVersion(binaryPath, expectedVersion string) error {
	expectedVersion = strings.TrimSpace(expectedVersion)
	if expectedVersion == "" {
		return fmt.Errorf("published CLI version is empty")
	}
	data, err := os.ReadFile(binaryPath) //nolint:gosec // G304: binaryPath is a temp file extracted from the selected build archive
	if err != nil {
		return fmt.Errorf("read extracted binary: %w", err)
	}
	matches := semverTokenRE.FindAll(data, -1)
	for _, match := range matches {
		if string(match) == expectedVersion {
			return nil
		}
	}

	found := dedupeVersionTokens(matches, 8)
	if len(found) == 0 {
		return fmt.Errorf("expected version token %q was not found; rebuild before publishing", expectedVersion)
	}
	return fmt.Errorf("expected version token %q was not found (found %s); rebuild before publishing", expectedVersion, strings.Join(found, ", "))
}

func dedupeVersionTokens(tokens [][]byte, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, min(len(tokens), limit))
	for _, raw := range tokens {
		token := string(raw)
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
		if len(out) == limit {
			return out
		}
	}
	return out
}

// extractPlatform derives the platform key from an archive filename:
// "<artifact>-<platform>.tar.gz" → "<platform>". Returns "" when the filename
// carries no "<artifact>-" prefix.
func extractPlatform(filename, artifact string) string {
	name := strings.TrimSuffix(filename, ".tar.gz")
	platform := strings.TrimPrefix(name, artifact+"-")
	if platform == name {
		return ""
	}
	return platform
}

// windowsExecutableSuffix is the file name suffix Windows requires of a
// program it starts.
const windowsExecutableSuffix = ".exe"

// cliArchiveBinaryEntry is the archive entry that holds the CLI executable
// called binaryName in the archive published under platform (an
// "<os>-<arch>" key from extractPlatform): compiled/<binaryName>.exe in a
// Windows archive and compiled/<binaryName> in every other one. A name that
// already ends in ".exe" keeps its spelling. This is the framework's
// pkgmeta.ExecutableName rule, the one its packager writes into a Windows
// archive (decision D-W2), so the publisher looks up exactly that entry and
// no other: a Windows archive without the .exe entry, and a Unix archive that
// holds only an .exe, are both refused.
func cliArchiveBinaryEntry(platform, binaryName string) string {
	name := binaryName
	if strings.HasPrefix(platform, "windows-") && !strings.HasSuffix(strings.ToLower(name), windowsExecutableSuffix) {
		name += windowsExecutableSuffix
	}
	return "compiled/" + name
}

// registryRef derives the registry namespace and package from an artifact name.
// Scoped "@putnami/go" → ("putnami","go"); the CLI artifact "putnami" →
// ("putnami","cli"); encoded "putnami-go" → ("putnami","go"); else
// ("putnami", artifact).
func registryRef(artifact string) (namespace, pkg string) {
	artifact = strings.TrimPrefix(artifact, "@")
	if before, after, found := strings.Cut(artifact, "/"); found {
		return before, after
	}
	if artifact == "putnami" {
		return "putnami", "cli"
	}
	if rest, ok := strings.CutPrefix(artifact, "putnami-"); ok {
		return "putnami", rest
	}
	return "putnami", artifact
}

// ArchivePackageCoordinate returns the native Put coordinate written by the
// archive publisher for an artifact name. Workspace probes use this exact
// resolver so a planned member cannot disagree with the publisher's special
// putnami CLI mapping or scoped-package handling.
func ArchivePackageCoordinate(artifact string) string {
	namespace, pkg := registryRef(artifact)
	return namespace + "/" + pkg
}

// detectCommitHash is shared by site-content publication. Archive publication
// no longer derives channels from git, but removing that authority must not
// remove the immutable source revision used by the remaining publisher.
func detectCommitHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
