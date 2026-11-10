package deliverycli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"

	"go.putnami.dev/cloud/extension/internal/clicore"
)

// THE BRANCH'S RULES MUST LET THE QUEUE LAND. The merge queue lands a change
// by pushing the base branch itself, as the Putnami GitHub App. GitHub checks
// that push against every ruleset of the branch, so the App must bypass the
// pull_request rule. A bypass actor bypasses its whole ruleset: in a ruleset
// that holds other rules, the App would also skip the required checks and the
// signature rule. So the pull_request rule lives alone in its own ruleset,
// with the App as its bypass actor, and the App bypasses no other ruleset.
//
// THE PERSON WRITES THE RULES, NEVER PUTNAMI. The App holds Administration:
// read only. `putnami cloud ci queue enable` changes a ruleset with the
// GitHub credentials of the person who runs it, after it showed the change
// and the person confirmed it.

const (
	// queueDefaultAppID is the putnami GitHub App of Putnami Cloud.
	queueDefaultAppID  int64 = 4203649
	queueGitHubAPI           = "https://api.github.com"
	rulePullRequest          = "pull_request"
	ruleRequiredChecks       = "required_status_checks"
	ruleMergeQueue           = "merge_queue"
	bypassActorApp           = "Integration"
	bypassModeAlways         = "always"
	bypassModeExempt         = "exempt"
	// queueRulesPerPage bounds the rules and rulesets one read lists.
	queueRulesPerPage = 100
)

// queueChecks are the required checks a queue commit reports: Putnami
// publishes both on it. Any other required check never reports there, and
// GitHub would refuse every landing.
var queueChecks = []string{"Putnami CI", "Putnami Verdict"}

// githubBranchRule is one rule GitHub enforces on a branch, with the ruleset
// that holds it.
type githubBranchRule struct {
	Type              string          `json:"type"`
	RulesetID         int64           `json:"ruleset_id"`
	RulesetSourceType string          `json:"ruleset_source_type"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
}

// githubRuleset is one repository ruleset as GitHub answers it. The members
// the command does not change pass through as read.
type githubRuleset struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Target       json.RawMessage   `json:"target,omitempty"`
	Enforcement  json.RawMessage   `json:"enforcement,omitempty"`
	Conditions   json.RawMessage   `json:"conditions,omitempty"`
	BypassActors []json.RawMessage `json:"bypass_actors"`
	Rules        []json.RawMessage `json:"rules"`
}

// githubRulesetWrite is the body that creates or replaces a ruleset.
type githubRulesetWrite struct {
	Name         string            `json:"name"`
	Target       json.RawMessage   `json:"target,omitempty"`
	Enforcement  json.RawMessage   `json:"enforcement,omitempty"`
	Conditions   json.RawMessage   `json:"conditions,omitempty"`
	BypassActors []json.RawMessage `json:"bypass_actors"`
	Rules        []json.RawMessage `json:"rules"`
}

type githubBypassActor struct {
	ActorID    *int64 `json:"actor_id"`
	ActorType  string `json:"actor_type"`
	BypassMode string `json:"bypass_mode"`
}

type githubTypedRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

func ruleTypeOf(raw json.RawMessage) string {
	var rule githubTypedRule
	_ = json.Unmarshal(raw, &rule)
	return strings.TrimSpace(rule.Type)
}

func (r *githubRuleset) types() []string {
	types := make([]string, 0, len(r.Rules))
	for _, raw := range r.Rules {
		types = append(types, ruleTypeOf(raw))
	}
	return types
}

func (r *githubRuleset) holds(ruleType string) bool { return slices.Contains(r.types(), ruleType) }

func (r *githubRuleset) holdsOnly(ruleType string) bool {
	types := r.types()
	return len(types) > 0 && !slices.ContainsFunc(types, func(t string) bool { return t != ruleType })
}

// appBypass reports whether the App is a bypass actor of the ruleset, and
// whether in a mode that bypasses at any time.
func (r *githubRuleset) appBypass(appID int64) (listed, atAnyTime bool) {
	for _, raw := range r.BypassActors {
		var actor githubBypassActor
		if json.Unmarshal(raw, &actor) != nil || actor.ActorType != bypassActorApp || actor.ActorID == nil || *actor.ActorID != appID {
			continue
		}
		listed = true
		if mode := strings.TrimSpace(actor.BypassMode); mode == bypassModeAlways || mode == bypassModeExempt {
			atAnyTime = true
		}
	}
	return listed, atAnyTime
}

// queueRulesPlan is what the rules of a branch need before the queue can
// land there. Create and Update are nil when the rules already have the
// shape.
type queueRulesPlan struct {
	// Source is the ruleset that holds the pull_request rule beside other
	// rules, and Update is what it becomes: the same ruleset without that
	// rule and without the App's bypass.
	Source *githubRuleset
	Update *githubRulesetWrite
	// AppRemoved: Update drops the App from Source's bypass actors.
	AppRemoved bool
	// Create is the ruleset to create: the pull_request rule alone, with
	// the App as its only bypass actor. Nil when a former run created it.
	Create *githubRulesetWrite
	// Split is the ruleset that already holds the pull_request rule alone,
	// with the App as bypass actor.
	Split *githubRuleset
}

// Ready reports whether the rules already let the queue land.
func (p *queueRulesPlan) Ready() bool { return p.Update == nil && p.Create == nil }

// queueRulesRefusal is a shape of the rules this command does not change by
// itself. Its message says what to fix.
func queueRulesRefusal(format string, args ...any) error {
	return clicore.NewError(fmt.Sprintf(format, args...), clicore.ExitFailure)
}

// planQueueRules reads the rules of a branch and decides what makes them
// let the queue land. names are the names of every ruleset of the
// repository, to keep a new ruleset's name unique.
func planQueueRules(branch string, rules []githubBranchRule, rulesets map[int64]*githubRuleset, names []string, appID int64) (*queueRulesPlan, error) {
	if len(rules) == 0 {
		return nil, queueRulesRefusal("branch %s has no rule: it needs a ruleset with the pull_request rule before it can have a queue", branch)
	}
	var missing []string
	for _, rule := range rules {
		switch {
		case rule.Type == ruleMergeQueue:
			return nil, queueRulesRefusal("GitHub's own merge queue rule applies to branch %s: remove it first", branch)
		case rule.Type == rulePullRequest && rule.RulesetSourceType != "" && rule.RulesetSourceType != "Repository":
			return nil, queueRulesRefusal("a ruleset of the %s holds the pull_request rule of branch %s: split it by hand, see the merge queue activation runbook",
				strings.ToLower(rule.RulesetSourceType), branch)
		case rule.Type == ruleRequiredChecks:
			missing = append(missing, foreignRequiredChecks(rule.Parameters)...)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, queueRulesRefusal("branch %s requires checks that never report on a queue commit: %s. "+
			"A queue commit lives on a putnami-queue/<n> branch and has no pull request, so GitHub would refuse every landing. "+
			"Remove them from the required checks, or run their workflow on pushes to putnami-queue/** too",
			branch, strings.Join(slices.Compact(missing), ", "))
	}

	var mixed, split []*githubRuleset
	for _, id := range rulesetIDs(rules) {
		ruleset := rulesets[id]
		if ruleset == nil {
			return nil, queueRulesRefusal("ruleset %d of branch %s could not be read", id, branch)
		}
		listed, atAnyTime := ruleset.appBypass(appID)
		switch {
		case ruleset.holdsOnly(rulePullRequest) && atAnyTime:
			split = append(split, ruleset)
		case ruleset.holdsOnly(rulePullRequest):
			return nil, queueRulesRefusal("ruleset %q holds the pull_request rule alone, but the Putnami App (id %d) is not its bypass actor in mode always: add it", ruleset.Name, appID)
		case ruleset.holds(rulePullRequest):
			mixed = append(mixed, ruleset)
		case listed:
			return nil, queueRulesRefusal("the Putnami App (id %d) bypasses ruleset %q, which holds other rules than pull_request: remove it from that ruleset's bypass actors", appID, ruleset.Name)
		}
	}
	if len(mixed) > 1 || len(split) > 1 {
		return nil, queueRulesRefusal("several rulesets hold the pull_request rule of branch %s: split it by hand, see the merge queue activation runbook", branch)
	}
	if len(mixed) == 0 && len(split) == 0 {
		return nil, queueRulesRefusal("no ruleset holds the pull_request rule of branch %s: a branch with a queue needs one", branch)
	}

	plan := &queueRulesPlan{}
	if len(split) == 1 {
		plan.Split = split[0]
	}
	if len(mixed) == 0 {
		return plan, nil
	}
	source := mixed[0]
	plan.Source = source
	update := &githubRulesetWrite{Name: source.Name, Target: source.Target, Enforcement: source.Enforcement, Conditions: source.Conditions,
		BypassActors: []json.RawMessage{}, Rules: []json.RawMessage{}}
	var pullRequest []json.RawMessage
	for _, raw := range source.Rules {
		if ruleTypeOf(raw) == rulePullRequest {
			pullRequest = append(pullRequest, raw)
			continue
		}
		update.Rules = append(update.Rules, raw)
	}
	for _, raw := range source.BypassActors {
		var actor githubBypassActor
		if json.Unmarshal(raw, &actor) == nil && actor.ActorType == bypassActorApp && actor.ActorID != nil && *actor.ActorID == appID {
			plan.AppRemoved = true
			continue
		}
		update.BypassActors = append(update.BypassActors, raw)
	}
	plan.Update = update

	if plan.Split != nil {
		// A former run created the pull_request ruleset, then stopped before
		// it changed the first one. Both must hold the same rule.
		if !sameRules(plan.Split.Rules, pullRequest) {
			return nil, queueRulesRefusal("rulesets %q and %q hold different pull_request rules: make them the same, or split them by hand", plan.Split.Name, source.Name)
		}
		return plan, nil
	}
	name := source.Name + ": pull requests"
	if slices.Contains(names, name) {
		return nil, queueRulesRefusal("a ruleset named %q exists, but it does not hold the pull_request rule of branch %s: rename or delete it", name, branch)
	}
	actor, _ := json.Marshal(githubBypassActor{ActorID: &appID, ActorType: bypassActorApp, BypassMode: bypassModeAlways})
	plan.Create = &githubRulesetWrite{Name: name, Target: source.Target, Enforcement: source.Enforcement, Conditions: source.Conditions,
		BypassActors: []json.RawMessage{actor}, Rules: pullRequest}
	return plan, nil
}

// foreignRequiredChecks lists the required checks a queue commit never
// reports.
func foreignRequiredChecks(parameters json.RawMessage) []string {
	var params struct {
		Checks []struct {
			Context string `json:"context"`
		} `json:"required_status_checks"`
	}
	if json.Unmarshal(parameters, &params) != nil {
		return nil
	}
	var foreign []string
	for _, check := range params.Checks {
		if name := strings.TrimSpace(check.Context); name != "" && !slices.Contains(queueChecks, name) {
			foreign = append(foreign, name)
		}
	}
	return foreign
}

func rulesetIDs(rules []githubBranchRule) []int64 {
	ids := make([]int64, 0, len(rules))
	for _, rule := range rules {
		if !slices.Contains(ids, rule.RulesetID) {
			ids = append(ids, rule.RulesetID)
		}
	}
	slices.Sort(ids)
	return ids
}

// sameRules compares two lists of rules as JSON values, in any key order.
func sameRules(a, b []json.RawMessage) bool {
	return canonicalJSON(a) == canonicalJSON(b)
}

func canonicalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return ""
	}
	canonical, _ := json.Marshal(decoded)
	return string(canonical)
}

// verifySplitRules checks that the branch enforces the rules it enforced
// before the split, with the pull_request rule once, alone in its ruleset.
// A resumed run started with that rule in both rulesets.
func verifySplitRules(before, after []githubBranchRule) error {
	others := func(rules []githubBranchRule) []string {
		var out []string
		for _, rule := range rules {
			if rule.Type != rulePullRequest {
				out = append(out, rule.Type+" "+canonicalJSON(rule.Parameters))
			}
		}
		slices.Sort(out)
		return out
	}
	pullRequests := func(rules []githubBranchRule) []string {
		var out []string
		for _, rule := range rules {
			if rule.Type == rulePullRequest {
				out = append(out, canonicalJSON(rule.Parameters))
			}
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	if !slices.Equal(others(before), others(after)) {
		return fmt.Errorf("the branch no longer enforces the same rules besides pull_request")
	}
	if !slices.Equal(pullRequests(before), pullRequests(after)) {
		return fmt.Errorf("the branch's pull_request rule changed")
	}
	var pullRequestRuleset int64
	for _, rule := range after {
		if rule.Type == rulePullRequest {
			if pullRequestRuleset != 0 && pullRequestRuleset != rule.RulesetID {
				return fmt.Errorf("the pull_request rule is in two rulesets")
			}
			pullRequestRuleset = rule.RulesetID
		}
	}
	for _, rule := range after {
		if rule.RulesetID == pullRequestRuleset && rule.Type != rulePullRequest {
			return fmt.Errorf("the pull_request rule shares its ruleset with %s", rule.Type)
		}
	}
	return nil
}

// githubRules reads and writes the rulesets of a repository with the
// credentials of the person who runs the command.
type githubRules struct {
	base   string
	token  string
	client *http.Client
}

// githubAuthToken runs `gh auth token`. Tests replace it.
var githubAuthToken = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	return strings.TrimSpace(string(out)), err
}

// newGitHubRules finds the person's GitHub credentials: GH_TOKEN, then
// GITHUB_TOKEN, then the GitHub CLI's login. GITHUB_API_URL names another
// API host, such as GitHub Enterprise Server.
func newGitHubRules(ctx context.Context, env map[string]string, client *http.Client) (*githubRules, error) {
	base := strings.TrimRight(strings.TrimSpace(env["GITHUB_API_URL"]), "/")
	if base == "" {
		base = queueGitHubAPI
	}
	token := clicore.FirstString(strings.TrimSpace(env["GH_TOKEN"]), strings.TrimSpace(env["GITHUB_TOKEN"]))
	if token == "" {
		token, _ = githubAuthToken(ctx)
	}
	if token == "" {
		return nil, clicore.NewError("cloud ci queue needs your GitHub credentials to read the branch's rulesets: "+
			"run `gh auth login`, or set GH_TOKEN to a token with admin rights on the repository", clicore.ExitAuth)
	}
	return &githubRules{base: base, token: token, client: clicore.WithUserAgent(client)}, nil
}

// githubError is an answer GitHub refused.
type githubError struct {
	status  int
	message string
}

func (e *githubError) Error() string {
	if e.message == "" {
		return clicore.StatusLine(e.status)
	}
	return fmt.Sprintf("%s: %s", clicore.StatusLine(e.status), e.message)
}

// call sends one request to the GitHub REST API and decodes its answer.
func (g *githubRules) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var refusal struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &refusal)
		return &githubError{status: resp.StatusCode, message: strings.TrimSpace(refusal.Message)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("GitHub %s %s: invalid answer: %w", method, path, err)
	}
	return nil
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// admin reports whether the person administers the repository.
func (g *githubRules) admin(ctx context.Context, repo string) (bool, error) {
	var answer struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := g.call(ctx, http.MethodGet, repoPath(repo), nil, &answer); err != nil {
		return false, err
	}
	return answer.Permissions.Admin, nil
}

func (g *githubRules) branchRules(ctx context.Context, repo, branch string) ([]githubBranchRule, error) {
	var rules []githubBranchRule
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	path := fmt.Sprintf("%s/rules/branches/%s?per_page=%d", repoPath(repo), strings.Join(segments, "/"), queueRulesPerPage)
	return rules, g.call(ctx, http.MethodGet, path, nil, &rules)
}

func (g *githubRules) ruleset(ctx context.Context, repo string, id int64) (*githubRuleset, error) {
	var ruleset githubRuleset
	return &ruleset, g.call(ctx, http.MethodGet, fmt.Sprintf("%s/rulesets/%d", repoPath(repo), id), nil, &ruleset)
}

func (g *githubRules) rulesetNames(ctx context.Context, repo string) ([]string, error) {
	var rulesets []githubRuleset
	if err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/rulesets?per_page=%d", repoPath(repo), queueRulesPerPage), nil, &rulesets); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rulesets))
	for _, ruleset := range rulesets {
		names = append(names, ruleset.Name)
	}
	return names, nil
}

func (g *githubRules) createRuleset(ctx context.Context, repo string, body *githubRulesetWrite) (*githubRuleset, error) {
	var created githubRuleset
	return &created, g.call(ctx, http.MethodPost, repoPath(repo)+"/rulesets", body, &created)
}

func (g *githubRules) updateRuleset(ctx context.Context, repo string, id int64, body *githubRulesetWrite) error {
	return g.call(ctx, http.MethodPut, fmt.Sprintf("%s/rulesets/%d", repoPath(repo), id), body, nil)
}

// readQueueRules reads the rules of a branch, every ruleset that holds one,
// and the names of the repository's rulesets.
func (g *githubRules) readQueueRules(ctx context.Context, repo, branch string) ([]githubBranchRule, map[int64]*githubRuleset, []string, error) {
	rules, err := g.branchRules(ctx, repo, branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the rules of branch %s: %w", branch, err)
	}
	rulesets := map[int64]*githubRuleset{}
	for _, id := range rulesetIDs(rules) {
		ruleset, err := g.ruleset(ctx, repo, id)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read ruleset %d: %w", id, err)
		}
		rulesets[id] = ruleset
	}
	names, err := g.rulesetNames(ctx, repo)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list the rulesets of %s: %w", repo, err)
	}
	return rules, rulesets, names, nil
}
