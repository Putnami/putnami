package runtimecli

// env_status.go backs `putnami cloud env status [<env>]`: what each
// environment putnami.ci.json declares, against what the workspace runs.
//
//	env  ok  prod: 30 of 30 workloads ready, canary gen 310
//
// With no environment named, the node has one child per environment, each
// with one grandchild per workload. With one named, that environment is the
// node and its workloads are the children. Every environment also has a
// channel child: the newest move of the channel it follows, with the full id
// of each release the move opened, so the reader can pass one to
// `putnami cloud deploy status`.
//
// It reads the deployments summary once and each declared environment's
// channel-follow answer once, at most envStatusParallelism at a time, and
// never the readiness route: `env doctor` is the slow, complete check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// envStatusParallelism bounds the channel-follow reads in flight at once.
const envStatusParallelism = 8

// EnvDeclaration is one environment putnami.ci.json declares: the channel it
// follows and the workloads it selects. Problem says what is wrong with the
// declaration, and is empty when nothing is.
type EnvDeclaration struct {
	Name      string
	Channel   string
	Workloads []EnvDoctorWorkload
	Problem   string
}

// EnvStatusFacts is everything `env status` read. Named is the environment
// the command names, empty for every environment. DeploymentsErr is set when
// the deployments summary could not be read, and Follow holds each declared
// environment's channel-follow answer by name.
type EnvStatusFacts struct {
	Named          string
	Declared       []EnvDeclaration
	Deployments    []deploymentSummaryEntry
	DeploymentsErr error
	Follow         map[string]statusChannelFollow
}

// EnvStatus backs `putnami cloud env status [<env>]`. args are the arguments
// after `status`. --health and --provenance keep the deployment table of one
// environment; the default view is the status node.
func EnvStatus(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	named, err := envStatusName(params, args)
	if err != nil {
		return err
	}
	if clicore.BoolParam(params, false, "health") || clicore.BoolParam(params, false, "provenance") {
		params["env"] = clicore.FirstString(named, envLinkedEnvironment(workspaceRoot), "prod")
		return Status(params, nil, workspaceRoot, env, ioctx)
	}
	reqCtx, stop := runtimeCommandContext(ioctx)
	defer stop()
	facts, err := collectEnvStatus(reqCtx, params, named, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	return clicore.WriteStatus(params, ioctx, EnvStatusNodeFrom(facts))
}

// EnvStatusNode is the env line of `putnami cloud status`: every declared
// environment against what the workspace runs, or only the one --env names.
// A read that cannot start (no session, no linked workspace) makes the node
// unknown.
func EnvStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	reqCtx, stop := runtimeCommandContext(ioctx)
	defer stop()
	facts, err := collectEnvStatus(reqCtx, params, clicore.StringParam(params, "env", "environment"), workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("env", "env", err, "putnami cloud env status")
	}
	return EnvStatusNodeFrom(facts)
}

// envStatusName is the environment the command names: the positional, else
// --env. Naming two different ones is a usage error.
func envStatusName(params map[string]any, args []string) (string, error) {
	positionals := clicore.Positionals(args)
	if len(positionals) > 1 {
		return "", clicore.NewError("cloud env status takes at most one environment", clicore.ExitUsage)
	}
	flag := clicore.StringParam(params, "env", "environment")
	if len(positionals) == 0 {
		return flag, nil
	}
	if flag != "" && flag != positionals[0] {
		return "", clicore.NewError(fmt.Sprintf("cloud env status takes one environment; got %q and --env %q", positionals[0], flag), clicore.ExitUsage)
	}
	return positionals[0], nil
}

// envLinkedEnvironment is the environment .putnami/cloud-link.json records,
// empty when there is none.
func envLinkedEnvironment(workspaceRoot string) string {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(clicore.StringValue(link["environment"]))
}

// runtimeCommandContext is ioctx.Context, or the background, canceled on
// Ctrl-C and SIGTERM.
func runtimeCommandContext(ioctx clicore.IO) (context.Context, context.CancelFunc) {
	base := ioctx.Context
	if base == nil {
		base = context.Background()
	}
	return signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
}

// collectEnvStatus reads the declarations, the deployments summary and the
// channel-follow answers. It fails only when nothing can be read: the
// workspace cannot be resolved, or the control plane refuses the session.
func collectEnvStatus(reqCtx context.Context, params map[string]any, named, workspaceRoot string, env map[string]string, ioctx clicore.IO) (EnvStatusFacts, error) {
	facts := EnvStatusFacts{Named: named, Declared: envDeclarations(workspaceRoot)}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return facts, err
	}
	var follow []string
	for _, declaration := range facts.Declared {
		if named == "" || declaration.Name == named {
			follow = append(follow, declaration.Name)
		}
	}
	answers := make([]statusChannelFollow, len(follow))
	headerCtx := statusHeaderContext(ctx)
	var wg sync.WaitGroup
	slots := make(chan struct{}, envStatusParallelism)
	for index, environment := range follow {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			answers[index] = fetchChannelFollow(headerCtx, reqCtx, environment)
		}()
	}
	summary, err := statusFetch(ctx, reqCtx)
	wg.Wait()
	if err != nil {
		if clicore.ExitCode(err) == clicore.ExitAuth {
			return facts, err
		}
		facts.DeploymentsErr = err
	} else {
		facts.Deployments = summary.Deployments
	}
	facts.Follow = make(map[string]statusChannelFollow, len(answers))
	for _, answer := range answers {
		facts.Follow[answer.Environment] = answer
	}
	return facts, nil
}

// envDeclarations reads every environment putnami.ci.json declares, sorted by
// name. A file that cannot be read declares none.
func envDeclarations(workspaceRoot string) []EnvDeclaration {
	document, err := readEnvCIDocument(workspaceRoot)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(document.Envs))
	for name := range document.Envs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]EnvDeclaration, 0, len(names))
	for _, name := range names {
		row, _, workloads := envDoctorCIEnvs(workspaceRoot, name)
		declaration := EnvDeclaration{Name: name, Channel: document.Envs[name].Channel, Workloads: workloads}
		if row.Status != EnvDoctorOK {
			declaration.Problem = row.Detail
		}
		out = append(out, declaration)
	}
	return out
}

// EnvStatusNodeFrom folds what `env status` read into its node. A declared
// workload that is Ready is ok; one still provisioning is degraded; one that
// failed is failing; one declared and not deployed, or deployed and not
// declared, is degraded.
func EnvStatusNodeFrom(facts EnvStatusFacts) clicore.StatusNode {
	declared := make(map[string]EnvDeclaration, len(facts.Declared))
	for _, declaration := range facts.Declared {
		declared[declaration.Name] = declaration
	}
	if facts.Named != "" {
		node, counts := envNode(facts, facts.Named, declared)
		node.ID = "env"
		node.Title = "env " + facts.Named
		node.Fix = clicore.FirstString(node.Fix, firstFix(node.Children))
		node.Metrics = envMetrics([]envCounts{counts}, false)
		return node
	}
	root := clicore.StatusNode{ID: "env", Title: "env"}
	var all []envCounts
	var details []string
	for _, name := range envNames(facts) {
		child, counts := envNode(facts, name, declared)
		root.Children = append(root.Children, child)
		all = append(all, counts)
		details = append(details, name+": "+child.Detail)
	}
	if len(root.Children) == 0 {
		if facts.DeploymentsErr != nil {
			return clicore.UnknownStatus("env", "env", facts.DeploymentsErr, "")
		}
		root.State = clicore.StatusDegraded
		root.Detail = "putnami.ci.json declares no environment and nothing is deployed"
		root.Fix = "putnami cloud env doctor"
		return root
	}
	root.State = clicore.WorstStatus(root.ChildStates()...)
	root.Detail = strings.Join(details, "; ")
	root.Fix = firstFix(root.Children)
	root.Metrics = envMetrics(all, true)
	return root
}

// firstFix is the fix of the first check in the worst state, depth first:
// a failing check's fix comes before a degraded one's.
func firstFix(children []clicore.StatusNode) string {
	for _, state := range []clicore.StatusState{clicore.StatusFailing, clicore.StatusUnknown, clicore.StatusDegraded} {
		if fix := firstFixIn(children, state); fix != "" {
			return fix
		}
	}
	return ""
}

func firstFixIn(children []clicore.StatusNode, state clicore.StatusState) string {
	for _, child := range children {
		if child.State == state && child.Fix != "" {
			return child.Fix
		}
		if fix := firstFixIn(child.Children, state); fix != "" {
			return fix
		}
	}
	return ""
}

// envNames is every environment putnami.ci.json declares, then every
// environment the summary names that it does not, each sorted.
func envNames(facts EnvStatusFacts) []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(facts.Declared))
	for _, declaration := range facts.Declared {
		seen[declaration.Name] = true
		names = append(names, declaration.Name)
	}
	var extra []string
	for _, deployment := range facts.Deployments {
		if deployment.Environment != "" && !seen[deployment.Environment] {
			seen[deployment.Environment] = true
			extra = append(extra, deployment.Environment)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

// envCounts is what one environment adds to the metrics.
type envCounts struct {
	environment string
	declared    int
	ready       int
	notDeployed int
	undeclared  int
	generation  int64
	followed    bool
	channel     string
}

// envNode is one environment: its channel, then its declared workloads in
// the order putnami.ci.json selects them, then the workloads it runs that
// putnami.ci.json does not declare.
func envNode(facts EnvStatusFacts, name string, declared map[string]EnvDeclaration) (clicore.StatusNode, envCounts) {
	node := clicore.StatusNode{ID: "env." + name, Title: name}
	counts := envCounts{environment: name}
	declaration, isDeclared := declared[name]
	running := map[string]deploymentSummaryEntry{}
	var order []string
	for _, deployment := range facts.Deployments {
		if deployment.Environment != name {
			continue
		}
		if _, dup := running[deployment.Project]; !dup {
			order = append(order, deployment.Project)
		}
		running[deployment.Project] = deployment
	}

	if !isDeclared {
		if facts.DeploymentsErr != nil {
			node.State = clicore.StatusUnknown
			node.Detail = "putnami.ci.json declares no envs." + name + ", deployments not read"
			return node, counts
		}
		for _, project := range order {
			child := envUndeclaredWorkload(name, running[project])
			child.Fix = ""
			node.Children = append(node.Children, child)
		}
		if len(node.Children) == 0 {
			node.State = clicore.StatusFailing
			node.Detail = "putnami.ci.json declares no envs." + name + " and nothing is deployed to it"
			node.Fix = "putnami cloud env doctor " + name
			return node, counts
		}
		// No command declares an environment: the detail says what to edit.
		node.State = clicore.StatusDegraded
		node.Detail = "not declared in putnami.ci.json, " + countNoun(len(node.Children), "workload") + " deployed"
		counts.undeclared = len(node.Children)
		return node, counts
	}

	channelNode, generation, followed := envChannelNode(name, declaration, facts.Follow[name])
	node.Children = append(node.Children, channelNode)
	counts.generation, counts.followed = generation, followed
	counts.channel = strings.TrimSpace(strings.TrimPrefix(channelNode.Title, "channel"))
	if declaration.Problem != "" {
		node.Children = append(node.Children, clicore.StatusNode{
			ID: "env." + name + ".declaration", Title: "declaration", State: clicore.StatusDegraded,
			Detail: declaration.Problem, Fix: "putnami cloud env doctor " + name,
		})
	}
	if facts.DeploymentsErr != nil {
		node.Children = append(node.Children, clicore.UnknownStatus("env."+name+".deployments", "deployments", facts.DeploymentsErr, ""))
	}
	used := map[string]bool{}
	for _, workload := range declaration.Workloads {
		counts.declared++
		if facts.DeploymentsErr != nil {
			continue
		}
		deployment, found := running[workload.Name]
		key := workload.Name
		if !found {
			deployment, found = running[workload.Path]
			key = workload.Path
		}
		if !found {
			counts.notDeployed++
			node.Children = append(node.Children, clicore.StatusNode{
				ID: "env." + name + "." + workload.Name, Title: workload.Name, State: clicore.StatusDegraded,
				Detail: "declared, not deployed", Fix: "putnami cloud env doctor " + name,
			})
			continue
		}
		used[key] = true
		child := envWorkloadNode(name, workload.Name, deployment)
		if strings.EqualFold(strings.TrimSpace(deployment.State), "ready") {
			counts.ready++
		}
		node.Children = append(node.Children, child)
	}
	for _, project := range order {
		if !used[project] {
			counts.undeclared++
			node.Children = append(node.Children, envUndeclaredWorkload(name, running[project]))
		}
	}
	node.State = clicore.WorstStatus(node.ChildStates()...)
	node.Detail = envDetail(counts, facts.DeploymentsErr != nil)
	return node, counts
}

// envDetail is "30 of 30 workloads ready, canary gen 310".
func envDetail(counts envCounts, unread bool) string {
	detail := fmt.Sprintf("%d of %d workloads ready", counts.ready, counts.declared)
	switch {
	case counts.declared == 0:
		detail = "no workload declared"
	case unread:
		detail = countNoun(counts.declared, "workload") + " declared, deployments not read"
	}
	if counts.notDeployed > 0 {
		detail += fmt.Sprintf(", %d not deployed", counts.notDeployed)
	}
	if counts.undeclared > 0 {
		detail += fmt.Sprintf(", %d deployed and not declared", counts.undeclared)
	}
	if counts.followed && counts.channel != "" {
		detail += fmt.Sprintf(", %s gen %d", counts.channel, counts.generation)
	}
	return detail
}

// envWorkloadNode is one declared workload the workspace runs.
func envWorkloadNode(environment, name string, deployment deploymentSummaryEntry) clicore.StatusNode {
	node := clicore.StatusNode{ID: "env." + environment + "." + name, Title: name}
	revision := ""
	if deployment.Revision != "" {
		revision = ", revision " + shortRevision(deployment.Revision)
	}
	state := strings.ToLower(strings.TrimSpace(deployment.State))
	switch state {
	case "ready":
		node.State = clicore.StatusOK
		node.Detail = "ready" + revision
		if lastRunFailed(deployment.LastRunStatus) {
			node.State = clicore.StatusDegraded
			node.Detail += ", last run " + strings.ToLower(deployment.LastRunStatus)
			if reason := statusShortReason(deployment.LastRunReason); reason != "" {
				node.Detail += ": " + reason
			}
			node.Fix = "putnami cloud env doctor " + environment
			if deployment.LastReleaseID != "" {
				node.Fix = "putnami cloud deploy status " + deployment.LastReleaseID
			}
		}
	case "provisioning":
		node.State = clicore.StatusDegraded
		node.Detail = "provisioning" + revision
	case "failed", "partial", "skipped":
		node.State = clicore.StatusFailing
		node.Detail = state + revision
		if reason := statusShortReason(deployment.LastRunReason); reason != "" {
			node.Detail += ": " + reason
		}
		node.Fix = "putnami cloud env doctor " + environment
		if deployment.LastReleaseID != "" {
			node.Fix = "putnami cloud deploy status " + deployment.LastReleaseID
		}
	case "":
		node.State = clicore.StatusUnknown
		node.Detail = "no state reported" + revision
	default:
		node.State = clicore.StatusUnknown
		node.Detail = "state " + state + revision
	}
	return node
}

// lastRunFailed reports a last run that ended without serving.
func lastRunFailed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "partial":
		return true
	default:
		return false
	}
}

// envUndeclaredWorkload is a workload the workspace runs in an environment
// that putnami.ci.json does not select it for. It has no fix: selecting it in
// putnami.ci.json is an edit, and deleting a retired service is a runbook
// step, not a command.
func envUndeclaredWorkload(environment string, deployment deploymentSummaryEntry) clicore.StatusNode {
	detail := "deployed, not declared in putnami.ci.json"
	if deployment.Revision != "" {
		detail += ", revision " + shortRevision(deployment.Revision)
	}
	return clicore.StatusNode{
		ID: "env." + environment + "." + deployment.Project, Title: deployment.Project, State: clicore.StatusDegraded,
		Detail: detail,
	}
}

// shortRevision is the revision name without its resource path:
// projects/p/locations/l/services/s/revisions/s-00042 is s-00042.
func shortRevision(revision string) string {
	if index := strings.LastIndex(revision, "/"); index >= 0 && index < len(revision)-1 {
		return revision[index+1:]
	}
	return revision
}

// envChannelNode is the newest move of the channel the environment follows.
// It answers the move's generation, and whether one was read.
func envChannelNode(environment string, declaration EnvDeclaration, follow statusChannelFollow) (clicore.StatusNode, int64, bool) {
	title := "channel"
	if declaration.Channel != "" {
		title = "channel " + declaration.Channel
	}
	node := clicore.StatusNode{ID: "env." + environment + ".channel", Title: title}
	if follow.Unavailable != "" || follow.Environment == "" {
		reason := follow.Unavailable
		if reason == "" {
			reason = "no answer"
		}
		node.State = clicore.StatusUnknown
		node.Detail = "channel follow not read: " + reason
		return node, 0, false
	}
	var followed *followedChannel
	for index := range follow.Channels {
		if declaration.Channel == "" || follow.Channels[index].Channel == declaration.Channel {
			followed = &follow.Channels[index]
			break
		}
	}
	if followed == nil {
		// An environment putnami.ci.json declares without a channel follows
		// none, as env doctor reports; only a declared channel the accepted
		// definition does not follow is a gap.
		if declaration.Channel == "" {
			node.State = clicore.StatusOK
			node.Detail = "follows no channel"
			return node, 0, false
		}
		node.State = clicore.StatusDegraded
		node.Detail = "the accepted environment definition does not follow " + declaration.Channel
		node.Fix = "putnami cloud env doctor " + environment
		return node, 0, false
	}
	if declaration.Channel == "" {
		node.Title = "channel " + followed.Channel
	}
	receipt := followed.Receipt
	node.Detail = statusMove(receipt)
	if receipt == nil {
		node.State = clicore.StatusOK
		return node, 0, false
	}
	node.State = clicore.StatusOK
	if receipt.Settled {
		switch receipt.Disposition {
		case "partially_opened", "not_deployable":
			node.State = clicore.StatusDegraded
			node.Fix = "putnami cloud env doctor " + environment
			if len(receipt.ReleaseIDs) > 0 {
				node.Fix = "putnami cloud deploy status " + receipt.ReleaseIDs[len(receipt.ReleaseIDs)-1]
			}
		}
	}
	return node, receipt.Generation, true
}

// envMetrics are the counts over every environment, and the CD generation
// of each one that follows a channel.
func envMetrics(all []envCounts, perEnvironment bool) []clicore.StatusMetric {
	var declared, ready, notDeployed, undeclared int
	for _, counts := range all {
		declared += counts.declared
		ready += counts.ready
		notDeployed += counts.notDeployed
		undeclared += counts.undeclared
	}
	metrics := []clicore.StatusMetric{
		clicore.CountMetric("workloads_declared", "workloads declared", declared, clicore.MetricUsage),
		clicore.CountMetric("workloads_ready", "workloads ready", ready, clicore.MetricHealth).Of(float64(declared)),
		clicore.CountMetric("workloads_not_deployed", "workloads not deployed", notDeployed, clicore.MetricHealth),
		clicore.CountMetric("workloads_not_declared", "workloads not declared", undeclared, clicore.MetricHealth),
	}
	for _, counts := range all {
		if !counts.followed {
			continue
		}
		id, title := "cd_generation", "CD generation"
		if perEnvironment {
			id = "cd_generation_" + metricSuffix(counts.environment)
			title = counts.environment + " CD generation"
		}
		metrics = append(metrics, clicore.CountMetric(id, title, int(counts.generation), clicore.MetricHealth))
	}
	return metrics
}

// metricSuffix turns an environment name into a snake_case id segment.
func metricSuffix(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, name)
}

// readEnvCIDocument reads putnami.ci.json.
func readEnvCIDocument(workspaceRoot string) (envDoctorCIDocument, error) {
	var document envDoctorCIDocument
	data, err := os.ReadFile(filepath.Join(workspaceRoot, "putnami.ci.json"))
	if err != nil {
		return document, err
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return document, errors.New("putnami.ci.json does not parse: " + err.Error())
	}
	return document, nil
}

// countNoun is "1 workload" or "2 workloads".
func countNoun(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
