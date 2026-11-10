package deliverycli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const testAppID int64 = 4203649

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

var testPullRequestRule = map[string]any{"type": "pull_request", "parameters": map[string]any{
	"required_approving_review_count": 0, "allowed_merge_methods": []string{"squash"}, "dismiss_stale_reviews_on_push": false,
}}

func testChecksRule(names ...string) map[string]any {
	checks := make([]map[string]any, 0, len(names))
	for _, name := range names {
		checks = append(checks, map[string]any{"context": name, "integration_id": testAppID})
	}
	return map[string]any{"type": "required_status_checks", "parameters": map[string]any{
		"strict_required_status_checks_policy": false, "required_status_checks": checks,
	}}
}

func testActor(actorType string, id int64, mode string) map[string]any {
	return map[string]any{"actor_id": id, "actor_type": actorType, "bypass_mode": mode}
}

// mainRuleset is Putnami Cloud's main ruleset before the split: every rule
// in one ruleset, organization admins as bypass actors.
func mainRuleset(t *testing.T, extraActors ...map[string]any) *githubRuleset {
	t.Helper()
	actors := []json.RawMessage{rawJSON(t, testActor("OrganizationAdmin", 1, "always"))}
	for _, actor := range extraActors {
		actors = append(actors, rawJSON(t, actor))
	}
	return &githubRuleset{
		ID: 100, Name: "main", Target: rawJSON(t, "branch"), Enforcement: rawJSON(t, "active"),
		Conditions:   rawJSON(t, map[string]any{"ref_name": map[string]any{"include": []string{"~DEFAULT_BRANCH"}, "exclude": []string{}}}),
		BypassActors: actors,
		Rules: []json.RawMessage{
			rawJSON(t, map[string]any{"type": "deletion"}), rawJSON(t, map[string]any{"type": "non_fast_forward"}),
			rawJSON(t, map[string]any{"type": "required_signatures"}), rawJSON(t, testPullRequestRule),
			rawJSON(t, testChecksRule("Putnami CI", "Putnami Verdict")),
		},
	}
}

func splitRuleset(t *testing.T, id int64, mode string) *githubRuleset {
	t.Helper()
	return &githubRuleset{
		ID: id, Name: "main: pull requests", Target: rawJSON(t, "branch"), Enforcement: rawJSON(t, "active"),
		BypassActors: []json.RawMessage{rawJSON(t, testActor("Integration", testAppID, mode))},
		Rules:        []json.RawMessage{rawJSON(t, testPullRequestRule)},
	}
}

// branchRulesOf is what GitHub answers for the branch: every rule of every
// ruleset, with its ruleset.
func branchRulesOf(rulesets ...*githubRuleset) []githubBranchRule {
	var rules []githubBranchRule
	for _, ruleset := range rulesets {
		for _, raw := range ruleset.Rules {
			var rule githubTypedRule
			_ = json.Unmarshal(raw, &rule)
			rules = append(rules, githubBranchRule{Type: rule.Type, RulesetID: ruleset.ID, RulesetSourceType: "Repository", Parameters: rule.Parameters})
		}
	}
	return rules
}

func byID(rulesets ...*githubRuleset) map[int64]*githubRuleset {
	out := map[int64]*githubRuleset{}
	for _, ruleset := range rulesets {
		out[ruleset.ID] = ruleset
	}
	return out
}

func typesOf(rules []json.RawMessage) string {
	types := make([]string, 0, len(rules))
	for _, raw := range rules {
		types = append(types, ruleTypeOf(raw))
	}
	return strings.Join(types, ",")
}

func TestPlanSplitsTheMainRuleset(t *testing.T) {
	main := mainRuleset(t)
	plan, err := planQueueRules("main", branchRulesOf(main), byID(main), []string{"main"}, testAppID)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Ready() || plan.Source != main || plan.AppRemoved {
		t.Fatalf("plan = %+v, want a split of ruleset 100 that removes no App", plan)
	}
	if plan.Create == nil || plan.Create.Name != "main: pull requests" || typesOf(plan.Create.Rules) != "pull_request" {
		t.Fatalf("create = %+v, want the pull_request rule alone", plan.Create)
	}
	if got := canonicalJSON(plan.Create.BypassActors); got != `[{"actor_id":4203649,"actor_type":"Integration","bypass_mode":"always"}]` {
		t.Fatalf("create bypass actors = %s", got)
	}
	if canonicalJSON(plan.Create.Conditions) != canonicalJSON(main.Conditions) || string(plan.Create.Enforcement) != `"active"` {
		t.Fatalf("create does not keep the target, enforcement and conditions: %+v", plan.Create)
	}
	if typesOf(plan.Update.Rules) != "deletion,non_fast_forward,required_signatures,required_status_checks" {
		t.Fatalf("update rules = %s", typesOf(plan.Update.Rules))
	}
	if canonicalJSON(plan.Update.BypassActors) != canonicalJSON(main.BypassActors) {
		t.Fatalf("update changed the bypass actors: %s", canonicalJSON(plan.Update.BypassActors))
	}
}

func TestPlanRemovesTheAppFromTheRulesetItSplits(t *testing.T) {
	main := mainRuleset(t, testActor("Integration", testAppID, "always"))
	plan, err := planQueueRules("main", branchRulesOf(main), byID(main), nil, testAppID)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.AppRemoved || len(plan.Update.BypassActors) != 1 || strings.Contains(string(plan.Update.BypassActors[0]), "Integration") {
		t.Fatalf("plan = %+v, want the App removed from ruleset 100", plan)
	}
}

func TestPlanReadyWhenTheRulesAreSplit(t *testing.T) {
	for _, mode := range []string{"always", "exempt"} {
		main := mainRuleset(t)
		main.Rules = append(main.Rules[:3], main.Rules[4:]...)
		split := splitRuleset(t, 200, mode)
		plan, err := planQueueRules("main", branchRulesOf(main, split), byID(main, split), nil, testAppID)
		if err != nil || !plan.Ready() || plan.Split != split {
			t.Fatalf("mode %s: plan = %+v, err = %v, want ready", mode, plan, err)
		}
	}
}

func TestPlanResumesAfterTheCreate(t *testing.T) {
	main, split := mainRuleset(t), splitRuleset(t, 200, "always")
	plan, err := planQueueRules("main", branchRulesOf(main, split), byID(main, split), []string{"main", "main: pull requests"}, testAppID)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Create != nil || plan.Update == nil || plan.Split != split {
		t.Fatalf("plan = %+v, want only the update of ruleset 100", plan)
	}
}

func TestPlanRefusesShapesItDoesNotChange(t *testing.T) {
	differentRule := splitRuleset(t, 200, "always")
	differentRule.Rules = []json.RawMessage{rawJSON(t, map[string]any{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 1}})}
	appOnly := splitRuleset(t, 200, "pull_request")
	other := &githubRuleset{ID: 300, Name: "tags", BypassActors: []json.RawMessage{rawJSON(t, testActor("Integration", testAppID, "always"))},
		Rules: []json.RawMessage{rawJSON(t, map[string]any{"type": "creation"})}}
	second := mainRuleset(t)
	second.ID, second.Name = 101, "main again"
	noPullRequest := mainRuleset(t)
	noPullRequest.Rules = noPullRequest.Rules[:3]
	foreign := mainRuleset(t)
	foreign.Rules[4] = rawJSON(t, testChecksRule("Putnami CI", "Contributor CI / gate", "Putnami Verdict"))
	mergeQueue := mainRuleset(t)
	mergeQueue.Rules = append(mergeQueue.Rules, rawJSON(t, map[string]any{"type": "merge_queue"}))

	for _, tc := range []struct {
		name     string
		rules    []githubBranchRule
		rulesets map[int64]*githubRuleset
		names    []string
		want     string
	}{
		{name: "no rule", want: "has no rule"},
		{name: "no pull_request rule", rules: branchRulesOf(noPullRequest), rulesets: byID(noPullRequest), want: "no ruleset holds the pull_request rule"},
		{name: "a check that never reports", rules: branchRulesOf(foreign), rulesets: byID(foreign), want: "Contributor CI / gate"},
		{name: "GitHub's merge queue", rules: branchRulesOf(mergeQueue), rulesets: byID(mergeQueue), want: "GitHub's own merge queue"},
		{name: "an organization ruleset", rules: []githubBranchRule{{Type: "pull_request", RulesetID: 9, RulesetSourceType: "Organization"}}, want: "organization"},
		{name: "the split rule differs", rules: branchRulesOf(mainRuleset(t), differentRule), rulesets: byID(mainRuleset(t), differentRule), want: "different pull_request rules"},
		{name: "the pull_request ruleset lacks the App", rules: branchRulesOf(appOnly), rulesets: byID(appOnly), want: "not its bypass actor"},
		{name: "the App bypasses another ruleset", rules: branchRulesOf(mainRuleset(t), other), rulesets: byID(mainRuleset(t), other), want: `bypasses ruleset "tags"`},
		{name: "two rulesets hold the rule", rules: branchRulesOf(mainRuleset(t), second), rulesets: byID(mainRuleset(t), second), want: "several rulesets"},
		{name: "the new name is taken", rules: branchRulesOf(mainRuleset(t)), rulesets: byID(mainRuleset(t)), names: []string{"main: pull requests"}, want: "exists"},
		{name: "a ruleset was not read", rules: branchRulesOf(mainRuleset(t)), rulesets: map[int64]*githubRuleset{}, want: "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planQueueRules("main", tc.rules, tc.rulesets, tc.names, testAppID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestVerifySplitRules(t *testing.T) {
	main := mainRuleset(t)
	before := branchRulesOf(main)
	plan, err := planQueueRules("main", before, byID(main), nil, testAppID)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	updated := &githubRuleset{ID: 100, Rules: plan.Update.Rules}
	created := &githubRuleset{ID: 200, Rules: plan.Create.Rules}
	if err := verifySplitRules(before, branchRulesOf(updated, created)); err != nil {
		t.Fatalf("a clean split: %v", err)
	}
	// A resumed run starts with the pull_request rule in both rulesets.
	if err := verifySplitRules(branchRulesOf(main, created), branchRulesOf(updated, created)); err != nil {
		t.Fatalf("a resumed split: %v", err)
	}
	lost := &githubRuleset{ID: 100, Rules: plan.Update.Rules[:2]}
	shared := &githubRuleset{ID: 200, Rules: append(append([]json.RawMessage{}, plan.Create.Rules...), plan.Update.Rules[0])}
	twice := &githubRuleset{ID: 100, Rules: append(append([]json.RawMessage{}, plan.Update.Rules...), plan.Create.Rules...)}
	changed := &githubRuleset{ID: 200, Rules: []json.RawMessage{rawJSON(t, map[string]any{"type": "pull_request", "parameters": map[string]any{"required_approving_review_count": 2}})}}
	for name, after := range map[string][]githubBranchRule{
		"a rule is lost":              branchRulesOf(lost, created),
		"pull_request shares":         branchRulesOf(&githubRuleset{ID: 100, Rules: plan.Update.Rules[1:]}, shared),
		"pull_request in two":         branchRulesOf(twice, created),
		"the pull_request rule moved": branchRulesOf(updated, changed),
	} {
		if err := verifySplitRules(before, after); err == nil {
			t.Fatalf("%s: verify passed", name)
		}
	}
}

func TestGitHubCredentials(t *testing.T) {
	saved := githubAuthToken
	t.Cleanup(func() { githubAuthToken = saved })
	githubAuthToken = func(context.Context) (string, error) { return "from-gh", nil }

	for _, tc := range []struct {
		env       map[string]string
		wantToken string
		wantBase  string
	}{
		{env: map[string]string{"GH_TOKEN": "gh-token", "GITHUB_TOKEN": "github-token"}, wantToken: "gh-token", wantBase: queueGitHubAPI},
		{env: map[string]string{"GITHUB_TOKEN": "github-token", "GITHUB_API_URL": "https://ghe.test/api/v3/"}, wantToken: "github-token", wantBase: "https://ghe.test/api/v3"},
		{env: map[string]string{}, wantToken: "from-gh", wantBase: queueGitHubAPI},
	} {
		gh, err := newGitHubRules(context.Background(), tc.env, http.DefaultClient)
		if err != nil || gh.token != tc.wantToken || gh.base != tc.wantBase {
			t.Fatalf("env %v: gh = %+v, err = %v", tc.env, gh, err)
		}
	}
	githubAuthToken = func(context.Context) (string, error) { return "", context.Canceled }
	if _, err := newGitHubRules(context.Background(), map[string]string{}, http.DefaultClient); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("no credentials: err = %v", err)
	}
}

// pathRecorder answers every GitHub call with an empty list and keeps the
// path each one asked for, as sent on the wire.
type pathRecorder struct{ paths []string }

func (p *pathRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	p.paths = append(p.paths, req.URL.EscapedPath())
	return reportJSONResponse(http.StatusOK, []any{}), nil
}

func TestBranchRulesKeepTheBranchSlashes(t *testing.T) {
	recorder := &pathRecorder{}
	gh := &githubRules{base: "https://" + testGitHubHost, token: "t", client: &http.Client{Transport: recorder}}
	if _, err := gh.branchRules(context.Background(), "acme/app", "release/2026 q4"); err != nil {
		t.Fatalf("branch rules: %v", err)
	}
	if want := "/repos/acme/app/rules/branches/release/2026%20q4"; len(recorder.paths) != 1 || recorder.paths[0] != want {
		t.Fatalf("paths = %v, want %s", recorder.paths, want)
	}
}
