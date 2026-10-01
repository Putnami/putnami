package ci

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// `ci init` seeds the commands the workspace's own extensions declare, in the
// conventional verification order rather than the discovery order, so the file
// it writes is the invocation a contributor already runs.
func TestCIInitSeedsCommands(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "commands-not-gate", "init-seeds-commands")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)

	output, err := captureCICommand(func() error { return CIInit(root, cfg, false, "jsonl") })
	if err != nil {
		t.Fatalf("CIInit: %v", err)
	}
	assertCIReportAction(t, output, "init")

	document, err := ciproto.Parse(readCIDocument(t, root))
	if err != nil {
		t.Fatalf("seeded document is invalid: %v", err)
	}
	if got := commandNames(document); !reflect.DeepEqual(got, []string{"lint", "test", "build"}) {
		t.Fatalf("seeded commands = %v; want the discovered jobs in verification order", got)
	}
	for _, command := range document.Commands {
		if !command.Blocking() {
			t.Fatalf("seeded command %q is advisory; the scaffolded gate blocks", command.Name)
		}
	}
	if document.Distribution != nil || len(document.Envs) != 0 {
		t.Fatalf("`ci init` requested provider authority: %#v %#v", document.Distribution, document.Envs)
	}
	if err := CIInit(root, cfg, false, "jsonl"); !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIInit over an existing document = %v, want a refusal", err)
	}
}

// distribution and envs describe channels, environments, visibility, and
// grants. A workspace with no release-set provider can serve none of them, so
// validate refuses the section instead of accepting intent nothing executes.
func TestCIValidateRefusesProviderSectionsWithoutProvider(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "provider-sections-need-a-provider", "validate-refuses-without-provider")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)

	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"distribution": {"namespace": "fixture", "channels": {"canary": {}}}
	}`)
	output, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.provider_required", "distribution")

	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"envs": {"prod": {"channel": "canary"}}
	}`)
	output, err = captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.provider_required", "envs")

	// The same workspace validates the same commands once the provider
	// sections are gone: the refusal is about authority, not about the file.
	writeCIDocument(t, root, `{"version": 3, "commands": ["lint", "test"]}`)
	if _, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") }); err != nil {
		t.Fatalf("CIValidate without provider sections: %v", err)
	}

	// And the same sections validate in a workspace whose extension serves the
	// reserved release-set command, so the refusal names a missing provider
	// rather than a forbidden section.
	served := newCIProviderWorkspace(t)
	servedCfg := wsproto.Load(served)
	workspace.InvalidateLoadCache(served)
	writeCIDocument(t, served, `{
		"version": 3,
		"commands": ["lint", "test"],
		"distribution": {"namespace": "fixture", "channels": {"canary": {}}},
		"envs": {"prod": {"channel": "canary"}}
	}`)
	if _, err := captureCICommand(func() error { return CIValidate(served, servedCfg, "jsonl") }); err != nil {
		t.Fatalf("CIValidate with a release-set provider: %v", err)
	}
}

// A tag with no matching rule publishes by convention. `ci explain` says so
// rather than reporting "nothing happens", which is what the v2 branch-rule
// model would have said.
func TestCIExplainTagIsImplicit(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "rules-and-triggers", "tag-publishes-by-convention")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"tags": "wip/*", "publish": false}, {"branches": "main", "publish": ["canary"]}]
	}`)

	explanation := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventTag, Tag: "ts/v0.3.0"})
	if explanation.Matched || !explanation.ImplicitTag || len(explanation.Publish) != 0 {
		t.Fatalf("explanation = %#v; want the tag convention", explanation)
	}
	optedOut := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventTag, Tag: "wip/spike"})
	if !optedOut.Matched || optedOut.ImplicitTag || len(optedOut.Publish) != 0 {
		t.Fatalf("explanation = %#v; want the rule's opt-out", optedOut)
	}
	human, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventTag, Tag: "ts/v0.3.0"}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(human, "publishes by convention") {
		t.Fatalf("human explanation does not name the convention:\n%s", human)
	}
}

// A protected channel is refused in any rule, so a branch can never move the
// channel a human owns.
func TestCIValidateRefusesAProtectedChannelInARule(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "rules-and-triggers", "protected-channel-is-refused")
	root := newCIProviderWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"distribution": {"namespace": "fixture", "channels": {"latest": {"protected": true}}},
		"rules": [{"branches": "main", "publish": ["latest"]}]
	}`)
	output, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.protected_channel_in_rule", "rules[0].publish[0]")
}

// Explain names, for each channel a rule advances, the environments that
// follow it — including the ones that follow it only through a workload rule.
func TestCIExplainListsEnvironmentsPerChannel(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "environments-follow-channels", "explain-lists-followers")
	root := newCIProviderWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"branches": "main", "publish": ["canary"]}],
		"envs": {
			"staging": {"channel": "canary"},
			"prod": {"channel": "latest", "workloads": [{"select": "tag:api", "channel": "canary"}]},
			"isolated": {"channel": "latest"}
		}
	}`)

	explanation := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "main"})
	if !reflect.DeepEqual(explanation.Environments, map[string][]string{"canary": {"prod", "staging"}}) {
		t.Fatalf("environments = %#v; prod follows canary through a workload rule", explanation.Environments)
	}
	human, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "main"}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(human, "channel=canary followed by prod, staging") {
		t.Fatalf("human explanation does not list the followers:\n%s", human)
	}
	if !strings.Contains(human, "putnami lint,test --impacted") {
		t.Fatalf("human explanation does not name the invocation:\n%s", human)
	}
}

func TestCICommandsRoundTripLocalContract(t *testing.T) {
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	graph, graphErr := loadCIGraph(root, cfg)
	if graphErr != nil {
		t.Fatalf("loadCIGraph: %v", graphErr)
	}
	if graph.provider != nil {
		t.Fatalf("fixture workspace resolved a release-set provider: %#v", graph.provider)
	}
	discovery, discoveryErr := extension.DiscoverExtensionsDetailed(root, cfg, []string{"ext"})
	if discoveryErr != nil || len(discovery.Extensions) == 0 {
		t.Fatalf("discovery extensions=%#v skipped=%#v err=%v config=%v", discovery.Extensions, discovery.Skipped, discoveryErr, cfg.Extensions.Names())
	}
	for _, command := range []string{"lint", "test", "build"} {
		if _, exists := graph.known[command]; !exists {
			t.Fatalf("known commands = %#v, missing %s", graph.known, command)
		}
	}

	output, err := captureCICommand(func() error { return CIInit(root, cfg, false, "jsonl") })
	if err != nil {
		t.Fatalf("CIInit: %v", err)
	}
	assertCIReportAction(t, output, "init")

	output, err = captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if err != nil {
		t.Fatalf("CIValidate: %v", err)
	}
	assertCIReportAction(t, output, "validate")

	path := filepath.Join(root, ciproto.Filename)
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := os.WriteFile(path, append(data, ' ', '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = captureCICommand(func() error { return CIFormat(root, cfg, true, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIFormat(check) error = %v", err)
	}
	if _, err := captureCICommand(func() error { return CIFormat(root, cfg, false, "jsonl") }); err != nil {
		t.Fatalf("CIFormat(apply): %v", err)
	}
	formatted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, formatErr := ciproto.IsFormatted(formatted); formatErr != nil || !ok {
		t.Fatalf("IsFormatted = %t, %v", ok, formatErr)
	}

	explanation := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "main", PullRequestNumber: 7})
	if explanation.Matched || len(explanation.Publish) != 0 ||
		!reflect.DeepEqual(commandNames(ciproto.Document{Commands: explanation.Commands}), []string{"lint", "test", "build"}) {
		t.Fatalf("explanation = %#v; want the commands only, no rule", explanation)
	}
}

// A rule that publishes a pull-request channel also declares when that channel
// goes away, and `ci explain --event pull_request` reports it — in the
// structured report and in the human form — so a reader learns the lifetime
// from the same command that names the channel.
func TestCIExplainReportsTheDeclaredChannelLifetime(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "channel-lifetime-is-declared", "explain-reports-the-declared-lifetime")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [
			{"pullRequests": true, "publish": ["pr-{number}"], "retain": "while-open"},
			{"branches": "release/*", "publish": ["rc"], "retain": "30d"}
		]
	}`)

	pull := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3612})
	if !reflect.DeepEqual(pull.Publish, []string{"pr-3612"}) || pull.Retain != ciproto.RetainWhileOpen {
		t.Fatalf("explanation = %#v; want pr-3612 retained while the pull request is open", pull)
	}
	human, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3612}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(human, "Channel lifetime: while-open") ||
		!strings.Contains(human, "retract the channel when the pull request closes or merges") {
		t.Fatalf("human explanation does not report the lifetime:\n%s", human)
	}

	push := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "release/2"})
	if !reflect.DeepEqual(push.Publish, []string{"rc"}) || push.Retain != "30d" {
		t.Fatalf("explanation = %#v; want rc retained 30d", push)
	}
	humanPush, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "release/2"}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(humanPush, "Channel lifetime: 30d") || !strings.Contains(humanPush, "expire the channel 30 days after its last move") {
		t.Fatalf("human explanation does not report the duration:\n%s", humanPush)
	}

	// The sentence around the authored value reads for one day too.
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"branches": "release/*", "publish": ["rc"], "retain": "1d"}]
	}`)
	humanSingle, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "release/2"}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(humanSingle, "expire the channel 1 day after its last move") {
		t.Fatalf("human explanation does not spell a one-day lifetime:\n%s", humanSingle)
	}

	// A rule that declares no lifetime is told so, because a channel nothing
	// ever retracts is exactly the gap the member closes.
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "publish": ["pr-{number}"]}]
	}`)
	undeclared := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3612})
	if undeclared.Retain != "" {
		t.Fatalf("retain = %q, want none declared", undeclared.Retain)
	}
	humanUndeclared, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3612}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(humanUndeclared, "Channel lifetime: none declared") {
		t.Fatalf("human explanation hides an undeclared lifetime:\n%s", humanUndeclared)
	}
}

// A lifetime is refused where it could never apply: on a rule that publishes no
// channel, and, for the pull-request keyword, on a trigger where nothing
// closes. The diagnostic addresses the exact member.
func TestCIValidateRefusesAnInapplicableChannelLifetime(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "channel-lifetime-is-declared", "an-inapplicable-lifetime-is-refused")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)

	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "retain": "while-open"}]
	}`)
	output, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.invalid_retain", "rules[0].retain")

	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"branches": "main", "publish": ["canary"], "retain": "while-open"}]
	}`)
	output, err = captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.invalid_retain", "rules[0].retain")

	// A channel several open pull requests share has no single pull request to
	// close, so `while-open` cannot name when it is retracted.
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "publish": ["pr-{number}", "preview"], "retain": "while-open"}]
	}`)
	output, err = captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.invalid_retain", "rules[0].retain")

	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "publish": ["pr-{number}"], "retain": "720h"}]
	}`)
	output, err = captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
	if !errors.Is(err, cmderr.ErrInvalidConfig) {
		t.Fatalf("CIValidate = %v, want a refusal", err)
	}
	assertCIReportDiagnostic(t, output, "ci.invalid_retain", "rules[0].retain")

	// The same rule with the accepted spelling validates, so the refusal is
	// about the value and not about the member.
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "publish": ["pr-{number}"], "retain": "30d"}]
	}`)
	if _, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") }); err != nil {
		t.Fatalf("CIValidate with an accepted lifetime: %v", err)
	}
}

// A rule may name the channel its FIRST publish measures against without
// advancing it, and `ci explain` reports it beside the channels the rule
// advances — which is what lets a runner render `--channel` and
// `--baseline-channel` from one report.
func TestCIExplainReportsTheBaselineChannel(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "rules-and-triggers", "explain-reports-the-baseline-channel")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [
			{"pullRequests": true, "publish": ["pr-{number}"], "baseline": "canary"},
			{"branches": "main", "publish": ["canary"]}
		]
	}`)

	pull := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3677})
	if !reflect.DeepEqual(pull.Publish, []string{"pr-3677"}) || pull.Baseline != "canary" {
		t.Fatalf("explanation = %#v; want pr-3677 measured against canary", pull)
	}
	human, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPullRequest, Branch: "feature/x", PullRequestNumber: 3677}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(human, "Impact baseline: canary") || !strings.Contains(human, "read and never advanced") {
		t.Fatalf("human explanation does not report the baseline:\n%s", human)
	}

	// The branch rule advances the same channel and names no baseline of its
	// own; a rule that declares none is told so.
	push := explainCI(t, root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "main"})
	if !reflect.DeepEqual(push.Publish, []string{"canary"}) || push.Baseline != "" {
		t.Fatalf("explanation = %#v; a rule that declares no baseline reports none", push)
	}
	humanPush, err := iox.CaptureStdout(func() error {
		return CIExplain(root, cfg, ciproto.ExplainInput{Event: ciproto.EventPush, Branch: "main"}, "")
	})
	if err != nil {
		t.Fatalf("CIExplain(human): %v", err)
	}
	if !strings.Contains(humanPush, "Impact baseline: none declared") {
		t.Fatalf("human explanation hides an undeclared baseline:\n%s", humanPush)
	}
}

// A baseline is refused where it could never be read: on a rule that publishes
// nothing, with a placeholder no single head could render, and on a channel the
// same rule advances. The diagnostic addresses the exact member.
func TestCIValidateRefusesAnUnreadableBaselineChannel(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "rules-and-triggers", "an-unreadable-baseline-is-refused")
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)

	for _, document := range []string{
		`{"version":3,"commands":["lint","test"],"rules":[{"pullRequests":true,"baseline":"canary"}]}`,
		`{"version":3,"commands":["lint","test"],"rules":[{"pullRequests":true,"publish":["pr-{number}"],"baseline":"preview-{number}"}]}`,
		`{"version":3,"commands":["lint","test"],"rules":[{"branches":"main","publish":["canary"],"baseline":"canary"}]}`,
	} {
		writeCIDocument(t, root, document)
		output, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") })
		if !errors.Is(err, cmderr.ErrInvalidConfig) {
			t.Fatalf("CIValidate(%s) = %v, want a refusal", document, err)
		}
		assertCIReportDiagnostic(t, output, "ci.invalid_baseline", "rules[0].baseline")
	}

	// The same rule measuring against another channel validates, so the refusal
	// is about the value and not about the member.
	writeCIDocument(t, root, `{
		"version": 3,
		"commands": ["lint", "test"],
		"rules": [{"pullRequests": true, "publish": ["pr-{number}"], "baseline": "canary"}]
	}`)
	if _, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") }); err != nil {
		t.Fatalf("CIValidate with a readable baseline: %v", err)
	}
}

// A missing document is a NotFound with an actionable next command, not a
// crash and not a silent success.
func TestCIValidateReportsAnAbsentDocument(t *testing.T) {
	root := newCICommandWorkspace(t)
	cfg := wsproto.Load(root)
	workspace.InvalidateLoadCache(root)
	if _, err := captureCICommand(func() error { return CIValidate(root, cfg, "jsonl") }); !errors.Is(err, cmderr.ErrNotFound) {
		t.Fatalf("CIValidate without a document = %v, want NotFound", err)
	}
}

// newCICommandWorkspace builds a workspace whose only extension declares the
// three generic jobs and no release-set provider.
func newCICommandWorkspace(t *testing.T) string {
	t.Helper()
	return newCIWorkspace(t, false)
}

// newCIProviderWorkspace adds one extension that serves the reserved
// release-set command, which is what makes distribution and envs authorable.
func newCIProviderWorkspace(t *testing.T) string {
	t.Helper()
	return newCIWorkspace(t, true)
}

func newCIWorkspace(t *testing.T, withProvider bool) string {
	t.Helper()
	root := t.TempDir()
	extensionDir := filepath.Join(root, "ext")
	if err := os.MkdirAll(extensionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerCommand := ""
	if withProvider {
		providerCommand = `"` + distribution.ProviderCommandName + `":{"run":[{"id":"release-set","task":"release-set-exec"}]},`
	}
	providerTask := ""
	if withProvider {
		providerTask = `"release-set-exec":{"kind":"command","command":"echo","args":["release-set"]},`
	}
	manifest := `{
		"name":"@putnami/test-ci",
		"cliContract":4,
		"commands":{
			` + providerCommand + `
			"lint":{"run":[{"id":"lint","task":"lint-exec"}]},
			"test":{"run":[{"id":"test","task":"test-exec"}]},
			"build":{"run":[{"id":"build","task":"build-exec"}]}
		},
		"tasks":{
			` + providerTask + `
			"lint-exec":{"kind":"command","command":"echo","args":["lint"]},
			"test-exec":{"kind":"command","command":"echo","args":["test"]},
			"build-exec":{"kind":"command","command":"echo","args":["build"]}
		}
	}`
	if err := os.WriteFile(filepath.Join(extensionDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionDir, "putnami.json"), []byte(`{"name":"@putnami/test-ci"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceConfig := `{
		"name":"ci-test",
		"version":"0.1.0",
		"includes":["ext"],
		"extensions":["/ext"],
		"options":{}
	}`
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(workspaceConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeCIDocument(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCIDocument(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ciproto.Filename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func commandNames(document ciproto.Document) []string {
	names := make([]string, 0, len(document.Commands))
	for _, command := range document.Commands {
		names = append(names, command.Name)
	}
	return names
}

func explainCI(t *testing.T, root string, cfg *wsproto.Config, input ciproto.ExplainInput) ciproto.Explanation {
	t.Helper()
	output, err := captureCICommand(func() error { return CIExplain(root, cfg, input, "jsonl") })
	if err != nil {
		t.Fatalf("CIExplain: %v", err)
	}
	report := decodeCIReport(t, output)
	if report.Explanation == nil {
		t.Fatalf("report carries no explanation: %s", output)
	}
	return *report.Explanation
}

func captureCICommand(run func() error) (string, error) {
	return iox.CaptureStdout(run)
}

func decodeCIReport(t *testing.T, output string) CICommandReport {
	t.Helper()
	var report CICommandReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, output)
	}
	return report
}

func assertCIReportAction(t *testing.T, output, want string) {
	t.Helper()
	report := decodeCIReport(t, output)
	if report.Action != want || !report.Valid {
		t.Fatalf("report = %#v", report)
	}
}

func assertCIReportDiagnostic(t *testing.T, output, code, field string) {
	t.Helper()
	report := decodeCIReport(t, output)
	for _, finding := range report.Diagnostics {
		if finding.Code == code && finding.Field == field {
			return
		}
	}
	t.Fatalf("missing diagnostic %q on %q in %#v", code, field, report.Diagnostics)
}
