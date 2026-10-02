package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// InternalReleaseSetProviderCapabilityEnv is an implementation-private hop
// marker used only between the release-set coordinator and the nested Putnami
// process it invokes with fixed protocol argv. It is not a user or extension
// contract. Its value binds that child to the provider core already resolved;
// the child captures and removes both marker and bearer before any workspace
// artifact, hook, runtime, probe, or repository command can run.
const InternalReleaseSetProviderCapabilityEnv = "PUTNAMI_INTERNAL_RELEASE_SET_PROVIDER_CAPABILITY"

// processCapabilityContextKey is private so repository-controlled code cannot
// synthesize or inspect the captured values through a job context. They travel
// only in the orchestrator's in-memory context.Context.
type processCapabilityContextKey struct{}

// processCapabilityGrantContextKey is a scheduler-owned, per-execution grant.
// A manifest can declare traits and edges, but cannot manufacture this private
// marker or put it into a serialized job context.
type processCapabilityGrantContextKey struct{}

// releaseSetProviderGrantContextKey is separate from scheduler authorization:
// a provider invocation has no planned DAG, and only App may mint this grant
// after validating the raw fixed argv and the freshly discovered exact owner.
type releaseSetProviderGrantContextKey struct{}

type releaseSetProviderIdentity struct {
	ExtensionName string `json:"extensionName"`
	Version       string `json:"version,omitempty"`
	Command       string `json:"command"`
}

type releaseSetProviderGrant struct {
	identity releaseSetProviderIdentity
	jobKey   string
}

type processCapabilities struct {
	planCallback       string
	planHandoffMu      sync.Mutex
	planHandoffDigest  string
	cloudToken         string
	after              string
	releaseSetProvider *releaseSetProviderIdentity
	publicationsFile   string
	membersFile        string
	providerMarkerErr  error
}

// String keeps an accidental context diagnostic from rendering either value.
func (*processCapabilities) String() string { return "<redacted process capabilities>" }

// CaptureProcessCapabilities opts into runner-scoped transport only when the
// trusted process supplies a non-empty AFTER contract. Token-only invocations
// retain the historical local publish/upgrade behavior. Once opted in, callers
// invoke this at their earliest trusted boundary; repeated calls preserve the
// first capture while defensively removing a later reintroduction. A callback
// without AFTER is refused with a diagnostic and its capability is withheld.
func CaptureProcessCapabilities(ctx context.Context) context.Context {
	callback, _ := os.LookupEnv(InternalReleasePlanCallbackEnv)
	_ = os.Unsetenv(InternalReleasePlanCallbackEnv)
	if processCapabilitiesFromContext(ctx) != nil {
		_ = os.Unsetenv(extensionproto.CloudTokenEnv)
		_ = os.Unsetenv(extensionproto.CloudCapabilityAfterEnv)
		_ = os.Unsetenv(InternalReleaseSetProviderCapabilityEnv)
		_ = os.Unsetenv(runtimeproto.ReleaseSetPublishedImagesFileEnv)
		_ = os.Unsetenv(runtimeproto.ReleaseSetMembersFileEnv)
		return ctx
	}
	providerMarker, providerMode := os.LookupEnv(InternalReleaseSetProviderCapabilityEnv)
	if providerMode && providerMarker != "" {
		token, _ := os.LookupEnv(extensionproto.CloudTokenEnv)
		publicationsFile, _ := os.LookupEnv(runtimeproto.ReleaseSetPublishedImagesFileEnv)
		membersFile, _ := os.LookupEnv(runtimeproto.ReleaseSetMembersFileEnv)
		_ = os.Unsetenv(extensionproto.CloudTokenEnv)
		_ = os.Unsetenv(extensionproto.CloudCapabilityAfterEnv)
		_ = os.Unsetenv(InternalReleaseSetProviderCapabilityEnv)
		_ = os.Unsetenv(runtimeproto.ReleaseSetPublishedImagesFileEnv)
		_ = os.Unsetenv(runtimeproto.ReleaseSetMembersFileEnv)

		identity := new(releaseSetProviderIdentity)
		err := json.Unmarshal([]byte(providerMarker), identity)
		if err == nil {
			canonical, marshalErr := json.Marshal(identity)
			if marshalErr != nil || string(canonical) != providerMarker {
				err = fmt.Errorf("internal release-set provider marker is not canonical")
			}
		}
		if err == nil {
			switch {
			case identity.ExtensionName == "":
				err = fmt.Errorf("internal release-set provider marker has no extension identity")
			case identity.Command != distribution.ProviderCommandName:
				err = fmt.Errorf("internal release-set provider marker names non-reserved command %q", identity.Command)
			}
		}
		return context.WithValue(ctx, processCapabilityContextKey{}, &processCapabilities{
			cloudToken:         token,
			releaseSetProvider: identity,
			publicationsFile:   publicationsFile,
			membersFile:        membersFile,
			providerMarkerErr:  err,
		})
	}
	after, afterPresent := os.LookupEnv(extensionproto.CloudCapabilityAfterEnv)
	if (!afterPresent || after == "") && callback == "" {
		return ctx
	}
	if strings.TrimSpace(after) == "" && callback != "" {
		_, _ = fmt.Fprintln(os.Stderr, "putnami: release plan callback refused: PUTNAMI_CLOUD_CAPABILITY_AFTER is required; cloud capability withheld")
		callback = ""
		_ = os.Unsetenv(extensionproto.CloudTokenEnv)
	}
	token, _ := os.LookupEnv(extensionproto.CloudTokenEnv)
	_ = os.Unsetenv(extensionproto.CloudTokenEnv)
	_ = os.Unsetenv(extensionproto.CloudCapabilityAfterEnv)
	_ = os.Unsetenv(runtimeproto.ReleaseSetPublishedImagesFileEnv)
	_ = os.Unsetenv(runtimeproto.ReleaseSetMembersFileEnv)
	return context.WithValue(ctx, processCapabilityContextKey{}, &processCapabilities{
		cloudToken:   token,
		after:        after,
		planCallback: callback,
	})
}

// HasInternalReleaseSetProviderCapability reports the private child marker
// before capture. App uses it only to move capture ahead of relaunch; the
// coordinator already invokes the authoritative current executable, so a
// second workspace-pin relaunch would unnecessarily keep the bearer ambient.
func HasInternalReleaseSetProviderCapability() bool {
	marker, present := os.LookupEnv(InternalReleaseSetProviderCapabilityEnv)
	return present && marker != ""
}

// ValidateReleaseSetProviderInvocation validates the coordinator-owned raw
// argv before App reads aliases, installs artifacts, bootstraps the workspace,
// or dispatches any child. It returns true only for provider-only mode.
func ValidateReleaseSetProviderInvocation(ctx context.Context, args []string) (bool, error) {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.releaseSetProvider == nil {
		return false, nil
	}
	if capabilities.providerMarkerErr != nil {
		return true, capabilities.providerMarkerErr
	}
	if len(args) != 5 ||
		args[0] != distribution.CloudCommand ||
		args[1] != distribution.ReleaseSetCommand ||
		args[3] != distribution.RequestFileFlag ||
		strings.TrimSpace(args[4]) == "" || !filepath.IsAbs(args[4]) {
		return true, fmt.Errorf("internal release-set provider invocation has unexpected argv")
	}
	switch args[2] {
	case distribution.ResolveCommand, distribution.ReleaseCommand,
		distribution.ChannelSetCommand, distribution.ChannelStatusCommand:
		return true, nil
	default:
		return true, fmt.Errorf("internal release-set provider invocation has unexpected operation %q", args[2])
	}
}

// ValidateReleaseSetProviderResolution binds provider-only mode to the freshly
// discovered owner and reserved flat command. It runs before runtime
// synchronization but does not grant the token; runtime preparation therefore
// stays capability-free.
func ValidateReleaseSetProviderResolution(
	ctx context.Context,
	extensionName, version, command string,
	interactive bool,
) error {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.releaseSetProvider == nil {
		return nil
	}
	if capabilities.providerMarkerErr != nil {
		return capabilities.providerMarkerErr
	}
	want := capabilities.releaseSetProvider
	if !interactive || extensionName != want.ExtensionName || version != want.Version ||
		command != want.Command || command != distribution.ProviderCommandName {
		return fmt.Errorf("internal release-set provider resolved to an unexpected extension command")
	}
	return nil
}

// GrantReleaseSetProviderJob mints the one direct-execution grant after App has
// built the exact interactive job from the validated resolved provider.
func GrantReleaseSetProviderJob(ctx context.Context, job *ScheduledJob) (context.Context, error) {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.releaseSetProvider == nil {
		return ctx, nil
	}
	if job == nil || job.JobDef == nil || job.Extension == nil {
		return ctx, fmt.Errorf("internal release-set provider job is incomplete")
	}
	want := capabilities.releaseSetProvider
	if job.Extension.Name != want.ExtensionName || job.Extension.Version != want.Version ||
		job.CommandName() != want.Command || job.CommandName() != distribution.ProviderCommandName {
		return ctx, fmt.Errorf("internal release-set provider job does not match the resolved provider")
	}
	grant := &releaseSetProviderGrant{identity: *want, jobKey: job.Key()}
	return context.WithValue(ctx, releaseSetProviderGrantContextKey{}, grant), nil
}

func processCapabilitiesFromContext(ctx context.Context) *processCapabilities {
	if ctx == nil {
		return nil
	}
	capabilities, _ := ctx.Value(processCapabilityContextKey{}).(*processCapabilities)
	return capabilities
}

// ProcessCapabilityAuthorization is an opaque engine-produced proof that a
// final plan places every registry/cloud job after every configured functional
// gate leaf. It deliberately contains no credential and has no exported fields,
// JSON shape, or manifest constructor.
type ProcessCapabilityAuthorization struct {
	planShape              [sha256.Size]byte
	requiredSuccessesByJob map[string][]string
}

// AuthorizeProcessCapabilities validates the final planned DAG and produces
// the scheduler-only authorization object. No token, no trusted AFTER contract,
// or no side-effecting job means there is nothing to arm and therefore no
// authorization object.
func AuthorizeProcessCapabilities(
	ctx context.Context,
	planned []*ScheduledJob,
	executesJobs bool,
) (*ProcessCapabilityAuthorization, error) {
	if !executesJobs {
		return nil, nil
	}
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.cloudToken == "" || capabilities.after == "" {
		return nil, nil
	}

	protected := make([]*ScheduledJob, 0)
	// gated is the subset the AFTER leaves must dominate. A release-set member
	// publication is protected (the scheduler still delivers its capability only
	// once its own functional ancestors succeeded) but not gated: the release-set
	// stamp waits for the gate commands on its behalf (ADR 0023).
	gated := make([]*ScheduledJob, 0)
	for _, job := range planned {
		if hasCloudCapabilitySideEffects(job) {
			protected = append(protected, job)
			if !isReleaseSetMemberPublication(job) {
				gated = append(gated, job)
			}
		}
	}
	if len(protected) == 0 {
		return nil, nil
	}

	requiredCommands, err := parseCapabilityAfter(capabilities.after)
	if err != nil {
		return nil, err
	}
	if err := validatePlanDAG(planned); err != nil {
		return nil, fmt.Errorf("capability plan is not schedulable: %w", err)
	}

	jobsByKey := jobsByPlanKey(planned)
	leavesByCommand := capabilityLeavesByCommand(planned)
	requiredLeaves := make([]string, 0)
	for _, command := range requiredCommands {
		leaves := leavesByCommand[command]
		if len(leaves) == 0 {
			proofByJob := make(map[string]bool, len(planned))
			resolvedProof := make(map[string]bool, len(planned))
			for _, job := range gated {
				if !hasPlannerNoopGateProof(job, command, jobsByKey, proofByJob, resolvedProof) {
					return nil, fmt.Errorf(
						"capability job %s has no planner proof for no-op gate command %q",
						job.Key(), command,
					)
				}
			}
			continue
		}
		for _, leaf := range leaves {
			if isFinalizerJob(jobsByKey[leaf]) {
				return nil, fmt.Errorf("capability gate command %q has a non-schedulable finalizer leaf", command)
			}
		}
		requiredLeaves = append(requiredLeaves, leaves...)
	}
	requiredLeaves = dedupeSorted(requiredLeaves)

	authorization := &ProcessCapabilityAuthorization{
		planShape:              capabilityPlanShape(planned),
		requiredSuccessesByJob: make(map[string][]string, len(protected)),
	}
	for _, job := range protected {
		if isFinalizerJob(job) {
			return nil, fmt.Errorf("capability job %s is a finalizer and cannot be functionally gated", job.Key())
		}
		ancestors := functionalAncestors(job, jobsByKey)
		if !isReleaseSetMemberPublication(job) {
			for _, leaf := range requiredLeaves {
				if _, ok := ancestors[leaf]; !ok {
					return nil, fmt.Errorf("capability job %s is not functionally after required leaf %s", job.Key(), leaf)
				}
			}
		}
		// AFTER establishes the minimum functional dominance. Runtime admission
		// is stricter: every functional ancestor must actually have succeeded, so
		// --continue-on-error cannot carry a capability past an unrelated failed
		// package/config prerequisite.
		requiredSuccesses := make([]string, 0, len(ancestors))
		for ancestor := range ancestors {
			requiredSuccesses = append(requiredSuccesses, ancestor)
		}
		sort.Strings(requiredSuccesses)
		authorization.requiredSuccessesByJob[job.Key()] = requiredSuccesses
	}
	return authorization, nil
}

// hasPlannerNoopGateProof accepts a planner-stamped proof on the job itself or
// a proof carried by every final functional dependency branch. This lets a
// protected release inherit the no-op gates proven by all of its publish
// prerequisites without treating their redundant direct package ancestors as
// independent branches. An extra unproven branch still keeps authorization
// fail-closed. SerializeAfter is intentionally excluded: ordering alone is not
// functional dominance.
func hasPlannerNoopGateProof(
	job *ScheduledJob,
	command string,
	jobsByKey map[string]*ScheduledJob,
	proofByJob map[string]bool,
	resolvedProof map[string]bool,
) bool {
	if job == nil {
		return false
	}
	key := job.Key()
	if resolvedProof[key] {
		return proofByJob[key]
	}

	for _, gate := range job.SessionPrerequisiteNoopGates {
		if gate == command {
			proofByJob[key] = true
			resolvedProof[key] = true
			return true
		}
	}
	if len(job.DependsOn) == 0 {
		resolvedProof[key] = true
		return false
	}
	redundantDependencies := make(map[string]struct{})
	for _, dependencyKey := range job.DependsOn {
		dependency := jobsByKey[dependencyKey]
		if dependency == nil {
			continue
		}
		for ancestorKey := range functionalAncestors(dependency, jobsByKey) {
			redundantDependencies[ancestorKey] = struct{}{}
		}
	}
	for _, dependencyKey := range job.DependsOn {
		if _, redundant := redundantDependencies[dependencyKey]; redundant {
			continue
		}
		if !hasPlannerNoopGateProof(
			jobsByKey[dependencyKey], command, jobsByKey, proofByJob, resolvedProof,
		) {
			resolvedProof[key] = true
			return false
		}
	}

	proofByJob[key] = true
	resolvedProof[key] = true
	return true
}

func capabilityPlanShape(planned []*ScheduledJob) [sha256.Size]byte {
	rows := make([]string, 0, len(planned))
	for _, job := range planned {
		dependsOn := append([]string(nil), job.DependsOn...)
		serializeAfter := append([]string(nil), job.SerializeAfter...)
		noopGates := append([]string(nil), job.SessionPrerequisiteNoopGates...)
		selectedProjectIDs := make([]string, 0, len(job.SelectedProjects))
		for _, project := range job.SelectedProjects {
			if project != nil {
				selectedProjectIDs = append(selectedProjectIDs, project.ID)
			}
		}
		sort.Strings(dependsOn)
		sort.Strings(serializeAfter)
		sort.Strings(noopGates)
		selectedProjectIDs = dedupeSorted(selectedProjectIDs)
		rows = append(rows, strings.Join([]string{
			job.Key(),
			job.CommandName(),
			job.JobDef.Traits.SideEffects,
			strings.Join(declaredCapabilityEffects(job), "\x00"),
			strings.Join(dependsOn, "\x00"),
			strings.Join(serializeAfter, "\x00"),
			strings.Join(noopGates, "\x00"),
			strings.Join(selectedProjectIDs, "\x00"),
		}, "\x01"))
	}
	sort.Strings(rows)
	return sha256.Sum256([]byte(strings.Join(rows, "\x02")))
}

func parseCapabilityAfter(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	commands := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		command := strings.TrimSpace(part)
		if command == "" {
			return nil, fmt.Errorf("%s contains an empty command", extensionproto.CloudCapabilityAfterEnv)
		}
		if !seen[command] {
			seen[command] = true
			commands = append(commands, command)
		}
	}
	sort.Strings(commands)
	return commands, nil
}

func capabilityLeavesByCommand(planned []*ScheduledJob) map[string][]string {
	type groupKey struct {
		project string
		command string
	}
	groups := make(map[groupKey][]*ScheduledJob)
	for _, job := range planned {
		// Finalizers are invocation-coordinator work, not functional DAG nodes.
		// They can neither prove a gate nor replace the last ordinary leaf.
		if isFinalizerJob(job) {
			continue
		}
		key := groupKey{project: job.Project.ID, command: job.CommandName()}
		groups[key] = append(groups[key], job)
	}

	out := make(map[string][]string)
	for key, group := range groups {
		members := make(map[string]bool, len(group))
		dependedOn := make(map[string]bool, len(group))
		for _, job := range group {
			members[job.Key()] = true
		}
		for _, job := range group {
			for _, dependency := range job.DependsOn {
				if members[dependency] {
					dependedOn[dependency] = true
				}
			}
		}
		for _, job := range group {
			if !dependedOn[job.Key()] {
				out[key.command] = append(out[key.command], job.Key())
			}
		}
	}
	for command, leaves := range out {
		out[command] = dedupeSorted(leaves)
	}
	return out
}

func functionalAncestors(job *ScheduledJob, jobsByKey map[string]*ScheduledJob) map[string]struct{} {
	ancestors := make(map[string]struct{})
	pending := append([]string(nil), job.DependsOn...)
	for len(pending) > 0 {
		last := len(pending) - 1
		key := pending[last]
		pending = pending[:last]
		if _, seen := ancestors[key]; seen {
			continue
		}
		ancestors[key] = struct{}{}
		if dependency := jobsByKey[key]; dependency != nil {
			pending = append(pending, dependency.DependsOn...)
		}
	}
	return ancestors
}

func hasCloudCapabilitySideEffects(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil {
		return false
	}
	switch job.JobDef.Traits.SideEffects {
	case extensionproto.SideEffectsRegistry, extensionproto.SideEffectsCloud:
		return true
	}
	return len(declaredCapabilityEffects(job)) > 0
}

// declaredCapabilityEffects projects the actual manifest task's registry/cloud
// effects into capability classification. Command traits describe a whole
// command and cannot safely stand in for a side-effecting nested task: an
// unclassified task would otherwise receive no gating, while a post-authorization
// task swap could retain a grant unless the declaration also enters plan shape.
func declaredCapabilityEffects(job *ScheduledJob) []string {
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	effects := make([]string, 0, 2)
	for _, effect := range declaration.Effects {
		switch effect {
		case extensionproto.EffectRegistry, extensionproto.EffectCloud:
			effects = append(effects, effect)
		}
	}
	return dedupeSorted(effects)
}

// processCapabilityRuntime owns grants proven against terminal dependency
// results. The authorization object proves graph dominance; this second check
// proves every functional ancestor actually succeeded, even with
// continue-on-error.
type processCapabilityRuntime struct {
	authorization *ProcessCapabilityAuthorization
	mu            sync.RWMutex
	granted       map[string]struct{}
}

func newProcessCapabilityRuntime(
	authorization *ProcessCapabilityAuthorization,
	planned []*ScheduledJob,
) *processCapabilityRuntime {
	if authorization == nil || authorization.planShape != capabilityPlanShape(planned) {
		return nil
	}
	return &processCapabilityRuntime{
		authorization: authorization,
		granted:       make(map[string]struct{}),
	}
}

func (runtime *processCapabilityRuntime) authorizeReady(
	job *ScheduledJob,
	results map[string]*JobResult,
	resultsMu *sync.Mutex,
) bool {
	if runtime == nil || runtime.authorization == nil || job == nil {
		return true
	}
	required, protected := runtime.authorization.requiredSuccessesByJob[job.Key()]
	if !protected {
		return true
	}
	resultsMu.Lock()
	for _, leaf := range required {
		if !capabilityAncestorSatisfied(results[leaf]) {
			resultsMu.Unlock()
			return false
		}
	}
	resultsMu.Unlock()

	runtime.mu.Lock()
	runtime.granted[job.Key()] = struct{}{}
	runtime.mu.Unlock()
	return true
}

// capabilityAncestorSatisfied reports whether one functional ancestor's
// terminal result lets a protected job receive its grant.
//
// A deterministic in-job skip — the task ran, exited 0 and reported "skipped"
// with no error (a tidy or describe step with nothing to do, a config merge
// with no inputs) — is the complete terminal result of a task that was
// consulted, not a gap in the gate. Only a failure, a cancellation, an absent
// result, or a skip the scheduler synthesized on the task's behalf denies the
// grant: skipJob and the pruned-invocation path always record their cause as
// the result's error, so a skip that carries one never came from the task.
func capabilityAncestorSatisfied(result *JobResult) bool {
	if result == nil {
		return false
	}
	switch result.Status {
	case "success":
		return true
	case "skipped":
		return result.Error == nil
	default:
		return false
	}
}

func (runtime *processCapabilityRuntime) contextForJob(ctx context.Context, job *ScheduledJob) context.Context {
	if runtime == nil || job == nil {
		return ctx
	}
	runtime.mu.RLock()
	_, granted := runtime.granted[job.Key()]
	runtime.mu.RUnlock()
	if !granted {
		return ctx
	}
	return context.WithValue(ctx, processCapabilityGrantContextKey{}, job.Key())
}

// scopeProcessCapabilities applies the captured token at the final exec seam,
// after planning, context construction, cache lookup, and runtime gate checks.
// Direct RunJob calls have no private grant and therefore never receive it.
func scopeProcessCapabilities(ctx context.Context, env []string, job *ScheduledJob) []string {
	// AFTER and the internal provider marker are orchestrator control data, never
	// job input. Strip even manifest-owned attempts to declare either one.
	env = removeEnvKey(env, extensionproto.CloudCapabilityAfterEnv)
	env = removeEnvKey(env, InternalReleasePlanCallbackEnv)
	env = removeEnvKey(env, InternalReleaseSetProviderCapabilityEnv)
	env = removeEnvKey(env, runtimeproto.ReleaseSetPublishedImagesFileEnv)
	env = removeEnvKey(env, runtimeproto.ReleaseSetMembersFileEnv)
	// The outbox is engine control data too: only a publication job of a
	// publication-v1 run receives one, and it then receives no credential and
	// no registry route, whatever the process captured.
	env = removeEnvKey(env, extensionproto.PublicationOutboxEnv)
	if outbox := publicationOutboxFor(ctx, job); outbox != "" {
		return publicationOutboxEnv(env, outbox)
	}
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil {
		return env
	}
	env = removeEnvKey(env, extensionproto.CloudTokenEnv)
	if capabilities.releaseSetProvider != nil {
		if !releaseSetProviderGrantMatches(ctx, capabilities, job) {
			return env
		}
		if capabilities.cloudToken != "" {
			env = append(env, extensionproto.CloudTokenEnv+"="+capabilities.cloudToken)
		}
		if capabilities.membersFile != "" {
			env = append(env, runtimeproto.ReleaseSetMembersFileEnv+"="+capabilities.membersFile)
		}
		if capabilities.publicationsFile != "" {
			env = append(env, runtimeproto.ReleaseSetPublishedImagesFileEnv+"="+capabilities.publicationsFile)
		}
		return env
	}
	if capabilities.cloudToken == "" {
		return env
	}
	if !hasCloudCapabilitySideEffects(job) {
		return env
	}
	grantedJob, _ := ctx.Value(processCapabilityGrantContextKey{}).(string)
	if grantedJob == "" || grantedJob != job.Key() {
		return env
	}
	return append(env, extensionproto.CloudTokenEnv+"="+capabilities.cloudToken)
}

func releaseSetProviderGrantMatches(ctx context.Context, capabilities *processCapabilities, job *ScheduledJob) bool {
	if capabilities == nil || capabilities.releaseSetProvider == nil || job == nil || job.Extension == nil {
		return false
	}
	grant, _ := ctx.Value(releaseSetProviderGrantContextKey{}).(*releaseSetProviderGrant)
	return grant != nil && grant.jobKey == job.Key() &&
		job.Extension.Name == grant.identity.ExtensionName &&
		job.Extension.Version == grant.identity.Version &&
		job.CommandName() == grant.identity.Command
}

func processCapabilityOutputNeedsRedaction(ctx context.Context, capabilities *processCapabilities, job *ScheduledJob) bool {
	if capabilities != nil && capabilities.releaseSetProvider != nil {
		return releaseSetProviderGrantMatches(ctx, capabilities, job)
	}
	return hasCloudCapabilitySideEffects(job)
}

// ReleaseSetProviderProcessEnv binds every coordinator call to the nested
// authoritative Putnami provider-only path. In runner mode it transports the
// captured resource-qualified bearer; on a local Mac it transports the
// historical ambient token without changing that token's behavior for the
// parent publish/upgrade jobs. AFTER remains parent control data.
func ReleaseSetProviderProcessEnv(ctx context.Context, provider *extension.ResolvedProvider) []string {
	capabilities := processCapabilitiesFromContext(ctx)
	if provider == nil || provider.ExtensionName == "" || provider.Command != distribution.ProviderCommandName {
		return nil
	}
	token := os.Getenv(extensionproto.CloudTokenEnv)
	if capabilities != nil {
		token = capabilities.cloudToken
	}
	env := removeEnvKey(os.Environ(), extensionproto.CloudTokenEnv)
	env = removeEnvKey(env, extensionproto.CloudCapabilityAfterEnv)
	env = removeEnvKey(env, InternalReleasePlanCallbackEnv)
	env = removeEnvKey(env, InternalReleaseSetProviderCapabilityEnv)
	env = removeEnvKey(env, runtimeproto.ReleaseSetPublishedImagesFileEnv)
	env = removeEnvKey(env, runtimeproto.ReleaseSetMembersFileEnv)
	marker, err := json.Marshal(releaseSetProviderIdentity{
		ExtensionName: provider.ExtensionName,
		Version:       provider.Version,
		Command:       provider.Command,
	})
	if err != nil {
		return env
	}
	env = append(env, InternalReleaseSetProviderCapabilityEnv+"="+string(marker))
	if token != "" {
		env = append(env, extensionproto.CloudTokenEnv+"="+token)
	}
	return env
}

// processCapabilityEventSink scrubs the in-memory bearer before an event can
// reach a live renderer. The token is read only from the private runtime
// context; it is never added to a persistent secret registry or job model.
func processCapabilityEventSink(ctx context.Context, job *ScheduledJob, handler EventHandler) EventHandler {
	capabilities := processCapabilitiesFromContext(ctx)
	if handler == nil || capabilities == nil || capabilities.cloudToken == "" ||
		!processCapabilityOutputNeedsRedaction(ctx, capabilities, job) {
		return handler
	}
	needles := []string{capabilities.cloudToken}
	return func(event RawJobEvent) {
		// RawJobEvent is passed by value but Data is a reference tree. Redacting
		// that tree in place would rewrite the parser's copy before it extracts a
		// result status, turning a capability echo into a seemingly benign marker.
		// The live projection owns its clone; the result guard still sees the
		// producer's original value and can fail closed.
		event = cloneProcessCapabilityEvent(event)
		redactProcessCapabilityEvent(&event, needles)
		handler(event)
	}
}

func cloneProcessCapabilityEvent(event RawJobEvent) RawJobEvent {
	payload, err := json.Marshal(event)
	if err != nil {
		// Runtime event payloads originate in JSON and should always marshal. If
		// an in-process mapper introduced an unsupported value, omit the payload
		// from the live surface rather than risk sharing and leaking its tree.
		event.Data = nil
		return event
	}
	cloned := RawJobEvent{Timestamp: event.Timestamp}
	if err := json.Unmarshal(payload, &cloned); err != nil {
		event.Data = nil
		return event
	}
	return cloned
}

func redactProcessCapabilityEvent(event *RawJobEvent, needles []string) bool {
	if event == nil {
		return false
	}
	event.Type, _ = redactSensitive(event.Type, needles)
	event.Time, _ = redactSensitive(event.Time, needles)
	event.Level, _ = redactSensitive(event.Level, needles)
	event.Message, _ = redactSensitive(event.Message, needles)
	// Unlike invocation-artifact redaction, a process capability has no declared
	// output-path exemption: every data string and object key is private. Keys
	// are deleted, never renamed, because replacement could collide with an
	// existing key and silently change a producer's meaning.
	return redactProcessCapabilityData(event.Data, needles)
}

// redactProcessCapabilityData scrubs values and removes any object member whose
// key carries the bearer. It reports key leaks separately because, unlike a
// redacted value, a removed key changes the JSON shape and must fail the result
// closed before it can be cached or persisted.
func redactProcessCapabilityData(data extension.ParamMap, needles []string) bool {
	_, keyLeaked := redactProcessCapabilityMap(data, needles)
	return keyLeaked
}

func redactProcessCapabilityMap(data extension.ParamMap, needles []string) (bool, bool) {
	valueLeaked := false
	keyLeaked := false
	for key, value := range data {
		if _, hit := redactSensitive(key, needles); hit {
			delete(data, key)
			keyLeaked = true
			continue
		}
		scrubbed, valueHit, nestedKeyHit := redactProcessCapabilityValue(value, needles)
		if valueHit {
			data[key] = scrubbed
			valueLeaked = true
		}
		keyLeaked = keyLeaked || nestedKeyHit
	}
	return valueLeaked, keyLeaked
}

func redactProcessCapabilityValue(value any, needles []string) (any, bool, bool) {
	switch typed := value.(type) {
	case string:
		scrubbed, hit := redactSensitive(typed, needles)
		return scrubbed, hit, false
	case extension.ParamMap:
		valueLeaked, keyLeaked := redactProcessCapabilityMap(typed, needles)
		return typed, valueLeaked, keyLeaked
	case []any:
		valueLeaked := false
		keyLeaked := false
		for index, element := range typed {
			scrubbed, valueHit, nestedKeyHit := redactProcessCapabilityValue(element, needles)
			if valueHit {
				typed[index] = scrubbed
				valueLeaked = true
			}
			keyLeaked = keyLeaked || nestedKeyHit
		}
		return typed, valueLeaked, keyLeaked
	default:
		return value, false, false
	}
}

// redactProcessCapabilityResult closes every persistent path after parsing:
// JobResult feeds completion renderers, session projection, and local/remote
// cache conversion. Scrubbing it here makes those downstream surfaces safe by
// construction, including subprocess stderr promoted to JobError.
func redactProcessCapabilityResult(ctx context.Context, job *ScheduledJob, result *JobResult) *JobResult {
	capabilities := processCapabilitiesFromContext(ctx)
	if result == nil || capabilities == nil || capabilities.cloudToken == "" ||
		!processCapabilityOutputNeedsRedaction(ctx, capabilities, job) {
		return result
	}
	needles := []string{capabilities.cloudToken}
	_, statusLeaked := redactSensitive(result.Status, needles)
	keyLeaked := redactProcessCapabilityData(result.Data, needles)
	for i := range result.Events {
		keyLeaked = redactProcessCapabilityEvent(&result.Events[i], needles) || keyLeaked
	}
	if statusLeaked || keyLeaked {
		// Status controls both DAG admission and the session verdict. A redaction
		// marker is not a valid neutral status, and deleting a sensitive JSON key
		// changes the payload shape. Fail before a dependent or persistent surface
		// can mistake either echo for success.
		result.Status = "failed"
		result.SourceMutated = false
	}
	if result.Error != nil {
		result.Error.Message, _ = redactSensitive(result.Error.Message, needles)
		result.Error.Code, _ = redactSensitive(result.Error.Code, needles)
	}
	if statusLeaked || keyLeaked {
		result.Error = &JobError{
			Code:    extensionproto.FailureSensitiveLeakDetected,
			Message: "task emitted a protected process capability in a control field; the value was withheld and the task failed",
		}
	}
	return result
}

// withoutUncapturedCapabilities returns env without the capability transport
// CaptureProcessCapabilities takes from the process: AFTER, the internal
// release-plan and release-set markers and files, and the cloud token when
// AFTER or a release-plan callback opts into the transport. After capture,
// env holds none of them and is returned unchanged. A token-only environment
// keeps its cloud token, as every job's does.
func withoutUncapturedCapabilities(env []string) []string {
	optedIn := false
	for _, key := range []string{extensionproto.CloudCapabilityAfterEnv, InternalReleasePlanCallbackEnv} {
		if envkeys.Host.Last(env, key) != "" {
			optedIn = true
		}
	}
	if optedIn {
		env = removeEnvKey(env, extensionproto.CloudTokenEnv)
	}
	for _, key := range []string{
		extensionproto.CloudCapabilityAfterEnv,
		InternalReleasePlanCallbackEnv,
		InternalReleaseSetProviderCapabilityEnv,
		runtimeproto.ReleaseSetPublishedImagesFileEnv,
		runtimeproto.ReleaseSetMembersFileEnv,
	} {
		env = removeEnvKey(env, key)
	}
	return env
}
