package ci

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

const (
	// Version is the dedicated-file contract version. Earlier versions are not
	// read: the repository migrates its own file by hand.
	Version = 3
	// Filename is discovered beside putnami.workspace.json.
	Filename = "putnami.ci.json"
	// SchemaURL is the published schema identifier used by generated files.
	SchemaURL = "https://putnami.dev/schemas/putnami-ci.json"
	// MaxDocumentBytes prevents unbounded policy input.
	MaxDocumentBytes = 256 * 1024
	// MaxCommands bounds the ordered commands a document may declare.
	MaxCommands = 32
	// MaxRules bounds the ordered trigger rules a document may declare.
	MaxRules = 128
	// MaxEnvironments bounds the environments a document may declare.
	MaxEnvironments = 64
	// MaxWorkloads bounds the workload rules one environment may declare.
	MaxWorkloads = 64
	// PullRequestNumberPlaceholder is the only template a publish channel may
	// carry, and only on a rule that opts pull requests in.
	PullRequestNumberPlaceholder = "{number}"
	// RetainWhileOpen declares that the channels a pull-request rule publishes
	// live exactly as long as the pull request: the provider is expected to
	// retract them when it closes or merges.
	RetainWhileOpen = "while-open"
	// MaxRetainDays bounds the declared lifetime of a published channel. A
	// channel meant to outlive a year is not ephemeral; it is a channel the
	// repository declares in `distribution.channels`.
	MaxRetainDays = 365
	// DefaultVisibility is the repository level a document that declares none
	// inherits, and the floor of the whole inheritance chain.
	DefaultVisibility = "internal"
)

//go:embed schemas/putnami-ci.json
var schemaBytes []byte

// Schema returns a defensive copy of the version 3 JSON Schema.
func Schema() []byte { return append([]byte(nil), schemaBytes...) }

// Event is a normalized Source event a rule is evaluated against.
type Event string

const (
	// EventPush selects a branch update.
	EventPush Event = "push"
	// EventTag selects a tag creation.
	EventTag Event = "tag"
	// EventPullRequest selects a pull request using its head branch.
	EventPullRequest Event = "pull_request"
)

// Valid reports whether the event belongs to the closed vocabulary.
func (e Event) Valid() bool { return e == EventPush || e == EventTag || e == EventPullRequest }

// Document is the complete source-controlled CI intent: the ordered commands
// every run executes, the flags appended to that invocation, what the runner
// needs to configure itself, what the repository declares to its release-set
// provider, the ordered rules that name the channels a trigger publishes, and
// the environments that follow those channels.
type Document struct {
	// Schema is the optional editor-facing schema identifier.
	Schema string `json:"$schema,omitempty"`
	// Version selects the dedicated-file contract semantics.
	Version int `json:"version"`
	// Commands names the canonical framework graph roots every run executes,
	// in the order the CLI runs them, as one invocation
	// `putnami <c1,c2,…> --impacted <flags>`.
	Commands []CommandEntry `json:"commands"`
	// Flags is appended verbatim to that invocation; each command takes the
	// flags it declares.
	Flags []string `json:"flags,omitempty"`
	// Runner carries what the runner needs to configure itself and nothing
	// else. Its content is open and owned by the runner implementation, so it
	// is preserved as authored and only re-serialized with sorted keys.
	Runner json.RawMessage `json:"runner,omitempty"`
	// Distribution is what the repository declares to its release-set
	// provider: namespace, visibility chain, registries, channels, members.
	Distribution *Distribution `json:"distribution,omitempty"`
	// Rules is evaluated in declaration order with first-match semantics. A
	// trigger that matches no rule runs the commands only.
	Rules []Rule `json:"rules,omitempty"`
	// Envs declares the environments that follow a channel.
	Envs map[string]Environment `json:"envs,omitempty"`
	// Review is bounded semantic guidance consumed from the target revision.
	// It grants no credentials, tools, execution or publication authority.
	Review *ReviewProfile `json:"review,omitempty"`
}

// CommandEntry is one command of the ordered invocation. It is authored as a
// bare string when it blocks, which is the default, and as an object when it
// reports its errors as warnings instead.
type CommandEntry struct {
	// Name is the canonical framework graph root the runner executes.
	Name string `json:"name"`
	// FailOnError set to false runs the command and reports its errors as
	// warnings: the run passes. Absent or true, the command blocks.
	FailOnError *bool `json:"failOnError,omitempty"`
}

// Blocking reports whether a failure of this command fails the run.
func (c CommandEntry) Blocking() bool { return c.FailOnError == nil || *c.FailOnError }

// UnmarshalJSON accepts the bare string form and the object form.
func (c *CommandEntry) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		*c = CommandEntry{Name: name}
		return nil
	}
	type plain CommandEntry
	var decoded plain
	if err := strictUnmarshalObject(data, &decoded); err != nil {
		return err
	}
	*c = CommandEntry(decoded)
	return nil
}

// MarshalJSON writes the bare string form for a blocking command, so the
// canonical document is the form a human authors.
func (c CommandEntry) MarshalJSON() ([]byte, error) {
	if c.FailOnError == nil {
		return json.Marshal(c.Name)
	}
	type plain CommandEntry
	return json.Marshal(plain(c))
}

// Distribution is what the repository declares to its release-set provider.
// The CLI carries it; it computes no visibility level itself.
type Distribution struct {
	// Namespace is the repository's declared namespace at the provider.
	Namespace string `json:"namespace"`
	// Visibility is the repository level of the inheritance chain. It
	// defaults to internal.
	Visibility string `json:"visibility,omitempty"`
	// MemberAttribution opts the repository into recording each member's
	// source project and artifact kind on the sets it publishes
	// (protocols/distribution ADR 0005). It is off by default because every
	// consumer of a set decodes it strictly: the extensions that publish its
	// members read the plan, and the release-set provider and its backend read
	// the released set, and each refuses a field it does not know. Turn it on
	// only once every extension the workspace pins and its provider know the
	// two fields; an older consumer fails the publication with "unknown field".
	MemberAttribution bool `json:"memberAttribution,omitempty"`
	// MemberSourceTree opts the repository into recording, on each member it
	// republishes, the git tree of the checkout the member was built from
	// (protocols/distribution ADR 0005). It is off by default for the same
	// reason as MemberAttribution: the provider and its backend refuse a
	// member field they do not know, and an extension built before the field
	// refuses a plan whose carried-over member records it. Turn it on only
	// once every extension the workspace pins and its provider know the field.
	MemberSourceTree bool `json:"memberSourceTree,omitempty"`
	// Registries declares, per ecosystem id, the registry level of the chain
	// and the external target public members are mirrored to.
	Registries map[string]RegistryPolicy `json:"registries,omitempty"`
	// Channels declares, per channel name, its level and whether it is
	// protected.
	Channels map[string]ChannelPolicy `json:"channels,omitempty"`
	// Versions declares the level a version shape resolves to.
	Versions *VersionPolicy `json:"versions,omitempty"`
	// Members declares, in order, the selection rules that give a member its
	// own level.
	Members []MemberPolicy `json:"members,omitempty"`
}

// RegistryPolicy is one ecosystem's registry level and mirror target.
type RegistryPolicy struct {
	// Visibility is the registry level of the inheritance chain.
	Visibility string `json:"visibility,omitempty"`
	// Mirror is the external target the backend copies public members to.
	Mirror *Mirror `json:"mirror,omitempty"`
}

// Mirror is an external distribution target.
type Mirror struct {
	// To is the external registry the backend copies public members to.
	To string `json:"to"`
}

// ChannelPolicy is one channel's level and protection.
type ChannelPolicy struct {
	// Visibility is the channel level of the inheritance chain.
	Visibility string `json:"visibility,omitempty"`
	// Protected marks a channel no rule may target and no CI principal may
	// move: only `putnami channel set` by a user moves it.
	Protected bool `json:"protected,omitempty"`
}

// VersionPolicy maps a version shape to a visibility level.
type VersionPolicy struct {
	// Stable is the level a release version resolves to.
	Stable string `json:"stable,omitempty"`
	// Prerelease is the level a prerelease version resolves to.
	Prerelease string `json:"prerelease,omitempty"`
}

// MemberPolicy gives the members of the selected projects their own level,
// the finest of the inheritance chain.
type MemberPolicy struct {
	// Select uses the CLI selection vocabulary.
	Select Selectors `json:"select"`
	// Visibility is the member level of the inheritance chain.
	Visibility string `json:"visibility"`
}

// Selectors is a CLI selection vocabulary list authored as one string or an
// array: `tag:<tag>`, `group:<group>`, `scope:<path>`, or a project id.
type Selectors []string

// UnmarshalJSON accepts one string or an array of strings.
func (s *Selectors) UnmarshalJSON(data []byte) error {
	values, err := decodeStringOrArray(data)
	if err != nil {
		return err
	}
	*s = values
	return nil
}

// MarshalJSON writes the bare string form for a single selector.
func (s Selectors) MarshalJSON() ([]byte, error) { return encodeStringOrArray(s) }

// Rule selects one trigger and lists the channels a publish advances for it.
type Rule struct {
	// Branches selects a push whose branch matches one of the globs.
	Branches Globs `json:"branches,omitempty"`
	// Tags selects a tag whose name matches one of the globs.
	Tags Globs `json:"tags,omitempty"`
	// PullRequests selects every pull request. It is the only way a pull
	// request publishes.
	PullRequests bool `json:"pullRequests,omitempty"`
	// Publish lists the channels the publish advances, in `--impacted` mode,
	// or opts out with false.
	Publish Publish `json:"publish,omitzero"`
	// Retain declares how long the channels this rule publishes are kept:
	// `while-open` on a pull-request rule, or a whole number of days such as
	// `30d`. An absent Retain declares no lifetime, and the channel is kept
	// until someone removes it. The contract states what a provider is
	// expected to do with the declaration; it never retracts anything itself.
	Retain string `json:"retain,omitempty"`
	// Baseline names the channel a publish measures impact against when the
	// first channel of Publish has no head yet. It is read, never advanced,
	// and never appears in Publish. It is one portable channel name: no
	// `{number}`, because every pull request reads the same baseline. Absent,
	// a first publish on an empty channel republishes every member.
	Baseline string `json:"baseline,omitempty"`
}

// RetainsWhileOpen reports the pull-request-scoped lifetime: the provider is
// expected to retract the rule's channels when the pull request closes or
// merges.
func (r Rule) RetainsWhileOpen() bool { return r.Retain == RetainWhileOpen }

// RetainDays returns the declared lifetime in whole days, and false when the
// rule declares no day-based lifetime. It is the single parser of the `<n>d`
// spelling, so validation, explanation, and any consumer read one value.
func (r Rule) RetainDays() (int, bool) {
	match := retainDaysPattern.FindStringSubmatch(r.Retain)
	if match == nil {
		return 0, false
	}
	days, err := strconv.Atoi(match[1])
	if err != nil || days < 1 || days > MaxRetainDays {
		return 0, false
	}
	return days, true
}

// retainDaysPattern is the accepted duration spelling: a whole number of days.
// Days are the unit a channel lifetime is authored in, and the only one, so
// one intent has exactly one spelling and therefore one canonical digest
// (ADR 0003).
var retainDaysPattern = regexp.MustCompile(`^([1-9][0-9]{0,2})d$`)

// Trigger reports which event the rule selects, and whether it selects one.
func (r Rule) Trigger() (Event, bool) {
	switch {
	case len(r.Branches) > 0 && len(r.Tags) == 0 && !r.PullRequests:
		return EventPush, true
	case len(r.Tags) > 0 && len(r.Branches) == 0 && !r.PullRequests:
		return EventTag, true
	case r.PullRequests && len(r.Branches) == 0 && len(r.Tags) == 0:
		return EventPullRequest, true
	default:
		return "", false
	}
}

// Globs is a slash-aware glob list authored as one string or an array.
type Globs []string

// UnmarshalJSON accepts one string or an array of strings.
func (g *Globs) UnmarshalJSON(data []byte) error {
	values, err := decodeStringOrArray(data)
	if err != nil {
		return err
	}
	*g = values
	return nil
}

// MarshalJSON writes the bare string form for a single glob.
func (g Globs) MarshalJSON() ([]byte, error) { return encodeStringOrArray(g) }

// Publish is the channel list a rule advances, or the explicit opt-out `false`
// that stops a tag from publishing by convention.
type Publish struct {
	// Channels are the unresolved channel names the publish advances.
	Channels []string `json:"channels,omitempty"`
	// Disabled is the authored `false`: this trigger publishes nothing.
	Disabled bool `json:"disabled,omitempty"`
}

// UnmarshalJSON accepts an array of channel names or the literal false.
func (p *Publish) UnmarshalJSON(data []byte) error {
	if string(data) == "false" {
		*p = Publish{Disabled: true}
		return nil
	}
	if string(data) == "true" {
		return fmt.Errorf("publish must be an array of channel names or false")
	}
	var channels []string
	if err := json.Unmarshal(data, &channels); err != nil {
		return fmt.Errorf("publish must be an array of channel names or false: %w", err)
	}
	*p = Publish{Channels: channels}
	return nil
}

// MarshalJSON is symmetric with UnmarshalJSON.
func (p Publish) MarshalJSON() ([]byte, error) {
	if p.Disabled {
		return []byte("false"), nil
	}
	if p.Channels == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.Channels)
}

// IsZero reports an unauthored publish, so a rule that declares none emits no
// member at all.
func (p Publish) IsZero() bool { return !p.Disabled && p.Channels == nil }

// Environment follows a channel: when the channel moves, the environment
// synchronizes its workloads on the named set under its constraints.
type Environment struct {
	// Channel is the channel this environment follows. Without one, the
	// environment only reacts to the command a person or an agent types.
	Channel string `json:"channel,omitempty"`
	// Constraints gates the synchronization; `approval: manual` is the one
	// every implementation supports.
	Constraints map[string]any `json:"constraints,omitempty"`
	// Rollout is how the synchronization advances.
	Rollout *Rollout `json:"rollout,omitempty"`
	// Variants declares parallel channels routed beside the main one.
	Variants map[string]Variant `json:"variants,omitempty"`
	// Workloads lists ordered selection rules, first match; a workload no
	// rule selects is not part of the environment.
	Workloads []WorkloadRule `json:"workloads,omitempty"`
}

// Rollout is how an environment advances a synchronization.
type Rollout struct {
	// Strategy is `progressive` or `all-at-once`.
	Strategy string `json:"strategy"`
	// Steps are ascending percentages in 1..100 whose last entry is 100.
	Steps []int `json:"steps,omitempty"`
	// Advance is `manual` or a Go duration between two steps.
	Advance string `json:"advance,omitempty"`
	// AbortOn names the signal that rolls the step back.
	AbortOn string `json:"abortOn,omitempty"`
}

// Variant is a channel routed beside the environment's main channel.
type Variant struct {
	// Channel is the channel the variant follows.
	Channel string `json:"channel"`
	// Routing is the implementation-owned routing description.
	Routing map[string]any `json:"routing"`
}

// WorkloadRule selects workloads and overrides what the environment declares.
type WorkloadRule struct {
	// Select uses the CLI selection vocabulary.
	Select Selectors `json:"select"`
	// Channel overrides the environment's channel for these workloads.
	Channel string `json:"channel,omitempty"`
	// Rollout overrides the environment's rollout for these workloads.
	Rollout *Rollout `json:"rollout,omitempty"`
	// Constraints overrides the environment's constraints for these
	// workloads; an empty object clears them.
	Constraints map[string]any `json:"constraints,omitempty"`
}

// HasProviderSections reports whether the document declares anything that only
// a release-set provider can serve.
func HasProviderSections(d Document) bool {
	return d.Distribution != nil || len(d.Envs) > 0
}

func decodeStringOrArray(data []byte) ([]string, error) {
	if len(data) > 0 && data[0] == '"' {
		var single string
		if err := json.Unmarshal(data, &single); err != nil {
			return nil, err
		}
		return []string{single}, nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("must be a string or an array of strings: %w", err)
	}
	return values, nil
}

func encodeStringOrArray(values []string) ([]byte, error) {
	if len(values) == 1 {
		return json.Marshal(values[0])
	}
	if values == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(values)
}

func strictUnmarshalObject(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
