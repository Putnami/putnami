package ci

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	distributionproto "go.putnami.dev/protocol/distribution"
)

var (
	mirrorTargetPattern = regexp.MustCompile(distributionproto.MirrorTargetPattern)
	commandPattern      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	flagPattern         = regexp.MustCompile(`^--[a-z0-9][a-z0-9-]*(=[^\s]+)?$`)
	channelPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	ecosystemPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	namespacePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	projectIDPattern    = regexp.MustCompile(`^/?[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// visibilityLevels is the ordered closed vocabulary of the inheritance chain.
var visibilityLevels = map[string]struct{}{"internal": {}, "private": {}, "public": {}}

// rolloutStrategies is the closed vocabulary of a rollout strategy.
var rolloutStrategies = map[string]struct{}{"progressive": {}, "all-at-once": {}}

// selectorPrefixes are the CLI selection vocabulary prefixes a select value may
// carry. A value with no prefix is a project id.
var selectorPrefixes = []string{"tag:", "group:", "scope:"}

// Validate returns deterministic, field-addressable findings without mutating
// document.
func Validate(document Document) []diag.Diagnostic {
	findings := make([]diag.Diagnostic, 0)
	add := func(finding diag.Diagnostic) { findings = append(findings, finding) }

	if document.Schema != "" && document.Schema != SchemaURL {
		add(diag.Errorf("ci.unknown_schema", "$schema", "must be %q when present", SchemaURL))
	}
	if document.Version != Version {
		add(diag.Errorf("ci.unknown_version", "version", "must be %d; migrate the document by hand", Version))
	}
	validateCommands(add, document)
	validateFlags(add, document)
	validateRunner(add, document)
	validateDistribution(add, document.Distribution)
	validateRules(add, document)
	validateEnvs(add, document)
	validateReview(add, document.Review)
	return findings
}

func validateCommands(add func(diag.Diagnostic), document Document) {
	if len(document.Commands) == 0 {
		add(diag.Errorf("ci.required", "commands", "must name at least one command"))
	}
	if len(document.Commands) > MaxCommands {
		add(diag.Errorf("ci.too_many_entries", "commands", "has %d entries; maximum is %d", len(document.Commands), MaxCommands))
	}
	seen := map[string]int{}
	for index, command := range document.Commands {
		field := fmt.Sprintf("commands[%d]", index)
		if strings.TrimSpace(command.Name) != command.Name || !commandPattern.MatchString(command.Name) {
			add(diag.Errorf("ci.invalid_task", field, "must be a canonical Putnami graph job name"))
		}
		if previous, duplicate := seen[command.Name]; duplicate {
			add(diag.Errorf("ci.duplicate_task", field, "duplicates commands[%d]", previous))
		} else {
			seen[command.Name] = index
		}
	}
}

func validateFlags(add func(diag.Diagnostic), document Document) {
	for index, flag := range document.Flags {
		if !flagPattern.MatchString(flag) {
			add(diag.Errorf("ci.invalid_flag", fmt.Sprintf("flags[%d]", index),
				"must be a long flag such as --enforce-coverage or --name=value"))
		}
	}
}

func validateRunner(add func(diag.Diagnostic), document Document) {
	if len(document.Runner) == 0 {
		return
	}
	if _, err := canonicalObject(document.Runner); err != nil {
		add(diag.Errorf("ci.invalid_json", "runner", "must be a JSON object: %v", err))
	}
}

func validateDistribution(add func(diag.Diagnostic), distribution *Distribution) {
	if distribution == nil {
		return
	}
	if distribution.Namespace == "" {
		add(diag.Errorf("ci.required", "distribution.namespace", "field is required"))
	} else if !namespacePattern.MatchString(distribution.Namespace) {
		add(diag.Errorf("ci.invalid_namespace", "distribution.namespace", "must match %s", namespacePattern))
	}
	validateVisibility(add, "distribution.visibility", distribution.Visibility)
	for _, ecosystem := range sortedMapKeys(distribution.Registries) {
		registry := distribution.Registries[ecosystem]
		field := "distribution.registries." + ecosystem
		if !ecosystemPattern.MatchString(ecosystem) {
			add(diag.Errorf("ci.invalid_ecosystem", field, "registry key must match the ecosystem id alphabet %s", ecosystemPattern))
		}
		validateVisibility(add, field+".visibility", registry.Visibility)
		if registry.Mirror != nil {
			target := registry.Mirror.To
			switch {
			case strings.TrimSpace(target) == "":
				add(diag.Errorf("ci.required", field+".mirror.to", "field is required"))
			case len(target) > distributionproto.MaxMirrorTargetBytes || !mirrorTargetPattern.MatchString(target):
				add(diag.Errorf("ci.invalid_mirror_target", field+".mirror.to", "must be a non-secret HTTPS or native registry destination of at most %d bytes", distributionproto.MaxMirrorTargetBytes))
			}
		}
	}
	for _, name := range sortedMapKeys(distribution.Channels) {
		field := "distribution.channels." + name
		if !channelPattern.MatchString(name) {
			add(diag.Errorf("ci.invalid_channel", field, "must match the portable channel alphabet %s", channelPattern))
		}
		validateVisibility(add, field+".visibility", distribution.Channels[name].Visibility)
	}
	if distribution.Versions != nil {
		validateVisibility(add, "distribution.versions.stable", distribution.Versions.Stable)
		validateVisibility(add, "distribution.versions.prerelease", distribution.Versions.Prerelease)
	}
	for index, member := range distribution.Members {
		field := fmt.Sprintf("distribution.members[%d]", index)
		validateSelectors(add, field+".select", member.Select)
		if member.Visibility == "" {
			add(diag.Errorf("ci.required", field+".visibility", "field is required"))
		} else {
			validateVisibility(add, field+".visibility", member.Visibility)
		}
	}
}

func validateRules(add func(diag.Diagnostic), document Document) {
	if len(document.Rules) > MaxRules {
		add(diag.Errorf("ci.too_many_entries", "rules", "has %d entries; maximum is %d", len(document.Rules), MaxRules))
	}
	seen := map[string]int{}
	for index, rule := range document.Rules {
		field := fmt.Sprintf("rules[%d]", index)
		event, ok := rule.Trigger()
		if !ok {
			add(diag.Errorf("ci.rule_trigger", field, "must select exactly one of branches, tags, or pullRequests"))
			continue
		}
		validateRuleGlobs(add, field, rule)
		predicate := string(event) + "\x00" + strings.Join(rule.Branches, ",") + "\x00" + strings.Join(rule.Tags, ",")
		if previous, duplicate := seen[predicate]; duplicate {
			add(diag.Errorf("ci.duplicate_rule", field, "duplicates rules[%d]", previous))
		} else {
			seen[predicate] = index
		}
		validateRulePublish(add, field, rule, event, document.Distribution)
		validateRuleRetain(add, field, rule, event)
		validateRuleBaseline(add, field, rule)
	}
}

func validateRuleGlobs(add func(diag.Diagnostic), field string, rule Rule) {
	for globIndex, glob := range rule.Branches {
		if err := ValidateBranchGlob(glob); err != nil {
			add(diag.Errorf("ci.invalid_branch_glob", fmt.Sprintf("%s.branches[%d]", field, globIndex), "%v", err))
		}
	}
	for globIndex, glob := range rule.Tags {
		if err := ValidateBranchGlob(glob); err != nil {
			add(diag.Errorf("ci.invalid_branch_glob", fmt.Sprintf("%s.tags[%d]", field, globIndex), "%v", err))
		}
	}
}

func validateRulePublish(add func(diag.Diagnostic), field string, rule Rule, event Event, distribution *Distribution) {
	for channelIndex, channel := range rule.Publish.Channels {
		channelField := fmt.Sprintf("%s.publish[%d]", field, channelIndex)
		if !validChannelName(channel, event == EventPullRequest) {
			add(diag.Errorf("ci.invalid_channel", channelField,
				"must match the portable channel alphabet %s; %s is allowed once, on a pullRequests rule",
				channelPattern, PullRequestNumberPlaceholder))
			continue
		}
		if distribution != nil && distribution.Channels[channel].Protected {
			add(diag.Errorf("ci.protected_channel_in_rule", channelField,
				"channel %q is protected; only `putnami channel set` by a user moves it", channel))
		}
	}
}

// validateRuleRetain checks the declared lifetime of the channels this rule
// publishes. A lifetime with no channel to apply to is an authoring mistake,
// not a silent no-op; `while-open` needs a pull request to be open and a
// channel that belongs to exactly one; and a duration is authored in whole
// days, the single accepted spelling (ADR 0003).
func validateRuleRetain(add func(diag.Diagnostic), field string, rule Rule, event Event) {
	if rule.Retain == "" {
		return
	}
	retainField := field + ".retain"
	if len(rule.Publish.Channels) == 0 {
		add(diag.Errorf("ci.invalid_retain", retainField,
			"declares a lifetime, but this rule publishes no channel"))
		return
	}
	if rule.RetainsWhileOpen() {
		if event != EventPullRequest {
			add(diag.Errorf("ci.invalid_retain", retainField,
				"%q needs a pull request to be open; a %s rule declares a number of days such as \"30d\"",
				RetainWhileOpen, event))
			return
		}
		validateWhileOpenChannels(add, retainField, rule)
		return
	}
	if _, ok := rule.RetainDays(); !ok {
		add(diag.Errorf("ci.invalid_retain", retainField,
			"must be %q or a whole number of days from 1d to %dd, such as \"30d\"; a Go duration such as \"720h\" is not accepted here",
			RetainWhileOpen, MaxRetainDays))
	}
}

// validateRuleBaseline checks the channel this rule's publish measures against
// when its first published channel is still empty. The baseline is read and
// never advanced, which is what makes the three rules below the whole contract:
// it needs a publish to be the baseline OF, it cannot carry `{number}` because
// every pull request reads the same head, and it cannot be one of the channels
// the same rule advances — that channel is already its own baseline, and naming
// it twice would state two different things about one head. A protected channel
// is a valid baseline: protection forbids advancing a channel, not reading it.
func validateRuleBaseline(add func(diag.Diagnostic), field string, rule Rule) {
	if rule.Baseline == "" {
		return
	}
	baselineField := field + ".baseline"
	if rule.Publish.Disabled || len(rule.Publish.Channels) == 0 {
		add(diag.Errorf("ci.invalid_baseline", baselineField,
			"names a baseline channel, but this rule publishes no channel to measure against it"))
		return
	}
	if !validChannelName(rule.Baseline, false) {
		add(diag.Errorf("ci.invalid_baseline", baselineField,
			"must match the portable channel alphabet %s; %s is not allowed, because every pull request measures against the same baseline",
			channelPattern, PullRequestNumberPlaceholder))
		return
	}
	if slices.Contains(rule.Publish.Channels, rule.Baseline) {
		add(diag.Errorf("ci.invalid_baseline", baselineField,
			"channel %q is advanced by this rule's publish; a baseline is read and never advanced, and an advanced channel is already its own baseline",
			rule.Baseline))
	}
}

// validateWhileOpenChannels refuses a `while-open` lifetime on a channel
// several pull requests share. The declaration asks a provider to retract the
// channel when *the* pull request closes, which only names one channel when
// the channel belongs to one pull request; `{number}` is the only thing in
// this contract that makes it so. The lifetime applies to every channel the
// rule publishes, so one shared channel in the list is enough to make the
// declaration unsound, and the finding is anchored on the lifetime rather than
// on a channel name that is perfectly valid on its own.
func validateWhileOpenChannels(add func(diag.Diagnostic), retainField string, rule Rule) {
	shared := make([]string, 0, len(rule.Publish.Channels))
	for _, channel := range rule.Publish.Channels {
		if !strings.Contains(channel, PullRequestNumberPlaceholder) {
			shared = append(shared, fmt.Sprintf("%q", channel))
		}
	}
	if len(shared) == 0 {
		return
	}
	add(diag.Errorf("ci.invalid_retain", retainField,
		"%q retracts the channel when one pull request closes, so every channel this rule publishes must belong to one: %s carries no %s; carry it, as in \"pr-%s\", or declare a number of days such as \"30d\" for a channel several pull requests share",
		RetainWhileOpen, strings.Join(shared, ", "), PullRequestNumberPlaceholder, PullRequestNumberPlaceholder))
}

func validateEnvs(add func(diag.Diagnostic), document Document) {
	if len(document.Envs) > MaxEnvironments {
		add(diag.Errorf("ci.too_many_entries", "envs", "has %d entries; maximum is %d", len(document.Envs), MaxEnvironments))
	}
	for _, name := range sortedMapKeys(document.Envs) {
		validateEnvironment(add, "envs."+name, document.Envs[name])
	}
}

func validateEnvironment(add func(diag.Diagnostic), field string, environment Environment) {
	validateEnvChannel(add, field+".channel", environment.Channel)
	validateConstraints(add, field+".constraints", environment.Constraints)
	validateRollout(add, field+".rollout", environment.Rollout)
	for _, name := range sortedMapKeys(environment.Variants) {
		variant := environment.Variants[name]
		if variant.Channel == "" {
			add(diag.Errorf("ci.required", field+".variants."+name+".channel", "field is required"))
		} else {
			validateEnvChannel(add, field+".variants."+name+".channel", variant.Channel)
		}
	}
	if len(environment.Workloads) > MaxWorkloads {
		add(diag.Errorf("ci.too_many_entries", field+".workloads",
			"has %d entries; maximum is %d", len(environment.Workloads), MaxWorkloads))
	}
	for index, workload := range environment.Workloads {
		workloadField := fmt.Sprintf("%s.workloads[%d]", field, index)
		validateSelectors(add, workloadField+".select", workload.Select)
		validateEnvChannel(add, workloadField+".channel", workload.Channel)
		validateConstraints(add, workloadField+".constraints", workload.Constraints)
		validateRollout(add, workloadField+".rollout", workload.Rollout)
	}
}

func validateEnvChannel(add func(diag.Diagnostic), field, channel string) {
	if channel == "" {
		return
	}
	if !channelPattern.MatchString(channel) {
		add(diag.Errorf("ci.invalid_channel", field, "must match the portable channel alphabet %s", channelPattern))
	}
}

func validateConstraints(add func(diag.Diagnostic), field string, constraints map[string]any) {
	approval, present := constraints["approval"]
	if !present {
		return
	}
	if value, ok := approval.(string); !ok || value != "manual" {
		add(diag.Errorf("ci.invalid_constraint", field+".approval", "must be \"manual\" when present"))
	}
}

func validateRollout(add func(diag.Diagnostic), field string, rollout *Rollout) {
	if rollout == nil {
		return
	}
	if _, ok := rolloutStrategies[rollout.Strategy]; !ok {
		add(diag.Errorf("ci.invalid_rollout", field+".strategy", "must be progressive or all-at-once"))
	}
	validateRolloutSteps(add, field, rollout.Steps)
	if rollout.Advance != "" && rollout.Advance != "manual" {
		if _, err := time.ParseDuration(rollout.Advance); err != nil {
			add(diag.Errorf("ci.invalid_rollout", field+".advance", "must be \"manual\" or a Go duration"))
		}
	}
}

func validateRolloutSteps(add func(diag.Diagnostic), field string, steps []int) {
	if len(steps) == 0 {
		return
	}
	previous := 0
	for index, step := range steps {
		if step < 1 || step > 100 || step <= previous {
			add(diag.Errorf("ci.invalid_rollout", fmt.Sprintf("%s.steps[%d]", field, index),
				"steps must ascend strictly within 1..100"))
			return
		}
		previous = step
	}
	if steps[len(steps)-1] != 100 {
		add(diag.Errorf("ci.invalid_rollout", field+".steps", "the last step must be 100"))
	}
}

func validateSelectors(add func(diag.Diagnostic), field string, selectors Selectors) {
	if len(selectors) == 0 {
		add(diag.Errorf("ci.required", field, "must name at least one selector"))
		return
	}
	for index, selector := range selectors {
		if !validSelector(selector) {
			add(diag.Errorf("ci.invalid_selector", fmt.Sprintf("%s[%d]", field, index),
				"must be tag:<tag>, group:<group>, scope:<path>, or a project id"))
		}
	}
}

func validateVisibility(add func(diag.Diagnostic), field, value string) {
	if value == "" {
		return
	}
	if _, ok := visibilityLevels[value]; !ok {
		add(diag.Errorf("ci.invalid_visibility", field, "must be internal, private, or public"))
	}
}

// ValidateTaskReferences resolves the document's command names against the
// workspace graph's known job vocabulary. known maps accepted spellings to
// canonical identities.
func ValidateTaskReferences(document Document, known map[string]string) []diag.Diagnostic {
	var findings []diag.Diagnostic
	for index, command := range document.Commands {
		canonical, exists := known[command.Name]
		if !exists || canonical == "" {
			findings = append(findings, diag.Errorf(
				"ci.unknown_task", fmt.Sprintf("commands[%d]", index),
				"command %q is not present in the workspace graph", command.Name,
			))
		}
	}
	return findings
}

func validChannelName(channel string, allowPlaceholder bool) bool {
	rendered := channel
	if strings.Contains(channel, PullRequestNumberPlaceholder) {
		if !allowPlaceholder || strings.Count(channel, PullRequestNumberPlaceholder) != 1 {
			return false
		}
		rendered = strings.Replace(channel, PullRequestNumberPlaceholder, "1", 1)
	}
	if strings.ContainsAny(rendered, "{}") {
		return false
	}
	return channelPattern.MatchString(rendered)
}

func validSelector(selector string) bool {
	if strings.TrimSpace(selector) != selector || selector == "" {
		return false
	}
	for _, prefix := range selectorPrefixes {
		if strings.HasPrefix(selector, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(selector, prefix)) == strings.TrimPrefix(selector, prefix) &&
				strings.TrimPrefix(selector, prefix) != ""
		}
	}
	return projectIDPattern.MatchString(selector) && !containsDotSegment(selector)
}

func containsDotSegment(value string) bool {
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == "." || segment == ".." || segment == "" {
			return true
		}
	}
	return false
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
