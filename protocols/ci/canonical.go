package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Normalize makes schema defaults explicit and copies the document. Order is
// preserved where it is semantic — commands, flags, rules, members, workloads,
// and rollout steps — while maps are serialized in sorted key order and the
// opaque runner object is re-marshaled with sorted keys.
func Normalize(document Document) Document {
	out := Document{
		Schema:   SchemaURL,
		Version:  Version,
		Commands: normalizeCommands(document.Commands),
		Flags:    append([]string(nil), document.Flags...),
		Rules:    normalizeRules(document.Rules),
	}
	if canonical, err := canonicalObject(document.Runner); err == nil {
		out.Runner = canonical
	} else {
		out.Runner = append(json.RawMessage(nil), document.Runner...)
	}
	if document.Distribution != nil {
		out.Distribution = normalizeDistribution(*document.Distribution)
	}
	if len(document.Envs) > 0 {
		out.Envs = make(map[string]Environment, len(document.Envs))
		for name, environment := range document.Envs {
			out.Envs[name] = normalizeEnvironment(environment)
		}
	}
	if document.Review != nil {
		profile := *document.Review
		profile.Focus = append([]string{}, profile.Focus...)
		profile.Instructions = append([]string{}, profile.Instructions...)
		out.Review = &profile
	}
	return out
}

// normalizeCommands drops the redundant explicit `failOnError: true`, which
// means exactly what the absent member means, so two spellings of one intent
// produce one canonical document and one digest.
func normalizeCommands(commands []CommandEntry) []CommandEntry {
	if commands == nil {
		return nil
	}
	out := make([]CommandEntry, len(commands))
	for index, command := range commands {
		out[index] = CommandEntry{Name: command.Name}
		if command.FailOnError != nil && !*command.FailOnError {
			blocking := false
			out[index].FailOnError = &blocking
		}
	}
	return out
}

func normalizeRules(rules []Rule) []Rule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]Rule, len(rules))
	for index, rule := range rules {
		out[index] = Rule{
			Branches:     append(Globs(nil), rule.Branches...),
			Tags:         append(Globs(nil), rule.Tags...),
			PullRequests: rule.PullRequests,
			Publish: Publish{
				Channels: append([]string(nil), rule.Publish.Channels...),
				Disabled: rule.Publish.Disabled,
			},
			Retain:   rule.Retain,
			Baseline: rule.Baseline,
		}
	}
	return out
}

func normalizeDistribution(distribution Distribution) *Distribution {
	out := Distribution{
		Namespace:         distribution.Namespace,
		Visibility:        distribution.Visibility,
		MemberAttribution: distribution.MemberAttribution,
		MemberSourceTree:  distribution.MemberSourceTree,
	}
	if out.Visibility == "" {
		out.Visibility = DefaultVisibility
	}
	if len(distribution.Registries) > 0 {
		out.Registries = make(map[string]RegistryPolicy, len(distribution.Registries))
		for ecosystem, registry := range distribution.Registries {
			copied := RegistryPolicy{Visibility: registry.Visibility}
			if registry.Mirror != nil {
				mirror := *registry.Mirror
				copied.Mirror = &mirror
			}
			out.Registries[ecosystem] = copied
		}
	}
	if len(distribution.Channels) > 0 {
		out.Channels = make(map[string]ChannelPolicy, len(distribution.Channels))
		for name, channel := range distribution.Channels {
			out.Channels[name] = channel
		}
	}
	if distribution.Versions != nil {
		versions := *distribution.Versions
		out.Versions = &versions
	}
	if len(distribution.Members) > 0 {
		out.Members = make([]MemberPolicy, len(distribution.Members))
		for index, member := range distribution.Members {
			out.Members[index] = MemberPolicy{
				Select:     append(Selectors(nil), member.Select...),
				Visibility: member.Visibility,
			}
		}
	}
	return &out
}

func normalizeEnvironment(environment Environment) Environment {
	out := Environment{
		Channel:     environment.Channel,
		Constraints: environment.Constraints,
		Rollout:     copyRollout(environment.Rollout),
	}
	if len(environment.Variants) > 0 {
		out.Variants = make(map[string]Variant, len(environment.Variants))
		for name, variant := range environment.Variants {
			out.Variants[name] = variant
		}
	}
	if len(environment.Workloads) > 0 {
		out.Workloads = make([]WorkloadRule, len(environment.Workloads))
		for index, workload := range environment.Workloads {
			out.Workloads[index] = WorkloadRule{
				Select:      append(Selectors(nil), workload.Select...),
				Channel:     workload.Channel,
				Rollout:     copyRollout(workload.Rollout),
				Constraints: workload.Constraints,
			}
		}
	}
	return out
}

func copyRollout(rollout *Rollout) *Rollout {
	if rollout == nil {
		return nil
	}
	copied := *rollout
	copied.Steps = append([]int(nil), rollout.Steps...)
	return &copied
}

// canonicalObject re-marshals an opaque JSON object with sorted keys, so a
// member the contract deliberately does not model still contributes a stable
// digest. It refuses anything that is not an object.
func canonicalObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("must not be null")
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// CanonicalBytes returns compact deterministic intent bytes.
func CanonicalBytes(document Document) ([]byte, error) {
	return json.Marshal(Normalize(document))
}

// Digest returns the lowercase SHA-256 digest of canonical intent.
func Digest(document Document) (string, error) {
	data, err := CanonicalBytes(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Format parses and writes canonical two-space JSON with a trailing newline.
func Format(data []byte) ([]byte, error) {
	document, err := Parse(data)
	if err != nil {
		return nil, err
	}
	formatted, err := json.MarshalIndent(Normalize(document), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(formatted, '\n'), nil
}

// IsFormatted reports whether data already equals its canonical formatting.
func IsFormatted(data []byte) (bool, error) {
	formatted, err := Format(data)
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, formatted), nil
}
