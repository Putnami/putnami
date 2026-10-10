package distributioncli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/sitecontent"
)

// siteContentManifestMediaType is the manifest media type put-server stores for
// a site-content bundle publish. put-server stores it verbatim (no allowlist).
//
// The stored manifest is a put-native registry wrapper, NOT the raw contract
// bundle.json:
//
//	{"artifact":{"blob":"<payloadDigest>","mediaType":"application/gzip","size":N},
//	 "bundle": <site-content-bundle/v1 bundle.json>}
//
// The wrapper exists because put-server records package blob membership only
// through a published manifest that references the blob (artifact.blob or
// artifacts.*.digest — see put-server's packageReferencesBlob); a put-native
// blob upload is not linked to its package. Carrying the payload digest in the
// recognized artifact.blob shape is what makes
// /put/<namespace>/<package>/blobs/<payloadDigest> readable after publish (and
// lets a consumer rediscover the payload digest from the registry). The
// authoritative bundle.json travels INSIDE the archive blob (the contract's
// in-tar manifest entry, so one pinned digest covers manifest + payload); the
// wrapper's bundle sub-object is a strict-valid convenience copy for
// registry-side discovery, not what the consumer verifies.
const siteContentManifestMediaType = "application/vnd.putnami.sitecontent.bundle+json"

// siteContentBlobMediaType is the media type of the bundle archive blob: the
// Content-Type of its upload and the mediaType the registry wrapper records.
const siteContentBlobMediaType = "application/gzip"

// siteContentNamespace is the Put namespace site-content bundles publish
// under. It is the Cloud workspace's own namespace: Cloud owns the docs
// artifact and the putnami.dev site only consumes it.
const siteContentNamespace = "cloud"

// siteContentPackagePrefix names each section's package inside
// siteContentNamespace: section <s> publishes as package "doc-contents-<s>".
const siteContentPackagePrefix = "doc-contents-"

// Derivation constants.
//
//	siteContentDocRoot — every Putnami site serves its doc sections under this
//	  URL prefix, so a section <s> mounts at "/docs/<s>".
//	siteContentDefaultRepo — the source repository every bundle manifest
//	  records.
//
// Discovery of the docs/<site-domain>/ trees does not happen here.
const (
	siteContentDocRoot     = "/docs"
	siteContentDefaultRepo = "putnami-cloud"
)

// SiteContentPublishResult is one published bundle reference the command reports.
type SiteContentPublishResult struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Channel string `json:"channel"`
	Name    string `json:"name"`
	Mount   string `json:"mount"`
	// PayloadDigest is the sha256 bare-hex digest of the tar.gz payload — the
	// content address a consumer pins. Version is derived from it, so identical
	// content publishes an identical version.
	PayloadDigest string `json:"payload_digest"`
	// BlobDigest is the digest put-server returned for the uploaded payload
	// (its own "sha256:"-prefixed form), verified against PayloadDigest.
	BlobDigest string `json:"blob_digest"`
	Files      int    `json:"files"`
}

// SiteContentSection is one discovered publishable section:
// docs/<Site>/<Section>/ → bundle Name mounted at Mount, published as package
// <siteContentNamespace>/<Package>. Name == Section; Package is
// siteContentPackagePrefix + Section.
type SiteContentSection struct {
	Site    string // site-domain directory, e.g. "putnami.dev"
	Section string // section slug, e.g. "platform"
	Dir     string // absolute source directory
	Name    string // bundle manifest name
	Package string // registry package inside siteContentNamespace, e.g. "doc-contents-platform"
	Mount   string // site URL prefix, "/docs/<section>"
}

// SiteContentBundle is a section assembled + contract-validated in memory,
// ready to publish (or report under --dry-run). Assembling every section
// before any upload keeps a run all-or-nothing on validation errors.
type SiteContentBundle struct {
	section      SiteContentSection
	manifest     *sitecontent.Manifest
	manifestJSON []byte
	payload      []byte
	files        []siteContentFile
	digestHex    string
	version      string
}

// Section returns the section the bundle was assembled from.
func (b SiteContentBundle) Section() SiteContentSection { return b.section }

// Version returns the bundle's version: the first 12 hex characters of its
// payload digest for a direct publish.
func (b SiteContentBundle) Version() string { return b.version }

// PayloadDigest returns the sha256 bare-hex digest of the bundle archive.
func (b SiteContentBundle) PayloadDigest() string { return b.digestHex }

// FilePaths returns the payload paths of the bundle's files, in archive
// order.
func (b SiteContentBundle) FilePaths() []string {
	paths := make([]string, 0, len(b.files))
	for _, f := range b.files {
		paths = append(paths, f.payloadPath)
	}
	return paths
}

// AssembleSiteContentSection collects one section and assembles its
// deterministic site-content-bundle/v1 archive (tar.gz with bundle.json
// embedded at the root, per the contract's in-tar manifest entry), versioned
// by its content address: identical content gives an identical version. The
// bundle self-validates against the go.putnami.dev/protocol/sitecontent
// contract before it returns, so a non-conformant bundle never reaches an
// upload. shipped is false for a section with no regular files.
func AssembleSiteContentSection(sec SiteContentSection, commit string) (bundle SiteContentBundle, shipped bool, err error) {
	bundle, shipped, err = assembleSection(sec, commit)
	if err != nil || !shipped {
		return SiteContentBundle{}, shipped, err
	}
	bundle.version = bundle.digestHex[:12]
	return bundle, true, nil
}

// PublishSiteContentBundles publishes each bundle as the content-addressed
// registry package cloud/doc-contents-<section> on channel, and returns the
// Put base URL with one result per bundle. A version that already exists
// moves the channel instead (the idempotent same-content republish).
//
// It uses the same credential seam as archive publication: a stored machine
// credential for the Put host, else the native runner's loopback publication
// broker (PUTNAMI_REGISTRY_PUT_URL + the run's local capability), else a
// minted user registry token. Under native CI the broker holds the upstream
// bearer and admits only planned members.
func PublishSiteContentBundles(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO, bundles []SiteContentBundle, channel string) (string, []SiteContentPublishResult, error) {
	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return "", nil, err
	}
	if credential.Endpoint.Registry != RegistryPut || strings.TrimSpace(credential.Endpoint.URL) == "" {
		return "", nil, clicore.NewError("site-content credential resolved an invalid Put endpoint", clicore.ExitAuth)
	}
	baseURL, token := credential.Endpoint.URL, credential.Token

	results := make([]SiteContentPublishResult, 0, len(bundles))
	for _, p := range bundles {
		res, err := publishSiteContentToRegistry(context.Background(), ioctx.Client, baseURL, token, siteContentPublishOptions{
			Namespace:     siteContentNamespace,
			Package:       p.section.Package,
			Version:       p.version,
			Channel:       channel,
			Name:          p.section.Name,
			Mount:         p.section.Mount,
			PayloadDigest: p.digestHex,
			ManifestJSON:  p.manifestJSON,
			Payload:       p.payload,
			Files:         len(p.files),
			SourceRef:     clicore.StringParam(params, "source-ref", "sourceRef"),
		})
		if err != nil {
			return "", nil, err
		}
		results = append(results, res)
	}
	return baseURL, results, nil
}

// DeriveSiteContentSections derives the sections of one site directory: each
// immediate subdirectory is a section, and files directly under the site
// directory are meta. When only is non-empty, sections are filtered to that
// slug. byPackage carries the cross-site duplicate guard: it maps each claimed
// section slug to its owning "site/section". The result keeps directory
// order; callers sort.
func DeriveSiteContentSections(siteDir, site, only string, byPackage map[string]string) ([]SiteContentSection, error) {
	subEntries, err := os.ReadDir(siteDir)
	if err != nil {
		return nil, clicore.NewError("read site directory "+siteDir+": "+err.Error(), clicore.ExitUsage)
	}
	var sections []SiteContentSection
	for _, sectionEntry := range subEntries {
		// A dot-directory is tooling state, never a section: once a site
		// directory is a project, putnami writes .gen/ into it.
		if !sectionEntry.IsDir() || strings.HasPrefix(sectionEntry.Name(), ".") {
			continue
		}
		section := sectionEntry.Name()
		if only != "" && section != only {
			continue
		}
		mount := siteContentDocRoot + "/" + section
		if !sitecontent.ValidMountPrefix(mount) {
			return nil, clicore.NewError(fmt.Sprintf(
				"section %q (under docs/%s) maps to invalid mount %q — section directories must be lowercase URL-safe slugs (letters, digits, '.', '_', '-'); NN- ordering prefixes belong on pages inside a section, not on the section directory",
				section, site, mount), clicore.ExitUsage)
		}
		if owner, dup := byPackage[section]; dup {
			return nil, clicore.NewError(fmt.Sprintf(
				"duplicate section %q: %s/%s and %s both publish package %s/%s%s — section slugs must be unique across sites",
				section, site, section, owner, siteContentNamespace, siteContentPackagePrefix, section), clicore.ExitUsage)
		}
		byPackage[section] = site + "/" + section
		sections = append(sections, SiteContentSection{
			Site:    site,
			Section: section,
			Dir:     filepath.Join(siteDir, section),
			Name:    section,
			Package: siteContentPackagePrefix + section,
			Mount:   mount,
		})
	}
	return sections, nil
}

// assembleSection collects one section and assembles its bundle. shipped is
// false for a section with no regular files. assembleSiteContentBundle fails
// closed: BuildArchive self-verifies the archive (manifest strict-parse +
// validation + every entry) before returning, so a non-conformant bundle
// never reaches an upload.
func assembleSection(sec SiteContentSection, commit string) (bundle SiteContentBundle, shipped bool, err error) {
	files, err := collectSiteContentFiles(sec.Dir, sec.Mount)
	if err != nil {
		return SiteContentBundle{}, false, err
	}
	if len(files) == 0 {
		return SiteContentBundle{}, false, nil
	}
	manifest, manifestJSON, payload, err := assembleSiteContentBundle(files, sec.Name, sec.Mount, siteContentDefaultRepo, commit)
	if err != nil {
		return SiteContentBundle{}, false, err
	}
	sum := sha256.Sum256(payload)
	return SiteContentBundle{
		section:      sec,
		manifest:     manifest,
		manifestJSON: manifestJSON,
		payload:      payload,
		files:        files,
		digestHex:    hex.EncodeToString(sum[:]),
	}, true, nil
}

// siteContentFile is one collected source file: its payload-relative path
// (already prefixed to serve under the mount) and its absolute source path.
type siteContentFile struct {
	payloadPath string
	absPath     string
}

// collectSiteContentFiles walks the source tree, collecting regular files only,
// mapped into the mount's URL space and sorted lexically by payload path (the
// deterministic entry order of the payload). A payload file at path p is served
// at "/"+p, so source-relative paths are prefixed with the mount (minus its
// leading "/") — e.g. index.md under mount /docs/platform → docs/platform/index.md.
// An absent source dir returns (nil, nil) so the caller can soft-skip under
// --if-present. Symlinks and other irregular entries are rejected: the contract
// forbids them in the payload, and following them silently would smuggle
// content from outside the designated tree.
func collectSiteContentFiles(sourceDir, mount string) ([]siteContentFile, error) {
	var files []siteContentFile
	err := walkSiteContentFiles(sourceDir, mount, func(file siteContentFile) {
		files = append(files, file)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].payloadPath < files[j].payloadPath })
	return files, nil
}

// assembleSiteContentBundle builds the bundle.json manifest and the canonical
// publishable archive for the collected files (already sorted). Assembly is
// delegated to the contract's own sitecontent.BuildArchive: a deterministic
// tar.gz that embeds bundle.json at the archive root — so the ONE pinned
// payload digest covers manifest + payload together — and self-verifies with
// VerifyArchive before returning, so a non-conformant bundle can never leave
// the machine. The returned manifestJSON is the compact copy carried in the
// put-registry wrapper for registry-side discovery; the in-archive bundle.json
// is what the consumer verifies.
func assembleSiteContentBundle(files []siteContentFile, name, mount, repo, commit string) (*sitecontent.Manifest, []byte, []byte, error) {
	manifestFiles := make([]sitecontent.File, 0, len(files))
	payloadFiles := make(map[string][]byte, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.absPath) //nolint:gosec // G304: absPath was enumerated from the user-designated source tree
		if err != nil {
			return nil, nil, nil, clicore.NewError("read "+f.absPath+": "+err.Error(), clicore.ExitAPI)
		}
		payloadFiles[f.payloadPath] = data
		sum := sha256.Sum256(data)
		manifestFiles = append(manifestFiles, sitecontent.File{
			Path:   f.payloadPath,
			Digest: hex.EncodeToString(sum[:]),
		})
	}

	manifest := &sitecontent.Manifest{
		Schema:        sitecontent.SchemaID,
		FormatVersion: sitecontent.FormatVersion,
		Name:          name,
		Mounts:        []sitecontent.Mount{{URLPrefix: mount}},
		Source:        sitecontent.Source{Repo: repo, Commit: commit},
		Files:         manifestFiles,
	}
	payload, diags := sitecontent.BuildArchive(manifest, payloadFiles)
	if diag.HasErrors(diags) {
		return nil, nil, nil, clicore.NewError("assemble site-content archive:\n"+formatDiagnostics(diags), clicore.ExitAPI)
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, nil, nil, clicore.NewError("marshal bundle manifest: "+err.Error(), clicore.ExitAPI)
	}
	return manifest, manifestJSON, payload, nil
}

// formatDiagnostics renders contract diagnostics one per line for a fail-closed
// error message.
func formatDiagnostics(diags []diag.Diagnostic) string {
	lines := make([]string, 0, len(diags))
	for _, d := range diags {
		lines = append(lines, "  - "+d.String())
	}
	return strings.Join(lines, "\n")
}

// siteContentPublishOptions are the inputs to publishSiteContentToRegistry.
type siteContentPublishOptions struct {
	Namespace     string
	Package       string
	Version       string
	Channel       string
	Name          string
	Mount         string
	PayloadDigest string // sha256 bare-hex of Payload, precomputed by the caller
	ManifestJSON  []byte // bundle.json, already contract-validated
	Payload       []byte // tar.gz
	Files         int
	// SourceRef is the caller-supplied opaque provenance ref (--source-ref)
	// recorded on the channel move this publish performs.
	SourceRef string
}

// publishSiteContentToRegistry uploads the tar.gz payload as a content-addressed
// blob, then atomic-publishes the bundle.json manifest on the channel — falling
// back to a channel-pointer PUT when the version already exists (HTTP 409, the
// idempotent same-content republish). Pure of CLI plumbing so it is testable
// against an httptest server.
func publishSiteContentToRegistry(ctx context.Context, client *http.Client, baseURL, token string, opts siteContentPublishOptions) (SiteContentPublishResult, error) {
	publisher, err := newPutPublisher(client, baseURL, token)
	if err != nil {
		return SiteContentPublishResult{}, err
	}
	blobDigest, err := uploadSiteContentBlob(ctx, publisher, opts.Namespace, opts.Package, opts.Payload)
	if err != nil {
		return SiteContentPublishResult{}, err
	}
	// The version is the payload's content address: refuse to publish if the
	// registry's digest for the uploaded bytes disagrees with what we hashed.
	if got := strings.TrimPrefix(blobDigest, "sha256:"); got != opts.PayloadDigest {
		return SiteContentPublishResult{}, clicore.NewError(fmt.Sprintf(
			"payload digest mismatch: registry stored %s, expected %s — refusing to publish a version that does not address its content",
			blobDigest, opts.PayloadDigest), clicore.ExitAPI)
	}

	if err := publishSiteContentManifest(ctx, publisher, opts, blobDigest); err != nil {
		return SiteContentPublishResult{}, err
	}

	return SiteContentPublishResult{
		Package:       opts.Namespace + "/" + opts.Package,
		Version:       opts.Version,
		Channel:       opts.Channel,
		Name:          opts.Name,
		Mount:         opts.Mount,
		PayloadDigest: opts.PayloadDigest,
		BlobDigest:    blobDigest,
		Files:         opts.Files,
	}, nil
}

// uploadSiteContentBlob POSTs the tar.gz payload to /put/{ns}/{pkg}/blobs.
// put-server deduplicates by digest, so re-uploading identical content is a
// safe no-op server-side.
func uploadSiteContentBlob(ctx context.Context, publisher *putPublisher, ns, pkg string, payload []byte) (digest string, err error) {
	callCtx, refusal := publisher.call(withUploadLength(ctx, int64(len(payload))))
	blob, err := publisher.put.CreatePutBlobs(callCtx, putserverclient.CreatePutBlobsInput{
		Path:        putserverclient.CreatePutBlobsPath{Namespace: ns, Package: pkg},
		ContentType: siteContentBlobMediaType,
		Body:        bytes.NewReader(payload),
	})
	if invalidPutAnswer(err) {
		return "", clicore.NewError("upload site-content blob: invalid response (no digest)", clicore.ExitAPI)
	}
	if err != nil {
		return "", putLegError("upload site-content blob", refusal, err)
	}
	if blob == nil || stringValue(blob.Digest) == "" {
		return "", clicore.NewError("upload site-content blob: invalid response (no digest)", clicore.ExitAPI)
	}
	return *blob.Digest, nil
}

// publishSiteContentManifest atomic-publishes the registry manifest for the
// channel. The published payload is the put-native wrapper
// ({"artifact":{"blob":<payloadDigest>,...},"bundle":<bundle.json>}) — see
// siteContentManifestMediaType for why the raw bundle.json alone would leave the
// payload blob unreadable. blobDigest is the digest put-server returned for the
// uploaded payload; it must be carried verbatim so the artifact.blob reference
// matches the blob's package-membership check (mirroring the archives publisher).
// A 409 means the version (content) is already published; the channel pointer is
// then moved instead (idempotent retag). Like the archives publisher the retag is
// NOT best-effort: the runtime overlay resolves this content through the channel,
// so a bundle published under a channel that never moved is a silent miss and
// fails the command. Both legs are idempotent, so a retry converges.
func publishSiteContentManifest(ctx context.Context, publisher *putPublisher, opts siteContentPublishOptions, blobDigest string) error {
	registryManifest, err := siteContentRegistryManifest(blobDigest, opts)
	if err != nil {
		return err
	}
	callCtx, refusal := publisher.call(ctx)
	_, err = publisher.put.CreatePutPublish(callCtx, siteContentPublishInput(opts, registryManifest))
	if clicore.ServiceStatus(err) == http.StatusConflict {
		if opts.Channel == "" {
			return nil // version-only publish already exists — idempotent
		}
		if cerr := updateSiteContentChannel(ctx, publisher, opts.Namespace, opts.Package, opts.Version, opts.Channel, opts.SourceRef); cerr != nil {
			return clicore.NewError(fmt.Sprintf(
				"channel %q not repointed to %s (the bundle is published, the channel is not): %s",
				opts.Channel, opts.Version, cerr.Error()), clicore.ExitAPI)
		}
		return nil
	}
	if err != nil {
		return putLegError("publish site-content bundle", refusal, err)
	}
	return nil
}

// siteContentPublishInput is the atomic publish of one site-content bundle:
// its registry manifest, the channel it moves, and the caller's source ref.
func siteContentPublishInput(opts siteContentPublishOptions, registryManifest []byte) putserverclient.CreatePutPublishInput {
	version, mediaType, channel, sourceRef := opts.Version, siteContentManifestMediaType, opts.Channel, opts.SourceRef
	return putserverclient.CreatePutPublishInput{
		Path: putserverclient.CreatePutPublishPath{Namespace: opts.Namespace, Package: opts.Package},
		Body: putserverclient.AtomicPublishRequest{
			Version: &version, MediaType: &mediaType, Payload: json.RawMessage(registryManifest),
			Channel: &channel, SourceRef: &sourceRef,
		},
	}
}

// siteContentRegistryManifest is the put-native wrapper both publish paths
// store: {"artifact":{"blob",...},"bundle":<bundle.json>}. blobDigest is the
// digest put-server returned for the uploaded payload, carried verbatim so the
// artifact.blob reference passes the blob's package-membership check.
func siteContentRegistryManifest(blobDigest string, opts siteContentPublishOptions) ([]byte, error) {
	registryManifest, err := json.Marshal(map[string]any{
		"artifact": map[string]any{
			"blob":      blobDigest,
			"mediaType": siteContentBlobMediaType,
			"size":      len(opts.Payload),
		},
		"bundle": json.RawMessage(opts.ManifestJSON),
	})
	if err != nil {
		return nil, clicore.NewError("marshal site-content registry manifest: "+err.Error(), clicore.ExitAPI)
	}
	return registryManifest, nil
}

// updateSiteContentChannel converges the site-content pointer after an
// idempotent version collision. Site content remains an active channel-based
// surface; archive and migration publishers never call this helper.
func updateSiteContentChannel(ctx context.Context, publisher *putPublisher, ns, pkg, version, channel, sourceRef string) error {
	callCtx, refusal := publisher.call(ctx)
	_, err := publisher.put.UpdatePutChannels(callCtx, putserverclient.UpdatePutChannelsInput{
		Path: putserverclient.UpdatePutChannelsPath{Namespace: ns, Package: pkg, Channel: channel},
		Body: putserverclient.UpdateChannelRequest{Version: &version, SourceRef: &sourceRef},
	})
	if err != nil {
		return putLegError("update channel "+channel, refusal, err)
	}
	return nil
}
