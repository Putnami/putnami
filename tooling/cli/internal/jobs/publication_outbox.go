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
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/gomodpublish"
	"go.putnami.dev/sdk/extension/npmpublish"
	"go.putnami.dev/sdk/extension/oci"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/publicationoutbox"
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
		switch string(member.Ecosystem) {
		case extproto.OutboxEcosystemNPM, extproto.OutboxEcosystemGo, extproto.OutboxEcosystemOCI:
		default:
			return nil, nil, fmt.Errorf("selected member %s: publication-v1 uploads %s, %s and %s members only",
				printableReleaseKey(releaseset.MemberKey(member.Ecosystem, member.Coordinate)),
				extproto.OutboxEcosystemNPM, extproto.OutboxEcosystemGo, extproto.OutboxEcosystemOCI)
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
		digest, err := run.uploadPacked(ctx, publish, outbox, packed)
		if err != nil {
			return nil, err
		}
		published = append(published, &extproto.PublishedMember{
			Ecosystem: packed.Ecosystem, Coordinate: packed.Coordinate, Version: packed.Version, ArtifactDigest: digest,
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

// uploadPacked uploads one checked member and returns its artifact digest:
// the npm tarball's, the Go module zip's, or the OCI manifest's.
func (run *ReleaseSetRun) uploadPacked(ctx context.Context, publish *ScheduledJob, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	switch packed.Ecosystem {
	case extproto.OutboxEcosystemNPM:
		return run.uploadNPM(ctx, publish.Project, outbox, packed)
	case extproto.OutboxEcosystemGo:
		return run.uploadGo(ctx, publish.Project, outbox, packed)
	case extproto.OutboxEcosystemOCI:
		return run.uploadOCI(ctx, publish, outbox, packed)
	}
	return "", fmt.Errorf("ecosystem %q has no upload", packed.Ecosystem)
}

func (run *ReleaseSetRun) uploadNPM(ctx context.Context, project *workspace.Project, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	endpoint, err := declaredRegistryEndpoint(project, packed.Ecosystem, "publish")
	if err != nil {
		return "", err
	}
	if endpoint == "" {
		return "", fmt.Errorf("no %s publish endpoint is declared: set registries.%s.publish", packed.Ecosystem, packed.Ecosystem)
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Host == "" {
		return "", fmt.Errorf("registries.%s.publish %q is not an absolute URL", packed.Ecosystem, endpoint)
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

// uploadOCI pushes the packed layout, then records the published image beside
// the publication job's output, where a same-run consumer reads it
// (pkgmeta.PublishedImageManifestPath).
func (run *ReleaseSetRun) uploadOCI(ctx context.Context, publish *ScheduledJob, outbox *publicationoutbox.Outbox, packed extproto.OutboxMember) (string, error) {
	repository, err := name.NewRepository(packed.OCI.Repository, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("OCI repository %q: %w", packed.OCI.Repository, err)
	}
	host := repository.RegistryStr()
	declared, err := declaredRegistryEndpoint(publish.Project, packed.Ecosystem, "publish")
	if err != nil {
		return "", err
	}
	if declaredHost, _, _ := strings.Cut(declared, "/"); declared != "" && !strings.EqualFold(declaredHost, host) {
		return "", fmt.Errorf("the publication job packed an image for %s; registries.%s.publish names %s", host, packed.Ecosystem, declaredHost)
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
// reports, in the shape the reconciliation reads (parsePublishedMemberEvent).
func publishedMemberRawEvent(member *extproto.PublishedMember) RawJobEvent {
	return RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeArtifact,
		Data: map[string]any{
			"kind":           extproto.PublishedMemberEventKind,
			"ecosystem":      member.Ecosystem,
			"coordinate":     member.Coordinate,
			"version":        member.Version,
			"artifactDigest": member.ArtifactDigest,
		},
	}
}
