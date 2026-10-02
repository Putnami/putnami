package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	put "go.putnami.dev/protocol/put"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
	"go.putnami.dev/sdk/extension/putpublish"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// publicationOpenKey is the plan key of the node that opens the plan.
const publicationOpenKey = "putnami:publish~open"

// publicationUploadStep is the step id of every upload node.
const publicationUploadStep = "upload"

// A native publication run exports one loopback registry route per registry
// kind, named PUTNAMI_REGISTRY_<KIND>_URL (the extension SDK's privatebroker),
// which a publication job uses when it uploads itself. A job that packs into
// an outbox receives none of them, whatever the kind. PUTNAMI_REGISTRY_URL,
// which names no kind, is the download mirror and is not a route.
const (
	registryRouteEnvPrefix = "PUTNAMI_REGISTRY_"
	registryRouteEnvSuffix = "_URL"
)

// outboxJobWithheldEnv is every other variable through which a publication
// job could reach a registry or cloud credential. A job that packs into an
// outbox receives none of them.
var outboxJobWithheldEnv = []string{
	extproto.CloudTokenEnv,
	extproto.JobCredentialFDEnv,
	extproto.CloudCapabilityAfterEnv,
	InternalReleasePlanCallbackEnv,
	InternalReleaseSetProviderCapabilityEnv,
	runtimeproto.ReleaseSetPublishedImagesFileEnv,
	runtimeproto.ReleaseSetMembersFileEnv,
}

// withheldFromOutboxJob reports whether a job that packs into an outbox is
// denied the variable name: a registry route of any kind, or a variable
// outboxJobWithheldEnv lists. Names compare the way the platform's process
// environment compares them.
func withheldFromOutboxJob(name string) bool {
	canonical := envkeys.Host.Canonical(name)
	if len(canonical) > len(registryRouteEnvPrefix)+len(registryRouteEnvSuffix) &&
		strings.HasPrefix(canonical, registryRouteEnvPrefix) && strings.HasSuffix(canonical, registryRouteEnvSuffix) {
		return true
	}
	return slices.ContainsFunc(outboxJobWithheldEnv, func(key string) bool {
		return envkeys.Host.Canonical(key) == canonical
	})
}

// publicationRunContextKey carries the run whose publication jobs pack into
// outboxes (PublicationContext).
type publicationRunContextKey struct{}

// PublicationContext gives every selected publication job of a publication-v1
// run a private outbox directory, mode 0700, and returns ctx carrying them
// with the function that removes them. The job finds its directory in
// PUTNAMI_PUBLICATION_OUTBOX and no credential in its environment
// (scopeProcessCapabilities). A run without a publication provider returns
// ctx unchanged.
func (run *ReleaseSetRun) PublicationContext(ctx context.Context) (context.Context, func(), error) {
	if !run.Publication() || len(run.publishJobKeys) == 0 {
		return ctx, func() {}, nil
	}
	root, err := os.MkdirTemp("", "putnami-publication-")
	if err != nil {
		return ctx, func() {}, fmt.Errorf("create the publication outboxes: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	outboxes := make(map[string]string, len(run.publishJobKeys))
	for index, key := range slices.Sorted(maps.Keys(run.publishJobKeys)) {
		if _, upload := run.uploads[key]; upload {
			continue
		}
		dir := filepath.Join(root, strconv.Itoa(index))
		if err := os.Mkdir(dir, 0o700); err != nil {
			cleanup()
			return ctx, func() {}, fmt.Errorf("create the publication outboxes: %w", err)
		}
		outboxes[key] = dir
	}
	run.outboxes = outboxes
	return context.WithValue(ctx, publicationRunContextKey{}, run), cleanup, nil
}

// publicationOutboxFor is the outbox job packs into, "" when it packs into
// none.
func publicationOutboxFor(ctx context.Context, job *ScheduledJob) string {
	if ctx == nil || job == nil || job.JobDef == nil || job.Project == nil {
		return ""
	}
	run, _ := ctx.Value(publicationRunContextKey{}).(*ReleaseSetRun)
	if run == nil {
		return ""
	}
	return run.outboxes[job.Key()]
}

// publicationOutboxEnv is env without any credential or registry route, and
// with the outbox the job packs into as its only PUTNAMI_PUBLICATION_OUTBOX.
func publicationOutboxEnv(env []string, outbox string) []string {
	kept := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); !withheldFromOutboxJob(name) {
			kept = append(kept, entry)
		}
	}
	return envkeys.Host.Set(kept, extproto.PublicationOutboxEnv, outbox)
}

// attachPublication adds the publication-v1 nodes to the plan. One open node
// waits for the barrier: the tasks of the bound request's barrier commands,
// or, for a local run, every task that neither publishes nor depends on a
// publication. One upload node per selected publication job waits for that
// job and for open; it uploads what the job packed. A deploy then waits for
// the release, as without publication-v1.
func (run *ReleaseSetRun) attachPublication(planned []*ScheduledJob) ([]*ScheduledJob, map[string]InternalJobRunner, error) {
	if run.open == nil {
		return nil, nil, errors.New("attach the publication: the plan was not bound before planning ended")
	}
	for _, member := range run.plan.SelectedMembers() {
		if err := admitPublicationMember(member, run.publicationKind(member)); err != nil {
			return nil, nil, err
		}
	}
	byKey := jobsByPlanKey(planned)
	open := &ScheduledJob{
		Project:   releaseSetInternalProject,
		Extension: releaseSetInternalExtension,
		DependsOn: run.openDependencies(schedulableJobs(planned)),
		JobDef: &extension.JobDefinition{
			ExtensionName: releaseSetInternalExtension.Name,
			Name:          "publish~open",
			CommandName:   "publish",
			StepID:        "open",
			Kind:          "release-set",
			Traits:        extension.CommandTraits{SideEffects: extension.SideEffectsCloud},
		},
	}
	if open.Key() != publicationOpenKey {
		return nil, nil, fmt.Errorf("publication open identity = %q, want %q", open.Key(), publicationOpenKey)
	}
	if _, collides := byKey[open.Key()]; collides {
		return nil, nil, fmt.Errorf("publication open key %q collides with a planned job", open.Key())
	}
	runners := map[string]InternalJobRunner{open.Key(): run.openRunner(open)}
	planned = append(planned, open)
	run.uploads = make(map[string]string, len(run.publishJobKeys))
	for _, publishKey := range slices.Sorted(maps.Keys(run.publishJobKeys)) {
		publish := byKey[publishKey]
		if publish == nil {
			return nil, nil, fmt.Errorf("selected publication job %q is not in the plan", publishKey)
		}
		upload := &ScheduledJob{
			Project:   publish.Project,
			Extension: releaseSetInternalExtension,
			DependsOn: dedupeSorted([]string{open.Key(), publishKey}),
			JobDef: &extension.JobDefinition{
				ExtensionName: releaseSetInternalExtension.Name,
				Name:          publish.PlanName() + "~" + publicationUploadStep,
				CommandName:   "publish",
				StepID:        publicationUploadStep,
				Kind:          "release-set",
				Traits:        extension.CommandTraits{SideEffects: extension.SideEffectsRegistry},
			},
		}
		if _, collides := byKey[upload.Key()]; collides {
			return nil, nil, fmt.Errorf("publication upload key %q collides with a planned job", upload.Key())
		}
		run.uploads[upload.Key()] = publishKey
		runners[upload.Key()] = run.uploadRunner(upload, publish, run.publishJobKeys[publishKey])
		planned = append(planned, upload)
	}
	for uploadKey, publishKey := range run.uploads {
		run.publishJobKeys[uploadKey] = run.publishJobKeys[publishKey]
	}
	planned, barrier, err := run.attachDeployBarrier(planned)
	if err != nil {
		return nil, nil, err
	}
	maps.Copy(runners, barrier)
	if err := validatePlanDAG(schedulableJobs(planned)); err != nil {
		return nil, nil, fmt.Errorf("attach the publication: %w", err)
	}
	return planned, runners, nil
}

// publicationKind is the kind the engine uploads member as: the kind its
// declared package and publish steps give it (releaseset.KindFor), which is
// the kind a plan with member attribution records. It does not depend on
// that opt-in.
func (run *ReleaseSetRun) publicationKind(member releaseset.PlannedMember) distribution.MemberKind {
	route := run.routes[releaseset.MemberKey(member.Ecosystem, member.Coordinate)]
	return releaseset.KindFor(member.Ecosystem, route.packageStep, route.publishStep)
}

// admitPublicationMember refuses, before open, a selected member the engine
// has no upload for: an ecosystem outside npm, go, oci, put and archive, or a
// Put registry member of a kind put-write/v1 does not publish. A release
// archive is the archive kind of the archive ecosystem; a config, migration
// or doc member belongs to the put ecosystem.
func admitPublicationMember(member releaseset.PlannedMember, kind distribution.MemberKind) error {
	key := printableReleaseKey(releaseset.MemberKey(member.Ecosystem, member.Coordinate))
	switch ecosystem := string(member.Ecosystem); ecosystem {
	case extproto.OutboxEcosystemNPM, extproto.OutboxEcosystemGo, extproto.OutboxEcosystemOCI:
		return nil
	case extproto.OutboxEcosystemPut, extproto.OutboxEcosystemArchive:
		_, published := put.ProfileFor(kind)
		if published && (ecosystem == extproto.OutboxEcosystemArchive) == (kind == distribution.KindArchive) {
			return nil
		}
		return fmt.Errorf("selected member %s: publication-v1 uploads no %s member of kind %q", key, ecosystem, kind)
	}
	return fmt.Errorf("selected member %s: publication-v1 uploads %s, %s, %s, %s and %s members only", key,
		extproto.OutboxEcosystemNPM, extproto.OutboxEcosystemGo, extproto.OutboxEcosystemOCI,
		extproto.OutboxEcosystemPut, extproto.OutboxEcosystemArchive)
}

// openDependencies is what open waits for. With a bound barrier, it is every
// task of a barrier command. Without one, it is every task that neither
// publishes nor depends on a task that does, so every check the run makes
// finishes before the provider issues a publish credential.
func (run *ReleaseSetRun) openDependencies(planned []*ScheduledJob) []string {
	var keys []string
	if len(run.barrierCommands) > 0 {
		for _, job := range planned {
			if job != nil && job.JobDef != nil && slices.Contains(run.barrierCommands, job.CommandName()) {
				keys = append(keys, job.Key())
			}
		}
		return dedupeSorted(keys)
	}
	byKey := jobsByPlanKey(planned)
	publishes := make(map[string]bool, len(planned))
	var reaches func(key string, visiting map[string]bool) bool
	reaches = func(key string, visiting map[string]bool) bool {
		if known, ok := publishes[key]; ok {
			return known
		}
		job := byKey[key]
		if job == nil || job.JobDef == nil || visiting[key] {
			return false
		}
		visiting[key] = true
		result := slices.Contains(runner.PublicationCommands, job.CommandName()) || hasCloudCapabilitySideEffects(job)
		for _, dependency := range job.DependsOn {
			if result {
				break
			}
			result = reaches(dependency, visiting)
		}
		publishes[key] = result
		return result
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.Project == nil {
			continue
		}
		if !reaches(job.Key(), map[string]bool{}) {
			keys = append(keys, job.Key())
		}
	}
	return dedupeSorted(keys)
}

// openRunner opens the plan once every task open waits for succeeded and
// nothing in the run failed.
func (run *ReleaseSetRun) openRunner(open *ScheduledJob) InternalJobRunner {
	return func(ctx context.Context, results map[string]*JobResult) *JobResult {
		if waiting := unsatisfiedDependency(open, results); waiting != "" {
			return &JobResult{Status: "skipped", Error: &JobError{Message: fmt.Sprintf("the plan is not opened: %s did not succeed", waiting)}}
		}
		for key, result := range results {
			if result != nil && (result.Status == "failed" || result.Status == "canceled") {
				return &JobResult{Status: "skipped", Error: &JobError{Message: fmt.Sprintf("the plan is not opened: %s %s", key, result.Status)}}
			}
		}
		if run.dryRun {
			return &JobResult{Status: "success"}
		}
		if err := run.publication.Open(ctx, run.open); err != nil {
			return releaseSetFailure(fmt.Errorf("release-set publish: %w", err))
		}
		return &JobResult{Status: "success"}
	}
}

// uploadRunner uploads what publish packed for memberKey, once publish and
// open succeeded.
func (run *ReleaseSetRun) uploadRunner(upload, publish *ScheduledJob, memberKey string) InternalJobRunner {
	return func(ctx context.Context, results map[string]*JobResult) *JobResult {
		if waiting := unsatisfiedDependency(upload, results); waiting != "" {
			return &JobResult{Status: "skipped", Error: &JobError{Message: fmt.Sprintf("nothing is uploaded for %s: %s did not succeed", printableReleaseKey(memberKey), waiting)}}
		}
		if run.dryRun {
			return &JobResult{Status: "success"}
		}
		published, err := run.uploadOutbox(ctx, publish, memberKey)
		if err != nil {
			return releaseSetFailure(fmt.Errorf("upload %s: %w", printableReleaseKey(memberKey), err))
		}
		result := &JobResult{Status: "success"}
		for _, member := range published {
			result.Events = append(result.Events, publishedMemberRawEvent(member))
		}
		return result
	}
}

// unsatisfiedDependency is the first dependency of job that did not succeed,
// "" when every one did.
func unsatisfiedDependency(job *ScheduledJob, results map[string]*JobResult) string {
	for _, dependency := range job.DependsOn {
		if !capabilityAncestorSatisfied(results[dependency]) {
			return dependency
		}
	}
	return ""
}

// uploadOutbox reads the outbox publish packed, checks every packed member
// against the plan before any upload, and uploads each with a publish
// credential the provider issues for its registry. A job that packed nothing
// wrote no descriptor: it published, or skipped, on its own route.
func (run *ReleaseSetRun) uploadOutbox(ctx context.Context, publish *ScheduledJob, memberKey string) ([]*extproto.PublishedMember, error) {
	root := run.outboxes[publish.Key()]
	if root == "" {
		return nil, errors.New("the publication job had no outbox")
	}
	if _, err := os.Lstat(filepath.Join(root, extproto.PublicationOutboxDescriptor)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	outbox, err := publicationoutbox.Read(root)
	if err != nil {
		return nil, err
	}
	planned, ok := run.selectedMember(memberKey)
	if !ok {
		return nil, fmt.Errorf("the plan does not select %s", printableReleaseKey(memberKey))
	}
	for _, packed := range outbox.Descriptor.Members {
		if err := run.checkPacked(packed, planned); err != nil {
			return nil, err
		}
	}
	published := make([]*extproto.PublishedMember, 0, len(outbox.Descriptor.Members))
	for _, packed := range outbox.Descriptor.Members {
		digest, platforms, err := run.uploadPacked(ctx, publish, outbox, packed, run.publicationKind(planned))
		if err != nil {
			return nil, err
		}
		published = append(published, &extproto.PublishedMember{
			Ecosystem: packed.Ecosystem, Coordinate: packed.Coordinate, Version: packed.Version, ArtifactDigest: digest,
			Platforms: platforms,
		})
	}
	return published, nil
}

func (run *ReleaseSetRun) selectedMember(key string) (releaseset.PlannedMember, bool) {
	for _, member := range run.plan.SelectedMembers() {
		if releaseset.MemberKey(member.Ecosystem, member.Coordinate) == key {
			return member, true
		}
	}
	return releaseset.PlannedMember{}, false
}

// checkPacked refuses a packed member that is not the member this job
// publishes for the plan: another member, a member the plan does not select,
// another version, or another project.
func (run *ReleaseSetRun) checkPacked(packed extproto.OutboxMember, planned releaseset.PlannedMember) error {
	key := releaseset.MemberKey(distribution.Ecosystem(packed.Ecosystem), packed.Coordinate)
	if packed.Ecosystem != string(planned.Ecosystem) || packed.Coordinate != planned.Coordinate {
		if _, selected := run.selectedMember(key); !selected {
			return fmt.Errorf("the publication job packed %s, which the plan does not select", printableReleaseKey(key))
		}
		return fmt.Errorf("the publication job packed %s, which another publication job publishes", printableReleaseKey(key))
	}
	if packed.Version != planned.Version {
		return fmt.Errorf("the publication job packed %s at version %q; the plan publishes %q", printableReleaseKey(key), packed.Version, planned.Version)
	}
	if packed.Project != planned.ProjectID {
		return fmt.Errorf("the publication job packed %s for project %q; the plan assigns it to %q", printableReleaseKey(key), packed.Project, planned.ProjectID)
	}
	return nil
}

// uploadPacked uploads one checked member of the planned kind and returns its
// artifact digest: the npm tarball's, the Go module zip's, the OCI
// manifest's, or the stored Put manifest payload's, with the platforms of an
// archive.
func (run *ReleaseSetRun) uploadPacked(ctx context.Context, publish *ScheduledJob, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember, kind distribution.MemberKind) (string, map[string]string, error) {
	var digest string
	var err error
	switch packed.Ecosystem {
	case extproto.OutboxEcosystemNPM:
		digest, err = run.uploadNPM(ctx, publish.Project, outbox, packed)
	case extproto.OutboxEcosystemGo:
		digest, err = run.uploadGo(ctx, publish.Project, outbox, packed)
	case extproto.OutboxEcosystemOCI:
		digest, err = run.uploadOCI(ctx, publish, outbox, packed)
	case extproto.OutboxEcosystemPut, extproto.OutboxEcosystemArchive:
		return run.uploadPut(ctx, publish.Project, outbox, packed, kind)
	default:
		err = fmt.Errorf("ecosystem %q has no upload", packed.Ecosystem)
	}
	return digest, nil, err
}

// uploadNPM uploads the member to the registry it names
// (memberRegistryEndpoint) with the provider's publish bearer for that
// registry.
func (run *ReleaseSetRun) uploadNPM(ctx context.Context, project *workspace.Project, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	endpoint, err := memberRegistryEndpoint(project, packed)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	tarball, err := outbox.ReadFile(packed.NPM.Tarball)
	if err != nil {
		return "", err
	}
	manifest, err := outbox.ReadFile(packed.NPM.Manifest)
	if err != nil {
		return "", err
	}
	bearer, err := run.publication.PublishBearer(ctx, target)
	if err != nil {
		return "", err
	}
	client, err := npmpublish.NewHTTPClient()
	if err != nil {
		return "", err
	}
	artifact := npmpublish.Artifact{Name: packed.Coordinate, Version: packed.Version, Manifest: manifest, Tarball: tarball}
	if _, err := npmpublish.Publish(ctx, client, endpoint, bearer, artifact); err != nil {
		return "", err
	}
	return npmpublish.Digest(tarball), nil
}

// maxPrintedRegistry bounds a registry URL or host an upload error names.
const maxPrintedRegistry = 256

// printableRegistry is endpoint cut to maxPrintedRegistry bytes.
func printableRegistry(endpoint string) string {
	if len(endpoint) <= maxPrintedRegistry {
		return endpoint
	}
	return endpoint[:maxPrintedRegistry] + "..."
}

// memberRegistryEndpoint is the registry the npm member names, normalized by
// the managed rules (extproto.ManagedNPMRegistry). When the project declares
// registries.npm.publish, the member must name that same registry
// (registryURLIdentity): the engine uploads nowhere the declaration
// contradicts. Neither URL carries a credential, a query or a fragment, so the
// refusal names both, cut to maxPrintedRegistry.
func memberRegistryEndpoint(project *workspace.Project, packed extproto.OutboxMember) (string, error) {
	endpoint, err := extproto.ManagedNPMRegistry(packed.NPM.Registry)
	if err != nil {
		return "", err
	}
	declared, err := declaredRegistryEndpoint(project, packed.Ecosystem, "publish")
	if err != nil {
		return "", err
	}
	if declared == "" {
		return endpoint, nil
	}
	want, err := extproto.ManagedNPMRegistry(declared)
	if err != nil {
		return "", fmt.Errorf("registries.%s.publish: %w", packed.Ecosystem, err)
	}
	if registryURLIdentity(endpoint) != registryURLIdentity(want) {
		return "", fmt.Errorf("the publication job packed %s for the registry %q; registries.%s.publish names %q",
			packed.Coordinate, printableRegistry(endpoint), packed.Ecosystem, printableRegistry(want))
	}
	return endpoint, nil
}

// registryURLIdentity is what names the registry at a managed npm registry
// URL: its scheme, its host lowercased, its port unless it is the scheme's
// default, and its escaped path. Two URLs with the same identity name the same
// registry.
func registryURLIdentity(endpoint string) string {
	target, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	port := target.Port()
	if (target.Scheme == "https" && port == "443") || (target.Scheme == "http" && port == "80") {
		port = ""
	}
	return strings.Join([]string{target.Scheme, strings.ToLower(target.Hostname()), port, target.EscapedPath()}, "\x00")
}

func (run *ReleaseSetRun) uploadGo(ctx context.Context, project *workspace.Project, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	declared, err := declaredRegistryEndpoint(project, packed.Ecosystem, "origin")
	if err != nil {
		return "", err
	}
	if declared == "" {
		return "", fmt.Errorf("no Go module origin is declared: set registries.%s.origin", packed.Ecosystem)
	}
	endpoint, err := gomodpublish.ValidateRegistryURL(declared)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	zip, err := outbox.ReadFile(packed.Go.Zip)
	if err != nil {
		return "", err
	}
	mod, err := outbox.ReadFile(packed.Go.Mod)
	if err != nil {
		return "", err
	}
	info, err := outbox.ReadFile(packed.Go.Info)
	if err != nil {
		return "", err
	}
	var stated struct{ Version string }
	if err := json.Unmarshal(info, &stated); err != nil || stated.Version != packed.Version {
		return "", fmt.Errorf("the module .info does not state version %s", packed.Version)
	}
	bearer, err := run.publication.PublishBearer(ctx, target)
	if err != nil {
		return "", err
	}
	client, err := gomodpublish.NewHTTPClient()
	if err != nil {
		return "", err
	}
	digest, _, err := gomodpublish.Publish(ctx, client, nil, endpoint, bearer, gomodpublish.Module{
		Path: packed.Coordinate, Version: packed.Version, Zip: zip, Mod: mod,
	})
	if err != nil {
		return "", err
	}
	if digest != packed.Go.Zip.Digest {
		return "", fmt.Errorf("the registry stored zip digest %s, not the packed %s", digest, packed.Go.Zip.Digest)
	}
	return digest, nil
}

// uploadPut uploads a Put registry member to putRegistryEndpoint with the
// provider's publish bearer for that registry: the blobs its manifest
// references, one at a time, then the manifest, with no channel. Every check
// that needs no registry runs before the bearer is asked for: the member's
// shape (putpublish.Check), then the size and digest of every blob, streamed
// from the outbox. Each blob is read and verified again when it is uploaded.
// The digest is the SHA-256 of the manifest payload the registry stores,
// which must be the packed manifest's.
func (run *ReleaseSetRun) uploadPut(ctx context.Context, project *workspace.Project, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember, kind distribution.MemberKind) (string, map[string]string, error) {
	endpoint, err := putRegistryEndpoint(project)
	if err != nil {
		return "", nil, err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return "", nil, err
	}
	manifest, err := outbox.ReadFile(packed.Put.Manifest)
	if err != nil {
		return "", nil, err
	}
	member := putpublish.Member{
		Kind: kind, Coordinate: packed.Coordinate, Version: packed.Version,
		MediaType: packed.Put.MediaType, Manifest: manifest,
	}
	for _, blob := range packed.Put.Blobs {
		file := blob.File()
		member.Blobs = append(member.Blobs, putpublish.Blob{
			MediaType: blob.MediaType, Digest: blob.Digest, Size: blob.Size,
			Read: func() ([]byte, error) { return outbox.ReadFile(file) },
		})
	}
	if _, err := putpublish.Check(member); err != nil {
		return "", nil, err
	}
	for _, blob := range packed.Put.Blobs {
		if err := outbox.VerifyFile(blob.File()); err != nil {
			return "", nil, err
		}
	}
	bearer, err := run.publication.PublishBearer(ctx, target)
	if err != nil {
		return "", nil, err
	}
	client, err := putpublish.NewHTTPClient()
	if err != nil {
		return "", nil, err
	}
	published, err := putpublish.Publish(ctx, client, endpoint, bearer, member)
	if err != nil {
		return "", nil, err
	}
	if published.Digest != packed.Put.Manifest.Digest {
		return "", nil, fmt.Errorf("the registry stored manifest %s, not the packed %s", published.Digest, packed.Put.Manifest.Digest)
	}
	return published.Digest, published.Platforms, nil
}

// putRegistryEndpoint is the Put registry the project's registries.put entry
// names in its registry field, or the default Put registry, as an upload
// endpoint (putpublish.ValidateRegistryURL). A release archive and a config,
// migration or doc member are served from that one registry.
func putRegistryEndpoint(project *workspace.Project) (string, error) {
	declared, err := declaredRegistryEndpoint(project, extension.PutRegistryEcosystem, "registry")
	if err != nil {
		return "", err
	}
	if declared == "" {
		declared = extension.DefaultPutRegistryURL
	}
	return putpublish.ValidateRegistryURL(declared)
}

// uploadOCI pushes the packed layout, then records the published image beside
// the publication job's output, where a same-run consumer reads it
// (pkgmeta.PublishedImageManifestPath).
func (run *ReleaseSetRun) uploadOCI(ctx context.Context, publish *ScheduledJob, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	repository, err := name.NewRepository(packed.OCI.Repository, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("OCI repository %q: %w", packed.OCI.Repository, err)
	}
	host := repository.RegistryStr()
	declaredHost, err := declaredOCIRegistryHost(publish.Project, packed.Ecosystem)
	if err != nil {
		return "", err
	}
	if declaredHost != "" && !strings.EqualFold(declaredHost, host) {
		return "", fmt.Errorf("the publication job packed an image for %s; registries.%s.publish names %s", host, packed.Ecosystem, printableRegistry(declaredHost))
	}
	candidate, err := pkgmeta.ReadDockerManifest(run.root, publish.Project.Path)
	if err != nil {
		return "", fmt.Errorf("read the packaged image: %w", err)
	}
	if candidate.Digest != packed.OCI.Digest {
		return "", fmt.Errorf("the publication job packed image %s; the package step built %s", packed.OCI.Digest, candidate.Digest)
	}
	layout, err := outbox.Layout(packed.OCI.Layout)
	if err != nil {
		return "", err
	}
	bearer, err := run.publication.PublishBearer(ctx, &url.URL{Scheme: repository.Scheme(), Host: host})
	if err != nil {
		return "", err
	}
	pushed, err := oci.PushLayout(ctx, layout, oci.LayoutTarget{
		Repository: packed.OCI.Repository, Digest: packed.OCI.Digest, Tags: packed.OCI.Tags,
	}, bearer)
	if err != nil {
		return "", err
	}
	if pushed.Digest != packed.OCI.Digest {
		return "", fmt.Errorf("the registry holds manifest %s, not the packed %s", pushed.Digest, packed.OCI.Digest)
	}
	outputDir := filepath.Join(run.root, ".putnami", "out", publish.Project.Path, publish.CommandName())
	if err := pkgmeta.WritePublishedImageManifest(outputDir, pkgmeta.PublishedImageManifest{
		Image:           packed.OCI.Repository,
		ImmutableRef:    packed.OCI.Repository + "@" + pushed.Digest,
		Digest:          pushed.Digest,
		CandidateDigest: candidate.Digest,
		ContentHash:     candidate.ContentHash,
		Platform:        candidate.Platform,
		TargetRegistry:  host,
		Verified:        true,
	}); err != nil {
		return "", fmt.Errorf("record the published image: %w", err)
	}
	return pushed.Digest, nil
}

// declaredOCIRegistryHost is the registry host the project's
// registries.<ecosystem>.publish entry names, "" when it declares none. The
// entry is a repository prefix such as "ghcr.io/team" or a URL such as
// "https://ghcr.io/team"; both name the host "ghcr.io", normalized as an image
// repository's host is (name.NewRegistry), so "docker.io" names
// "index.docker.io". An entry that names no valid host is refused without
// being printed, since it may carry a credential.
func declaredOCIRegistryHost(project *workspace.Project, ecosystem string) (string, error) {
	declared, err := declaredRegistryEndpoint(project, ecosystem, "publish")
	if err != nil || declared == "" {
		return "", err
	}
	host, _, _ := strings.Cut(declared, "/")
	if strings.Contains(declared, "://") {
		target, err := url.Parse(declared)
		if err != nil || target.User != nil {
			return "", fmt.Errorf("registries.%s.publish is not a registry URL without credentials", ecosystem)
		}
		host = target.Host
	}
	registry, err := name.NewRegistry(host, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("registries.%s.publish names no registry host", ecosystem)
	}
	return registry.RegistryStr(), nil
}

// declaredRegistryEndpoint reads field of the project's registries entry for
// ecosystem, "" when the project declares no such entry or field. The
// workspace keys its registries by ecosystem id, the id an outbox member
// names.
func declaredRegistryEndpoint(project *workspace.Project, ecosystem, field string) (string, error) {
	if project == nil {
		return "", nil
	}
	raw, ok := project.Registries[ecosystem]
	if !ok || len(raw) == 0 {
		return "", nil
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", fmt.Errorf("registries.%s: %w", ecosystem, err)
	}
	value, ok := entry[field]
	if !ok {
		return "", nil
	}
	var endpoint string
	if err := json.Unmarshal(value, &endpoint); err != nil {
		return "", fmt.Errorf("registries.%s.%s is not a string", ecosystem, field)
	}
	return strings.TrimRight(strings.TrimSpace(endpoint), "/"), nil
}

// publishedMemberRawEvent is the published-member event an upload node
// reports, in the shape the reconciliation reads (parsePublishedMemberEvent),
// with the artifact envelope a publication job's own event carries: the
// ecosystem as its id and the coordinate as its name.
func publishedMemberRawEvent(member *extproto.PublishedMember) RawJobEvent {
	data := map[string]any{
		"id":             member.Ecosystem,
		"name":           member.Coordinate,
		"kind":           extproto.PublishedMemberEventKind,
		"ecosystem":      member.Ecosystem,
		"coordinate":     member.Coordinate,
		"version":        member.Version,
		"artifactDigest": member.ArtifactDigest,
	}
	if len(member.Platforms) > 0 {
		platforms := make(map[string]any, len(member.Platforms))
		for platform, digest := range member.Platforms {
			platforms[platform] = digest
		}
		data["platforms"] = platforms
	}
	return RawJobEvent{Version: runtimeproto.MaxKnownProtocolVersion, Type: EventTypeArtifact, Data: data}
}

// ensureEngineUploadedPutMembers refuses a publication-v1 release in which a
// put or archive member was not uploaded by an engine upload node: a
// published-member event from any other node, or a selected member no upload
// node published. A publication job holds no publish credential and packs a
// Put registry member into its outbox; an event it reports for one names
// bytes the engine never uploaded.
func (run *ReleaseSetRun) ensureEngineUploadedPutMembers(results map[string]*JobResult) error {
	if !run.Publication() {
		return nil
	}
	uploaded := map[string]bool{}
	var foreign []string
	for resultKey, result := range results {
		if result == nil {
			continue
		}
		for _, event := range result.Events {
			kind, _ := event.Data["kind"].(string)
			ecosystem, _ := event.Data["ecosystem"].(string)
			if event.Type != EventTypeArtifact || kind != extproto.PublishedMemberEventKind ||
				(ecosystem != extproto.OutboxEcosystemPut && ecosystem != extproto.OutboxEcosystemArchive) {
				continue
			}
			coordinate, _ := event.Data["coordinate"].(string)
			key := releaseset.MemberKey(distribution.Ecosystem(ecosystem), coordinate)
			if publishJob, upload := run.uploads[resultKey]; upload && run.publishJobKeys[publishJob] == key {
				uploaded[key] = true
				continue
			}
			foreign = append(foreign, fmt.Sprintf("%s by %s", printableReleaseKey(key), resultKey))
		}
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return fmt.Errorf("release-set publication refused: %d put or archive member(s) were reported by a job the engine did not upload for: %s",
			len(foreign), strings.Join(foreign, "; "))
	}
	var missing []string
	for _, member := range run.plan.SelectedMembers() {
		ecosystem := string(member.Ecosystem)
		key := releaseset.MemberKey(member.Ecosystem, member.Coordinate)
		if (ecosystem == extproto.OutboxEcosystemPut || ecosystem == extproto.OutboxEcosystemArchive) && !uploaded[key] {
			missing = append(missing, printableReleaseKey(key))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("release-set publication refused: the engine uploaded no artifact for %d selected put or archive member(s): %s",
			len(missing), strings.Join(missing, "; "))
	}
	return nil
}
