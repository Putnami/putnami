package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	"go.putnami.dev/cloud/extension/internal/clicore"
)

// `putnami cloud ci queue [status|enable|disable]` turns the merge queue of a
// branch on or off. On a branch with a queue, Putnami tests the exact commit
// that becomes the head of the branch, then moves the branch to it: nothing
// is built again after the merge.
//
// enable does every step of the activation in one command: it reads the
// branch's rulesets with the person's GitHub credentials, shows the split the
// queue needs, applies it once the person confirms, checks that the branch
// enforces the same rules, then turns the Delivery setting on. Run again, it
// resumes where it stopped. disable turns the setting off and leaves the
// rulesets as they are.

const queueMaxDepth = 16

// CIQueueSetting is the CLI's twin of delivery-api's QueueSetting.
type CIQueueSetting struct {
	Repository string     `json:"repository"`
	Branch     string     `json:"branch"`
	Configured bool       `json:"configured"`
	Enabled    bool       `json:"enabled"`
	Depth      int        `json:"depth"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
}

// CIQueueRules is what the command found in the branch's rulesets.
type CIQueueRules struct {
	// Read is false when the rulesets could not be read; Problem says why.
	Read bool `json:"read"`
	// Ready: the rules already let the queue land.
	Ready bool `json:"ready"`
	// Problem is a shape of the rules the command does not change by itself,
	// or why they could not be read.
	Problem string `json:"problem,omitempty"`
	// Create and Update are the ruleset writes enable would make.
	Create *githubRulesetWrite   `json:"create,omitempty"`
	Update *CIQueueRulesetUpdate `json:"update,omitempty"`
}

// CIQueueRulesetUpdate is the write enable would make to an existing ruleset.
type CIQueueRulesetUpdate struct {
	ID         int64               `json:"id"`
	AppRemoved bool                `json:"appRemoved"`
	Ruleset    *githubRulesetWrite `json:"ruleset"`
}

// CIQueueResult is the structured output of every queue verb.
type CIQueueResult struct {
	Setting CIQueueSetting `json:"setting"`
	Rules   *CIQueueRules  `json:"rules,omitempty"`
	// Applied lists the ruleset writes enable made, and Backup the file that
	// holds the ruleset as it was before them.
	Applied []string `json:"applied,omitempty"`
	Backup  string   `json:"backup,omitempty"`
	DryRun  bool     `json:"dryRun,omitempty"`
}

func ciQueue(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	verb := ""
	if positionals := ciPositionals(args); len(positionals) > 1 {
		verb = positionals[1]
	}
	switch verb {
	case "", "status":
		return ciQueueStatus(params, workspaceRoot, env, ioctx)
	case "enable":
		return ciQueueEnable(params, workspaceRoot, env, ioctx)
	case "disable":
		return ciQueueDisable(params, workspaceRoot, env, ioctx)
	default:
		return clicore.NewError("unknown cloud ci queue verb: "+verb+"; expected status, enable or disable", clicore.ExitUsage)
	}
}

func queueBranch(params map[string]any) string {
	branch := strings.TrimPrefix(strings.TrimSpace(clicore.StringParam(params, "branch")), "refs/heads/")
	if branch == "" {
		return "main"
	}
	return branch
}

func queueAppID(params map[string]any) (int64, error) {
	raw := strings.TrimSpace(clicore.StringParam(params, "app-id", "appId"))
	if raw == "" {
		return queueDefaultAppID, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, clicore.NewError("--app-id must be the numeric id of a GitHub App", clicore.ExitUsage)
	}
	return id, nil
}

func queueDepth(params map[string]any) (int, error) {
	raw := strings.TrimSpace(clicore.StringParam(params, "depth"))
	if raw == "" {
		return 0, nil
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 1 || depth > queueMaxDepth {
		return 0, clicore.NewError(fmt.Sprintf("--depth must be a number from 1 to %d", queueMaxDepth), clicore.ExitUsage)
	}
	return depth, nil
}

// queueCall issues one generated delivery-api call and maps a refusal onto a
// message that names the queue verb and the provider's reason.
func queueCall[R any](reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, target string,
	call func(context.Context, *deliveryapiclient.DeliveryClient) (*R, error),
) (*CIQueueSetting, error) {
	api, err := newDeliveryAPIClient(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := clicore.CallWithSession(reqCtx, ctx, func(callCtx context.Context) (*R, error) {
		return call(callCtx, api)
	})
	if err != nil {
		if status := clicore.ServiceStatus(err); status != 0 {
			return nil, queueRefusal(verb, clicore.ServiceMessage(err), status)
		}
		return nil, ciCallError(reqCtx, "queue "+verb, target, err)
	}
	var out CIQueueSetting
	data, err := json.Marshal(resp)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	if err != nil {
		return nil, ciInvalidResponse("queue " + verb)
	}
	return &out, nil
}

func queueRefusal(verb, message string, status int) error {
	if message == "" {
		message = clicore.StatusLine(status)
	}
	prefix := "cloud ci queue " + verb + ": " + message
	switch status {
	case 401:
		return clicore.NewError(prefix+": run `putnami cloud login`", clicore.ExitAuth)
	case 403:
		return clicore.NewError(prefix+" (403 — changing the merge queue needs platform.workspace.manage, as a person: "+
			"run `putnami cloud login`; a machine token such as PUTNAMI_CLOUD_TOKEN cannot)", clicore.ExitAuth)
	default:
		return clicore.NewError(prefix, clicore.ExitAPI)
	}
}

func queueURL(ctx *clicore.WorkspaceContext, branch string) string {
	return ctx.WorkspaceURL("/ci/queue") + "?branch=" + url.QueryEscape(branch)
}

func readQueueSetting(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, branch string) (*CIQueueSetting, error) {
	return queueCall(reqCtx, ctx, verb, queueURL(ctx, branch),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.QueueSetting, error) {
			return api.GetV1WorkspacesCiQueue(callCtx, deliveryapiclient.GetV1WorkspacesCiQueueInput{
				Path:  deliveryapiclient.GetV1WorkspacesCiQueuePath{Workspace: ctx.WorkspaceID},
				Query: deliveryapiclient.GetV1WorkspacesCiQueueQuery{Branch: branch},
			})
		})
}

func writeQueueSetting(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, branch string, enabled bool, depth int) (*CIQueueSetting, error) {
	request := deliveryapiclient.QueueSettingRequest{Enabled: &enabled}
	if depth > 0 {
		value := int64(depth)
		request.Depth = &value
	}
	return queueCall(reqCtx, ctx, verb, queueURL(ctx, branch),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.QueueSetting, error) {
			return api.UpdateV1WorkspacesCiQueue(callCtx, deliveryapiclient.UpdateV1WorkspacesCiQueueInput{
				Path:  deliveryapiclient.UpdateV1WorkspacesCiQueuePath{Workspace: ctx.WorkspaceID},
				Query: deliveryapiclient.UpdateV1WorkspacesCiQueueQuery{Branch: branch},
				Body:  request,
			})
		})
}

func ciQueueStatus(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	appID, err := queueAppID(params)
	if err != nil {
		return err
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	setting, err := readQueueSetting(reqCtx, ctx, "status", branch)
	if err != nil {
		return err
	}
	result := CIQueueResult{Setting: *setting}
	if setting.Repository != "" {
		result.Rules = inspectQueueRules(reqCtx, env, ioctx, setting.Repository, branch, appID)
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(result, params, ioctx, "")
		return nil
	}
	ioctx.Stdout(queueSettingLine(setting, branch))
	if result.Rules != nil {
		for _, line := range queueRulesLines(result.Rules, branch, appID) {
			ioctx.Stdout(line)
		}
	}
	if !setting.Enabled {
		ioctx.Stdout("Turn it on with `putnami cloud ci queue enable" + queueBranchFlag(branch) + "`.")
	}
	return nil
}

// inspectQueueRules reads the branch's rulesets for status. A status never
// fails on GitHub: it says what it could not read.
func inspectQueueRules(reqCtx context.Context, env map[string]string, ioctx clicore.IO, repo, branch string, appID int64) *CIQueueRules {
	gh, err := newGitHubRules(reqCtx, env, ioctx.Client)
	if err != nil {
		return &CIQueueRules{Problem: "not read: no GitHub credentials (run `gh auth login`, or set GH_TOKEN)"}
	}
	rules, rulesets, names, err := gh.readQueueRules(reqCtx, repo, branch)
	if err != nil {
		return &CIQueueRules{Problem: "not read: " + err.Error()}
	}
	plan, err := planQueueRules(branch, rules, rulesets, names, appID)
	if err != nil {
		return &CIQueueRules{Read: true, Problem: err.Error()}
	}
	return queueRulesOf(plan)
}

func queueRulesOf(plan *queueRulesPlan) *CIQueueRules {
	out := &CIQueueRules{Read: true, Ready: plan.Ready(), Create: plan.Create}
	if plan.Update != nil {
		out.Update = &CIQueueRulesetUpdate{ID: plan.Source.ID, AppRemoved: plan.AppRemoved, Ruleset: plan.Update}
	}
	return out
}

func queueBranchFlag(branch string) string {
	if branch == "main" {
		return ""
	}
	return " --branch " + branch
}

func queueSettingLine(setting *CIQueueSetting, branch string) string {
	subject := fmt.Sprintf("Merge queue of %s, branch %s", clicore.FirstString(setting.Repository, "the workspace's repository"), clicore.FirstString(setting.Branch, branch))
	switch {
	case !setting.Configured:
		return subject + ": off. Pull requests merge through the pull request gate."
	case setting.Enabled:
		return fmt.Sprintf("%s: on, depth %d%s.", subject, setting.Depth, queueChangedBy(setting))
	default:
		return fmt.Sprintf("%s: off%s.", subject, queueChangedBy(setting))
	}
}

func queueChangedBy(setting *CIQueueSetting) string {
	if setting.UpdatedAt == nil {
		return ""
	}
	return fmt.Sprintf(", changed by %s on %s", clicore.FirstString(setting.UpdatedBy, "someone"), setting.UpdatedAt.UTC().Format("2006-01-02 15:04 UTC"))
}

func queueRulesLines(rules *CIQueueRules, branch string, appID int64) []string {
	switch {
	case !rules.Read:
		return []string{"Rules of " + branch + ": " + rules.Problem + "."}
	case rules.Problem != "":
		return []string{"Rules of " + branch + ": the queue cannot land: " + rules.Problem + "."}
	case rules.Ready:
		return []string{"Rules of " + branch + ": ready. The Putnami App bypasses the pull_request rule and no other rule."}
	}
	lines := []string{"Rules of " + branch + ": enable changes them first."}
	return append(lines, queuePlanLines(rules, appID)...)
}

func queuePlanLines(rules *CIQueueRules, appID int64) []string {
	var lines []string
	step := 1
	if rules.Create != nil {
		source := ""
		if rules.Update != nil {
			source = fmt.Sprintf(" of ruleset %q (%d)", rules.Update.Ruleset.Name, rules.Update.ID)
		}
		lines = append(lines, fmt.Sprintf("  %d. Create ruleset %q: the pull_request rule%s, with the Putnami App (id %d) as its only bypass actor, mode always.",
			step, rules.Create.Name, source, appID))
		step++
	}
	if rules.Update != nil {
		what := "drop the pull_request rule"
		if rules.Update.AppRemoved {
			what += " and the Putnami App's bypass"
		}
		kept := make([]string, 0, len(rules.Update.Ruleset.Rules))
		for _, raw := range rules.Update.Ruleset.Rules {
			kept = append(kept, ruleTypeOf(raw))
		}
		lines = append(lines, fmt.Sprintf("  %d. Update ruleset %q (%d): %s. It keeps %s.",
			step, rules.Update.Ruleset.Name, rules.Update.ID, what, clicore.FirstString(strings.Join(kept, ", "), "no rule")))
	}
	return lines
}

func ciQueueEnable(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	appID, err := queueAppID(params)
	if err != nil {
		return err
	}
	depth, err := queueDepth(params)
	if err != nil {
		return err
	}
	dryRun := clicore.Truthy(clicore.Param(params, "dry-run", "dryRun"))
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	structured := clicore.StructuredOutput(params)
	say := func(line string) {
		if !structured {
			ioctx.Stdout(line)
		}
	}

	setting, err := readQueueSetting(reqCtx, ctx, "enable", branch)
	if err != nil {
		return err
	}
	if setting.Enabled && (depth == 0 || depth == setting.Depth) {
		clicore.WriteResult(CIQueueResult{Setting: *setting}, params, ioctx, queueSettingLine(setting, branch))
		return nil
	}
	repo := strings.TrimSpace(setting.Repository)
	if repo == "" {
		return clicore.NewError("cloud ci queue enable: the workspace binds no repository", clicore.ExitFailure)
	}

	gh, err := newGitHubRules(reqCtx, env, ioctx.Client)
	if err != nil {
		return err
	}
	admin, err := gh.admin(reqCtx, repo)
	if err != nil {
		return clicore.NewError(fmt.Sprintf("cloud ci queue enable: read %s on GitHub: %v", repo, err), clicore.ExitAPI)
	}
	if !admin {
		return clicore.NewError(fmt.Sprintf("cloud ci queue enable: your GitHub account does not administer %s: a repository admin turns the merge queue on", repo), clicore.ExitAuth)
	}
	before, rulesets, names, err := gh.readQueueRules(reqCtx, repo, branch)
	if err != nil {
		return clicore.NewError("cloud ci queue enable: "+err.Error(), clicore.ExitAPI)
	}
	plan, err := planQueueRules(branch, before, rulesets, names, appID)
	if err != nil {
		return clicore.NewError("cloud ci queue enable: "+err.Error(), clicore.ExitFailure)
	}
	rules := queueRulesOf(plan)
	result := CIQueueResult{Setting: *setting, Rules: rules, DryRun: dryRun}

	say(queueSettingLine(setting, branch))
	if plan.Ready() {
		say("Rules of " + branch + ": ready. The Putnami App bypasses the pull_request rule and no other rule.")
	} else {
		say("Plan:")
		for _, line := range queuePlanLines(rules, appID) {
			say(line)
		}
	}
	say(fmt.Sprintf("  Then turn the merge queue on for %s%s.", branch, queueDepthNote(depth)))
	if dryRun {
		clicore.WriteResult(result, params, ioctx, "Dry run: nothing changed.")
		return nil
	}
	question := fmt.Sprintf("Turn the merge queue on for %s of %s?", branch, repo)
	if !plan.Ready() {
		question = fmt.Sprintf("Change the rulesets of %s and turn the merge queue on for %s?", repo, branch)
	}
	if err := ciConfirm(params, ioctx, question); err != nil {
		return err
	}

	if !plan.Ready() {
		backup, err := saveRulesetBackup(plan.Source)
		if err != nil {
			return clicore.NewError("cloud ci queue enable: keep a copy of the ruleset before changing it: "+err.Error(), clicore.ExitFailure)
		}
		result.Backup = backup
		say("Ruleset " + strconv.FormatInt(plan.Source.ID, 10) + " as it was: " + backup)
		if err := applyQueueRules(reqCtx, gh, repo, branch, plan, before, &result, say); err != nil {
			return err
		}
	}

	updated, err := writeQueueSetting(reqCtx, ctx, "enable", branch, true, depth)
	if err != nil {
		if !plan.Ready() {
			say("The rulesets keep their new shape: fix the cause, then run this again to turn the queue on.")
		}
		return err
	}
	result.Setting = *updated
	if structured {
		clicore.WriteResult(result, params, ioctx, "")
		return nil
	}
	ioctx.Stdout(queueSettingLine(updated, branch))
	ioctx.Stdout("Next: turn on auto-merge on a small green pull request:")
	ioctx.Stdout(fmt.Sprintf("  gh pr merge -R %s --auto --squash <number>", repo))
	ioctx.Stdout("Its Putnami Verdict check reads \"Merges through the queue\". Once it lands, the pull request shows merged.")
	return nil
}

func queueDepthNote(depth int) string {
	if depth == 0 {
		return ""
	}
	return fmt.Sprintf(", depth %d", depth)
}

// applyQueueRules creates the pull_request ruleset first, so the branch
// never runs without a pull_request rule, then removes that rule from the
// first ruleset, then checks the branch enforces the same rules.
func applyQueueRules(reqCtx context.Context, gh *githubRules, repo, branch string, plan *queueRulesPlan, before []githubBranchRule,
	result *CIQueueResult, say func(string),
) error {
	var created *githubRuleset
	if plan.Create != nil {
		var err error
		created, err = gh.createRuleset(reqCtx, repo, plan.Create)
		if err != nil {
			return clicore.NewError(fmt.Sprintf("cloud ci queue enable: GitHub refused ruleset %q: %v. Nothing changed", plan.Create.Name, err), clicore.ExitAPI)
		}
		result.Applied = append(result.Applied, fmt.Sprintf("created ruleset %q (%d)", plan.Create.Name, created.ID))
		say(fmt.Sprintf("Created ruleset %q (%d).", plan.Create.Name, created.ID))
	}
	if err := gh.updateRuleset(reqCtx, repo, plan.Source.ID, plan.Update); err != nil {
		hint := "Run this again to resume."
		if created != nil {
			hint = fmt.Sprintf("Run this again to resume, or delete ruleset %d to undo.", created.ID)
		}
		return clicore.NewError(fmt.Sprintf("cloud ci queue enable: GitHub refused the update of ruleset %q: %v. %s", plan.Source.Name, err, hint), clicore.ExitAPI)
	}
	result.Applied = append(result.Applied, fmt.Sprintf("updated ruleset %q (%d)", plan.Source.Name, plan.Source.ID))
	say(fmt.Sprintf("Updated ruleset %q (%d).", plan.Source.Name, plan.Source.ID))

	after, err := gh.branchRules(reqCtx, repo, branch)
	if err == nil {
		err = verifySplitRules(before, after)
	}
	if err != nil {
		return clicore.NewError(fmt.Sprintf("cloud ci queue enable: the rules of %s are not the ones expected after the split: %v. "+
			"Check them on GitHub; to undo, put ruleset %d back from %s", branch, err, plan.Source.ID, result.Backup), clicore.ExitFailure)
	}
	say(fmt.Sprintf("Verified: %s enforces the same rules, the pull_request rule alone in its ruleset.", branch))
	return nil
}

// saveRulesetBackup writes the ruleset as it was, in the shape a PUT takes
// back, to a file the person keeps.
func saveRulesetBackup(ruleset *githubRuleset) (string, error) {
	data, err := json.MarshalIndent(githubRulesetWrite{
		Name: ruleset.Name, Target: ruleset.Target, Enforcement: ruleset.Enforcement, Conditions: ruleset.Conditions,
		BypassActors: ruleset.BypassActors, Rules: ruleset.Rules,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", fmt.Sprintf("putnami-queue-ruleset-%d-*.json", ruleset.ID))
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return "", err
	}
	return file.Name(), nil
}

func ciQueueDisable(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	branch := queueBranch(params)
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	setting, err := readQueueSetting(reqCtx, ctx, "disable", branch)
	if err != nil {
		return err
	}
	if !setting.Enabled {
		clicore.WriteResult(CIQueueResult{Setting: *setting}, params, ioctx, queueSettingLine(setting, branch))
		return nil
	}
	if err := ciConfirm(params, ioctx, fmt.Sprintf("Turn the merge queue off for %s of %s? Its open entries drop, "+
		"and GitHub's auto-merge then merges each pull request that still has it on.", branch, setting.Repository)); err != nil {
		return err
	}
	updated, err := writeQueueSetting(reqCtx, ctx, "disable", branch, false, 0)
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(CIQueueResult{Setting: *updated}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout(queueSettingLine(updated, branch))
	ioctx.Stdout("Within 5 minutes, Delivery drops every open entry with the reason queue-off. The rulesets stay split: " +
		"the pull_request rule still applies to everyone but the Putnami App.")
	return nil
}
