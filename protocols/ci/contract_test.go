package ci

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestValidFixturesParseAndNormalize(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("fixtures", "valid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Fatalf("valid corpus = %v, want the six documented shapes", entries)
	}
	for _, path := range entries {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			document, findings := ParseWithDiagnostics(data)
			if diag.HasErrors(findings) {
				t.Fatalf("ParseWithDiagnostics: %v", findings)
			}
			if len(findings) != 0 {
				t.Fatalf("findings = %#v, want none", findings)
			}
			if document.Version != Version || document.Schema != SchemaURL {
				t.Fatalf("document = %#v", document)
			}
			formatted, formatErr := Format(data)
			if formatErr != nil {
				t.Fatalf("Format: %v", formatErr)
			}
			if !bytes.Equal(data, formatted) {
				t.Fatalf("%s is not canonically formatted:\n--- authored ---\n%s\n--- canonical ---\n%s", path, data, formatted)
			}
		})
	}
}

func TestWorkspaceFixtureCarriesEveryMember(t *testing.T) {
	document, err := Parse(readFixture(t, "valid", "workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantCommands := []string{"lint", "test", "build", "validate", "audit"}
	for index, command := range document.Commands {
		if command.Name != wantCommands[index] {
			t.Fatalf("commands = %#v, want the authored order %v", document.Commands, wantCommands)
		}
	}
	if document.Commands[4].Blocking() {
		t.Fatal("failOnError: false must not block the run")
	}
	if !document.Commands[0].Blocking() {
		t.Fatal("an entry without failOnError blocks the run")
	}
	if !reflect.DeepEqual(document.Flags, []string{"--enforce-coverage"}) {
		t.Fatalf("flags = %v", document.Flags)
	}
	if !strings.Contains(string(document.Runner), "timeoutMinutes") {
		t.Fatalf("runner = %s, want the authored object preserved", document.Runner)
	}
	if document.Distribution == nil || document.Distribution.Namespace != "putnami" ||
		!document.Distribution.Channels["latest"].Protected ||
		document.Distribution.Registries["npm"].Mirror.To != "https://registry.npmjs.org" ||
		document.Distribution.Versions.Stable != "public" ||
		!document.Distribution.MemberAttribution || !document.Distribution.MemberSourceTree ||
		!reflect.DeepEqual([]string(document.Distribution.Members[0].Select), []string{"tag:public-lib"}) {
		t.Fatalf("distribution = %#v", document.Distribution)
	}
	if normalized := Normalize(document); !normalized.Distribution.MemberAttribution || !normalized.Distribution.MemberSourceTree {
		t.Fatalf("normalization dropped a distribution opt-in: %#v", normalized.Distribution)
	}
	if len(document.Rules) != 4 || !document.Rules[2].Publish.Disabled || !document.Rules[3].PullRequests {
		t.Fatalf("rules = %#v", document.Rules)
	}
	if !HasProviderSections(document) {
		t.Fatal("a document with distribution and envs declares provider sections")
	}
	if HasProviderSections(mustParse(t, readFixture(t, "valid", "commands-only.json"))) {
		t.Fatal("a commands-only document declares no provider section")
	}
	if !HasProviderSections(mustParse(t, readFixture(t, "valid", "envs-only.json"))) {
		t.Fatal("envs alone is a provider section")
	}
}

// A rule's trigger is exactly one of the three, and Trigger is the single
// derivation validation, explain, and the runner all read.
func TestRuleTriggerIsExactlyOne(t *testing.T) {
	cases := []struct {
		name  string
		rule  Rule
		want  Event
		valid bool
	}{
		{"branches", Rule{Branches: Globs{"main"}}, EventPush, true},
		{"tags", Rule{Tags: Globs{"v*"}}, EventTag, true},
		{"pull requests", Rule{PullRequests: true}, EventPullRequest, true},
		{"none", Rule{}, "", false},
		{"branches and tags", Rule{Branches: Globs{"main"}, Tags: Globs{"v*"}}, "", false},
		{"branches and pull requests", Rule{Branches: Globs{"main"}, PullRequests: true}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.rule.Trigger()
			if ok != tc.valid || got != tc.want {
				t.Fatalf("Trigger() = %q, %t; want %q, %t", got, ok, tc.want, tc.valid)
			}
		})
	}
}

// The three authoring shorthands — a bare command name, a bare glob, a bare
// selector — decode to the same value as their array or object form, and
// canonicalize back to the shorthand so the file a human wrote is the file
// `ci fmt` writes.
func TestAuthoringShorthandsRoundTrip(t *testing.T) {
	blocking := true
	long := Document{Version: Version, Commands: []CommandEntry{{Name: "test", FailOnError: &blocking}},
		Rules: []Rule{{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}}}}
	short, err := Parse([]byte(`{"version":3,"commands":["test"],"rules":[{"branches":"main","publish":["canary"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	longDigest, err := Digest(long)
	if err != nil {
		t.Fatal(err)
	}
	shortDigest, err := Digest(short)
	if err != nil {
		t.Fatal(err)
	}
	if longDigest != shortDigest {
		t.Fatalf("explicit failOnError: true is the default and must not change the digest: %s != %s", longDigest, shortDigest)
	}
	canonical, err := CanonicalBytes(long)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"commands":["test"]`) || !strings.Contains(string(canonical), `"branches":"main"`) {
		t.Fatalf("canonical form lost the authoring shorthand:\n%s", canonical)
	}
	disabled, err := Parse([]byte(`{"version":3,"commands":["test"],"rules":[{"tags":"wip/*","publish":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err = CanonicalBytes(disabled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"publish":false`) {
		t.Fatalf("publish: false is not symmetric:\n%s", canonical)
	}
	if _, err := Parse([]byte(`{"version":3,"commands":["test"],"rules":[{"tags":"wip/*","publish":true}]}`)); err == nil {
		t.Fatal("publish: true accepted; only false opts out")
	}
}

func TestInvalidFixturesFail(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("fixtures", "invalid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 17 {
		t.Fatalf("invalid corpus unexpectedly small: %v", entries)
	}
	for _, path := range entries {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if _, parseErr := Parse(data); parseErr == nil {
				t.Fatal("Parse succeeded, want error")
			}
		})
	}
}

func TestInvalidFixturesReportStableCodes(t *testing.T) {
	for fixture, code := range map[string]string{
		"gate-present.json":              "ci.invalid_json",
		"deploy-on-rule.json":            "ci.invalid_json",
		"protected-channel-in-rule.json": "ci.protected_channel_in_rule",
		"bad-channel-name.json":          "ci.invalid_channel",
		"two-triggers.json":              "ci.rule_trigger",
		"no-trigger.json":                "ci.rule_trigger",
		"bad-selector.json":              "ci.invalid_selector",
		"bad-rollout-steps.json":         "ci.invalid_rollout",
		"namespace-missing.json":         "ci.required",
		"unknown-field.json":             "ci.invalid_json",
		"version-2.json":                 "ci.unknown_version",
		"retain-without-publish.json":    "ci.invalid_retain",
		"retain-go-duration.json":        "ci.invalid_retain",
		"retain-shared-channel.json":     "ci.invalid_retain",
		"baseline-in-publish.json":       "ci.invalid_baseline",
		"baseline-placeholder.json":      "ci.invalid_baseline",
		"baseline-without-publish.json":  "ci.invalid_baseline",
	} {
		_, findings := ParseWithDiagnostics(readFixture(t, "invalid", fixture))
		assertCIDiagnostic(t, findings, code)
	}
}

func TestParseRejectsDuplicateKeysAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"version":3,"version":3,"commands":["test"]}`,
		`{"version":3,"commands":["test"]} {}`,
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatalf("Parse(%s) succeeded", input)
		}
	}
}

func TestValidateRejectsBadReferencesAndBounds(t *testing.T) {
	document := DefaultDocument()
	document.Commands = append(document.Commands, CommandEntry{Name: "lint"}, CommandEntry{Name: "not a command"})
	document.Flags = []string{"--enforce-coverage", "-x"}
	document.Rules = []Rule{
		{Branches: Globs{"refs/heads/main"}},
		{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"pr-{number}"}}},
		{Branches: Globs{"main"}},
		{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}-{x}"}}},
	}
	findings := Validate(document)
	for _, code := range []string{
		"ci.duplicate_task", "ci.invalid_task", "ci.invalid_branch_glob",
		"ci.invalid_channel", "ci.duplicate_rule", "ci.invalid_flag",
	} {
		assertCIDiagnostic(t, findings, code)
	}

	oversized := DefaultDocument()
	for index := range MaxCommands {
		oversized.Commands = append(oversized.Commands, CommandEntry{Name: "task" + strings.Repeat("x", index+1)})
	}
	assertCIDiagnostic(t, Validate(oversized), "ci.too_many_entries")

	for _, empty := range []Document{{Version: Version}, {Version: Version, Commands: []CommandEntry{}}} {
		if findings := Validate(empty); !hasCIDiagnostic(findings, "ci.required") {
			t.Fatalf("empty commands accepted: %v", findings)
		}
	}
}

// The distribution section is the repository's declaration to its provider, so
// every member of it is validated against a closed vocabulary.
func TestValidateDistributionVocabulary(t *testing.T) {
	document := DefaultDocument()
	document.Distribution = &Distribution{
		Namespace:  "Putnami Framework",
		Visibility: "secret",
		Registries: map[string]RegistryPolicy{"NPM registry": {Visibility: "hidden", Mirror: &Mirror{To: " "}}},
		Channels:   map[string]ChannelPolicy{"Latest": {Visibility: "public"}},
		Versions:   &VersionPolicy{Stable: "everyone"},
		Members:    []MemberPolicy{{Select: Selectors{"label:x"}, Visibility: ""}},
	}
	findings := Validate(document)
	for _, code := range []string{
		"ci.invalid_namespace", "ci.invalid_visibility", "ci.required",
		"ci.invalid_channel", "ci.invalid_selector", "ci.invalid_ecosystem",
	} {
		assertCIDiagnostic(t, findings, code)
	}
}

// An environment's constraints, rollout, and workload rules are validated the
// same way whether they are declared on the environment or overridden by a
// workload rule.
func TestValidateEnvironmentConstraintsAndRollout(t *testing.T) {
	document := DefaultDocument()
	document.Envs = map[string]Environment{
		"prod": {
			Channel:     "canary",
			Constraints: map[string]any{"approval": "automatic"},
			Rollout:     &Rollout{Strategy: "canary", Steps: []int{10, 50}, Advance: "soon"},
			Workloads: []WorkloadRule{
				{Select: Selectors{"tag:api"}, Channel: "Latest"},
				{Select: Selectors{}, Constraints: map[string]any{"approval": "manual"}},
			},
		},
	}
	findings := Validate(document)
	for _, code := range []string{
		"ci.invalid_constraint", "ci.invalid_rollout", "ci.invalid_channel", "ci.required",
	} {
		assertCIDiagnostic(t, findings, code)
	}

	tooMany := DefaultDocument()
	environment := Environment{Channel: "canary"}
	for range MaxWorkloads + 1 {
		environment.Workloads = append(environment.Workloads, WorkloadRule{Select: Selectors{"tag:api"}})
	}
	tooMany.Envs = map[string]Environment{"prod": environment}
	assertCIDiagnostic(t, Validate(tooMany), "ci.too_many_entries")

	valid := DefaultDocument()
	valid.Envs = map[string]Environment{"prod": {
		Channel:     "canary",
		Constraints: map[string]any{"approval": "manual"},
		Rollout:     &Rollout{Strategy: "progressive", Steps: []int{10, 100}, Advance: "5m", AbortOn: "readyz"},
	}}
	if findings := Validate(valid); diag.HasErrors(findings) {
		t.Fatalf("valid environment rejected: %v", findings)
	}
}

func TestValidateTaskReferences(t *testing.T) {
	document := DefaultDocument()
	findings := ValidateTaskReferences(document, map[string]string{"lint": "lint", "test": "test"})
	if len(findings) != 1 || findings[0].Code != "ci.unknown_task" || findings[0].Field != "commands[2]" {
		t.Fatalf("findings = %#v", findings)
	}
}

func TestGlobDialect(t *testing.T) {
	cases := []struct {
		pattern, branch string
		want            bool
	}{
		{"main", "main", true},
		{"main", "main/x", false},
		{"release/*", "release/1.2", true},
		{"release/*", "release/1/2", false},
		{"release/**", "release/1/2", true},
		{"**", "any/depth/branch", true},
		{"feature/*-x", "feature/a-x", true},
		{"ts/v*", "ts/v0.3.0", true},
	}
	for _, tc := range cases {
		got, err := MatchBranch(tc.pattern, tc.branch)
		if err != nil || got != tc.want {
			t.Fatalf("MatchBranch(%q, %q) = %v, %v; want %v", tc.pattern, tc.branch, got, err, tc.want)
		}
	}
	for _, pattern := range []string{"", "refs/heads/main", "/main", "main/", "a//b", "a[b]", "a**b", "a?b"} {
		if err := ValidateBranchGlob(pattern); err == nil {
			t.Fatalf("ValidateBranchGlob(%q) accepted", pattern)
		}
	}
	if _, err := MatchBranch("**", "refs/heads/main"); err == nil {
		t.Fatal("provider-prefixed branch accepted")
	}
}

func TestCanonicalDigestAndFormatAreStable(t *testing.T) {
	document := DefaultDocument()
	first, err := Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Digest(DefaultDocument()); again != first {
		t.Fatalf("digest is not deterministic: %s != %s", first, again)
	}
	document.Commands = []CommandEntry{{Name: "test"}, {Name: "build"}, {Name: "lint"}}
	second, err := Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("commands are the order the CLI runs them; reordering must change the digest")
	}
	document.Rules = []Rule{
		{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}},
		{Tags: Globs{"wip/*"}, Publish: Publish{Disabled: true}},
	}
	ordered, _ := Digest(document)
	document.Rules[0], document.Rules[1] = document.Rules[1], document.Rules[0]
	reordered, _ := Digest(document)
	if ordered == reordered {
		t.Fatal("rule order is semantic and must change the digest")
	}
	raw, _ := json.Marshal(document)
	formatted, err := Format(raw)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := IsFormatted(formatted)
	if err != nil || !ok || !bytes.HasSuffix(formatted, []byte("\n")) {
		t.Fatalf("formatted = %t, err=%v", ok, err)
	}
	if !strings.Contains(string(formatted), `"$schema": "https://putnami.dev/schemas/putnami-ci.json"`) {
		t.Fatalf("formatted document lost the schema pin:\n%s", formatted)
	}
}

// The runner member is opaque and preserved, but its key order is not part of
// the intent: two authorings of the same object are one document.
func TestRunnerIsPreservedAndKeyOrderIsNot(t *testing.T) {
	first, err := Parse([]byte(`{"version":3,"commands":["test"],"runner":{"timeoutMinutes":45,"lanes":4}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(`{"version":3,"commands":["test"],"runner":{"lanes":4,"timeoutMinutes":45}}`))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := Digest(first)
	secondDigest, _ := Digest(second)
	if firstDigest != secondDigest {
		t.Fatalf("runner key order changed the digest: %s != %s", firstDigest, secondDigest)
	}
	if !strings.Contains(string(first.Runner), `"timeoutMinutes":45`) {
		t.Fatalf("runner content was not preserved: %s", first.Runner)
	}
	if _, err := Parse([]byte(`{"version":3,"commands":["test"],"runner":[1]}`)); err == nil {
		t.Fatal("a non-object runner was accepted")
	}
}

func TestExplainFirstMatchAndPullRequestOptIn(t *testing.T) {
	document, err := Parse(readFixture(t, "valid", "workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	push, err := Explain(document, ExplainInput{Event: EventPush, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !push.Matched || push.RuleIndex == nil || *push.RuleIndex != 0 ||
		!reflect.DeepEqual(push.Publish, []string{"canary", "staging"}) || push.ImplicitTag {
		t.Fatalf("push main = %#v", push)
	}
	if len(push.Commands) != 5 || push.Commands[0].Name != "lint" ||
		!reflect.DeepEqual(push.Flags, []string{"--enforce-coverage"}) {
		t.Fatalf("push commands = %#v flags = %v", push.Commands, push.Flags)
	}

	// A pull request never selects a rule that did not opt in.
	unmatchedPush, err := Explain(document, ExplainInput{Event: EventPush, Branch: "feature/x"})
	if err != nil {
		t.Fatal(err)
	}
	if unmatchedPush.Matched || len(unmatchedPush.Publish) != 0 || unmatchedPush.ImplicitTag {
		t.Fatalf("unmatched push = %#v; want the commands only", unmatchedPush)
	}
	pull, err := Explain(document, ExplainInput{Event: EventPullRequest, Branch: "feature/x", PullRequestNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	if !pull.Matched || *pull.RuleIndex != 3 || !reflect.DeepEqual(pull.Publish, []string{"pr-42"}) {
		t.Fatalf("pull request = %#v", pull)
	}
	if _, err := Explain(document, ExplainInput{Event: "cron", Branch: "main"}); err == nil {
		t.Fatal("unknown event accepted")
	}
}

// A tag publishes by convention: only a matching tags rule decides otherwise,
// and `publish: false` is how a repository opts a tag line out.
func TestExplainTagPublishesByConvention(t *testing.T) {
	document, err := Parse(readFixture(t, "valid", "workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	implicit, err := Explain(document, ExplainInput{Event: EventTag, Tag: "ts/v0.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	if implicit.Matched || !implicit.ImplicitTag || len(implicit.Publish) != 0 {
		t.Fatalf("unmatched tag = %#v; want the convention", implicit)
	}
	optedOut, err := Explain(document, ExplainInput{Event: EventTag, Tag: "wip/experiment"})
	if err != nil {
		t.Fatal(err)
	}
	if !optedOut.Matched || optedOut.ImplicitTag || len(optedOut.Publish) != 0 {
		t.Fatalf("opted-out tag = %#v; want no publish and no convention", optedOut)
	}
	if _, err := Explain(document, ExplainInput{Event: EventTag}); err == nil {
		t.Fatal("a tag event without a tag name was accepted")
	}
}

// Explain names the environments that follow each channel a rule advances,
// including the ones that follow it only through a workload rule.
func TestExplainListsEnvironmentsPerChannel(t *testing.T) {
	document, err := Parse(readFixture(t, "valid", "workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	push, err := Explain(document, ExplainInput{Event: EventPush, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(push.Environments, map[string][]string{"canary": {"prod", "staging"}}) {
		t.Fatalf("environments = %#v; prod follows canary through a workload rule", push.Environments)
	}
	release, err := Explain(document, ExplainInput{Event: EventPush, Branch: "release/2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(release.Environments) != 0 || !reflect.DeepEqual(release.Publish, []string{"rc"}) {
		t.Fatalf("release push = %#v; no environment follows rc", release)
	}
}

// A rule declares how long the channels it publishes are kept. The fixture is
// the conformance case: `while-open` on the pull-request rule, a whole number
// of days on a branch rule. The lifetime is part of the intent, so it survives
// canonicalization and changes the digest — a field dropped by normalization
// would be invisible in the canonical document and in every digest computed
// from it.
func TestRuleRetainDeclaresAChannelLifetime(t *testing.T) {
	document := mustParse(t, readFixture(t, "valid", "pull-request-lifetime.json"))
	if len(document.Rules) != 2 {
		t.Fatalf("rules = %#v", document.Rules)
	}
	if !document.Rules[0].RetainsWhileOpen() || document.Rules[0].Retain != RetainWhileOpen {
		t.Fatalf("pull-request rule = %#v, want the while-open lifetime", document.Rules[0])
	}
	if _, declared := document.Rules[0].RetainDays(); declared {
		t.Fatal("while-open is not a duration")
	}
	days, declared := document.Rules[1].RetainDays()
	if !declared || days != 30 {
		t.Fatalf("branch rule retain = %q, want 30 days", document.Rules[1].Retain)
	}

	withLifetime, err := Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	stripped := mustParse(t, readFixture(t, "valid", "pull-request-lifetime.json"))
	stripped.Rules[0].Retain = ""
	without, err := Digest(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if withLifetime == without {
		t.Fatal("the declared lifetime is intent; normalization must carry it into the digest")
	}
	shorter := mustParse(t, readFixture(t, "valid", "pull-request-lifetime.json"))
	shorter.Rules[1].Retain = "7d"
	if other, _ := Digest(shorter); other == withLifetime {
		t.Fatal("two different lifetimes produced one digest")
	}
	formatted, err := Format(readFixture(t, "valid", "pull-request-lifetime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), `"retain": "while-open"`) ||
		!strings.Contains(string(formatted), `"retain": "30d"`) {
		t.Fatalf("canonical form lost the lifetime:\n%s", formatted)
	}
}

// The accepted spellings are exactly one keyword and one unit. Anything else —
// a Go duration, a bare number, a lifetime on a rule that publishes nothing —
// is refused with one stable, field-addressable code.
func TestRetainSpellingAndPlacementAreValidated(t *testing.T) {
	cases := []struct {
		name     string
		rule     Rule
		accepted bool
	}{
		{"while-open on a pull request", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Retain: RetainWhileOpen}, true},
		{"while-open on several per-pull-request channels", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}", "preview-{number}"}}, Retain: RetainWhileOpen}, true},
		{"while-open on a shared channel", Rule{PullRequests: true, Publish: Publish{Channels: []string{"preview"}}, Retain: RetainWhileOpen}, false},
		{"while-open on a mixed list", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}", "preview"}}, Retain: RetainWhileOpen}, false},
		{"days on a shared pull-request channel", Rule{PullRequests: true, Publish: Publish{Channels: []string{"preview"}}, Retain: "30d"}, true},
		{"days on a branch", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}, Retain: "1d"}, true},
		{"the maximum", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}, Retain: "365d"}, true},
		{"no lifetime", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}}, true},
		{"a Go duration", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Retain: "720h"}, false},
		{"a bare number", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Retain: "30"}, false},
		{"zero days", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Retain: "0d"}, false},
		{"over the maximum", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Retain: "366d"}, false},
		{"while-open on a branch", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}, Retain: RetainWhileOpen}, false},
		{"while-open on a tag", Rule{Tags: Globs{"v*"}, Publish: Publish{Channels: []string{"rc"}}, Retain: RetainWhileOpen}, false},
		{"without a published channel", Rule{PullRequests: true, Retain: RetainWhileOpen}, false},
		{"on an opted-out publish", Rule{Tags: Globs{"wip/*"}, Publish: Publish{Disabled: true}, Retain: "30d"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := DefaultDocument()
			document.Rules = []Rule{tc.rule}
			findings := Validate(document)
			if diag.HasErrors(findings) == tc.accepted {
				t.Fatalf("Validate(%#v) = %v, accepted = %t", tc.rule, findings, tc.accepted)
			}
			if !tc.accepted {
				assertCIDiagnostic(t, findings, "ci.invalid_retain")
				if field := diag.Errors(findings)[0].Field; field != "rules[0].retain" {
					t.Fatalf("field = %q, want the addressable rules[0].retain", field)
				}
			}
		})
	}
	if _, err := Parse([]byte(`{"version":3,"commands":["test"],"rules":[{"pullRequests":true,"publish":["pr-{number}"],"retain":"while-open"}]}`)); err != nil {
		t.Fatalf("a declared lifetime was rejected by the strict parser: %v", err)
	}
}

// `while-open` names one pull request, so the diagnostic order matters: a rule
// with the wrong trigger is told about the trigger, and a pull-request rule is
// told exactly which of its channels several pull requests would share.
func TestWhileOpenNamesTheOneThingThatIsWrong(t *testing.T) {
	onABranch := DefaultDocument()
	onABranch.Rules = []Rule{{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}, Retain: RetainWhileOpen}}
	triggerFindings := diag.Errors(Validate(onABranch))
	if len(triggerFindings) != 1 || !strings.Contains(triggerFindings[0].Message, "needs a pull request to be open") {
		t.Fatalf("findings = %#v; want the trigger problem alone, not a placeholder problem", triggerFindings)
	}

	mixed := DefaultDocument()
	mixed.Rules = []Rule{{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}", "preview", "nightly"}}, Retain: RetainWhileOpen}}
	sharedFindings := diag.Errors(Validate(mixed))
	if len(sharedFindings) != 1 {
		t.Fatalf("findings = %#v; want one finding for the rule", sharedFindings)
	}
	message := sharedFindings[0].Message
	if !strings.Contains(message, `"preview", "nightly" carries no `+PullRequestNumberPlaceholder) {
		t.Fatalf("message does not name the shared channels in declaration order: %q", message)
	}
	if !strings.Contains(message, `"30d"`) {
		t.Fatalf("message does not offer the way out for a shared channel: %q", message)
	}
	if sharedFindings[0].Field != "rules[0].retain" {
		t.Fatalf("field = %q; the lifetime is what is unsound, not the channel name", sharedFindings[0].Field)
	}
}

// Explain reports the lifetime of the channels the matched rule advances, so
// `ci explain --event pull_request` answers "and when does this channel go
// away" locally, without resolving anything remote.
func TestExplainReportsTheDeclaredLifetime(t *testing.T) {
	document := mustParse(t, readFixture(t, "valid", "pull-request-lifetime.json"))
	pull, err := Explain(document, ExplainInput{Event: EventPullRequest, Branch: "feature/x", PullRequestNumber: 3612})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pull.Publish, []string{"pr-3612"}) || pull.Retain != RetainWhileOpen {
		t.Fatalf("pull request = %#v, want pr-3612 retained while open", pull)
	}
	push, err := Explain(document, ExplainInput{Event: EventPush, Branch: "release/2"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(push.Publish, []string{"rc"}) || push.Retain != "30d" {
		t.Fatalf("push = %#v, want rc retained 30d", push)
	}
	undeclared, err := Explain(mustParse(t, readFixture(t, "valid", "workspace.json")),
		ExplainInput{Event: EventPullRequest, Branch: "feature/x", PullRequestNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if undeclared.Retain != "" {
		t.Fatalf("retain = %q; a rule that declares no lifetime reports none", undeclared.Retain)
	}
}

func TestSchemaIsEmbeddedAndClosed(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("root is not closed: %#v", schema)
	}
	properties := schema["properties"].(map[string]any)
	for _, removed := range []string{"env", "runners", "jobs", "gate", "branches"} {
		if _, ok := properties[removed]; ok {
			t.Fatalf("version 3 schema still declares %q", removed)
		}
	}
	for _, required := range []string{"commands", "flags", "runner", "distribution", "rules", "envs"} {
		if _, ok := properties[required]; !ok {
			t.Fatalf("version 3 schema does not declare %q", required)
		}
	}
}

// TestSchemaVersionMatchesContractVersion pins the schema's version const to the
// Go Version const. They are two statements of one fact — which contract
// semantics a document selects — and an author editing one without the other
// would ship a schema that accepts documents the parser rejects, or the reverse.
func TestSchemaVersionMatchesContractVersion(t *testing.T) {
	var schema struct {
		Properties struct {
			Version struct {
				Const *float64 `json:"const"`
			} `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	pinned := schema.Properties.Version.Const
	if pinned == nil {
		t.Fatal("schema does not pin properties.version.const; the wire version would be unenforced")
	}
	if *pinned != float64(Version) {
		t.Fatalf("schema pins version %v, Go Version is %d", *pinned, Version)
	}
}

func mustParse(t *testing.T, data []byte) Document {
	t.Helper()
	document, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return document
}

func readFixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("fixtures", kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hasCIDiagnostic(findings []diag.Diagnostic, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

// A rule that publishes into a channel a pull request creates may name the
// channel its FIRST publish measures against. The baseline is read and never
// advanced, so it is part of the authored intent exactly like `retain`: it
// survives canonicalization and changes the digest.
func TestRuleBaselineNamesTheChannelAPublishMeasuresAgainst(t *testing.T) {
	document := mustParse(t, readFixture(t, "valid", "pull-request-baseline.json"))
	if len(document.Rules) != 2 {
		t.Fatalf("rules = %#v", document.Rules)
	}
	if document.Rules[0].Baseline != "canary" {
		t.Fatalf("pull-request rule = %#v, want canary as its baseline", document.Rules[0])
	}
	if document.Rules[1].Baseline != "" {
		t.Fatalf("branch rule = %#v; a rule that declares no baseline reports none", document.Rules[1])
	}
	// The same channel is advanced by the branch rule and read by the
	// pull-request rule. That is the shape the feature exists for, and it is
	// only unsound WITHIN one rule.
	if !reflect.DeepEqual(document.Rules[1].Publish.Channels, []string{"canary"}) {
		t.Fatalf("branch rule publish = %#v", document.Rules[1].Publish)
	}

	withBaseline, err := Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	stripped := mustParse(t, readFixture(t, "valid", "pull-request-baseline.json"))
	stripped.Rules[0].Baseline = ""
	without, err := Digest(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if withBaseline == without {
		t.Fatal("the named baseline is intent; normalization must carry it into the digest")
	}
	formatted, err := Format(readFixture(t, "valid", "pull-request-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(formatted), `"baseline": "canary"`) {
		t.Fatalf("canonical form lost the baseline:\n%s", formatted)
	}
}

// The baseline is refused where it could never be read soundly: on a rule that
// publishes nothing, with a `{number}` every pull request would render
// differently, and on a channel the same rule advances. A protected channel is
// accepted, because protection forbids advancing a channel, not reading it.
func TestBaselinePlacementAndSpellingAreValidated(t *testing.T) {
	cases := []struct {
		name     string
		rule     Rule
		accepted bool
	}{
		{"a pull-request rule measuring against a branch channel", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Baseline: "canary"}, true},
		{"a branch rule measuring against another channel", Rule{Branches: Globs{"release/*"}, Publish: Publish{Channels: []string{"rc"}}, Baseline: "canary"}, true},
		{"no baseline", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}}, true},
		{"a channel this rule advances", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"canary"}}, Baseline: "canary"}, false},
		{"one of several channels this rule advances", Rule{Branches: Globs{"main"}, Publish: Publish{Channels: []string{"next", "canary"}}, Baseline: "canary"}, false},
		{"a placeholder", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Baseline: "preview-{number}"}, false},
		{"a non-portable name", Rule{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Baseline: "Canary/Latest"}, false},
		{"without a published channel", Rule{PullRequests: true, Baseline: "canary"}, false},
		{"on an opted-out publish", Rule{Tags: Globs{"wip/*"}, Publish: Publish{Disabled: true}, Baseline: "canary"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document := DefaultDocument()
			document.Rules = []Rule{tc.rule}
			findings := Validate(document)
			if diag.HasErrors(findings) == tc.accepted {
				t.Fatalf("Validate(%#v) = %v, accepted = %t", tc.rule, findings, tc.accepted)
			}
			if !tc.accepted {
				assertCIDiagnostic(t, findings, "ci.invalid_baseline")
				if field := diag.Errors(findings)[0].Field; field != "rules[0].baseline" {
					t.Fatalf("field = %q, want the addressable rules[0].baseline", field)
				}
			}
		})
	}

	// A protected channel may not be ADVANCED by a rule, and is still a valid
	// thing to read: `latest` is exactly the head a first publish wants to
	// inherit from.
	protectedBaseline := DefaultDocument()
	protectedBaseline.Distribution = &Distribution{
		Namespace: "putnami",
		Channels:  map[string]ChannelPolicy{"latest": {Visibility: "public", Protected: true}},
	}
	protectedBaseline.Rules = []Rule{{PullRequests: true, Publish: Publish{Channels: []string{"pr-{number}"}}, Baseline: "latest"}}
	if findings := Validate(protectedBaseline); diag.HasErrors(findings) {
		t.Fatalf("a protected channel was refused as a baseline: %v", findings)
	}

	if _, err := Parse([]byte(`{"version":3,"commands":["test"],"rules":[{"pullRequests":true,"publish":["pr-{number}"],"baseline":"canary"}]}`)); err != nil {
		t.Fatalf("a declared baseline was rejected by the strict parser: %v", err)
	}
}

// Explain reports the baseline with the rule it matched, so the runner that
// renders the command line reads `--channel` and `--baseline-channel` from the
// same report, and a human asking `ci explain` is told which head the first
// publish of a pull request measures against.
func TestExplainReportsTheBaselineChannel(t *testing.T) {
	document := mustParse(t, readFixture(t, "valid", "pull-request-baseline.json"))
	pull, err := Explain(document, ExplainInput{Event: EventPullRequest, Branch: "feature/x", PullRequestNumber: 3677})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pull.Publish, []string{"pr-3677"}) || pull.Baseline != "canary" {
		t.Fatalf("pull request = %#v, want pr-3677 measured against canary", pull)
	}
	push, err := Explain(document, ExplainInput{Event: EventPush, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(push.Publish, []string{"canary"}) || push.Baseline != "" {
		t.Fatalf("push = %#v; a rule that declares no baseline reports none", push)
	}
	// A rule that publishes nothing reports no baseline either: there is no
	// publication to measure.
	quiet := mustParse(t, readFixture(t, "valid", "workspace.json"))
	report, err := Explain(quiet, ExplainInput{Event: EventPullRequest, Branch: "feature/x", PullRequestNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if report.Baseline != "" {
		t.Fatalf("baseline = %q, want none declared", report.Baseline)
	}
}
