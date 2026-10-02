package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	registryproto "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

const (
	releaseSetResultKey = "putnami:publish~release-set"
	// ReleaseSetHeadBaselineSource is the selection tier a release-set publish
	// reports: impact was measured against the channel head's recorded
	// fingerprints, never against a git ref.
	ReleaseSetHeadBaselineSource = "release-set-head"
	// ReleaseSetBaselineChannelSource is the tier reported when that head came
	// from the channel `--baseline-channel` named rather than from the channel
	// being advanced: the first publish into an empty channel measured against
	// another channel's head, which is a different statement about the same
	// set id and a reader must be able to tell the two apart.
	ReleaseSetBaselineChannelSource = "release-set-baseline-channel"
)

var releaseSetInternalProject = &workspace.Project{
	ID: "putnami", Name: "putnami", Path: ".putnami/internal/release-set",
}

var releaseSetInternalExtension = &extension.ExtensionDescription{
	Name: "@putnami/cli", Version: "internal",
}

var sourceRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// releaseSetProvider is the provider seam: one resolve that answers every
// listed channel — an empty one with a null head — and one release that stores
// the set and advances every listed channel by compare-and-swap in a single
// transaction.
type releaseSetProvider interface {
	Resolve(context.Context, *distribution.ResolveRequest) (*distribution.ResolveResponse, error)
	Release(context.Context, *distribution.ReleaseRequest) (*distribution.ReleaseResponse, error)
}

var newReleaseSetProvider = func(ctx context.Context, provider *extension.ResolvedProvider) (releaseSetProvider, error) {
	if provider == nil || provider.ExtensionName == "" || provider.Command != distribution.ProviderCommandName {
		return nil, fmt.Errorf("resolved release-set provider is not bound to the reserved command")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate putnami executable: %w", err)
	}
	// The client starts `putnami cloud release-set`, a CLI that loads the
	// workspace's extensions.
	runcredential.MarkRepositoryCodeStarted("putnami cloud release-set")
	client := releaseset.NewClient(executable)
	client.Env = ReleaseSetProviderProcessEnv(ctx, provider)
	return client, nil
}

// releaseSetProvenance is the tree identity a publish records on every
// selected member: the commit it publishes from and, when it can be read, the
// git tree its checkout holds. Whether a member is republished is decided by
// its selection fingerprint, which the engine computes from the package task's
// execution key (D13) — never from this revision or tree, and never from the
// git history between two publications.
type releaseSetProvenance struct {
	revision string
	// tree is HEAD's git tree on a clean checkout when the repository opts in
	// (releaseMemberSourceTree), "" otherwise.
	tree string
}

// newReleaseSetProvenance reads the workspace's git identity: the commit the
// publication is bound to, and, when withTree asks for it, the tree its
// checkout holds (releaseSourceTree). The commit is PUTNAMI_SOURCE_REVISION
// when a runner sets it, else HEAD. git.BoundSourceRevision accepts the
// override only when the version suffix can be stamped from it, so a
// publication is never recorded under a commit whose version could not be
// built. The override is still held to the Distribution protocol's
// 40-character shape, exactly as HEAD is.
var newReleaseSetProvenance = func(ws *workspace.Workspace, withTree bool) (*releaseSetProvenance, error) {
	revision, set, err := git.BoundSourceRevision(ws.Root)
	if err != nil {
		return nil, fmt.Errorf("release-set publish requires a full commit revision: %w", err)
	}
	if !set {
		revision, err = git.HeadSHA(ws.Root)
		if err != nil {
			return nil, fmt.Errorf("release-set publish requires a git checkout to record the source revision: %w", err)
		}
	}
	revision = strings.ToLower(strings.TrimSpace(revision))
	if !sourceRevisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("release-set publish requires a full commit revision, got %q", revision)
	}
	provenance := &releaseSetProvenance{revision: revision}
	if withTree {
		provenance.tree = releaseSourceTree(ws.Root)
	}
	return provenance, nil
}

// releaseSourceTree is the git tree the publication builds from: HEAD's tree,
// even when PUTNAMI_SOURCE_REVISION binds the publication to another commit,
// because HEAD's checkout is what the package tasks read. It is "" when the
// checkout has uncommitted or untracked changes, since HEAD's tree would then
// claim content the build did not read, and when git cannot answer. Publishing
// never fails for want of a tree: the field is optional provenance.
func releaseSourceTree(root string) string {
	if clean, err := git.WorktreeClean(root); err != nil || !clean {
		return ""
	}
	tree, err := git.HeadTree(root)
	if err != nil || !sourceRevisionPattern.MatchString(tree) {
		return ""
	}
	return tree
}

// currentReleaseSourceTree reads the tree again when the set is committed.
// Tests replace it with the provenance seam, because a fixture workspace is a
// temporary directory rather than a clone.
var currentReleaseSourceTree = releaseSourceTree

// dropRepublishedSourceTree removes the tree from every member the plan
// republished. A task that ran after planning changed the checkout, so the
// tree read at planning no longer names what the artifacts were built from.
// An inherited member keeps its head's tree: another publication built it.
func dropRepublishedSourceTree(set *distribution.ReleaseSet, plan *releaseset.Plan) {
	for index := range set.Members {
		member := &set.Members[index]
		if planned, ok := plan.Member(member.Ecosystem, member.Coordinate); ok && planned.Selected {
			member.SourceTree = ""
		}
	}
}

// ReleaseSetRun retains the one immutable plan and provider selected before
// package planning. The engine only carries this opaque coordinator between
// planning and finalization; release-set policy remains in the job layer.
type ReleaseSetRun struct {
	plan     *releaseset.Plan
	provider releaseSetProvider
	dryRun   bool
	// routes map each member identity to the exact package and publish steps in
	// the extension metadata block that declared it. A profile owner defines
	// registry semantics; an extension using that profile owns the jobs that emit
	// its members. Selection is per member, so project-level routing is not exact
	// enough when one project publishes (for example) both npm and OCI artifacts.
	routes map[string]releaseMemberRoute
	// headMeasured records that the session's project selection was decided
	// by this plan against the channel head rather than by a git baseline.
	headMeasured bool
	// visibility is the chain the repository declared, with every member
	// selector already resolved to coordinates. It is built once, with the
	// plan, so the release carries exactly the policy the plan was made under.
	visibility distribution.VisibilityChain
	// mirrors is an immutable copy of the declared destination hints. The
	// provider owns credentials, public eligibility and durable copy work.
	mirrors map[string]distribution.MirrorTarget
	// channelLevels is the level each listed channel confers, one step of the
	// chain, defaulting to the repository level.
	channelLevels map[string]distribution.Visibility
	// immutableChannel is the tag's channel: created once with no expected
	// head and never moved again. It is "" for an untagged publish.
	immutableChannel string
	// sourceRevision is the full HEAD commit the publication runs from. A
	// selected member records it too; the run keeps its own copy so a plan
	// that selects nothing can still be handed to the runner's broker under
	// the commit it releases from.
	sourceRevision string
	// sourceTree is the tree the plan recorded on its republished members, ""
	// when it recorded none, and root the checkout it was read from. The
	// commit reads the tree again and drops it when the checkout changed.
	sourceTree string
	root       string
	// releasedRef and releasedMembers are what the commit stored: the exact
	// set a same-session deploy synchronizes on, and the members it hands to
	// each workload. They exist only after the release, which is why the
	// barrier, and not the planner, completes a deploy contract with them.
	releasedRef     distribution.ReleaseSetRef
	releasedMembers []distribution.ReleaseSetMember
	// publishJobKeys maps each selected member's kept publish job key to the
	// member it must publish. AttachPlan records it so the commit can tell a
	// publish job the scheduler skipped apart from one that never ran at all,
	// and name it instead of reporting a bare missing record.
	publishJobKeys map[string]string
	// verification is set by ScopePlanning when the session named a gate
	// command beside the publish and handed over the selection that command
	// computed. It records that the plan the coordinator narrows is the
	// publish alone: the verification projects keep their gate nodes, the
	// selection block keeps the gate's mode and baseline, and AttachPlan drops
	// the publish and publish-only package nodes of every project that owns no
	// selected member. Nil for a publish-only session, which keeps the
	// coordinator-decided selection exactly as before.
	verification *releaseVerificationScope
	// publication is the run's publication-v1 provider, nil when the run
	// publishes through a provider process. With one, provider resolves
	// through it, open is what the open node sends (BindPublication), and
	// barrierCommands are the bound request's barrier commands.
	publication     PublicationProvider
	open            *registryproto.OpenParams
	barrierCommands []string
	// uploads maps each upload node's key to the publication job it uploads
	// for, and outboxes each publication job's key to its private outbox
	// directory (PublicationContext).
	uploads  map[string]string
	outboxes map[string]string
	// barrierAttached records that a deploy barrier releases the set, so the
	// session finalizer does not (Finalizer).
	barrierAttached bool
}

// releaseVerificationScope is what a mixed gate+publish session settled when it
// was scoped: the commands it verifies with, read back by AttachPlan to decide
// whether a package node may be an explicit root or is always a prerequisite.
type releaseVerificationScope struct {
	commands []string
}

type releaseMemberRoute struct {
	projectID        string
	publisher        string
	packagePublisher string
	packageStep      string
	publishCommand   string
	publishStep      string
}

// ReleaseSetMeasuresAgainstHead reports whether the engine must hand the
// whole workspace to the coordinator instead of resolving a git baseline: a
// publish-only --impacted release-set transaction measures impact against
// the channel head's recorded fingerprints, so a diff that may be stale after
// a CI gap must not narrow the candidates first.
func ReleaseSetMeasuresAgainstHead(options ReleaseSetOptions) bool {
	return RequestedReleaseSetMode(options) == ReleaseSetImpacted &&
		!options.PlannedPublish && !slices.Contains(options.Commands, "deploy")
}

// ReleaseSetMode describes whether a publish participates in release-set
// coordination and how it selects members.
type ReleaseSetMode uint8

const (
	ReleaseSetDisabled ReleaseSetMode = iota
	// ReleaseSetImpacted is publish --impacted --channel: every listed head is
	// resolved once and a member is republished exactly when its selection
	// fingerprint differs from the baseline head's record, or when one of its
	// internal dependencies is republished. Unchanged members inherit the
	// baseline record.
	ReleaseSetImpacted
	// ReleaseSetAll is publish --all --channel: every member is repackaged
	// and each listed head, when one exists, is still its own CAS expectation.
	ReleaseSetAll
)

// ReleaseSetOptions is the typed run input extracted by the engine from CLI
// flags. It prevents the coordinator from creating another untyped parameter
// boundary while preserving the exact terminal dry-run value.
type ReleaseSetOptions struct {
	Commands []string
	// Channels are the channels this publication advances, in the order the
	// caller named them. The FIRST is the baseline when it has a head: impact
	// is measured against it and unchanged members inherit from it. Every
	// other channel is advanced to the same set from its own resolved head
	// (D14).
	Channels []string
	// BaselineChannel is the channel impact is measured against when the first
	// advanced channel has no head yet. It is READ and never advanced, so a
	// pull-request channel's first publish inherits the head `main` already
	// produced instead of republishing every member (D14, ADR 0006). "" names
	// none, which is the unchanged behavior.
	BaselineChannel string
	Impacted        bool
	All             bool
	DryRun          bool
	// PlannedPublish is set only by the engine after the final command plan
	// proves that a dependent command expanded publish in this same session.
	// It is never CLI/manifest input and cannot turn an unrelated deploy into a
	// release-set transaction.
	PlannedPublish bool
	// Versions is the run's version per line. A member is stamped with the
	// version of its own project's line, never with one version for the run.
	Versions RunVersions
	// Profiles is the workspace's resolved ecosystem registry. The coordinator
	// names no ecosystem itself: what `npm` or `oci` means, and which extension
	// owns it, comes from here (D12).
	Profiles *extproto.ProfileRegistry
	// Fingerprints maps a member key to the selection fingerprint of its
	// package task's execution key, computed by the engine before planning.
	Fingerprints map[string]string
	// Tagged reports that HEAD carries the tag of the line named by Line, so
	// this publication is that line's cohort: every member of the line is
	// republished at the tag's version and the tag's immutable channel is
	// created (D1, D2). It is derived from Versions, never from a flag.
	Tagged bool
	// Line is the scope path of the version line a tagged publish releases. ""
	// is the root line, which is also the value of an untagged publish.
	Line string
	// Visibility is `publish --visibility <level>`: the per-publication level
	// of the inheritance chain, between the version and the member levels. It
	// is carried, never computed (D5).
	Visibility string
	// Policy is the repository's declared distribution section of
	// putnami.ci.json: the visibility chain and the protected channels. It is
	// nil in a workspace that declares no CI document.
	Policy *ciproto.Distribution
}

// ReleaseSetRequest is everything the engine knows about a run before it
// selects a single project: the commands, the flags a release-set publish
// reads, and the run's version per line. It exists so the whole "what does this
// run publish, and under which policy" question is answered in ONE call from
// the engine, which assembles runs and owns no distribution policy (ADR 0001
// §3).
type ReleaseSetRequest struct {
	// Commands is the run's command list.
	Commands []string
	// WorkspaceRoot is where putnami.ci.json is read from.
	WorkspaceRoot string
	// Channel is the raw `--channel a,b` value.
	Channel string
	// BaselineChannel is the raw `--baseline-channel <name>` value: one
	// channel the publication reads and never advances.
	BaselineChannel string
	// Visibility is the raw `--visibility <level>` value.
	Visibility string
	// Scope is the raw `--scope <line>` value: which version line a tagged
	// publish releases when HEAD carries the tag of several.
	Scope string
	// DryRun is the terminal dry-run value the publish jobs will see.
	DryRun bool
	// Impacted and All are the run's project-selection flags.
	Impacted, All bool
	// Versions is the run's version per line, derived from git.
	Versions RunVersions
}

// BuildReleaseSetOptions turns one run request into the typed release-set
// options every later stage reads: the channels named, the repository's
// declared distribution policy, and whether HEAD's tag makes this publication a
// line's cohort.
func BuildReleaseSetOptions(request ReleaseSetRequest) (ReleaseSetOptions, error) {
	channels, err := ParseReleaseSetChannels(request.Channel)
	if err != nil {
		return ReleaseSetOptions{}, cmderr.Usagef("%v", err)
	}
	baselineChannel, err := ParseReleaseSetBaselineChannel(request.BaselineChannel, channels)
	if err != nil {
		return ReleaseSetOptions{}, cmderr.Usagef("%v", err)
	}
	// The declared policy is read by the runs that distribute, and by them
	// alone. An invalid CI document must fail `ci validate` and every publish,
	// not the lint/test/build gate that never consults it.
	var policy *ciproto.Distribution
	if distributingRun(request.Commands) {
		if policy, err = LoadDistributionPolicy(request.WorkspaceRoot); err != nil {
			return ReleaseSetOptions{}, err
		}
	}
	line, tagged, err := taggedRunLine(request.Commands, request.Versions, request.Scope)
	if err != nil {
		return ReleaseSetOptions{}, err
	}
	return ReleaseSetOptions{
		Commands: request.Commands, Channels: channels, BaselineChannel: baselineChannel,
		Impacted: request.Impacted, All: request.All, DryRun: request.DryRun,
		Versions: request.Versions, Tagged: tagged, Line: line,
		Visibility: request.Visibility, Policy: policy,
	}, nil
}

// distributingRun reports a run that can consult the repository's distribution
// policy: a publish, or a deploy whose environment follows a channel.
func distributingRun(commands []string) bool {
	return slices.Contains(commands, "publish") || slices.Contains(commands, "deploy")
}

// taggedRunLine names the version line HEAD's tag releases.
//
// The question is only asked of a run that publishes: an ordinary build on a
// tagged commit is still an ordinary build, and must not be refused for an
// ambiguity it never consults. When several lines are tagged at once — the
// seeding commit that tags every line, or a runner checkout that carries every
// tag of the commit — `--scope` names the one being released; without it the
// publish is refused rather than guessing which cohort was meant (D9).
func taggedRunLine(commands []string, versions RunVersions, scope string) (string, bool, error) {
	if !slices.Contains(commands, "publish") {
		return "", false, nil
	}
	tagged := make([]string, 0, len(versions))
	for line, info := range versions {
		if info != nil && info.Tagged && strings.TrimSpace(info.Tag) != "" {
			tagged = append(tagged, line)
		}
	}
	sort.Strings(tagged)
	if requested := strings.TrimSpace(scope); requested != "" {
		if !slices.Contains(tagged, requested) {
			return "", false, cmderr.Usagef(
				"--scope %q does not name a version line tagged on HEAD", requested,
			)
		}
		return requested, true, nil
	}
	switch len(tagged) {
	case 0:
		return "", false, nil
	case 1:
		return tagged[0], true, nil
	}
	names := make([]string, 0, len(tagged))
	for _, line := range tagged {
		names = append(names, versions[line].Tag)
	}
	return "", false, cmderr.Usagef(
		"HEAD carries the tag of %d version lines (%s); a publish releases one line at a time — name it with --scope <line>",
		len(tagged), strings.Join(names, ", "))
}

// ParseReleaseSetChannels reads the `--channel a,b` list into the exact
// channel names one publication advances: trimmed, unique, order preserved,
// every one of them portable. An unusable name is refused here rather than
// after the upload, where the registry's own answer names a concept the caller
// never asked for.
func ParseReleaseSetChannels(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var channels []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !distribution.IsPortableChannel(name) {
			return nil, fmt.Errorf("--channel %s is not a portable channel name; a channel matches %s", name, distribution.ChannelPattern)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		channels = append(channels, name)
	}
	if len(channels) == 0 {
		return nil, fmt.Errorf("--channel %q names no channel", raw)
	}
	if len(channels) > distribution.MaxChannelsPerRelease {
		return nil, fmt.Errorf("--channel names %d channels, exceeding the limit of %d", len(channels), distribution.MaxChannelsPerRelease)
	}
	return channels, nil
}

// ParseReleaseSetBaselineChannel reads the `--baseline-channel <name>` value:
// one portable channel name the publication READS and never advances, or "" for
// none.
//
// It is refused when it names one of the advanced channels. That channel is
// already its own baseline — the precedence below prefers its own head — so
// naming it twice would say two different things about one head, and the single
// resolve would carry a duplicate the protocol refuses anyway.
//
// The bound belongs here too: the ONE resolve of the publication names the
// advanced channels plus the baseline, and a resolve request carries at most
// MaxChannelsPerRelease channels, so a publish that already advances that many
// cannot also read a baseline. Saying so at parse time costs nothing and names
// the flag; discovering it at the provider would name a limit the caller never
// asked about.
func ParseReleaseSetBaselineChannel(raw string, advanced []string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	if !distribution.IsPortableChannel(name) {
		return "", fmt.Errorf("--baseline-channel %s is not a portable channel name; a channel matches %s", name, distribution.ChannelPattern)
	}
	if slices.Contains(advanced, name) {
		return "", fmt.Errorf("--baseline-channel %s is already advanced by --channel; a baseline is read and never advanced, and an advanced channel is already its own baseline", name)
	}
	if len(advanced)+1 > distribution.MaxChannelsPerRelease {
		return "", fmt.Errorf("--channel names %d channels and --baseline-channel one more to resolve, exceeding the limit of %d", len(advanced), distribution.MaxChannelsPerRelease)
	}
	return name, nil
}

// releaseSetResolveChannels is what the ONE resolve of the publication names:
// the advanced channels in order, then the baseline when it is not already
// among them. The baseline's head must come from the SAME read as the others,
// or the plan would compare a head from one instant against expectations from
// another.
func releaseSetResolveChannels(options ReleaseSetOptions, channels []string) []string {
	resolved := append([]string(nil), channels...)
	if options.BaselineChannel != "" && !slices.Contains(resolved, options.BaselineChannel) {
		resolved = append(resolved, options.BaselineChannel)
	}
	return resolved
}

// releaseSetBaselineHead applies the precedence of D14 as amended by ADR 0006
// and reports which channel the head came from ("" for an empty baseline):
//
//  1. the first advanced channel's own head, when it exists;
//  2. otherwise the named baseline channel's head, when it has one;
//  3. otherwise nothing: every member is selected.
//
// Own-head-first is what keeps this a pure fallback. Every publication that
// already has a head behaves exactly as it did before a baseline could be
// named, so the second push of a pull request still measures against what the
// first one published rather than against a channel that moved meanwhile.
func releaseSetBaselineHead(
	options ReleaseSetOptions,
	channels []string,
	heads map[string]*distribution.ChannelHead,
) (*distribution.ChannelHead, string) {
	if len(channels) == 0 {
		return nil, ""
	}
	if head := heads[channels[0]]; head != nil {
		return head, channels[0]
	}
	if options.BaselineChannel == "" {
		return nil, ""
	}
	if head := heads[options.BaselineChannel]; head != nil {
		return head, options.BaselineChannel
	}
	return nil, ""
}

// ValidateReleaseSetSelection rejects a publish that names a channel the
// release-set coordinator owns but a selection the coordinator cannot serve.
//
// A named channel only enters release-set coordination through --all or
// --impacted. With any other selection the run used to fall through to the
// legacy per-package path, publish the artifacts, and only then have the
// registry refuse the channel write:
//
//	409 Conflict - dist-tag canary is managed by the canonical release-set channel
//
// That answer arrives after the upload, names a concept the caller never asked
// for, and leaves the version published but unreferenced. Refusing up front
// costs nothing and says which flag is missing.
//
// A tagged publish is accepted with no selection flag at all, and with no
// --channel: the tag itself names the cohort (its line) and the immutable
// channel the release creates (D1, D2). A tree with uncommitted changes is
// refused there, because the tag would then name content that is not what was
// tagged.
func ValidateReleaseSetSelection(options ReleaseSetOptions) error {
	if !slices.Contains(options.Commands, "publish") && !options.PlannedPublish {
		return nil
	}
	for _, name := range options.Channels {
		if ProtectedChannel(options.Policy, name) {
			return cmderr.Usagef("channel %s is protected; use putnami channel set", name)
		}
	}
	if options.Tagged {
		if info := options.Versions[options.Line]; info != nil && info.IsDirty {
			return cmderr.Usagef("a tagged publish needs a clean tree")
		}
	}
	if err := validateReleaseSetBaselineChannel(options); err != nil {
		return err
	}
	if len(options.Channels) == 0 || options.Impacted || options.All || options.Tagged {
		return nil
	}
	return cmderr.Usagef(
		"--channel %s selects release-set channels, which requires --all or --impacted; "+
			"the current selection would publish through the legacy per-package path and the registry rejects its channel write",
		strings.Join(options.Channels, ","),
	)
}

// validateReleaseSetBaselineChannel refuses a baseline on the publications that
// could not read one soundly.
//
// A publication that advances no channel at all is not a release-set
// publication: it falls to the legacy per-package path, where nothing reads a
// head and the flag would be a silent no-op. The document half refuses the
// same shape — a `baseline` on a rule that publishes nothing is
// `ci.invalid_baseline` (ADR 0004) — on the same grounds, that it is an
// authoring mistake rather than a no-op. Only a hand-typed invocation reaches
// this: `Explain` sets `Baseline` only when the rule publishes something, so a
// runner cannot render the combination.
//
// --all republishes every member by definition, so a head to inherit from would
// change nothing and naming one would misdescribe the run. A tagged publish is
// its line's cohort and already has its own inheritance rule, where the members
// of the tagged line are republished whatever any head recorded. And a
// selection that is neither --impacted nor --all does not measure impact at
// all, so there is nothing for the baseline to be the baseline OF.
func validateReleaseSetBaselineChannel(options ReleaseSetOptions) error {
	if options.BaselineChannel == "" {
		return nil
	}
	switch {
	case len(options.Channels) == 0 && !options.Tagged:
		return cmderr.Usagef(
			"--baseline-channel %s names the head a published channel measures against, but --channel names none to publish",
			options.BaselineChannel)
	case options.All:
		return cmderr.Usagef(
			"--baseline-channel %s measures impact against a head, which --all never does; drop one of the two flags",
			options.BaselineChannel)
	case options.Tagged:
		return cmderr.Usagef(
			"--baseline-channel %s cannot apply to a tagged publish: the tag republishes its whole version line whatever a head recorded",
			options.BaselineChannel)
	case !options.Impacted:
		return cmderr.Usagef(
			"--baseline-channel %s names the head impact is measured against, which requires --impacted",
			options.BaselineChannel)
	}
	return nil
}

// RequestedReleaseSetMode classifies a publish without resolving a provider.
//
// A tagged publish is ReleaseSetAll whatever the selection flags say: the tag
// is an implicit --all for its line (D1), and it coordinates a release set even
// when no --channel is named, because the immutable channel it creates is
// derived from the tag rather than requested.
func RequestedReleaseSetMode(options ReleaseSetOptions) ReleaseSetMode {
	if !slices.Contains(options.Commands, "publish") && !options.PlannedPublish {
		return ReleaseSetDisabled
	}
	if options.Tagged {
		return ReleaseSetAll
	}
	if len(options.Channels) == 0 {
		return ReleaseSetDisabled
	}
	if options.Impacted {
		return ReleaseSetImpacted
	}
	if options.All {
		return ReleaseSetAll
	}
	return ReleaseSetDisabled
}

// ReleaseSetPlansEveryProject reports whether the coordinator, not the caller's
// selection, decides which projects this session PUBLISHES. A channel-backed
// --impacted publish measures against the head, --all republishes every member,
// and a tagged publish selects a whole version line. Each would be silently
// truncated by a git diff, a cwd-derived default selection, or a workspace
// default tag exclusion, so the engine hands the whole workspace over and lets
// the release-set plan narrow it back. A publish pulled in by another command
// remains bounded by that command's project selection.
//
// A gate command that shares the session with the publish keeps the selection
// the caller asked for; the coordinator narrows the publish alone. The
// engine therefore resolves BOTH selections when this predicate holds beside
// another command: the caller's, for lint/test/build/validate, and the whole
// workspace, for the keying plan every member is fingerprinted from.
func ReleaseSetPlansEveryProject(options ReleaseSetOptions) bool {
	if options.PlannedPublish {
		return false
	}
	return RequestedReleaseSetMode(options) == ReleaseSetAll || ReleaseSetMeasuresAgainstHead(options)
}

// TaggedReleaseSetPublish reports a publish that HEAD's tag turned into its
// line's cohort. It is the one predicate every tagged behavior reads, so the
// engine's precomputed flag and the run's own versions can never disagree.
func TaggedReleaseSetPublish(options ReleaseSetOptions) bool {
	return taggedLineVersion(options) != nil
}

// taggedLineVersion is the version of the line a tagged publish releases, or
// nil when this publication is not a tagged one.
func taggedLineVersion(options ReleaseSetOptions) *JobContextVersion {
	if !options.Tagged {
		return nil
	}
	if !slices.Contains(options.Commands, "publish") && !options.PlannedPublish {
		return nil
	}
	info := options.Versions[options.Line]
	if info == nil || !info.Tagged || strings.TrimSpace(info.Tag) == "" {
		return nil
	}
	return info
}

// taggedReleaseChannel is the immutable channel a tagged publish creates: the
// tag itself in the portable encoding, ts/v0.3.0 becoming ts-v0.3.0 (D33). It
// is "" for an untagged publish.
func taggedReleaseChannel(options ReleaseSetOptions) (string, error) {
	info := taggedLineVersion(options)
	if info == nil {
		return "", nil
	}
	channel, err := distribution.EncodeTagAsChannel(info.Tag)
	if err != nil {
		return "", cmderr.Usagef("%v", err)
	}
	return channel, nil
}

// PlannedCommandProjects returns the deterministic project selection actually
// represented by one expanded command. It is used only after planning, so a
// dependent command's local apps/projectIf policy — not the outer CLI's wider
// selection — defines the publish jobs the release set may rely on.
func PlannedCommandProjects(planned []*ScheduledJob, command string) []*workspace.Project {
	byID := make(map[string]*workspace.Project)
	for _, job := range schedulableJobs(planned) {
		if job == nil || job.JobDef == nil || job.CommandName() != command {
			continue
		}
		if job.Project != nil {
			byID[job.Project.ID] = job.Project
		}
		for _, selected := range job.SelectedProjects {
			if selected != nil {
				byID[selected.ID] = selected
			}
		}
	}
	ids := slices.Sorted(maps.Keys(byID))
	projects := make([]*workspace.Project, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, byID[id])
	}
	return projects
}

// PrepareDependentRun decides everything that can only be decided once the
// FINAL command plan exists, and it is the one seam the engine calls for it.
//
// Two questions live here. A dependent publish — one the planner expanded
// under another command — coordinates a release set the outer command never
// named, and only the planned publish nodes say which members it may
// republish. A deploy that names an environment takes its workload contract
// from the set that environment follows, and only the planned deploy nodes say
// which workloads this session synchronizes. Both are plan-shaped answers, so
// both are answered after planning and neither is run assembly (ADR 0001 §3).
func PrepareDependentRun(
	ctx context.Context,
	prepared *ReleaseSetRun,
	options ReleaseSetOptions,
	deploy DeployOptions,
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	discovered *extension.DiscoveryResult,
) (*ReleaseSetRun, error) {
	run, err := prepareDependentReleaseSet(ctx, prepared, options, ws, planned, discovered)
	if err != nil {
		return nil, fmt.Errorf("release-set publish: %w", err)
	}
	if err := PrepareDeployTargets(ctx, deploy, ws, discovered, planned, run != nil); err != nil {
		return nil, err
	}
	return run, nil
}

// prepareDependentReleaseSet prepares the coordinator after the planner has
// expanded a dependent publish command. The outer deploy selection stays
// intact; the actual publish nodes bound which members this session can
// republish, and the plan fails closed when the head requires more.
func prepareDependentReleaseSet(
	ctx context.Context,
	prepared *ReleaseSetRun,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	discovered *extension.DiscoveryResult,
) (*ReleaseSetRun, error) {
	if prepared != nil || (len(options.Channels) == 0 && !options.Tagged) {
		return prepared, nil
	}
	projects := PlannedCommandProjects(planned, "publish")
	if len(projects) == 0 {
		return nil, nil
	}
	options.PlannedPublish = true
	return PrepareReleaseSet(ctx, options, ws, projects, discovered, options.Fingerprints)
}

// AttachBarrier makes the provider release an ordinary, visible DAG boundary
// whenever publish and deploy share one session. Every publication must
// finish before the framework-owned node runs; every deploy waits for its
// successful result. The callback is carried separately in memory, so a
// manifest can neither synthesize nor replace the coordinator implementation.
//
// The barrier is also where a same-session deploy receives its workload
// contract: the set it synchronizes on is the one this release just stored, so
// its ref and the member digests each workload owns are handed forward between
// the commit and the first deploy task (T13, ADR 0021 §13).
//
// A run that publishes through a publication-v1 provider first gains its open
// and upload nodes (attachPublication), and the barrier then waits for every
// upload too.
func (run *ReleaseSetRun) AttachBarrier(planned []*ScheduledJob) ([]*ScheduledJob, map[string]InternalJobRunner, error) {
	if run == nil || run.provider == nil || run.plan == nil {
		return planned, nil, nil
	}
	if run.Publication() {
		return run.attachPublication(planned)
	}
	return run.attachDeployBarrier(planned)
}

// attachDeployBarrier adds the release node every deploy waits for, when the
// plan holds a deploy.
func (run *ReleaseSetRun) attachDeployBarrier(planned []*ScheduledJob) ([]*ScheduledJob, map[string]InternalJobRunner, error) {
	var publishKeys []string
	var deployJobs []*ScheduledJob
	selectedByID := make(map[string]*workspace.Project)
	for _, job := range schedulableJobs(planned) {
		if job == nil || job.JobDef == nil || job.Project == nil {
			continue
		}
		switch job.CommandName() {
		case "publish":
			publishKeys = append(publishKeys, job.Key())
		case "deploy":
			deployJobs = append(deployJobs, job)
			for _, project := range job.SelectedProjects {
				if project != nil {
					selectedByID[project.ID] = project
				}
			}
		}
	}
	if len(deployJobs) == 0 {
		return planned, nil, nil
	}
	publishKeys = dedupeSorted(publishKeys)
	if len(publishKeys) == 0 {
		return nil, nil, fmt.Errorf("same-session release set has deploy jobs but no publish jobs")
	}
	for _, job := range planned {
		if job != nil && job.Key() == releaseSetResultKey {
			return nil, nil, fmt.Errorf("release-set coordinator key %q collides with a planned job", releaseSetResultKey)
		}
	}
	selectedIDs := slices.Sorted(maps.Keys(selectedByID))
	selected := make([]*workspace.Project, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		selected = append(selected, selectedByID[id])
	}
	barrier := &ScheduledJob{
		Project:          releaseSetInternalProject,
		Extension:        releaseSetInternalExtension,
		SelectedProjects: selected,
		DependsOn:        publishKeys,
		JobDef: &extension.JobDefinition{
			ExtensionName: releaseSetInternalExtension.Name,
			Name:          "publish~release-set",
			CommandName:   "publish",
			Kind:          "release-set",
			Cache:         false,
			Traits: extension.CommandTraits{
				SideEffects: extension.SideEffectsCloud,
			},
		},
	}
	if barrier.Key() != releaseSetResultKey {
		return nil, nil, fmt.Errorf("release-set coordinator identity = %q, want %q", barrier.Key(), releaseSetResultKey)
	}
	for _, deploy := range deployJobs {
		deploy.DependsOn = dedupeSorted(append(deploy.DependsOn, barrier.Key()))
	}
	planned = append(planned, barrier)
	if err := validatePlanDAG(schedulableJobs(planned)); err != nil {
		return nil, nil, fmt.Errorf("attach release-set coordinator: %w", err)
	}
	run.barrierAttached = true
	return planned, map[string]InternalJobRunner{
		barrier.Key(): func(ctx context.Context, results map[string]*JobResult) *JobResult {
			if run.dryRun {
				return &JobResult{Status: "success"}
			}
			result := run.commit(ctx, results)
			if result == nil || result.Status != "success" {
				return result
			}
			if err := completeDeployTargets(deployJobs, run.releasedRef, run.releasedMembers); err != nil {
				return releaseSetFailure(err)
			}
			return result
		},
	}, nil
}

// PrepareReleaseSet resolves every listed channel exactly once and builds the
// one plan every publish shares. --all selects every member; --impacted selects
// the members whose selection fingerprint differs from the baseline head's
// record plus every member that depends on a selected one. An empty channel is
// an ordinary answer (null head): every member is selected and that channel's
// release expects no head. Every later stage retains that immutable plan and
// never resolves a channel again.
//
// selected is the engine's project selection. An explicit publish ignores it
// for membership, because the head, not a git baseline, decides impact; a
// dependent publish (options.PlannedPublish) is bounded by it, because only
// those projects have publish jobs in the plan.
func PrepareReleaseSet(
	ctx context.Context,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	selected []*workspace.Project,
	discovered *extension.DiscoveryResult,
	fingerprints map[string]string,
) (*ReleaseSetRun, error) {
	if fingerprints != nil {
		options.Fingerprints = fingerprints
	}
	if err := ValidateReleaseSetSelection(options); err != nil {
		return nil, err
	}
	mode := RequestedReleaseSetMode(options)
	if mode == ReleaseSetDisabled {
		return nil, nil
	}
	if ws == nil {
		return nil, fmt.Errorf("release-set publish requires a workspace")
	}
	if options.Profiles == nil {
		return nil, fmt.Errorf("release-set publish requires the workspace ecosystem profiles")
	}
	candidates, err := releaseProjects(ws.Projects, options.Profiles)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		// No publishable member: there is nothing to coordinate and the
		// provider-neutral publish path proceeds unchanged.
		return nil, nil
	}

	// A credential provider that negotiated publication-v1 is the run's only
	// publication authority: it resolves, and no provider process starts.
	publication, err := currentPublicationProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("release-set publish: %w", err)
	}
	if publication != nil {
		if err := requireFullClone(ws.Root); err != nil {
			return nil, err
		}
		return prepareReleaseSetRun(ctx, options, ws, selected, candidates, sessionResolver{publication}, publication)
	}

	providerDescription, err := extension.ResolveReservedProvider(discovered.Extensions, distribution.ProviderCommandName)
	if err != nil {
		return nil, fmt.Errorf("resolve release-set provider: %w", err)
	}
	if providerDescription == nil {
		if mode == ReleaseSetAll {
			// Full publication is the provider-neutral compatibility floor. With
			// no provider it proceeds unchanged and does not claim a release set.
			return nil, nil
		}
		message := fmt.Sprintf("publish --impacted --channel requires exactly one installed extension declaring %q; use publish --all for the cloudless full-release path",
			distribution.ProviderCommandName)
		if cause := extension.SkippedProviderCause(discovered.Skipped); cause != "" {
			message += ". " + cause
		}
		return nil, fmt.Errorf("%w: %s", releaseset.ErrProviderAbsent, message)
	}

	// A publication that releases refuses a checkout that cannot answer what
	// version a commit carries: a shallow clone would stamp a number computed
	// from the fraction of history that happened to be fetched (D9).
	if err := requireFullClone(ws.Root); err != nil {
		return nil, err
	}

	provider, err := newReleaseSetProvider(ctx, providerDescription)
	if err != nil {
		return nil, err
	}
	return prepareReleaseSetRun(ctx, options, ws, selected, candidates, provider, nil)
}

// prepareReleaseSetRun resolves every listed channel through provider and
// builds the run's plan. publication is the run's publication-v1 provider,
// nil when provider is a provider process.
func prepareReleaseSetRun(
	ctx context.Context,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	selected []*workspace.Project,
	candidates map[string]*releaseCandidate,
	provider releaseSetProvider,
	publication PublicationProvider,
) (*ReleaseSetRun, error) {
	heads, err := resolveReleaseSetHeads(ctx, provider, releaseSetNamespace(ws, options.Policy),
		releaseSetResolveChannels(options, options.Channels))
	if err != nil {
		return nil, err
	}
	provenance, err := newReleaseSetProvenance(ws, releaseMemberSourceTree(options.Policy))
	if err != nil {
		return nil, err
	}
	plan, err := buildReleaseSetPlan(options, ws, candidates, heads, provenance)
	if err != nil {
		return nil, err
	}
	if options.PlannedPublish {
		if err := validatePlannedPublishCoverage(plan, selected, plan.Channels); err != nil {
			return nil, err
		}
	}
	routes := make(map[string]releaseMemberRoute, len(candidates))
	for key, candidate := range candidates {
		routes[key] = releaseMemberRoute{
			projectID:        candidate.project.ID,
			publisher:        candidate.publisher,
			packagePublisher: candidate.packagePublisher,
			packageStep:      candidate.packageStep,
			publishCommand:   candidate.publishCommand,
			publishStep:      candidate.publishStep,
		}
	}
	run := &ReleaseSetRun{
		plan: plan, provider: provider, dryRun: options.DryRun,
		headMeasured: ReleaseSetMeasuresAgainstHead(options), routes: routes,
		sourceRevision: provenance.revision,
		sourceTree:     provenance.tree, root: ws.Root,
		publication: publication,
	}
	if run.immutableChannel, err = taggedReleaseChannel(options); err != nil {
		return nil, err
	}
	if run.visibility, err = BuildVisibilityChain(ws, options.Policy, options.Visibility, plan.Members); err != nil {
		return nil, err
	}
	if run.mirrors, err = BuildMirrorTargets(options.Policy); err != nil {
		return nil, err
	}
	if run.channelLevels, err = releaseChannelLevels(options.Policy, plan.Channels); err != nil {
		return nil, err
	}
	return run, nil
}

// requireFullClone is the git precondition seam of a release. Tests replace it
// with the provenance seam beside it, because a fixture workspace is a
// temporary directory rather than a clone.
var requireFullClone = RequireFullClone

// releaseSetNamespace is the declared provider identity, independent of the
// workspace's local name. Repositories without a policy retain the name default.
// The rule itself lives in release_set_visibility.go beside the policy loader,
// because a reader — `putnami channel set`, `putnami channel status` — has to
// address the SAME namespace this publisher writes to.
func releaseSetNamespace(ws *workspace.Workspace, policy *ciproto.Distribution) string {
	return distributionNamespace(policy, ws.Name)
}

// resolveReleaseSetHeads performs the ONE resolve of the whole publication.
// Every listed head is answered here; resolving them lazily, channel by
// channel, would let two of them be read at different instants and produce a
// release that never existed as one consistent state.
//
// A tagged publish that names no channel resolves nothing: its only channel is
// the tag's immutable one, which by definition has no head to compare against.
func resolveReleaseSetHeads(
	ctx context.Context,
	provider releaseSetProvider,
	namespace string,
	channels []string,
) (map[string]*distribution.ChannelHead, error) {
	if len(channels) == 0 {
		return map[string]*distribution.ChannelHead{}, nil
	}
	request := &distribution.ResolveRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       namespace,
		Channels:        append([]string(nil), channels...),
	}
	named := strings.Join(channels, ",")
	response, err := provider.Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("resolve release-set channels %s: %w", named, err)
	}
	if response == nil {
		return nil, fmt.Errorf("resolve release-set channels %s: provider returned no response", named)
	}
	if diagnostics := distribution.ValidateResolveExchange(request, response); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("resolve release-set channels %s: invalid provider exchange: %s", named, firstReleaseSetDiagnostic(diagnostics))
	}
	return response.Heads, nil
}

// releaseChannelLevels resolves the level every listed channel confers. A
// channel the repository is silent about confers the repository level.
func releaseChannelLevels(policy *ciproto.Distribution, channels []string) (map[string]distribution.Visibility, error) {
	repo, err := RepoVisibility(policy)
	if err != nil {
		return nil, err
	}
	levels := make(map[string]distribution.Visibility, len(channels))
	for _, name := range channels {
		level, err := ChannelVisibility(policy, name, repo)
		if err != nil {
			return nil, err
		}
		levels[name] = level
	}
	return levels, nil
}

// ReleaseSetPreparation is everything one release-set publish decides between
// project selection and the plan that will actually run: the coordinator, the
// projects and extensions the planner is narrowed (or widened) to, and the
// selection block the session reports.
type ReleaseSetPreparation struct {
	// Run is the coordinator, or nil when this session coordinates no release
	// set — a cloudless full publish, or any command that names no channel.
	Run *ReleaseSetRun
	// Projects and Extensions are what the planner must build its final plan
	// from. They equal the caller's inputs when Run is nil.
	Projects   []*workspace.Project
	Extensions []*extension.ExtensionDescription
	// Verification is the gate selection this session verifies over, echoed
	// back so the engine can report both sets: Projects is what the run plans
	// (the verification set widened with the members the head requires) and
	// Verification is the half the coordinator does not own. Nil for a
	// publish-only session, which verifies nothing beside its members.
	Verification []*workspace.Project
	// Selection is the selection block the session reports.
	Selection *protocoljob.Selection
	// ConfirmsHead is set when no member changed, so the release confirms the
	// head. A plan left empty by the scope is then the session's complete
	// answer, not a selection that matched nothing: the engine lets it through
	// the empty-plan refusal, and the finalizer confirms the head.
	ConfirmsHead bool
	// Options are the caller's options enriched with the workspace ecosystem
	// registry and the selection fingerprints. A dependent publish the planner
	// only expands later is prepared from these same options.
	Options ReleaseSetOptions
}

// ReleaseSetVerification is the selection a mixed session's NON-publish
// commands were resolved for, before the coordinator was handed the whole
// workspace. It travels as one value because its two halves answer one
// question: which projects this session verifies, and how that set was chosen.
type ReleaseSetVerification struct {
	// Projects are the projects the session's other commands plan over. Empty
	// is a real answer: an `--impacted` gate over an unchanged tree verifies
	// nothing while the publish still republishes what the head requires.
	Projects []*workspace.Project
	// Selection is the block that resolution produced — its mode, the baseline
	// it measured against and the tier that produced the ref. The session
	// reports these rather than the channel head, because they describe how the
	// verification half was chosen; the head decided the members alone.
	Selection *protocoljob.Selection
}

// PrepareReleaseSetRun is the ONE release-set seam the engine calls between
// selecting projects and planning them. It exists as a single call because
// every step of it is release-set policy — which ecosystems exist, which
// members they yield, what identity decides a republication, which head each
// channel is compared against, and which projects and extensions the planner is
// left with — and none of it is run assembly (ADR 0001 §3).
//
// keying is the plan built over the session's own selection. It is read only to
// derive one selection fingerprint per member: the execution key of the
// member's package task minus the embedded version (D13). The plan is never
// filtered in place, because a narrowed plan would key a different task graph
// than it measured; the caller rebuilds it from Projects and Extensions.
//
// deploySession distinguishes the two narrowing rules. A publish-only
// transaction narrows the planner to exactly the members the head requires; a
// combined publish+deploy session keeps its workload deploy nodes and WIDENS to
// those members instead, because its deploy selection cannot grow after
// planning.
//
// verification is the selection the session's OTHER commands were resolved for,
// and nil when publish is the only command. A gate command that shares
// the session with the publish keeps the selection the caller asked for: the
// planner is left with the members the head requires UNION that selection, and
// the coordinator narrows the publish alone.
func PrepareReleaseSetRun(
	ctx context.Context,
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	selected []*workspace.Project,
	verification *ReleaseSetVerification,
	planning []*extension.ExtensionDescription,
	discovered *extension.DiscoveryResult,
	keying []*ScheduledJob,
	commandParams map[string]any,
	cache *store.CacheManager,
	selection *protocoljob.Selection,
	deploySession bool,
) (*ReleaseSetPreparation, error) {
	// Reject an unsupported channel selection before fingerprint resolution.
	// Fingerprinting assumes a coordinator-owned selection and may otherwise
	// report a missing package route for an unrelated workspace member instead
	// of the actionable --all/--impacted usage error.
	if err := ValidateReleaseSetSelection(options); err != nil {
		return nil, err
	}
	options, err := resolveSelectionFingerprints(options, ws, discovered, keying, commandParams, cache)
	if err != nil {
		return nil, err
	}
	prepared := &ReleaseSetPreparation{
		Projects: selected, Extensions: planning, Selection: selection, Options: options,
	}
	run, err := PrepareReleaseSet(ctx, options, ws, selected, discovered, options.Fingerprints)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return prepared, nil
	}
	prepared.Run = run
	if deploySession {
		prepared.Projects = run.WidenSelection(ws, selected)
		return prepared, nil
	}
	// nil and empty are different answers to ScopePlanning: nil is a
	// publish-only session with no verification half at all, and empty is a
	// gate that legitimately selected nothing.
	verified, reported := []*workspace.Project(nil), selection
	if verification != nil {
		verified, reported = verification.Projects, verification.Selection
		if verified == nil {
			verified = []*workspace.Project{}
		}
		prepared.Verification = verified
	}
	prepared.Projects, prepared.Extensions, err = run.ScopePlanning(selected, verified, planning, options.Profiles, options.Commands...)
	if err != nil {
		return nil, err
	}
	prepared.Selection = run.RunSelection(reported, prepared.Projects)
	prepared.ConfirmsHead = run.NoImpact()
	return prepared, nil
}

// resolveSelectionFingerprints fills the two inputs the coordinator cannot
// derive itself: the workspace's ecosystem registry, and one selection
// fingerprint per member. A session that coordinates no release set resolves
// neither, so an ordinary build pays nothing for either.
func resolveSelectionFingerprints(
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	discovered *extension.DiscoveryResult,
	keying []*ScheduledJob,
	commandParams map[string]any,
	cache *store.CacheManager,
) (ReleaseSetOptions, error) {
	// A session that names no channel and carries no line tag coordinates
	// nothing, now or after the planner expands a dependent publish, so it
	// resolves neither the registry nor a single fingerprint: an ordinary build
	// pays nothing for either.
	if len(options.Channels) == 0 && !options.Tagged {
		return options, nil
	}
	profiles, err := extension.ResolveInstalledProfiles(discovered)
	if err != nil {
		return options, err
	}
	options.Profiles = profiles
	members, err := ReleaseSetMembers(ws.Projects, profiles)
	if err != nil {
		return options, err
	}
	if len(members) == 0 {
		return options, nil
	}
	fingerprints, err := SelectionFingerprints(ws, keying, commandParams, cache, profiles, members)
	if err != nil {
		return options, err
	}
	options.Fingerprints = fingerprints
	return options, nil
}

// NewRunCacheManager builds the one cache manager a run uses, honoring a
// cache-verification store override. It is built even under --no-cache: the
// memoized file hashing it carries is what lets the execution keys and a
// release-set publish's selection keys share one pass over the tree, and the
// caller drops it before anything consumes it as a cache of results.
func NewRunCacheManager(workspaceRoot, storeRootOverride string) *store.CacheManager {
	storeRoot := store.ResolveStoreRoot(workspaceRoot)
	if strings.TrimSpace(storeRootOverride) != "" {
		storeRoot = storeRootOverride
	}
	return store.NewCacheManager(store.NewLocalStore(storeRoot))
}

// RunSelection returns the selection block the session reports. When the plan
// measured impact against the head, the block says so: mode impacted, the
// head id as baseline (empty for an empty channel), and the source tier that
// names WHICH head it was — the advanced channel's own, or the channel
// `--baseline-channel` named. Otherwise the previous block's mode and baseline
// are kept for the narrowed project list.
//
// A mixed gate+publish session keeps the GATE's mode, baseline and tier: they
// describe how the verification set was chosen, and the head decided only the
// members. Those members are reported separately, so a consumer reading
// the block can tell what the run verified from what the publish owns; the
// head rewrite stays for a publish-only session, where the two are one list.
func (run *ReleaseSetRun) RunSelection(previous *protocoljob.Selection, projects []*workspace.Project) *protocoljob.Selection {
	if previous == nil {
		return nil
	}
	mode, baseline, baselineSource := previous.Mode, previous.Baseline, previous.BaselineSource
	if run != nil && run.headMeasured && run.verification == nil {
		mode, baseline, baselineSource = protocoljob.SelectionModeImpacted, "", run.baselineSource()
		if head := run.HeadRef(); head != nil {
			baseline = head.ID
		}
	}
	selection := ResolvedRunSelection(mode, baseline, baselineSource, projects)
	selection.ReleaseSetProjects = run.memberProjectIDs()
	return selection
}

// memberProjectIDs is the sorted, duplicate-free id list of the projects whose
// publish and package steps this plan owns: the owners of its SELECTED members.
// It is nil when the run coordinates nothing or the plan selected no member,
// which is the honest answer in both cases — the session's release-set plan
// owns no publication.
func (run *ReleaseSetRun) memberProjectIDs() []string {
	if run == nil || run.plan == nil {
		return nil
	}
	ids := make(map[string]struct{})
	for _, member := range run.plan.SelectedMembers() {
		if member.ProjectID != "" {
			ids[member.ProjectID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(ids))
}

// baselineSource is the tier that produced the reported baseline. The set id in
// `selection.baseline` is the same kind of value either way; what changes is
// what it says about the run, so a reader can tell "this channel's previous
// set" from "another channel's set, inherited on a first publish".
func (run *ReleaseSetRun) baselineSource() string {
	if run == nil || run.plan == nil {
		return ReleaseSetHeadBaselineSource
	}
	if name := run.plan.BaselineChannelName(); name != "" && name == run.plan.BaselineChannel {
		return ReleaseSetBaselineChannelSource
	}
	return ReleaseSetHeadBaselineSource
}

// WidenSelection appends every member project the plan selects that the
// session's selection does not already name, so a combined publish+deploy
// session plans the publish jobs the release depends on while keeping its
// workload deploy nodes.
func (run *ReleaseSetRun) WidenSelection(ws *workspace.Workspace, selected []*workspace.Project) []*workspace.Project {
	seen := make(map[string]struct{}, len(selected))
	for _, project := range selected {
		if project != nil {
			seen[project.ID] = struct{}{}
		}
	}
	widened := append([]*workspace.Project(nil), selected...)
	for _, project := range run.SelectedProjects(ws) {
		if _, ok := seen[project.ID]; ok {
			continue
		}
		seen[project.ID] = struct{}{}
		widened = append(widened, project)
	}
	return widened
}

// validatePlannedPublishCoverage fails closed when the head requires
// republishing a member the dependent publish did not plan: the deploy's own
// selection cannot widen after planning, and a partial publication would
// leave the channel without a closed snapshot.
func validatePlannedPublishCoverage(plan *releaseset.Plan, planned []*workspace.Project, channels []string) error {
	plannedIDs := make(map[string]struct{}, len(planned))
	for _, project := range planned {
		if project != nil {
			plannedIDs[project.ID] = struct{}{}
		}
	}
	var missing []string
	for _, member := range plan.SelectedMembers() {
		if _, ok := plannedIDs[member.ProjectID]; !ok {
			missing = append(missing, printableReleaseKey(releaseset.MemberKey(member.Ecosystem, member.Coordinate)))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	list := strings.Join(channels, ",")
	return fmt.Errorf("the %q head requires republishing %v, which this session did not plan; run `putnami publish --impacted --channel %s` first or widen the deploy selection",
		list, missing, list)
}

// NoImpact reports the plan that selects no member: every member's fingerprint
// matches the head's record, so the release confirms the head unchanged and
// still hands an exact {id,digest} to a dependent deploy.
func (run *ReleaseSetRun) NoImpact() bool {
	return run != nil && run.plan != nil && len(run.plan.SelectedMembers()) == 0
}

// MemberProjectIDs returns the ID of every project that owns a member of this
// plan, selected or not. The coordinator accounts for each of their artifacts:
// AttachPlan requires exactly one package step and one publish step for every
// selected member, and an unselected member stays on the head unchanged. A
// session-level check that asks "did every declared artifact get a publisher?"
// must therefore leave these projects to the coordinator. Nil for a nil run.
func (run *ReleaseSetRun) MemberProjectIDs() map[string]struct{} {
	if run == nil || run.plan == nil {
		return nil
	}
	ids := make(map[string]struct{}, len(run.plan.Members))
	for _, member := range run.plan.Members {
		if member.ProjectID != "" {
			ids[member.ProjectID] = struct{}{}
		}
	}
	return ids
}

// SelectedProjects returns the workspace projects the plan republishes, in
// canonical id order, so a combined publish+deploy session can widen its
// project selection to every member the head requires.
func (run *ReleaseSetRun) SelectedProjects(ws *workspace.Workspace) []*workspace.Project {
	if run == nil || run.plan == nil || ws == nil {
		return nil
	}
	byID := make(map[string]*workspace.Project, len(ws.Projects))
	for _, project := range ws.Projects {
		if project != nil {
			byID[project.ID] = project
		}
	}
	selected := make([]*workspace.Project, 0)
	for _, member := range run.plan.SelectedMembers() {
		if project := byID[member.ProjectID]; project != nil {
			selected = append(selected, project)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	return selected
}

// HeadRef returns the baseline head the plan measured impact against, or nil
// for an empty baseline channel.
func (run *ReleaseSetRun) HeadRef() *distribution.ReleaseSetRef {
	if run == nil || run.plan == nil {
		return nil
	}
	baseline := run.plan.Baseline()
	if baseline == nil {
		return nil
	}
	ref := baseline.Ref
	return &ref
}

// ScopePlanning narrows a provider-backed release-set publish to the exact
// selected members and to the extension that declared each selected member.
// The provider was already resolved before this boundary; it must
// not contribute ordinary publish jobs (config, migrations, OCI, or any future
// side effect) to a library release-set transaction. A plan that selects no
// member plans nothing: the finalizer confirms the head.
//
// verification is the selection this session's OTHER commands were resolved
// for, and nil when publish is the only command — which keeps the result of a
// publish-only session byte for byte. With one, the narrowing applies to the
// publish alone: the projects returned are the selected members' owners
// UNION the verification set, so a library that owns no member still plans its
// own lint, test, build and validate. AttachPlan is what keeps the publication
// narrow, by dropping the publish and publish-only package nodes of every
// project the coordinator does not own.
func (run *ReleaseSetRun) ScopePlanning(
	selected, verification []*workspace.Project,
	extensions []*extension.ExtensionDescription,
	profiles *extproto.ProfileRegistry,
	commands ...string,
) ([]*workspace.Project, []*extension.ExtensionDescription, error) {
	if run == nil {
		return selected, extensions, nil
	}
	if run.plan == nil {
		return nil, nil, fmt.Errorf("scope release-set planning: plan is required")
	}
	if verification != nil {
		run.verification = &releaseVerificationScope{commands: append([]string(nil), commands...)}
	}
	if run.NoImpact() {
		verifiers := releaseVerificationExtensions(extensions, nil, commands)
		if len(verifiers) == 0 {
			return nil, nil, nil
		}
		// No member changed, so the release confirms the head unchanged and the
		// session is verification only. It runs over the selection that asked
		// for the verification: the gate's when one exists, and otherwise the
		// original selection ADR 0017 names.
		//
		// A gate that selected nothing plans nothing. The original
		// selection is the whole workspace the coordinator keyed its members
		// from, and falling back to it verified every project on a commit that
		// changed none. The engine lets this empty plan through
		// (ReleaseSetPreparation.ConfirmsHead), so the finalizer still confirms
		// the head.
		if verification != nil {
			return mergeProjectsByID(nil, verification), verifiers, nil
		}
		return selected, verifiers, nil
	}
	if profiles == nil {
		return nil, nil, fmt.Errorf("scope release-set planning: ecosystem profiles are required")
	}
	selectedIDs := make(map[string]struct{})
	selectedMembers := run.plan.SelectedMembers()
	for _, member := range run.plan.SelectedMembers() {
		selectedIDs[member.ProjectID] = struct{}{}
	}
	scopedProjects := make([]*workspace.Project, 0, len(selectedIDs))
	for _, project := range selected {
		if project == nil {
			continue
		}
		if _, ok := selectedIDs[project.ID]; ok {
			scopedProjects = append(scopedProjects, project)
			delete(selectedIDs, project.ID)
		}
	}
	if len(selectedIDs) != 0 {
		missing := slices.Sorted(maps.Keys(selectedIDs))
		return nil, nil, fmt.Errorf("scope release-set planning: selected projects are missing %v", missing)
	}

	byName := make(map[string]*extension.ExtensionDescription, len(extensions))
	for _, candidate := range extensions {
		if candidate != nil {
			byName[candidate.Name] = candidate
		}
	}
	selectedExtensions := make(map[string]struct{}, len(selectedMembers)*2)
	for _, member := range selectedMembers {
		profile, owner, known := profiles.Profile(string(member.Ecosystem))
		if !known {
			return nil, nil, fmt.Errorf("scope release-set planning: ecosystem %q is declared by no installed extension", member.Ecosystem)
		}
		key := releaseset.MemberKey(member.Ecosystem, member.Coordinate)
		route, routed := run.routes[key]
		if !routed || route.publisher == "" {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s has no execution route", printableReleaseKey(key))
		}
		publicationPublisher := route.publisher
		description := byName[publicationPublisher]
		if description == nil {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s is published by %q, which this run did not plan", printableReleaseKey(key), publicationPublisher)
		}
		if publicationPublisher != owner && !slices.Contains(description.Uses, string(member.Ecosystem)) {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s publisher %q neither owns nor declares use of ecosystem %q", printableReleaseKey(key), publicationPublisher, member.Ecosystem)
		}
		if route.publishCommand != profile.Publish {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s route names publish command %q, want ecosystem command %q", printableReleaseKey(key), route.publishCommand, profile.Publish)
		}
		if description.Jobs[route.publishCommand] == nil {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s publisher %q does not declare %s", printableReleaseKey(key), publicationPublisher, route.publishCommand)
		}
		packagePublisher := route.packagePublisher
		packageDescription := byName[packagePublisher]
		if packageDescription == nil {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s is packaged by %q, which this run did not plan", printableReleaseKey(key), packagePublisher)
		}
		if packageDescription.Jobs["package"] == nil {
			return nil, nil, fmt.Errorf("scope release-set planning: member %s package publisher %q does not declare package", printableReleaseKey(key), packagePublisher)
		}
		selectedExtensions[publicationPublisher] = struct{}{}
		selectedExtensions[packagePublisher] = struct{}{}
	}
	scopedExtensions := releaseVerificationExtensions(extensions, selectedExtensions, commands)
	return mergeProjectsByID(scopedProjects, verification), scopedExtensions, nil
}

// mergeProjectsByID returns the union of two project lists in canonical id
// order, free of duplicates. It is how a mixed session's planning scope is
// built: the members the head requires, plus the projects the session's other
// commands were selected for. Both lists come from one resolved workspace, so
// the id is the identity.
func mergeProjectsByID(members, verification []*workspace.Project) []*workspace.Project {
	if len(verification) == 0 {
		return members
	}
	byID := make(map[string]*workspace.Project, len(members)+len(verification))
	for _, project := range members {
		if project != nil {
			byID[project.ID] = project
		}
	}
	for _, project := range verification {
		if project != nil {
			byID[project.ID] = project
		}
	}
	merged := make([]*workspace.Project, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		merged = append(merged, byID[id])
	}
	return merged
}

// A mixed verification/publication DAG also needs command providers that emit
// no release member (for example SDD's validate). Retaining one must not bring
// its unrelated publication jobs into the release transaction.
func releaseVerificationExtensions(
	extensions []*extension.ExtensionDescription,
	selectedExtensions map[string]struct{},
	commands []string,
) []*extension.ExtensionDescription {
	scopedExtensions := make([]*extension.ExtensionDescription, 0, len(extensions))
	for _, candidate := range extensions {
		if candidate == nil {
			continue
		}
		if _, ok := selectedExtensions[candidate.Name]; ok {
			scopedExtensions = append(scopedExtensions, candidate)
			continue
		}
		for _, command := range commands {
			if command == "publish" || candidate.Jobs[command] == nil {
				continue
			}
			verification := *candidate
			verification.Jobs = maps.Clone(candidate.Jobs)
			delete(verification.Jobs, "publish")
			scopedExtensions = append(scopedExtensions, &verification)
			break
		}
	}
	return scopedExtensions
}

// releaseCandidate is one release-set member of the workspace: the project that
// produces it, its identity in one ecosystem, and the same-ecosystem
// coordinates its published artifact depends on. A project with two ecosystem
// declarations yields two candidates; the project is provenance, the
// (ecosystem, coordinate) pair is identity (D13).
type releaseCandidate struct {
	key              string
	project          *workspace.Project
	publisher        string
	packagePublisher string
	packageStep      string
	publishCommand   string
	publishStep      string
	ecosystem        distribution.Ecosystem
	coordinate       string
	dependencies     []string
}

// releaseProjects turns the workspace into the exact set of release-set
// members, one per (ecosystem, coordinate) declared by a project's release-set
// metadata. A project that declares none contributes no member: publishability
// is what the extension probe recorded, not a shape the CLI infers.
func releaseProjects(projects []*workspace.Project, profiles *extproto.ProfileRegistry) (map[string]*releaseCandidate, error) {
	result := make(map[string]*releaseCandidate)
	for _, project := range projects {
		if project == nil {
			continue
		}
		metadata, found, err := releaseset.ProjectReleaseMetadata(project.Metadata)
		if err != nil {
			return nil, fmt.Errorf("release-set member %q metadata: %w", project.ID, err)
		}
		if !found {
			continue
		}
		for _, declaration := range metadata.Ecosystems {
			publisher, declared := metadata.PublisherFor(declaration.Ecosystem, declaration.Coordinate)
			if !declared {
				return nil, fmt.Errorf("project %q release-set member %s has no declaring extension", project.ID, printableReleaseKey(releaseset.MemberKey(declaration.Ecosystem, declaration.Coordinate)))
			}
			packagePublisher, packaged := metadata.PackagePublisherFor(declaration.Ecosystem, declaration.Coordinate)
			if !packaged {
				return nil, fmt.Errorf("project %q release-set member %s has no package publisher", project.ID, printableReleaseKey(releaseset.MemberKey(declaration.Ecosystem, declaration.Coordinate)))
			}
			profile, _, known := profiles.Profile(string(declaration.Ecosystem))
			if !known {
				return nil, fmt.Errorf("project %q declares release-set ecosystem %q, which no installed extension declares", project.ID, declaration.Ecosystem)
			}
			publishStep, routed := metadata.PublishStepFor(declaration.Ecosystem, declaration.Coordinate)
			if !routed {
				return nil, fmt.Errorf("project %q release-set member %s has no publish step", project.ID, printableReleaseKey(releaseset.MemberKey(declaration.Ecosystem, declaration.Coordinate)))
			}
			if err := profiles.ValidateCoordinate(string(declaration.Ecosystem), declaration.Coordinate); err != nil {
				return nil, fmt.Errorf("project %q declares an invalid %s coordinate: %w", project.ID, declaration.Ecosystem, err)
			}
			key := releaseset.MemberKey(declaration.Ecosystem, declaration.Coordinate)
			if previous := result[key]; previous != nil {
				return nil, fmt.Errorf("release-set identity %s is shared by projects %q and %q", printableReleaseKey(key), previous.project.ID, project.ID)
			}
			result[key] = &releaseCandidate{
				key:              key,
				project:          project,
				publisher:        publisher,
				packagePublisher: packagePublisher,
				packageStep:      declaration.PackageStep,
				publishCommand:   profile.Publish,
				publishStep:      publishStep,
				ecosystem:        declaration.Ecosystem,
				coordinate:       declaration.Coordinate,
				dependencies:     append([]string(nil), declaration.Dependencies...),
			}
		}
	}
	return result, nil
}

// ReleaseSetMembers projects the workspace's release-set members onto the
// project that produces each one. The engine needs exactly this to compute one
// selection fingerprint per member, before it knows whether a plan will be
// built at all.
func ReleaseSetMembers(projects []*workspace.Project, profiles *extproto.ProfileRegistry) (map[string]*workspace.Project, error) {
	candidates, err := releaseProjects(projects, profiles)
	if err != nil {
		return nil, err
	}
	members := make(map[string]*workspace.Project, len(candidates))
	for key, candidate := range candidates {
		members[key] = candidate.project
	}
	return members, nil
}

// releaseSetPlanChannels is the exact channel list one publication advances:
// the channels the caller named, in order, plus the tag's immutable channel
// when HEAD carries its line's tag.
//
// The tag's channel is appended LAST on purpose. The first channel is the
// plan's baseline, the head unchanged members inherit from, and an immutable
// channel never has one; putting it first would turn every tagged publish that
// also names a channel into a full republication of the workspace.
func releaseSetPlanChannels(options ReleaseSetOptions) ([]string, error) {
	channels := append([]string(nil), options.Channels...)
	tag, err := taggedReleaseChannel(options)
	if err != nil {
		return nil, err
	}
	if tag != "" && !slices.Contains(channels, tag) {
		channels = append(channels, tag)
	}
	if len(channels) == 0 {
		return nil, fmt.Errorf("build release-set plan: at least one channel is required")
	}
	if len(channels) > distribution.MaxChannelsPerRelease {
		return nil, cmderr.Usagef("this publication advances %d channels, exceeding the limit of %d", len(channels), distribution.MaxChannelsPerRelease)
	}
	return channels, nil
}

// buildReleaseSetPlan is the one plan builder. It compares every workspace
// member with the baseline head's record and produces the next full snapshot:
// selected members carry the candidate version and the current provenance,
// unchanged members inherit their baseline artifact record exactly. The
// attribution of every member — its project and kind — is the workspace's
// declaration when the repository opts in, republished or not.
//
// The baseline is the head of the FIRST listed channel, or — when that channel
// is still empty and the publication named one — the head of the baseline
// channel, which is read and never advanced (D14, ADR 0006). Every other
// resolved head is carried in the plan for its own compare-and-swap expectation
// and never contributes anything to inherit.
func buildReleaseSetPlan(
	options ReleaseSetOptions,
	ws *workspace.Workspace,
	candidates map[string]*releaseCandidate,
	heads map[string]*distribution.ChannelHead,
	provenance *releaseSetProvenance,
) (*releaseset.Plan, error) {
	if ws == nil || provenance == nil {
		return nil, fmt.Errorf("build release-set plan: workspace and provenance are required")
	}
	channels, err := releaseSetPlanChannels(options)
	if err != nil {
		return nil, err
	}
	head, baselineChannel := releaseSetBaselineHead(options, channels, heads)
	baseMembers := make(map[string]distribution.ReleaseSetMember)
	if head != nil {
		if head.ReleaseSet == nil {
			return nil, fmt.Errorf("build release-set plan: resolved baseline head carries no snapshot")
		}
		if head.ReleaseSet.Namespace != releaseSetNamespace(ws, options.Policy) {
			return nil, fmt.Errorf("build release-set plan: resolved namespace %q does not match publication namespace %q", head.ReleaseSet.Namespace, releaseSetNamespace(ws, options.Policy))
		}
		for _, member := range head.ReleaseSet.Members {
			baseMembers[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
		}
	}

	tagged := TaggedReleaseSetPublish(options)
	keys := slices.Sorted(maps.Keys(candidates))
	selected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		baseMember, exists := baseMembers[key]
		fingerprint := options.Fingerprints[key]
		if fingerprint == "" {
			return nil, fmt.Errorf("release-set member %s has no selection fingerprint; its package task was not planned", printableReleaseKey(key))
		}
		// A tagged publish is its LINE's cohort: every member of the tagged
		// line is republished at the tag's version whatever its fingerprint
		// says, and every other member inherits the baseline record it already
		// has. A member with nothing to inherit — no baseline head, or a member
		// the head does not carry — is republished either way, because a
		// release set is a closed full snapshot and cannot omit it.
		if tagged {
			if candidates[key].project.Line == options.Line || head == nil || !exists {
				selected[key] = struct{}{}
			}
			continue
		}
		if options.All || head == nil || !exists || baseMember.SelectionFingerprint != fingerprint {
			selected[key] = struct{}{}
		}
	}
	// A member whose internal dependency is republished must be republished
	// too: its dependency record changes, and the snapshot stays closed only
	// when every record names the version that is actually in the set.
	for changed := true; changed; {
		changed = false
		for _, key := range keys {
			if _, done := selected[key]; done {
				continue
			}
			for _, coordinate := range candidates[key].dependencies {
				if _, ok := selected[releaseset.MemberKey(candidates[key].ecosystem, coordinate)]; ok {
					selected[key] = struct{}{}
					changed = true
					break
				}
			}
		}
	}

	versions := make(map[string]string, len(keys))
	for _, key := range keys {
		if _, ok := selected[key]; ok {
			version, err := releaseCandidateVersion(options, ws, candidates[key])
			if err != nil {
				return nil, err
			}
			versions[key] = version
			continue
		}
		versions[key] = baseMembers[key].Version
	}

	plan := &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       releaseSetNamespace(ws, options.Policy),
		Channels:        channels,
		Heads:           make(map[string]*distribution.ChannelHead, len(channels)+1),
		Members:         make([]releaseset.PlannedMember, 0, len(keys)),
	}
	attributed := releaseMemberAttribution(options.Policy)
	for _, name := range channels {
		plan.Heads[name] = heads[name]
	}
	// The baseline channel rides in Heads with the advanced ones — the same
	// resolve answered all of them — and in BaselineChannel, which is what
	// tells a reader that this head is inherited from and never released. It
	// is named only when it is the head this plan actually measured against;
	// a baseline the publication never had to fall back on leaves no trace.
	if baselineChannel != "" && !slices.Contains(channels, baselineChannel) {
		plan.BaselineChannel = baselineChannel
		plan.Heads[baselineChannel] = heads[baselineChannel]
	}
	for _, key := range keys {
		candidate := candidates[key]
		if _, ok := selected[key]; ok {
			dependencies, err := releaseDependencies(candidate, candidates, versions)
			if err != nil {
				return nil, err
			}
			member := releaseset.PlannedMember{
				Ecosystem: candidate.ecosystem, Coordinate: candidate.coordinate, Version: versions[key],
				Dependencies:         dependencies,
				SourceRevision:       provenance.revision,
				SelectionFingerprint: options.Fingerprints[key],
				Selected:             true, ProjectID: candidate.project.ID,
			}
			if attributed {
				member.Project = releaseMemberProject(candidate.project)
				member.Kind = releaseset.KindFor(candidate.ecosystem, candidate.packageStep, candidate.publishStep)
			}
			// The provenance read a tree only when the repository opted in
			// (releaseMemberSourceTree), so an empty tree records nothing.
			member.SourceTree = provenance.tree
			plan.Members = append(plan.Members, member)
			continue
		}
		// An unchanged member inherits its artifact record from the baseline
		// verbatim: version, digest, dependencies, provenance (source tree
		// included, opted in or not) and platforms belong to the publication
		// that produced them. Its attribution does not: which project
		// declares a member and what kind of artifact it is are what the
		// workspace states, whether or not this run republishes the member,
		// so an opted-in plan stamps every member it declares. A
		// head published before the opt-in would otherwise keep its members
		// unattributed until each one happened to change, and a consumer that
		// binds a workload's image, config and migrations by project could
		// bind nothing for the workloads that stood still. Off, inheritance
		// stays verbatim, attribution included.
		baseMember := baseMembers[key]
		member := releaseset.PlannedMember{
			Ecosystem: baseMember.Ecosystem, Coordinate: baseMember.Coordinate, Version: baseMember.Version,
			ArtifactDigest:       baseMember.ArtifactDigest,
			Dependencies:         append([]distribution.ReleaseSetDependency(nil), baseMember.Dependencies...),
			SourceRevision:       baseMember.SourceRevision,
			SelectionFingerprint: baseMember.SelectionFingerprint,
			Platforms:            maps.Clone(baseMember.Platforms),
			Project:              baseMember.Project,
			Kind:                 baseMember.Kind,
			SourceTree:           baseMember.SourceTree,
			Selected:             false, ProjectID: candidate.project.ID,
		}
		if attributed {
			member.Project = releaseMemberProject(candidate.project)
			member.Kind = releaseset.KindFor(candidate.ecosystem, candidate.packageStep, candidate.publishStep)
		}
		plan.Members = append(plan.Members, member)
	}
	plan = releaseset.NormalizePlan(plan)
	if err := releaseset.ValidatePlan(plan); err != nil {
		return nil, fmt.Errorf("build release-set plan: %w", err)
	}
	return plan, nil
}

// releaseMemberAttribution reports whether this publication records each
// member's source project and artifact kind (protocols/distribution ADR 0005),
// on the members it republishes and on the ones it carries over alike. The
// repository opts in through `distribution.memberAttribution` in putnami.ci.json, and the default is OFF, because every consumer of the
// plan and of the released set decodes it strictly: the extensions that
// publish the members read the plan from their job context, and the
// release-set provider and its backend read the released set, and each one
// refuses a field its build does not know. A publisher that emitted the two
// fields to an older consumer would fail every publication with
// `decode releaseSetPlan: json: unknown field "kind"`, which is what happened
// when this CLI first emitted them to an extension pinned before the field.
//
// Off, a selected member carries no attribution, and an unchanged member
// inherits its head record verbatim, attribution included: without the opt-in
// nothing rewrites a member the run did not republish. On, attribution is the
// plan's statement about every member it declares: a member published before
// the opt-in is attributed by the first opted-in plan that carries it, which
// moves the set's reference once even when nothing was republished.
func releaseMemberAttribution(policy *ciproto.Distribution) bool {
	return policy != nil && policy.MemberAttribution
}

// releaseMemberSourceTree reports whether a member this publication republishes
// records the git tree it was built from (protocols/distribution ADR 0005). The
// repository opts in through `distribution.memberSourceTree` in
// putnami.ci.json, and the default is OFF for the reason attribution is: the
// release-set provider and its backend refuse a member field they do not know.
//
// Unlike attribution, the tree is the publication's, not the plan's: only a
// republished member records the current one, and an unchanged member inherits
// its head's tree verbatim whether or not the repository opts in, because that
// member was built from the head's tree and not from this checkout.
func releaseMemberSourceTree(policy *ciproto.Distribution) bool {
	return policy != nil && policy.MemberSourceTree
}

// releaseMemberProject is the attribution a published member records: the
// project's canonical logical id without its leading slash, the same identity
// the private member-evidence handoff already carries.
//
// It is Project.ID and NOT Project.Path on purpose. A transparent group folder
// lives in the path and is absent from the id, and identity is what a consumer
// matches against: a ci document selects `apps/service`, never
// `apps/(internal)/service`, and the protocol grammar cannot even express the
// second. Recording the physical path would attribute the member to a name no
// selector can name — and, because the grammar rejects a parenthesis, would
// record nothing at all for exactly the grouped projects it meant to serve.
// Workspace loading already refuses two paths that collapse to one id, so the
// id it yields is unique.
//
// A project whose id falls outside the protocol grammar records NO project
// rather than failing the publication: attribution is optional metadata, and a
// release must not be refused because of the shape of a directory name.
func releaseMemberProject(project *workspace.Project) string {
	if project == nil {
		return ""
	}
	identity := strings.TrimPrefix(project.ID, "/")
	if !distribution.IsMemberProject(identity) {
		return ""
	}
	return identity
}

// releaseCandidateVersion resolves the exact immutable version the package and
// publish jobs will stamp for one member.
//
// The version's SHAPE belongs to the ecosystem profile, not to this function: a
// profile that refuses the bare version but accepts the "v"-prefixed spelling
// gets the prefixed one, which is how a Go module member takes its `vX.Y.Z`
// without the CLI naming the go ecosystem.
func releaseCandidateVersion(options ReleaseSetOptions, ws *workspace.Workspace, candidate *releaseCandidate) (string, error) {
	version := ""
	if info := VersionInfoForProject(options.Versions, candidate.project); info != nil {
		version = info.Full
	}
	if strings.TrimSpace(version) == "" {
		// Match the package jobs' established fallback when the member's line
		// has no computed version. The release-set member must name the exact
		// version those jobs will stamp and upload.
		version = "0.0.0"
	}
	if strings.TrimSpace(version) == "" {
		return "", fmt.Errorf("release-set member %s has no immutable candidate version", printableReleaseKey(candidate.key))
	}
	ecosystem := string(candidate.ecosystem)
	if options.Profiles != nil {
		if err := options.Profiles.ValidateVersion(ecosystem, version); err != nil {
			prefixed := "v" + version
			if options.Profiles.ValidateVersion(ecosystem, prefixed) != nil {
				return "", fmt.Errorf("release-set member %s has invalid candidate version %q: %w", printableReleaseKey(candidate.key), version, err)
			}
			version = prefixed
		}
	}
	probe := &distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       releaseSetNamespace(ws, options.Policy),
		Members: []distribution.ReleaseSetMember{{
			Ecosystem: candidate.ecosystem, Coordinate: candidate.coordinate, Version: version,
			ArtifactDigest: "sha256:" + strings.Repeat("0", 64), Dependencies: []distribution.ReleaseSetDependency{},
			SourceRevision: strings.Repeat("0", 40), SelectionFingerprint: "sha256:" + strings.Repeat("0", 64),
		}},
	}
	if diagnostics := distribution.ValidateReleaseSet(probe); diag.HasErrors(diagnostics) {
		return "", fmt.Errorf("release-set member %s has invalid candidate version %q: %s", printableReleaseKey(candidate.key), version, firstReleaseSetDiagnostic(diagnostics))
	}
	return version, nil
}

func releaseDependencies(
	candidate *releaseCandidate,
	all map[string]*releaseCandidate,
	versions map[string]string,
) ([]distribution.ReleaseSetDependency, error) {
	dependencies := make([]distribution.ReleaseSetDependency, 0, len(candidate.dependencies))
	for _, coordinate := range candidate.dependencies {
		key := releaseset.MemberKey(candidate.ecosystem, coordinate)
		target := all[key]
		if target == nil {
			return nil, fmt.Errorf("release-set member %s declares internal %s dependency %q that is not a publishable member", printableReleaseKey(candidate.key), candidate.ecosystem, coordinate)
		}
		version := versions[key]
		if strings.TrimSpace(version) == "" {
			return nil, fmt.Errorf("release-set member %s dependency %q has no exact version", printableReleaseKey(candidate.key), coordinate)
		}
		dependencies = append(dependencies, distribution.ReleaseSetDependency{
			Ecosystem: candidate.ecosystem, Coordinate: target.coordinate, Version: version,
		})
	}
	sort.Slice(dependencies, func(i, j int) bool {
		return releaseset.MemberKey(dependencies[i].Ecosystem, dependencies[i].Coordinate) <
			releaseset.MemberKey(dependencies[j].Ecosystem, dependencies[j].Coordinate)
	})
	return dependencies, nil
}

type releaseTaskRoute struct {
	projectID string
	publisher string
	command   string
	step      string
}

func packageTaskRoute(route releaseMemberRoute) releaseTaskRoute {
	return releaseTaskRoute{
		projectID: route.projectID, publisher: route.packagePublisher,
		command: "package", step: route.packageStep,
	}
}

func publishTaskRoute(route releaseMemberRoute) releaseTaskRoute {
	return releaseTaskRoute{
		projectID: route.projectID, publisher: route.publisher,
		command: route.publishCommand, step: route.publishStep,
	}
}

func plannedTaskRoute(job *ScheduledJob) (releaseTaskRoute, bool) {
	if job == nil || job.Project == nil || job.Extension == nil || job.JobDef == nil {
		return releaseTaskRoute{}, false
	}
	return releaseTaskRoute{
		projectID: job.Project.ID, publisher: job.Extension.Name,
		command: job.CommandName(), step: job.StepID(),
	}, true
}

// AttachPlan binds the immutable release-set plan to its execution jobs and
// removes artifact steps that the plan did not select. Project-level scoping is
// insufficient here: one project may publish several independent members, such
// as an npm package and an OCI image. In particular, an unselected publish step
// must be removed before scheduling because reconciliation happens after remote
// side effects and therefore cannot make an accidental registry write safe.
//
// An unselected package step is local preparation, never a registry write, so
// it is removed only when nothing the plan keeps still needs it. Beside a
// selected member of the same project it is a removal candidate: it goes when
// it only fed its own dropped publication, and it stays when a kept node
// reaches it — a project's shared build step routinely depends on the package
// step that declares another member of the same project, and removing that
// node would leave the kept build without its input. Package jobs of
// wholly unchanged dependency projects are reached transitively the same way.
func (run *ReleaseSetRun) AttachPlan(planned []*ScheduledJob) ([]*ScheduledJob, error) {
	if run == nil {
		return planned, nil
	}
	if run.plan == nil {
		return nil, fmt.Errorf("attach release-set plan: plan is required")
	}

	selectedMembers := make(map[string]struct{})
	selectedProjects := make(map[string]struct{})
	memberSelection := make(map[string]bool, len(run.plan.Members))
	memberProjects := make(map[string]string, len(run.plan.Members))
	for _, member := range run.plan.Members {
		key := releaseset.MemberKey(member.Ecosystem, member.Coordinate)
		memberSelection[key] = member.Selected
		memberProjects[key] = member.ProjectID
		if member.Selected {
			selectedMembers[key] = struct{}{}
			selectedProjects[member.ProjectID] = struct{}{}
		}
	}

	allPackageRoutes := make(map[releaseTaskRoute]struct{}, len(run.routes))
	allPublishRoutes := make(map[releaseTaskRoute]struct{}, len(run.routes))
	selectedPackageRoutes := make(map[releaseTaskRoute]struct{}, len(selectedMembers))
	selectedPublishRoutes := make(map[releaseTaskRoute]struct{}, len(selectedMembers))
	publishRouteMembers := make(map[releaseTaskRoute]string, len(run.routes))
	publishCommands := make(map[string]struct{})
	for key, route := range run.routes {
		selected, plannedMember := memberSelection[key]
		if !plannedMember {
			continue
		}
		if route.projectID != memberProjects[key] {
			return nil, fmt.Errorf("attach release-set plan: member %s route belongs to project %q, want %q", printableReleaseKey(key), route.projectID, memberProjects[key])
		}
		packageRoute := packageTaskRoute(route)
		publishRoute := publishTaskRoute(route)
		if previous, duplicate := publishRouteMembers[publishRoute]; duplicate && previous != key {
			return nil, fmt.Errorf("attach release-set plan: members %s and %s share publish route %q/%s~%s and cannot be selected independently",
				printableReleaseKey(previous), printableReleaseKey(key), route.publisher, route.publishCommand, route.publishStep)
		}
		publishRouteMembers[publishRoute] = key
		allPackageRoutes[packageRoute] = struct{}{}
		allPublishRoutes[publishRoute] = struct{}{}
		publishCommands[route.publishCommand] = struct{}{}
		if selected {
			selectedPackageRoutes[packageRoute] = struct{}{}
			selectedPublishRoutes[publishRoute] = struct{}{}
		}
	}

	packageJobs := make(map[releaseTaskRoute][]*ScheduledJob, len(selectedPackageRoutes))
	publishJobs := make(map[releaseTaskRoute][]*ScheduledJob, len(selectedPublishRoutes))
	for _, job := range planned {
		route, ok := plannedTaskRoute(job)
		if !ok {
			continue
		}
		if _, selected := selectedPackageRoutes[route]; selected {
			packageJobs[route] = append(packageJobs[route], job)
		}
		if _, selected := selectedPublishRoutes[route]; selected {
			publishJobs[route] = append(publishJobs[route], job)
		}
	}
	for key := range selectedMembers {
		route, ok := run.routes[key]
		if !ok {
			return nil, fmt.Errorf("attach release-set plan: selected member %s has no execution route", printableReleaseKey(key))
		}
		if jobs := packageJobs[packageTaskRoute(route)]; len(jobs) != 1 {
			return nil, fmt.Errorf("attach release-set plan: selected member %s has %d %q package steps %q in project %q, want exactly one",
				printableReleaseKey(key), len(jobs), route.packagePublisher, route.packageStep, route.projectID)
		}
		if jobs := publishJobs[publishTaskRoute(route)]; len(jobs) != 1 {
			return nil, fmt.Errorf("attach release-set plan: selected member %s has %d %q %s steps %q in project %q, want exactly one",
				printableReleaseKey(key), len(jobs), route.publisher, route.publishCommand, route.publishStep, route.projectID)
		}
	}
	run.publishJobKeys = make(map[string]string, len(selectedMembers))
	for key := range selectedMembers {
		run.publishJobKeys[publishJobs[publishTaskRoute(run.routes[key])][0].Key()] = key
	}

	// dropped is removed unconditionally; candidates is local work the plan did
	// not select, removed only when no kept node still reaches it.
	dropped := make(map[string]struct{})
	candidates := make(map[string]struct{})
	for _, job := range planned {
		route, ok := plannedTaskRoute(job)
		if !ok {
			continue
		}
		if _, artifact := allPublishRoutes[route]; artifact {
			if _, selected := selectedPublishRoutes[route]; !selected {
				dropped[job.Key()] = struct{}{}
			}
			continue
		}
		if _, selectedProject := selectedProjects[route.projectID]; selectedProject {
			if _, artifact := allPackageRoutes[route]; artifact {
				if _, selected := selectedPackageRoutes[route]; !selected {
					candidates[job.Key()] = struct{}{}
				}
			}
		}
	}
	// A mixed gate+publish session planned every command over the verification
	// set too, so this plan carries publish and package nodes for projects the
	// coordinator owns no member of. They are removed here, which leaves the
	// publication exactly what a publish-only session would have planned.
	run.collectUncoordinatedPublication(planned, selectedProjects, publishCommands, dropped, candidates)
	dropUnreachedCandidates(planned, candidates, dropped)

	scoped := make([]*ScheduledJob, 0, len(planned)-len(dropped))
	for _, job := range planned {
		if job == nil {
			scoped = append(scoped, job)
			continue
		}
		if _, remove := dropped[job.Key()]; remove {
			continue
		}
		if job.InvocationProducer != nil {
			if _, remove := dropped[job.InvocationProducer.Key()]; remove {
				return nil, fmt.Errorf("attach release-set plan: kept job %q consumes invocation output from removed publish step %q", job.Key(), job.InvocationProducer.Key())
			}
		}
		job.DependsOn = withoutReleaseJobs(job.DependsOn, dropped)
		job.SerializeAfter = withoutReleaseJobs(job.SerializeAfter, dropped)
		scoped = append(scoped, job)
	}
	attachReleaseSetContext(scoped, run.plan, publishCommands)
	return scoped, nil
}

// collectUncoordinatedPublication marks, in a plan scoped with a verification
// set, every node the session neither asked for nor owns.
//
// A verification project is in the plan so the session's OTHER commands run
// over it. Everything else the planner reached for it exists only because a
// publication was planned there, and that publication is not this release
// set's: publish-only sessions never planned it either. Two rules, and the
// difference between them is what a node WRITES.
//
// A publish node — the `publish` command, or the publish command of any member
// this plan carries — is a registry write the transaction never accounted for.
// It is dropped whatever depends on it, because retaining it on an edge a
// repository declared would publish outside the release.
//
// Everything else the session did not name is local work, so it becomes a
// candidate: dropUnreachedCandidates removes it only when nothing the plan
// keeps still needs it. That is what protects the package job of a wholly
// unchanged dependency, routinely reached from a selected member's own package
// chain, and every prerequisite command a named command pulled in. A session
// that NAMES package keeps its package nodes: the caller asked for the
// artifacts.
func (run *ReleaseSetRun) collectUncoordinatedPublication(
	planned []*ScheduledJob,
	memberProjects map[string]struct{},
	publishCommands map[string]struct{},
	dropped map[string]struct{},
	candidates map[string]struct{},
) {
	if run == nil || run.verification == nil {
		return
	}
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil {
			continue
		}
		if _, owns := memberProjects[job.Project.ID]; owns {
			continue
		}
		command := job.CommandName()
		if _, publishes := publishCommands[command]; publishes || command == "publish" {
			dropped[job.Key()] = struct{}{}
			continue
		}
		if !slices.Contains(run.verification.commands, command) {
			candidates[job.Key()] = struct{}{}
		}
	}
}

// dropUnreachedCandidates adds to dropped every candidate no kept node still
// reaches. Reach is transitive over the functional edges — DependsOn and the
// invocation producer — from every node that is neither dropped nor a
// candidate, so a candidate reached only from another candidate, or only from
// a dropped publication, is reached only from work the plan removes.
// SerializeAfter is ordering alone and never keeps a node: the edge goes with
// its target.
//
// It runs once every rule has filled dropped and candidates, so a node it
// retains is retained by a node the session actually keeps.
func dropUnreachedCandidates(planned []*ScheduledJob, candidates, dropped map[string]struct{}) {
	if len(candidates) == 0 {
		return
	}
	byKey := make(map[string]*ScheduledJob, len(planned))
	reachable := make([]*ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if job == nil {
			continue
		}
		key := job.Key()
		byKey[key] = job
		if _, isCandidate := candidates[key]; isCandidate {
			continue
		}
		if _, removed := dropped[key]; removed {
			continue
		}
		reachable = append(reachable, job)
	}
	for len(reachable) > 0 {
		job := reachable[len(reachable)-1]
		reachable = reachable[:len(reachable)-1]
		needed := job.DependsOn
		if job.InvocationProducer != nil {
			needed = append(append([]string(nil), needed...), job.InvocationProducer.Key())
		}
		for _, key := range needed {
			if _, isCandidate := candidates[key]; !isCandidate {
				continue
			}
			delete(candidates, key)
			if retained := byKey[key]; retained != nil {
				reachable = append(reachable, retained)
			}
		}
	}
	for key := range candidates {
		dropped[key] = struct{}{}
	}
}

func withoutReleaseJobs(keys []string, dropped map[string]struct{}) []string {
	if len(keys) == 0 || len(dropped) == 0 {
		return keys
	}
	kept := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, remove := dropped[key]; !remove {
			kept = append(kept, key)
		}
	}
	return kept
}

func attachReleaseSetContext(planned []*ScheduledJob, plan *releaseset.Plan, publishCommands map[string]struct{}) {
	if plan == nil {
		return
	}
	members := make(map[string]struct{}, len(plan.Members))
	for _, member := range plan.Members {
		members[member.ProjectID] = struct{}{}
	}
	for _, plannedJob := range planned {
		if plannedJob == nil || plannedJob.Project == nil || plannedJob.JobDef == nil {
			continue
		}
		if _, ok := members[plannedJob.Project.ID]; !ok {
			continue
		}
		command := plannedJob.CommandName()
		_, isPublish := publishCommands[command]
		if command != "package" && !isPublish {
			continue
		}
		definition := *plannedJob.JobDef
		definition.BoundParams = maps.Clone(plannedJob.JobDef.BoundParams)
		if definition.BoundParams == nil {
			definition.BoundParams = extension.ParamMap{}
		}
		definition.BoundParams[releaseset.ContextParamName] = releaseset.ClonePlan(plan)
		plannedJob.JobDef = &definition
	}
}

// isReleaseSetMemberPublication reports whether a job is a publish of a
// release-set member: attachReleaseSetContext bound the immutable plan to it.
// Such a job is admitted on its own functional ancestors; the AFTER gate
// applies to the release-set stamp instead (ADR 0023).
//
// Presence of the key is not enough: a manifest's session-prerequisite params
// can bind any name into BoundParams, and a literal decodes as a map. Only the
// coordinator's typed plan counts, and only when it names this job's project
// as a member — otherwise a repository could exempt an arbitrary publish from
// the gate by spelling the key.
func isReleaseSetMemberPublication(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil || job.Project == nil || job.CommandName() != "publish" {
		return false
	}
	plan, ok := job.JobDef.BoundParams[releaseset.ContextParamName].(*releaseset.Plan)
	if !ok || plan == nil {
		return false
	}
	for _, member := range plan.Members {
		if member.ProjectID == job.Project.ID {
			return true
		}
	}
	return false
}

// AttachReleaseSetPlan carries one SDK-owned plan through every selected
// package and publish node, including package nodes reached transitively.
func AttachReleaseSetPlan(planned []*ScheduledJob, plan *releaseset.Plan) error {
	if plan == nil {
		return nil
	}
	members := make(map[string]struct{}, len(plan.Members))
	selected := make(map[string]struct{})
	for _, member := range plan.Members {
		members[member.ProjectID] = struct{}{}
	}
	for _, member := range plan.SelectedMembers() {
		selected[member.ProjectID] = struct{}{}
	}
	packageJobs := make(map[string]int, len(selected))
	publishJobs := make(map[string]int, len(selected))
	for _, plannedJob := range planned {
		if plannedJob == nil || plannedJob.Project == nil || plannedJob.JobDef == nil {
			continue
		}
		if _, ok := members[plannedJob.Project.ID]; !ok {
			continue
		}
		command := plannedJob.CommandName()
		if command != "package" && command != "publish" {
			continue
		}
		if _, isSelected := selected[plannedJob.Project.ID]; isSelected {
			if command == "package" {
				packageJobs[plannedJob.Project.ID]++
			} else {
				publishJobs[plannedJob.Project.ID]++
			}
		}
	}
	attachReleaseSetContext(planned, plan, map[string]struct{}{"publish": {}})
	for projectID := range selected {
		if packageJobs[projectID] == 0 || publishJobs[projectID] == 0 {
			return fmt.Errorf("release-set member %q does not have both package and publish tasks in the plan", projectID)
		}
	}
	return nil
}

// Plan returns the immutable plan carried by this coordinator.
func (run *ReleaseSetRun) Plan() *releaseset.Plan {
	if run == nil {
		return nil
	}
	return run.plan
}

// Finalizer reconciles publication proofs and releases the set through the
// provider's one transactional operation. It is nil when the run has no plan,
// and when a deploy barrier releases the set instead (AttachBarrier).
func (run *ReleaseSetRun) Finalizer(ctx context.Context) func(map[string]*JobResult) {
	if run == nil || run.provider == nil || run.plan == nil || run.barrierAttached {
		return nil
	}
	return func(results map[string]*JobResult) {
		if run.dryRun {
			return
		}
		for _, result := range results {
			if result != nil && (result.Status == "failed" || result.Status == "canceled") {
				return
			}
		}
		results[releaseSetResultKey] = run.commit(ctx, results)
	}
}

// commit is the one provider mutation path shared by the terminal publish-only
// finalizer and the same-session DAG barrier. Callers decide WHEN it is safe;
// this method validates the result set and performs the one release call that
// stores the snapshot and advances EVERY listed channel by compare-and-swap
// from the head the plan resolved for it. Acceptance is atomic: a conflict on
// any one channel leaves every other head where it was. A plan that selected
// nothing releases the head set unchanged, which the provider answers with
// already-current.
func (run *ReleaseSetRun) commit(ctx context.Context, results map[string]*JobResult) *JobResult {
	if err := ensureNoReleaseSetOutcome(results); err != nil {
		return releaseSetFailure(err)
	}
	if err := run.ensureNoSkippedPublication(results); err != nil {
		return releaseSetFailure(err)
	}
	if err := run.ensureNoReusedPublication(results); err != nil {
		return releaseSetFailure(err)
	}
	finalSet, err := reconcilePublishedReleaseSet(run.plan, results)
	if err != nil {
		return releaseSetFailure(err)
	}
	if run.sourceTree != "" && currentReleaseSourceTree(run.root) != run.sourceTree {
		dropRepublishedSourceTree(finalSet, run.plan)
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(finalSet)
	if diag.HasErrors(diagnostics) {
		return releaseSetFailure(fmt.Errorf("derive released set ref: %s", firstReleaseSetDiagnostic(diagnostics)))
	}
	// The exact snapshot this session releases, retained for the one consumer
	// that cannot be served before it exists: the deploy contract of a
	// same-session workload, completed by the barrier from these members.
	run.releasedRef, run.releasedMembers = ref, finalSet.Members
	chain := run.visibility
	if chain.Repo == "" {
		// A repository that declares nothing publishes at the narrowest level.
		chain.Repo = distribution.VisibilityInternal
	}
	channels := make([]distribution.ChannelRequest, 0, len(run.plan.Channels))
	for _, name := range run.plan.Channels {
		level, declared := run.channelLevels[name]
		if !declared {
			level = chain.Repo
		}
		request := distribution.ChannelRequest{Name: name, Visibility: level}
		if name == run.immutableChannel {
			// The tag's channel is created once, asserting that it has no head,
			// and refuses every later move. It is never compared against a
			// resolved head, because resolving one would mean it already exists.
			request.Immutable = true
		} else if head := run.plan.Heads[name]; head != nil {
			expected := head.Ref
			request.Expected = &expected
		}
		channels = append(channels, request)
	}
	request := &distribution.ReleaseRequest{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       run.plan.Namespace,
		ReleaseSet:      *finalSet,
		Channels:        channels,
		// The chain the repository declared in putnami.ci.json, with every
		// member selector already resolved to coordinates. The CLI computes no
		// level: the provider resolves the chain per member and ratchets it
		// (D5).
		Visibility: chain,
		Mirrors:    maps.Clone(run.mirrors),
	}
	response, err := run.release(ctx, request, finalSet)
	if err != nil {
		return releaseSetFailure(fmt.Errorf("release release set: %w", err))
	}
	if response == nil || (response.Outcome != distribution.ReleaseOutcomeReleased && response.Outcome != distribution.ReleaseOutcomeAlreadyCurrent) {
		return releaseSetFailure(run.channelConflictError(response))
	}
	if len(response.Current) == 0 {
		return releaseSetFailure(fmt.Errorf("release-set provider reported %s without a current head", response.Outcome))
	}
	return releaseSetSuccess(distribution.ReleaseSetPublishOutcome{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       run.plan.Namespace,
		Ref:             ref,
		Channels:        maps.Clone(response.Current),
	})
}

// channelConflictError explains a failed release in terms of what the plan
// expected and what the provider observed, so an operator can tell a genuine
// two-writer race from a stale plan. Every channel whose observed head differs
// from its expectation is named: a conflict on one channel writes nothing on
// any of them, so reporting only the first would hide which head moved.
func (run *ReleaseSetRun) channelConflictError(response *distribution.ReleaseResponse) error {
	var conflicts []string
	for _, name := range run.plan.Channels {
		expected := "no head"
		if head := run.plan.Heads[name]; head != nil {
			expected = head.Ref.ID
		}
		observed := "no head"
		if response != nil {
			if head, reported := response.Current[name]; reported && head != nil {
				observed = head.Ref.ID
			}
		}
		if expected == observed {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("%s: expected %s, observed %s", name, expected, observed))
	}
	if len(conflicts) == 0 {
		return fmt.Errorf("release-set release failed without naming a moved channel head; re-run the publish to plan against the current heads")
	}
	return fmt.Errorf("release-set channel CAS conflict; no channel was advanced (%s); re-run the publish to plan against the current heads",
		strings.Join(conflicts, "; "))
}

// ensureNoSkippedPublication refuses to release when the scheduler skipped a
// selected member's publish job. Every other terminal status already stops
// the finalizer (failed and canceled) or is verified by reconciliation
// (success must carry the published-member event). A skip is the one status
// that reaches the commit with no failure anywhere in the session, and the
// generic "missing verified records" it would produce hides the actual cause:
// the job never ran because a gate or dependency withheld it. Name the jobs
// and their recorded causes so the session shows why nothing was published.
func (run *ReleaseSetRun) ensureNoSkippedPublication(results map[string]*JobResult) error {
	var skipped []string
	for jobKey, memberKey := range run.publishJobKeys {
		result := results[jobKey]
		if result == nil || result.Status != "skipped" {
			continue
		}
		cause := "no cause recorded"
		if result.Error != nil && result.Error.Message != "" {
			cause = result.Error.Message
		}
		skipped = append(skipped, fmt.Sprintf("%s for %s (%s)", jobKey, printableReleaseKey(memberKey), cause))
	}
	if len(skipped) == 0 {
		return nil
	}
	sort.Strings(skipped)
	return fmt.Errorf("release-set publication refused: %d selected publish job(s) were skipped and published nothing: %s",
		len(skipped), strings.Join(skipped, "; "))
}

// ensureNoReusedPublication refuses to release under publication-v1 when a
// selected publication job's result was reused instead of executed: a local
// or remote cache hit, or a coalesced result. Such a job ran no process with
// this run's outbox, so it packed nothing into it, and any published-member
// event its result replays names an upload this run did not make. Without
// publication-v1 the check is not made.
func (run *ReleaseSetRun) ensureNoReusedPublication(results map[string]*JobResult) error {
	if !run.Publication() {
		return nil
	}
	var reused []string
	for jobKey, memberKey := range run.publishJobKeys {
		result := results[jobKey]
		if result == nil || !result.ReuseKind().Reused() {
			continue
		}
		reused = append(reused, fmt.Sprintf("%s for %s (%s)", jobKey, printableReleaseKey(memberKey), result.ReuseKind()))
	}
	if len(reused) == 0 {
		return nil
	}
	sort.Strings(reused)
	return fmt.Errorf("release-set publication refused: %d selected publication job(s) were reused instead of executed and packed nothing in this run: %s",
		len(reused), strings.Join(reused, "; "))
}

func ensureNoReleaseSetOutcome(results map[string]*JobResult) error {
	if _, occupied := results[releaseSetResultKey]; occupied {
		return fmt.Errorf("duplicate release-set coordinator result")
	}
	for key, result := range results {
		if result == nil || result.Data == nil {
			continue
		}
		if _, duplicate := result.Data[runtimeproto.ReleaseSetResultDataKey]; duplicate {
			return fmt.Errorf("job %q emitted an unauthorized duplicate data.releaseSet outcome", key)
		}
	}
	return nil
}

// reconcilePublishedReleaseSet turns the plan and the verified publication
// events into the next full snapshot. Every selected member needs exactly one
// verified, non-dry-run publication at its planned version; unchanged members
// keep their head record.
func reconcilePublishedReleaseSet(plan *releaseset.Plan, results map[string]*JobResult) (*distribution.ReleaseSet, error) {
	selected := make(map[string]releaseset.PlannedMember)
	for _, member := range plan.SelectedMembers() {
		selected[releaseset.MemberKey(member.Ecosystem, member.Coordinate)] = member
	}
	published := make(map[string]*extproto.PublishedMember, len(selected))
	for resultKey, result := range results {
		if result == nil {
			continue
		}
		for _, event := range result.Events {
			kind, _ := event.Data["kind"].(string)
			if event.Type != EventTypeArtifact || kind != extproto.PublishedMemberEventKind {
				continue
			}
			member, err := parsePublishedMemberEvent(event.Data)
			if err != nil {
				return nil, fmt.Errorf("job %q emitted an invalid published member: %w", resultKey, err)
			}
			key := releaseset.MemberKey(distribution.Ecosystem(member.Ecosystem), member.Coordinate)
			plannedMember, expected := selected[key]
			if !expected {
				return nil, fmt.Errorf("job %q published unexpected release-set member %s", resultKey, printableReleaseKey(key))
			}
			if _, duplicate := published[key]; duplicate {
				return nil, fmt.Errorf("release-set member %s was published more than once", printableReleaseKey(key))
			}
			if member.Version != plannedMember.Version {
				return nil, fmt.Errorf("published release-set member %s version %q does not match planned %q", printableReleaseKey(key), member.Version, plannedMember.Version)
			}
			published[key] = member
		}
	}
	if len(published) != len(selected) {
		var missing []string
		for key := range selected {
			if _, ok := published[key]; !ok {
				missing = append(missing, printableReleaseKey(key))
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("partial release-set publication; missing verified records for %v", missing)
	}
	finalSet := &distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       plan.Namespace,
		Members:         make([]distribution.ReleaseSetMember, 0, len(plan.Members)),
	}
	for _, member := range plan.Members {
		digest := member.ArtifactDigest
		platforms := maps.Clone(member.Platforms)
		if member.Selected {
			record := published[releaseset.MemberKey(member.Ecosystem, member.Coordinate)]
			digest = record.ArtifactDigest
			platforms = maps.Clone(record.Platforms)
		}
		finalSet.Members = append(finalSet.Members, distribution.ReleaseSetMember{
			Ecosystem: member.Ecosystem, Coordinate: member.Coordinate,
			Version: member.Version, ArtifactDigest: digest,
			Dependencies:         append([]distribution.ReleaseSetDependency(nil), member.Dependencies...),
			SourceRevision:       member.SourceRevision,
			SelectionFingerprint: member.SelectionFingerprint,
			Platforms:            platforms,
			Project:              member.Project,
			Kind:                 member.Kind,
			SourceTree:           member.SourceTree,
		})
	}
	finalSet = distribution.NormalizeReleaseSet(finalSet)
	if diagnostics := distribution.ValidateReleaseSet(finalSet); diag.HasErrors(diagnostics) {
		return nil, fmt.Errorf("final release set is not a closed full snapshot: %s", firstReleaseSetDiagnostic(diagnostics))
	}
	return finalSet, nil
}

// publishedMemberEnvelopeKeys are the runtime-event fields the JSONL reader
// flattens into every event's data map (ParseRawEvent), plus the `kind` that
// routes this one. They are the envelope, not the member.
var publishedMemberEnvelopeKeys = []string{"id", "kind", "level", "message", "name", "path", "time", "type"}

// parsePublishedMemberEvent reads one published-member event through the
// protocol's own strict reader. Everything left after the envelope belongs to
// the protocol, which rejects an unknown field — a publish job that emitted a
// field this build does not know would otherwise have its member silently
// truncated into the release set.
func parsePublishedMemberEvent(data map[string]any) (*extproto.PublishedMember, error) {
	payload := maps.Clone(data)
	for _, key := range publishedMemberEnvelopeKeys {
		delete(payload, key)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	member, diagnostics := extproto.ParsePublishedMember(encoded)
	if member == nil || len(diagnostics) > 0 {
		return nil, fmt.Errorf("%s", firstPublishedMemberDiagnostic(diagnostics))
	}
	if diagnostics := extproto.ValidatePublishedMember(member); len(diagnostics) > 0 {
		return nil, fmt.Errorf("%s", firstPublishedMemberDiagnostic(diagnostics))
	}
	return member, nil
}

func firstPublishedMemberDiagnostic(diagnostics []diag.Diagnostic) string {
	if len(diagnostics) == 0 {
		return "validation failed"
	}
	return diagnostics[0].String()
}

func printableReleaseKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }

func firstReleaseSetDiagnostic(diagnostics []diag.Diagnostic) string {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == diag.Error {
			return diagnostic.String()
		}
	}
	return "validation failed"
}

func releaseSetFailure(err error) *JobResult {
	return &JobResult{Status: "failed", Error: &JobError{Message: err.Error()}}
}

func releaseSetSuccess(outcome distribution.ReleaseSetPublishOutcome) *JobResult {
	result := &JobResult{Status: "success", Data: extension.ParamMap{}}
	result.Data[runtimeproto.ReleaseSetResultDataKey] = outcome
	return result
}
