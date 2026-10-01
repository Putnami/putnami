package ci

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ExplainInput contains locally assumed normalized Source facts.
type ExplainInput struct {
	// Event is the local normalized Source event assumption.
	Event Event `json:"event"`
	// Branch is the pushed branch, or the pull request's head branch.
	Branch string `json:"branch,omitempty"`
	// Tag is the created tag, for the tag event.
	Tag string `json:"tag,omitempty"`
	// PullRequestNumber renders the sole supported source placeholder.
	PullRequestNumber int `json:"pullRequestNumber,omitempty"`
}

// Explanation is safe for local and structured output. It contains no
// resolved authority and labels every remote decision as unresolved.
type Explanation struct {
	// Assumption repeats the explicitly local, non-authoritative Source facts.
	Assumption ExplainInput `json:"assumption"`
	// Commands is the normalized ordered invocation every run executes.
	Commands []CommandEntry `json:"commands"`
	// Flags is appended to that invocation.
	Flags []string `json:"flags,omitempty"`
	// Matched reports whether an ordered rule selected intent.
	Matched bool `json:"matched"`
	// RuleIndex is the first matching declaration-order index.
	RuleIndex *int `json:"ruleIndex,omitempty"`
	// Publish names the channels the publish advances, placeholders rendered.
	Publish []string `json:"publish,omitempty"`
	// Retain is the lifetime the matched rule declares for every channel in
	// Publish, verbatim: `while-open` or a whole number of days such as `30d`.
	// It is empty when the rule declares none, which means the channel is kept
	// until someone removes it. Enforcing it is the provider's part; this
	// report resolves nothing.
	Retain string `json:"retain,omitempty"`
	// Baseline is the channel the matched rule measures impact against when
	// the first channel of Publish has no head yet, verbatim. The runner
	// renders it as `--baseline-channel <name>`. It is read, never advanced,
	// so it never appears in Publish, and it is empty when the rule declares
	// none.
	Baseline string `json:"baseline,omitempty"`
	// ImplicitTag reports a tag that publishes by convention because no tags
	// rule selected it. The channel it creates is named after the tag and is
	// resolved by the publisher, not by this document.
	ImplicitTag bool `json:"implicitTag,omitempty"`
	// Environments maps every published channel to the environments that
	// follow it, directly or through one of their workload rules.
	Environments map[string][]string `json:"environments,omitempty"`
	// Unresolved names decisions that only trusted Cloud planes can make.
	Unresolved []string `json:"unresolved"`
}

// Explain evaluates the ordered rules against local Source facts. The commands
// apply to every event; a pull request selects a rule only when the rule opts
// pull requests in, and a tag with no matching rule publishes by convention.
func Explain(document Document, input ExplainInput) (Explanation, error) {
	if findings := Validate(document); diag.HasErrors(findings) {
		return Explanation{}, &ValidationError{Diagnostics: findings}
	}
	if err := validateExplainInput(input); err != nil {
		return Explanation{}, err
	}
	document = Normalize(document)
	report := Explanation{
		Assumption: input,
		Commands:   append([]CommandEntry(nil), document.Commands...),
		Flags:      append([]string(nil), document.Flags...),
		Unresolved: []string{
			"trusted Source policy revision",
			"Runtime entitlement and allocation ceilings",
			"Distribution channel authorization and visibility",
			"Control environment authorization and approvals",
		},
	}
	index, rule, err := selectRule(document, input)
	if err != nil {
		return Explanation{}, err
	}
	if index >= 0 {
		report.Matched = true
		matchedIndex := index
		report.RuleIndex = &matchedIndex
		report.Publish = renderChannels(rule.Publish, input.PullRequestNumber)
		if len(report.Publish) > 0 {
			report.Retain = rule.Retain
			// The baseline carries no placeholder: every pull request measures
			// against the same head, so it is reported exactly as authored.
			report.Baseline = rule.Baseline
		}
	} else if input.Event == EventTag {
		// A tag publishes by convention without a rule; only a tags rule with
		// `publish: false` opts out, and that rule would have matched here.
		report.ImplicitTag = true
	}
	report.Environments = environmentsFollowing(document, report.Publish)
	return report, nil
}

func validateExplainInput(input ExplainInput) error {
	if !input.Event.Valid() {
		return fmt.Errorf("unknown event %q", input.Event)
	}
	if input.Event == EventTag {
		if input.Tag == "" {
			return fmt.Errorf("the tag event needs a tag name")
		}
		if _, err := MatchBranch("**", input.Tag); err != nil {
			return err
		}
		return nil
	}
	if _, err := MatchBranch("**", input.Branch); err != nil {
		return err
	}
	if input.Event == EventPullRequest && input.PullRequestNumber < 0 {
		return fmt.Errorf("pull-request number cannot be negative")
	}
	return nil
}

// selectRule returns the first matching rule, or index -1 when none matches.
func selectRule(document Document, input ExplainInput) (int, Rule, error) {
	for index, rule := range document.Rules {
		event, ok := rule.Trigger()
		if !ok || event != input.Event {
			continue
		}
		matched, err := matchRuleGlobs(rule, input)
		if err != nil {
			return -1, Rule{}, err
		}
		if matched {
			return index, rule, nil
		}
	}
	return -1, Rule{}, nil
}

func matchRuleGlobs(rule Rule, input ExplainInput) (bool, error) {
	switch input.Event {
	case EventPush:
		return matchAny(rule.Branches, input.Branch)
	case EventTag:
		return matchAny(rule.Tags, input.Tag)
	case EventPullRequest:
		// `pullRequests: true` selects every pull request; the head branch is
		// carried for the placeholder and the report, not for a match.
		return true, nil
	default:
		return false, fmt.Errorf("unknown event %q", input.Event)
	}
}

func matchAny(globs Globs, value string) (bool, error) {
	for _, glob := range globs {
		matched, err := MatchBranch(glob, value)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func renderChannels(publish Publish, pullRequestNumber int) []string {
	if publish.Disabled || len(publish.Channels) == 0 {
		return nil
	}
	rendered := make([]string, 0, len(publish.Channels))
	for _, channel := range publish.Channels {
		if pullRequestNumber > 0 {
			channel = strings.Replace(channel, PullRequestNumberPlaceholder, strconv.Itoa(pullRequestNumber), 1)
		}
		rendered = append(rendered, channel)
	}
	return rendered
}

// environmentsFollowing maps every published channel to the sorted names of
// the environments that follow it, either directly or through one of their
// workload rules.
func environmentsFollowing(document Document, channels []string) map[string][]string {
	if len(channels) == 0 || len(document.Envs) == 0 {
		return nil
	}
	followers := make(map[string][]string, len(channels))
	for _, channel := range channels {
		var names []string
		for name, environment := range document.Envs {
			if environmentFollows(environment, channel) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			followers[channel] = names
		}
	}
	if len(followers) == 0 {
		return nil
	}
	return followers
}

func environmentFollows(environment Environment, channel string) bool {
	if environment.Channel == channel {
		return true
	}
	for _, workload := range environment.Workloads {
		if workload.Channel == channel {
			return true
		}
	}
	return false
}

// DefaultDocument returns a safe, authority-free initial contract: the generic
// commands and no rule, so nothing publishes until the workspace says which
// trigger does.
func DefaultDocument() Document {
	return Normalize(Document{
		Version:  Version,
		Commands: []CommandEntry{{Name: "lint"}, {Name: "test"}, {Name: "build"}},
	})
}
