package distributioncli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	putserverclient "go.putnami.dev/cloud/clients/put-server/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/releaseset"
)

// SiteContentPublishEntry is the project `publish` entry that turns a site
// directory (docs/<site-domain>/) into a project whose sections are
// release-set members. The Cloud workspace probe, the package step, and the
// publish step all read this one rule.
const SiteContentPublishEntry = "site-content"

const siteContentMemberSubject = "site-content member publication"

// SiteContentCoordinate is the Put coordinate of one section's bundle,
// "<siteContentNamespace>/<siteContentPackagePrefix><section>". The namespace
// comes from siteContentNamespace and nowhere else.
func SiteContentCoordinate(section string) string {
	return siteContentNamespace + "/" + siteContentPackagePrefix + section
}

// SiteContentSections returns the sorted section slugs of one site project
// directory: each immediate subdirectory is a section, and files directly
// under the site directory (putnami.json, README.md) are meta. A section slug
// that cannot be a mount is an error.
func SiteContentSections(siteDir string) ([]string, error) {
	sections, err := DeriveSiteContentSections(siteDir, filepath.Base(siteDir), "", map[string]string{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, section.Section)
	}
	slices.Sort(names)
	return names, nil
}

// SiteContentMemberResult is one site-content bundle Put accepted, or
// byte-confirmed, at its planned release-set version.
type SiteContentMemberResult struct {
	Coordinate     string `json:"coordinate"`
	Version        string `json:"version"`
	Channel        string `json:"channel"`
	Mount          string `json:"mount"`
	PayloadDigest  string `json:"payload_digest"`
	ArtifactDigest string `json:"artifact_digest"`
	Files          int    `json:"files"`
}

type siteContentProject struct {
	id  string // "/<workspace-relative path>", the release-set ProjectID
	dir string // absolute site directory
}

// resolveSiteContentProject finds the project a package or publish step runs
// for and reports whether it declares site-content publication.
func resolveSiteContentProject(params map[string]any, args []string, workspaceRoot string) (siteContentProject, bool, error) {
	clicore.AdoptPositionalApp(params, args)
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return siteContentProject{}, false, err
	}
	projectPath, err := projectRelPath(workspaceRoot, app)
	if err != nil {
		return siteContentProject{}, false, err
	}
	dir := filepath.Join(realWorkspaceRoot(workspaceRoot), filepath.FromSlash(projectPath))
	declared, err := declaresSiteContent(dir)
	if err != nil || !declared {
		return siteContentProject{}, false, err
	}
	id := "/" + strings.TrimPrefix(projectPath, "./")
	if projectPath == "." || projectPath == "" {
		id = "/"
	}
	return siteContentProject{id: id, dir: dir}, true, nil
}

func declaresSiteContent(projectDir string) (bool, error) {
	file := filepath.Join(projectDir, "putnami.json")
	data, err := os.ReadFile(file) //nolint:gosec // G304: the project's own putnami.json
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, clicore.NewError("read "+file+": "+err.Error(), clicore.ExitUsage)
	}
	var project struct {
		Publish []string `json:"publish"`
	}
	if err := json.Unmarshal(data, &project); err != nil {
		return false, clicore.NewError("decode "+file+": "+err.Error(), clicore.ExitUsage)
	}
	return slices.Contains(project.Publish, SiteContentPublishEntry), nil
}

func emptySectionError(sec SiteContentSection) error {
	return clicore.NewError(fmt.Sprintf(
		"section %q under docs/%s has no files — a site-content member must ship content; add a page or remove the directory",
		sec.Section, sec.Site), clicore.ExitUsage)
}

// PackageSiteContent backs the package step of a site project. It assembles
// and contract-verifies every section bundle with no network, so a broken docs
// tree fails before publication. The step's task identity is the members'
// selection fingerprint: it keys on the project tree, so a docs edit
// republishes the sections and an unchanged tree does not.
func PackageSiteContent(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) error {
	project, found, err := resolveSiteContentProject(params, args, workspaceRoot)
	if err != nil {
		return err
	}
	if !found {
		writePublishResult(map[string]any{"status": "skipped", "reason": "project declares no site-content publication"},
			params, ioctx, "No site-content publication declared — skipping the site-content package.")
		return nil
	}
	sections, err := DeriveSiteContentSections(project.dir, filepath.Base(project.dir), "", map[string]string{})
	if err != nil {
		return err
	}
	commit := clicore.FirstString(detectCommitHash(workspaceRoot), "unknown")
	bundles := make([]map[string]any, 0, len(sections))
	for _, sec := range sections {
		bundle, shipped, err := assembleSection(sec, commit)
		if err != nil {
			return err
		}
		if !shipped {
			return emptySectionError(sec)
		}
		bundles = append(bundles, map[string]any{
			"coordinate": SiteContentCoordinate(sec.Section), "mount": sec.Mount,
			"payload_digest": bundle.digestHex, "files": len(bundle.files),
		})
	}
	writePublishResult(map[string]any{"status": "packaged", "project": project.id, "count": len(bundles), "bundles": bundles},
		params, ioctx, fmt.Sprintf("Packaged %d site-content bundle(s) for %s.", len(bundles), project.id))
	return nil
}

// PublishSiteContentMembers backs the publish step of a site project. It
// publishes exactly the site-content members the release-set plan selected
// for this project, at the planned version, with the planned source revision
// in each bundle.json, and moves the plan's first channel in the same atomic
// Put publish. Without a plan it publishes nothing: `putnami cloud
// publish-doc` remains the direct operator path. Under native CI the Put
// requests go to the runner's loopback broker, which admits only these
// planned members.
func PublishSiteContentMembers(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) ([]SiteContentMemberResult, error) {
	project, found, err := resolveSiteContentProject(params, args, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if !found {
		writePublishResult(map[string]any{"status": "skipped", "reason": "project declares no site-content publication"},
			params, ioctx, "No site-content publication declared — skipping the site-content publish.")
		return nil, nil
	}
	plan, present, err := archiveReleaseSetPlan(params)
	if err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	if err := validatePlannedPublishChannels(params, plan, present, "site-content"); err != nil {
		return nil, clicore.NewError(err.Error(), clicore.ExitUsage)
	}
	if !present {
		writePublishResult(map[string]any{"status": "skipped", "reason": "no release-set plan"}, params, ioctx,
			"Site-content members publish only through a release-set plan (pass --channel); `putnami operator publish-doc` publishes directly.")
		return nil, nil
	}
	if len(plan.Channels) == 0 {
		return nil, clicore.NewError("release-set plan names no channel", clicore.ExitUsage)
	}
	channel := plan.Channels[0]
	members := plannedSiteContentMembers(plan, project.id)
	if len(members) == 0 {
		writePublishResult(map[string]any{"status": "skipped", "reason": "no selected site-content member"}, params, ioctx,
			"The release-set plan selects no site-content member of "+project.id+".")
		return nil, nil
	}

	sections, err := DeriveSiteContentSections(project.dir, filepath.Base(project.dir), "", map[string]string{})
	if err != nil {
		return nil, err
	}
	byCoordinate := make(map[string]SiteContentSection, len(sections))
	for _, sec := range sections {
		byCoordinate[SiteContentCoordinate(sec.Section)] = sec
	}
	type plannedBundle struct {
		member releaseset.PlannedMember
		bundle SiteContentBundle
	}
	planned := make([]plannedBundle, 0, len(members))
	for _, member := range members {
		sec, ok := byCoordinate[member.Coordinate]
		if !ok {
			return nil, clicore.NewError(fmt.Sprintf("release-set plan selects %s, but %s has no matching section directory", member.Coordinate, project.id), clicore.ExitUsage)
		}
		bundle, shipped, err := assembleSection(sec, member.SourceRevision)
		if err != nil {
			return nil, err
		}
		if !shipped {
			return nil, emptySectionError(sec)
		}
		planned = append(planned, plannedBundle{member: member, bundle: bundle})
	}

	if clicore.Truthy(clicore.Param(params, "dry-run", "dryRun")) {
		report := make([]map[string]any, 0, len(planned))
		for _, p := range planned {
			report = append(report, map[string]any{
				"coordinate": p.member.Coordinate, "version": p.member.Version, "channel": channel,
				"mount": p.bundle.section.Mount, "payload_digest": p.bundle.digestHex,
				"source_revision": p.member.SourceRevision, "selection_fingerprint": p.member.SelectionFingerprint,
			})
		}
		writePublishResult(map[string]any{"status": "dry-run", "count": len(report), "members": report}, params, ioctx,
			fmt.Sprintf("Dry run: %d planned site-content member(s) ready for publication on %s.", len(report), channel))
		return nil, nil
	}

	if outbox := PublicationOutbox(env); outbox != "" {
		// publication-v1: pack the planned member without a channel and report
		// no published result; the engine uploads it and the provider's
		// release moves the plan's channels.
		if len(planned) != 1 {
			return nil, clicore.NewError(fmt.Sprintf("the release-set plan selects %d site-content members of %s; a publication outbox packs exactly one", len(planned), project.id), clicore.ExitUsage)
		}
		p := planned[0]
		packed, err := packSiteContentMember(outbox, project.id, p.member, p.bundle)
		if err != nil {
			return nil, err
		}
		writePublishResult(map[string]any{
			"status": "packed", "coordinate": packed.Coordinate, "version": packed.Version, "mount": packed.Mount,
			"payload_digest": packed.PayloadDigest, "artifact_digest": packed.ArtifactDigest,
		}, params, ioctx, fmt.Sprintf("Packed site-content member %s@%s into the publication outbox.", packed.Coordinate, packed.Version))
		return nil, nil
	}

	credential, err := resolveReleaseSetProviderCredential(params, workspaceRoot, env, ioctx)
	if err != nil {
		return nil, err
	}
	if credential.Endpoint.Registry != RegistryPut || strings.TrimSpace(credential.Endpoint.URL) == "" {
		return nil, clicore.NewError("site-content credential resolved an invalid Put endpoint", clicore.ExitAuth)
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	publisher, err := newPutPublisher(ioctx.Client, credential.Endpoint.URL, credential.Token)
	if err != nil {
		return nil, err
	}
	results := make([]SiteContentMemberResult, 0, len(planned))
	for _, p := range planned {
		ns, pkg, _ := strings.Cut(p.member.Coordinate, "/")
		opts := siteContentPublishOptions{
			Namespace: ns, Package: pkg, Version: p.member.Version, Channel: channel,
			Name: p.bundle.section.Name, Mount: p.bundle.section.Mount, PayloadDigest: p.bundle.digestHex,
			ManifestJSON: p.bundle.manifestJSON, Payload: p.bundle.payload, Files: len(p.bundle.files),
			SourceRef: clicore.StringParam(params, "source-ref", "sourceRef"),
		}
		blobDigest, err := uploadSiteContentBlob(ctx, publisher, ns, pkg, opts.Payload)
		if err != nil {
			return nil, err
		}
		if got := strings.TrimPrefix(blobDigest, "sha256:"); got != opts.PayloadDigest {
			return nil, clicore.NewError(fmt.Sprintf(
				"payload digest mismatch: registry stored %s, expected %s — refusing to publish", blobDigest, opts.PayloadDigest), clicore.ExitAPI)
		}
		artifactDigest, err := publishSiteContentMember(ctx, publisher, opts, blobDigest)
		if err != nil {
			return nil, err
		}
		results = append(results, SiteContentMemberResult{
			Coordinate: p.member.Coordinate, Version: p.member.Version, Channel: channel, Mount: opts.Mount,
			PayloadDigest: opts.PayloadDigest, ArtifactDigest: artifactDigest, Files: opts.Files,
		})
	}
	writePublishResult(map[string]any{"count": len(results), "published": results}, params, ioctx,
		fmt.Sprintf("Published %d site-content member(s) on %s to %s.", len(results), channel, credential.Endpoint.URL))
	return results, nil
}

// packSiteContentMember packs the bundle archive and the registry manifest
// PublishSiteContentMembers would publish for member into the publication
// outbox. The blob digest is the one Put records for the archive bytes, so
// the manifest payload, and with it the artifact digest, equals the direct
// path's. The packed member carries no channel.
func packSiteContentMember(outbox, projectID string, member releaseset.PlannedMember, bundle SiteContentBundle) (SiteContentMemberResult, error) {
	packer, err := newPutOutboxPacker(outbox)
	if err != nil {
		return SiteContentMemberResult{}, err
	}
	opts := siteContentPublishOptions{ManifestJSON: bundle.manifestJSON, Payload: bundle.payload}
	blobDigest, err := packer.addGzipBlobBytes(bundle.payload)
	if err != nil {
		return SiteContentMemberResult{}, err
	}
	registryManifest, err := siteContentRegistryManifest(blobDigest, opts)
	if err != nil {
		return SiteContentMemberResult{}, err
	}
	packed, err := packer.commit(distributionproto.KindDoc, extensionproto.OutboxEcosystemPut, member.Coordinate, member.Version, projectID,
		siteContentManifestMediaType, registryManifest)
	if err != nil {
		return SiteContentMemberResult{}, err
	}
	return SiteContentMemberResult{
		Coordinate: member.Coordinate, Version: member.Version, Mount: bundle.section.Mount,
		PayloadDigest: bundle.digestHex, ArtifactDigest: packed.Put.Manifest.Digest, Files: len(bundle.files),
	}, nil
}

// plannedSiteContentMembers returns the selected Put members of one project
// whose coordinate is a site-content package (SiteContentCoordinate).
func plannedSiteContentMembers(plan *releaseset.Plan, projectID string) []releaseset.PlannedMember {
	var selected []releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.ProjectID != projectID || member.Ecosystem != distributionproto.Ecosystem("put") ||
			!strings.HasPrefix(member.Coordinate, siteContentNamespace+"/"+siteContentPackagePrefix) {
			continue
		}
		selected = append(selected, member)
	}
	return selected
}

// publishSiteContentMember publishes one planned version and moves the plan
// channel in the same atomic Put publish, then returns the digest of the
// exact stored manifest bytes (the member's artifact digest). A 409 means this
// version already exists, which only an earlier attempt of the same plan can
// cause: the stored bytes are read back and must match, and no channel is
// moved outside the atomic publish.
func publishSiteContentMember(ctx context.Context, publisher *putPublisher, opts siteContentPublishOptions, blobDigest string) (string, error) {
	registryManifest, err := siteContentRegistryManifest(blobDigest, opts)
	if err != nil {
		return "", err
	}
	callCtx, refusal := publisher.call(ctx)
	stored, err := publisher.put.CreatePutPublish(callCtx, siteContentPublishInput(opts, registryManifest))
	if clicore.ServiceStatus(err) == http.StatusConflict {
		return readBackImmutablePutManifest(ctx, publisher, opts.Namespace, opts.Package, opts.Version,
			siteContentManifestMediaType, registryManifest, siteContentMemberSubject)
	}
	if invalidPutAnswer(err) {
		return "", clicore.NewError(siteContentMemberSubject+" returned an invalid atomic publish response", clicore.ExitAPI)
	}
	if err != nil {
		return "", putLegError(siteContentMemberSubject, refusal, err)
	}
	return verifySiteContentPublishResponse(stored, opts.Namespace, opts.Package, opts.Version, registryManifest)
}

func verifySiteContentPublishResponse(stored *putserverclient.AtomicPublishResponse, ns, pkg, version string, payload []byte) (string, error) {
	if stored == nil || stored.Version == nil || stored.Manifest == nil {
		return "", clicore.NewError(siteContentMemberSubject+" returned an invalid atomic publish response", clicore.ExitAPI)
	}
	storedVersion, storedManifest := stored.Version, stored.Manifest
	if stringValue(stored.Package) != ns+"/"+pkg || stringValue(storedVersion.Version) != version || stringValue(storedVersion.ManifestId) == "" ||
		stringValue(storedManifest.Id) != stringValue(storedVersion.ManifestId) ||
		stringValue(storedManifest.PackageId) != stringValue(storedVersion.PackageId) {
		return "", clicore.NewError(siteContentMemberSubject+" returned a different immutable package or version identity", clicore.ExitAPI)
	}
	return verifyImmutablePutManifest(storedManifest, siteContentManifestMediaType, payload, siteContentMemberSubject)
}
