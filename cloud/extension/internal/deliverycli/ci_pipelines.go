package deliverycli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.putnami.dev/client"
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	perrors "go.putnami.dev/errors"
)

// The pipeline reads: the runs, jobs and logs of any CI a workspace is
// connected to, over Delivery's neutral routes (/ci/providers, /ci/pipelines).
// The provider is a value these commands print. No code here branches on it.

// CIPipeline is one pipeline of one CI, as Delivery answers it.
type CIPipeline struct {
	Ref                string          `json:"ref"`
	Provider           string          `json:"provider"`
	PipelineID         string          `json:"pipelineId,omitempty"`
	Attempt            int64           `json:"attempt,omitempty"`
	Repository         string          `json:"repository,omitempty"`
	HeadSHA            string          `json:"headSha,omitempty"`
	GitRef             string          `json:"gitRef,omitempty"`
	URL                string          `json:"url,omitempty"`
	WorkflowName       string          `json:"workflowName,omitempty"`
	WorkflowPath       string          `json:"workflowPath,omitempty"`
	Event              string          `json:"event,omitempty"`
	Change             int64           `json:"change,omitempty"`
	Fork               bool            `json:"fork,omitempty"`
	Status             string          `json:"status,omitempty"`
	Conclusion         string          `json:"conclusion,omitempty"`
	ProviderStatus     string          `json:"providerStatus,omitempty"`
	ProviderConclusion string          `json:"providerConclusion,omitempty"`
	CreatedAt          *time.Time      `json:"createdAt,omitempty"`
	StartedAt          *time.Time      `json:"startedAt,omitempty"`
	CompletedAt        *time.Time      `json:"completedAt,omitempty"`
	QueueMS            *int64          `json:"queueMs,omitempty"`
	JobsCounted        int64           `json:"jobsCounted,omitempty"`
	JobsKept           int64           `json:"jobsKept,omitempty"`
	IndexedAt          *time.Time      `json:"indexedAt,omitempty"`
	Jobs               []CIPipelineJob `json:"jobs,omitempty"`
}

// CIPipelineJob is one job of a pipeline.
type CIPipelineJob struct {
	Ref                string     `json:"ref"`
	JobID              string     `json:"jobId,omitempty"`
	Attempt            int64      `json:"attempt,omitempty"`
	Name               string     `json:"name,omitempty"`
	Stage              string     `json:"stage,omitempty"`
	Status             string     `json:"status,omitempty"`
	Conclusion         string     `json:"conclusion,omitempty"`
	ProviderStatus     string     `json:"providerStatus,omitempty"`
	ProviderConclusion string     `json:"providerConclusion,omitempty"`
	AllowedFailure     bool       `json:"allowedFailure,omitempty"`
	Retried            bool       `json:"retried,omitempty"`
	StartedAt          *time.Time `json:"startedAt,omitempty"`
	CompletedAt        *time.Time `json:"completedAt,omitempty"`
	QueueMS            *int64     `json:"queueMs,omitempty"`
	RunnerClass        string     `json:"runnerClass,omitempty"`
	URL                string     `json:"url,omitempty"`
	FailedStep         string     `json:"failedStep,omitempty"`
	ExcerptState       string     `json:"excerptState,omitempty"`
	ExcerptReason      string     `json:"excerptReason,omitempty"`
}

// failed reports that the job ended in failure, whatever the pipeline made of
// it.
func (j CIPipelineJob) failed() bool {
	return j.Conclusion == "failure" || j.Conclusion == "timed_out"
}

// failsThePipeline reports that the job is a reason its pipeline failed: it
// failed, the pipeline does not allow it to, and no later attempt replaced it.
func (j CIPipelineJob) failsThePipeline() bool {
	return j.failed() && !j.AllowedFailure && !j.Retried
}

// CIPipelineList is the answer of the pipelines route.
type CIPipelineList struct {
	Pipelines []CIPipeline `json:"pipelines"`
	Truncated bool         `json:"truncated"`
}

// CIPipelineDetail is one pipeline with its kept jobs, and what its CI offers.
type CIPipelineDetail struct {
	Pipeline     CIPipeline `json:"pipeline"`
	Capabilities []string   `json:"capabilities,omitempty"`
}

// CIPipelineStep is one step of a job.
type CIPipelineStep struct {
	Number      int64      `json:"number"`
	Name        string     `json:"name,omitempty"`
	Status      string     `json:"status,omitempty"`
	Conclusion  string     `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// CIPipelineExcerpt is the part of a failed job's log that Delivery keeps.
type CIPipelineExcerpt struct {
	Text     string     `json:"text"`
	Bytes    int64      `json:"bytes,omitempty"`
	Lines    int64      `json:"lines,omitempty"`
	FromLine int64      `json:"fromLine,omitempty"`
	ToLine   int64      `json:"toLine,omitempty"`
	ReadAt   *time.Time `json:"readAt,omitempty"`
}

// CIPipelineJobDetail is one job with its steps and log excerpt.
type CIPipelineJobDetail struct {
	Pipeline *CIPipeline        `json:"pipeline,omitempty"`
	Job      CIPipelineJob      `json:"job"`
	Steps    []CIPipelineStep   `json:"steps,omitempty"`
	Excerpt  *CIPipelineExcerpt `json:"excerpt,omitempty"`
}

// CIPipelineFailures is the structured answer of `view --pipeline`: the
// pipeline, and why it failed.
type CIPipelineFailures struct {
	Pipeline     CIPipeline            `json:"pipeline"`
	Capabilities []string              `json:"capabilities"`
	Failures     []CIPipelineJobDetail `json:"failures"`
	// OmittedFailures counts the failed jobs past ciPipelineFailureLimit.
	OmittedFailures int `json:"omittedFailures"`
}

// CIProvider is one CI and whether Cloud reads it for the workspace.
type CIProvider struct {
	Provider     string     `json:"provider"`
	State        string     `json:"state"`
	Reason       string     `json:"reason,omitempty"`
	LastSeen     *time.Time `json:"lastSeen,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
}

// CIProviderList is the answer of the providers route.
type CIProviderList struct {
	Providers []CIProvider `json:"providers"`
}

// ciPipelineLogLine is one line of `logs --pipeline` in structured output.
type ciPipelineLogLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

const (
	// ciPipelineFailureLimit bounds the failed jobs `view` explains, one read
	// each.
	ciPipelineFailureLimit = 5
	// ciPipelineListMax is the most pipelines the route answers at once.
	ciPipelineListMax = 100
	// ciLogTailHeader says that a log answer holds the end of the log only.
	ciLogTailHeader = "Putnami-CI-Log-Tail"
)

// ciPipelineGit runs one Git command of a pipeline read. A package var so a
// test names the repository state.
var ciPipelineGit = evidenceGit

// --- what the caller named ---

// ciPipelineRef is the --pipeline reference, "" when the caller reads a hosted
// run.
func ciPipelineRef(params map[string]any) string {
	return clicore.ScalarStringParam(params, "pipeline")
}

// ciListsPipelines reports that `list` reads the pipelines of a commit or a
// change rather than the hosted runs.
func ciListsPipelines(params map[string]any) bool {
	return clicore.ScalarStringParam(params, "commit") != "" || clicore.ScalarStringParam(params, "change") != ""
}

// ciRefuseFlags refuses the first of the named flags that the caller set, so
// no flag is dropped in silence.
func ciRefuseFlags(params map[string]any, with string, names ...string) error {
	for _, name := range names {
		value := clicore.Param(params, name)
		if set, isBool := value.(bool); isBool && !set {
			continue
		}
		if value != nil && clicore.ScalarStringValue(value) != "" {
			return clicore.NewError("--"+name+" does not apply with "+with, clicore.ExitUsage)
		}
	}
	return nil
}

// resolvePipelineContext is clicore.ResolveWorkspaceContext for a read that
// also runs in a repository with no Putnami workspace: the workspace comes
// from --workspace, the link or the delegated session, and the control plane is
// the default one when nothing names another.
func resolvePipelineContext(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (*clicore.WorkspaceContext, error) {
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil && !errors.Is(err, clicore.ErrWorkspaceNotConfigured) {
		return nil, err
	}
	resolved := map[string]any{"control_plane_url": clicore.DefaultControlPlaneURL}
	for key, value := range link {
		if clicore.StringValue(value) != "" {
			resolved[key] = value
		}
	}
	if clicore.ResolveWorkspaceID(params, resolved) == "" {
		workspaceID := ciDelegatedWorkspace(env)
		if workspaceID == "" {
			return nil, clicore.NewError("this directory is linked to no Putnami Cloud workspace; pass --workspace <id, slug or name>", clicore.ExitUsage)
		}
		resolved["workspace_id"] = workspaceID
	}
	return clicore.ResolveWorkspaceContextFromLink(params, resolved, env, ioctx)
}

// ciDelegatedWorkspace is the workspace a delegated session acts in, "" for
// any other session.
func ciDelegatedWorkspace(env map[string]string) string {
	auth, err := clicore.ReadAuth(env, false)
	if err != nil {
		return ""
	}
	delegation, _ := clicore.DelegationOf(auth)
	return delegation.WorkspaceID
}

// ciPipelineRepository is the repository a pipeline list names: --repo, else
// the one of the origin remote.
func ciPipelineRepository(params map[string]any, dir string) (string, error) {
	if repo := strings.Trim(clicore.StringParam(params, "repo"), "/"); repo != "" {
		return repo, nil
	}
	remote, err := ciPipelineGit(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", clicore.NewError("no origin remote names the repository; pass --repo <owner/name>", clicore.ExitUsage)
	}
	repo := ciRepositoryFromRemote(remote)
	if repo == "" {
		// The remote is not printed: it may carry a credential.
		return "", clicore.NewError("the origin remote is not the address of a hosted repository; pass --repo <owner/name>", clicore.ExitUsage)
	}
	return repo, nil
}

// ciRepositoryFromRemote is the path of a remote on its host: owner/name, or
// the full path of a project under nested groups. It is "" for a remote that
// names no host, and for a path with no owner.
func ciRepositoryFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	var path string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Host == "" {
			return ""
		}
		path = parsed.Path
	} else {
		// The scp form, [user@]host:path. A slash before the colon makes it a
		// local path.
		host, rest, found := strings.Cut(remote, ":")
		if !found || host == "" || strings.ContainsAny(host, `/\`) {
			return ""
		}
		path = rest
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name, found := strings.Cut(path, "/")
	if !found || owner == "" || name == "" || strings.Contains(path, "//") {
		return ""
	}
	return path
}

var ciCommitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ciPipelineCommit is the full commit id of a revision. A full id is sent as
// given, so a commit that a shallow clone does not hold can be read.
func ciPipelineCommit(revision, dir string) (string, error) {
	if id := strings.ToLower(revision); ciCommitID.MatchString(id) {
		return id, nil
	}
	if strings.HasPrefix(revision, "-") {
		return "", clicore.NewError("--commit is not a revision: "+revision, clicore.ExitUsage)
	}
	id, err := ciPipelineGit(dir, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil || !ciCommitID.MatchString(id) {
		return "", clicore.NewError("--commit "+revision+" is no commit of this repository; fetch it, or pass its full id", clicore.ExitUsage)
	}
	return id, nil
}

// --- requests ---

// ciPipelineCall issues one neutral read and projects its answer onto T.
func ciPipelineCall[T any, R any](reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, target string,
	call func(context.Context, *deliveryapiclient.DeliveryClient) (*R, error),
) (*T, error) {
	return ciCallWith[T](reqCtx, ctx, verb, call, func(err error) error {
		return ciPipelineCallError(reqCtx, verb, target, err)
	})
}

// ciPipelineCallError is ciCallError with the words of the neutral routes: a
// 404 there never means an unknown run. The server's own message leads.
func ciPipelineCallError(reqCtx context.Context, verb, target string, err error) error {
	status := clicore.ServiceStatus(err)
	meaning := ""
	switch status {
	case http.StatusNotFound:
		if verb == "view" || verb == "logs" {
			meaning = " (404 — this workspace has no such pipeline or job, or you may not read its repository)"
		} else {
			meaning = " (404 — Cloud does not serve the reads of any CI to this workspace, or you may not read it)"
		}
	case http.StatusTooManyRequests:
		meaning = " (429 — the CI limits how often it is read: try again in a minute)"
	default:
		return ciCallError(reqCtx, verb, target, err)
	}
	message := clicore.FirstString(clicore.ServiceMessage(err), clicore.StatusLine(status))
	return ciStatusError{status: status, err: clicore.NewError("cloud ci "+verb+": "+message+meaning, clicore.ExitAPI)}
}

func ciGetPipelines(reqCtx context.Context, ctx *clicore.WorkspaceContext, query deliveryapiclient.GetV1WorkspacesCiPipelinesQuery) (*CIPipelineList, error) {
	return ciPipelineCall[CIPipelineList](reqCtx, ctx, "list", ctx.WorkspaceURL("/ci/pipelines"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.PipelinesResponse, error) {
			return api.GetV1WorkspacesCiPipelines(callCtx, deliveryapiclient.GetV1WorkspacesCiPipelinesInput{
				Path:  deliveryapiclient.GetV1WorkspacesCiPipelinesPath{Workspace: ctx.WorkspaceID},
				Query: query,
			})
		})
}

func ciGetProviders(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb string) (*CIProviderList, error) {
	return ciPipelineCall[CIProviderList](reqCtx, ctx, verb, ctx.WorkspaceURL("/ci/providers"),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.ProvidersResponse, error) {
			return api.GetV1WorkspacesCiProviders(callCtx, deliveryapiclient.GetV1WorkspacesCiProvidersInput{
				Path: deliveryapiclient.GetV1WorkspacesCiProvidersPath{Workspace: ctx.WorkspaceID},
			})
		})
}

func ciGetPipeline(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, ref string) (*CIPipelineDetail, error) {
	return ciPipelineCall[CIPipelineDetail](reqCtx, ctx, verb, ctx.WorkspaceURL("/ci/pipelines/"+url.PathEscape(ref)),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.PipelineResponse, error) {
			return api.GetV1WorkspacesWorkspaceCiPipelinesPipeline(callCtx, deliveryapiclient.GetV1WorkspacesWorkspaceCiPipelinesPipelineInput{
				Path: deliveryapiclient.GetV1WorkspacesWorkspaceCiPipelinesPipelinePath{Workspace: ctx.WorkspaceID, Pipeline: ref},
			})
		})
}

func ciGetPipelineJob(reqCtx context.Context, ctx *clicore.WorkspaceContext, verb, ref, job string) (*CIPipelineJobDetail, error) {
	return ciPipelineCall[CIPipelineJobDetail](reqCtx, ctx, verb, ciPipelineJobURL(ctx, ref, job),
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.JobResponse, error) {
			return api.GetV1WorkspacesCiPipelinesJobs(callCtx, deliveryapiclient.GetV1WorkspacesCiPipelinesJobsInput{
				Path: deliveryapiclient.GetV1WorkspacesCiPipelinesJobsPath{Workspace: ctx.WorkspaceID, Pipeline: ref, Job: job},
			})
		})
}

func ciPipelineJobURL(ctx *clicore.WorkspaceContext, ref, job string) string {
	return ctx.WorkspaceURL("/ci/pipelines/" + url.PathEscape(ref) + "/jobs/" + url.PathEscape(job))
}

// ciLogTailProbe records whether a log answer said it holds the end of the log
// only: the generated client hands over the bytes and no header.
type ciLogTailProbe struct {
	base http.RoundTripper
	tail atomic.Bool
}

func (p *ciLogTailProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(req)
	if err == nil && strings.EqualFold(resp.Header.Get(ciLogTailHeader), "true") {
		p.tail.Store(true)
	}
	return resp, err
}

// ciGetPipelineLog reads one job's log, and whether it is the end of it only.
func ciGetPipelineLog(reqCtx context.Context, ctx *clicore.WorkspaceContext, ref, job string) ([]byte, bool, error) {
	probe := &ciLogTailProbe{base: http.DefaultTransport}
	base := ctx.IO.Client
	if base == nil {
		base = http.DefaultClient
	}
	if base.Transport != nil {
		probe.base = base.Transport
	}
	httpClient := *base
	httpClient.Transport = probe
	probed := *ctx
	probed.IO.Client = &httpClient
	api, err := newDeliveryAPIClient(&probed)
	if err != nil {
		return nil, false, err
	}
	out, err := clicore.CallWithSession(reqCtx, &probed, func(callCtx context.Context) (*deliveryapiclient.GetV1WorkspacesCiPipelinesJobsLogOutput, error) {
		return api.GetV1WorkspacesCiPipelinesJobsLog(callCtx, deliveryapiclient.GetV1WorkspacesCiPipelinesJobsLogInput{
			Path: deliveryapiclient.GetV1WorkspacesCiPipelinesJobsLogPath{Workspace: ctx.WorkspaceID, Pipeline: ref, Job: job},
		})
	})
	if perrors.Is(err, client.CodeClientResponse) && reqCtx.Err() == nil {
		return nil, false, clicore.NewError("cloud ci logs: the log was not read whole; it is larger than this command reads, or the answer is no log", clicore.ExitAPI)
	}
	if err != nil {
		return nil, false, ciPipelineCallError(reqCtx, "logs", ciPipelineJobURL(ctx, ref, job)+"/log", err)
	}
	return out.Body, probe.tail.Load(), nil
}

// --- list ---

func ciPipelineList(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if len(ciPositionals(args)) > 1 {
		return clicore.NewError("cloud ci list takes no positional", clicore.ExitUsage)
	}
	if err := ciRefuseFlags(params, "--commit or --change", "branch", "ref", "sha", "pr", "status", "phase", "age", "pipeline", "job", "raw"); err != nil {
		return err
	}
	query, asked, err := ciPipelineQuery(params, workspaceRoot)
	if err != nil {
		return err
	}
	ctx, err := resolvePipelineContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	answer, err := ciGetPipelines(reqCtx, ctx, query)
	if err != nil {
		return err
	}
	structured := clicore.StructuredOutput(params)
	note := ioctx.Stdout
	if structured {
		note = ciStderr(ioctx)
		for i := range answer.Pipelines {
			ioctx.Stdout(ciCompactJSON(answer.Pipelines[i]))
		}
	} else if len(answer.Pipelines) > 0 {
		ciRenderPipelines(ioctx, answer.Pipelines)
		note("Read one with `putnami cloud ci view --pipeline <ref>`.")
	}
	if answer.Truncated {
		if len(answer.Pipelines) < ciPipelineListMax {
			note("… older pipelines were left out; raise --limit (" + strconv.Itoa(ciPipelineListMax) + " at most).")
		} else {
			note("… older pipelines were left out; " + strconv.Itoa(ciPipelineListMax) + " is the most one list reads.")
		}
	}
	if len(answer.Pipelines) > 0 {
		return nil
	}
	note("No pipeline found for " + asked + ".")
	return ciNoteProviders(reqCtx, ctx, note)
}

// ciPipelineQuery is the request of `list --commit|--change`, and the words
// that name what it asked for.
func ciPipelineQuery(params map[string]any, dir string) (deliveryapiclient.GetV1WorkspacesCiPipelinesQuery, string, error) {
	var query deliveryapiclient.GetV1WorkspacesCiPipelinesQuery
	revision, change := clicore.ScalarStringParam(params, "commit"), clicore.ScalarStringParam(params, "change")
	if revision != "" && change != "" {
		return query, "", clicore.NewError("pass --commit or --change, not both", clicore.ExitUsage)
	}
	if text := clicore.ScalarStringParam(params, "limit"); text != "" {
		limit, err := strconv.ParseInt(text, 10, 64)
		if err != nil || limit < 1 || limit > ciPipelineListMax {
			return query, "", clicore.NewError(fmt.Sprintf("--limit must be 1 to %d with --commit or --change (got %s)", ciPipelineListMax, text), clicore.ExitUsage)
		}
		query.Limit = &limit
	}
	asked := ""
	if change != "" {
		number, err := strconv.ParseInt(strings.TrimLeft(change, "#!"), 10, 64)
		if err != nil || number < 1 {
			return query, "", clicore.NewError("--change must be the number of a pull or merge request (got "+change+")", clicore.ExitUsage)
		}
		query.Change = &number
		asked = "change " + strconv.FormatInt(number, 10)
	} else {
		commit, err := ciPipelineCommit(revision, dir)
		if err != nil {
			return query, "", err
		}
		query.Commit = &commit
		asked = "commit " + shortSHA(commit)
	}
	repo, err := ciPipelineRepository(params, dir)
	if err != nil {
		return query, "", err
	}
	query.Repository = &repo
	return query, asked + " of " + repo, nil
}

func ciRenderPipelines(ioctx clicore.IO, pipelines []CIPipeline) {
	rows := make([][]string, 0, len(pipelines))
	for i := range pipelines {
		p := &pipelines[i]
		rows = append(rows, []string{
			ciPlain(p.Provider), dash(ciPlain(p.WorkflowName)), dash(ciPlain(ciStateOf(p.Status, p.Conclusion))), shortSHA(ciPlain(p.HeadSHA)),
			dash(ciPipelineEvent(p)), ciSpan(p.StartedAt, p.CompletedAt), ciFormatTime(deref(p.CreatedAt)), ciPlain(p.Ref),
		})
	}
	ciRenderTable(ioctx, []string{"CI", "WORKFLOW", "STATE", "SHA", "EVENT", "DURATION", "CREATED", "PIPELINE"}, rows)
}

// ciNoteProviders says which CI Cloud reads, so a list with no pipeline tells a
// commit with no CI from a CI that Cloud may not read. Only a canceled read
// fails the list.
func ciNoteProviders(reqCtx context.Context, ctx *clicore.WorkspaceContext, note func(string)) error {
	answer, err := ciGetProviders(reqCtx, ctx, "list")
	if err != nil {
		if reqCtx.Err() != nil {
			return err
		}
		note("The state of each CI could not be read: " + err.Error())
		return nil
	}
	if len(answer.Providers) == 0 {
		note("Cloud reads no CI for this workspace.")
		return nil
	}
	note("What Cloud reads for this workspace:")
	for i := range answer.Providers {
		p := &answer.Providers[i]
		line := "  " + ciPlain(p.Provider) + ": " + ciPlain(p.State)
		if p.Reason != "" {
			line += " (" + ciPlain(p.Reason) + ")"
		}
		note(line)
	}
	return nil
}

// --- providers ---

func ciProviders(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	ctx, err := resolvePipelineContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	answer, err := ciGetProviders(reqCtx, ctx, "providers")
	if err != nil {
		return err
	}
	if clicore.StructuredOutput(params) {
		for i := range answer.Providers {
			ioctx.Stdout(ciCompactJSON(answer.Providers[i]))
		}
		return nil
	}
	if len(answer.Providers) == 0 {
		ioctx.Stdout("Cloud reads no CI for this workspace.")
		return nil
	}
	rows := make([][]string, 0, len(answer.Providers))
	for i := range answer.Providers {
		p := &answer.Providers[i]
		rows = append(rows, []string{
			ciPlain(p.Provider), dash(ciPlain(p.State)), ciFormatTime(deref(p.LastSeen)),
			dash(ciPlain(strings.Join(p.Capabilities, ","))), dash(ciPlain(p.Reason)),
		})
	}
	ciRenderTable(ioctx, []string{"CI", "STATE", "LAST SEEN", "OFFERS", "REASON"}, rows)
	return nil
}

// --- view ---

func ciPipelineView(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if ciRunID(args) != "" {
		return clicore.NewError("pass a run id or --pipeline, not both", clicore.ExitUsage)
	}
	if err := ciRefuseFlags(params, "view --pipeline", "raw"); err != nil {
		return err
	}
	ctx, err := resolvePipelineContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	ref := ciPipelineRef(params)
	detail, err := ciGetPipeline(reqCtx, ctx, "view", ref)
	if err != nil {
		return err
	}
	structured := clicore.StructuredOutput(params)
	if selector := clicore.ScalarStringParam(params, "job"); selector != "" {
		job, err := ciSelectJob(detail.Pipeline.Jobs, selector)
		if err != nil {
			return err
		}
		answer, err := ciGetPipelineJob(reqCtx, ctx, "view", ref, job.Ref)
		if err != nil {
			return err
		}
		if structured {
			ioctx.Stdout(ciCompactJSON(answer))
			return nil
		}
		ciRenderPipelineJob(ioctx, ref, answer, detail.Capabilities)
		return nil
	}

	answer := CIPipelineFailures{Pipeline: detail.Pipeline, Capabilities: detail.Capabilities, Failures: []CIPipelineJobDetail{}}
	if answer.Capabilities == nil {
		answer.Capabilities = []string{}
	}
	for i := range detail.Pipeline.Jobs {
		job := &detail.Pipeline.Jobs[i]
		if !job.failsThePipeline() {
			continue
		}
		if len(answer.Failures) == ciPipelineFailureLimit {
			answer.OmittedFailures++
			continue
		}
		failure, err := ciGetPipelineJob(reqCtx, ctx, "view", ref, job.Ref)
		if err != nil {
			if reqCtx.Err() != nil {
				return err
			}
			// The job is still named: one unread job never hides the others.
			ciStderr(ioctx)("note: job " + ciPlain(job.Name) + " was not read whole: " + err.Error())
			failure = &CIPipelineJobDetail{Job: *job}
		}
		failure.Pipeline = nil
		answer.Failures = append(answer.Failures, *failure)
	}
	if structured {
		ioctx.Stdout(ciCompactJSON(answer))
		return nil
	}
	ciRenderPipeline(ioctx, &answer)
	return nil
}

// ciSelectJob finds the job a selector names: a reference, the provider's id,
// or a name one job alone carries.
func ciSelectJob(jobs []CIPipelineJob, selector string) (*CIPipelineJob, error) {
	var named []*CIPipelineJob
	for i := range jobs {
		if jobs[i].Ref == selector || jobs[i].JobID == selector {
			return &jobs[i], nil
		}
		if jobs[i].Name == selector {
			named = append(named, &jobs[i])
		}
	}
	switch len(named) {
	case 0:
		return nil, ciJobChoiceError("no job is named "+ciPlain(selector), jobs)
	case 1:
		return named[0], nil
	}
	shared := make([]CIPipelineJob, 0, len(named))
	for _, job := range named {
		shared = append(shared, *job)
	}
	return nil, ciJobChoiceError(fmt.Sprintf("%d jobs are named %s", len(named), ciPlain(selector)), shared)
}

// ciJobChoiceError refuses to guess a job, and lists the ones to pick from.
func ciJobChoiceError(reason string, jobs []CIPipelineJob) error {
	if len(jobs) == 0 {
		return clicore.NewError("cloud ci: "+reason+"; the pipeline has no job yet", clicore.ExitUsage)
	}
	var b strings.Builder
	b.WriteString("cloud ci: " + reason + "; pass --job <id>:")
	for _, job := range ciFailedFirst(jobs) {
		b.WriteString("\n  " + ciPlain(clicore.FirstString(job.JobID, job.Ref)) + "  " + dash(ciPlain(ciStateOf(job.Status, job.Conclusion))) + "  " + ciPlain(job.Name))
	}
	return clicore.NewError(b.String(), clicore.ExitUsage)
}

// ciFailedFirst orders the jobs for a reader: those that fail the pipeline,
// then the other failed ones, then the rest, each in the pipeline's order.
func ciFailedFirst(jobs []CIPipelineJob) []CIPipelineJob {
	ordered := make([]CIPipelineJob, 0, len(jobs))
	for _, rank := range []func(CIPipelineJob) bool{
		CIPipelineJob.failsThePipeline,
		func(j CIPipelineJob) bool { return j.failed() && !j.failsThePipeline() },
		func(j CIPipelineJob) bool { return !j.failed() },
	} {
		for _, job := range jobs {
			if rank(job) {
				ordered = append(ordered, job)
			}
		}
	}
	return ordered
}

func ciRenderPipeline(ioctx clicore.IO, answer *CIPipelineFailures) {
	p := &answer.Pipeline
	field := func(label, value string) {
		if value != "" {
			ioctx.Stdout(fmt.Sprintf("%-13s%s", label+":", value))
		}
	}
	field("Pipeline", strings.TrimSpace(ciPlain(p.WorkflowName)+" "+ciParenthesized(ciPlain(p.Provider))))
	field("State", ciStateLine(p.Status, p.Conclusion, p.ProviderConclusion))
	field("Repository", ciPlain(p.Repository))
	field("Commit", strings.TrimSpace(ciPlain(p.HeadSHA)+" "+ciParenthesized(ciPlain(p.GitRef))))
	field("Event", ciPipelineEvent(p))
	if p.Attempt > 1 {
		field("Attempt", strconv.FormatInt(p.Attempt, 10))
	}
	field("Workflow", ciPlain(p.WorkflowPath))
	if p.CreatedAt != nil {
		field("Created", ciFormatTime(*p.CreatedAt))
	}
	if p.QueueMS != nil {
		field("Queued", ciFormatDurationMS(*p.QueueMS))
	}
	if p.StartedAt != nil && p.CompletedAt != nil {
		field("Duration", ciSpan(p.StartedAt, p.CompletedAt))
	}
	field("Page", ciPlain(p.URL))
	field("Reference", ciPlain(p.Ref))

	ioctx.Stdout("")
	if len(p.Jobs) == 0 {
		ioctx.Stdout("No job yet.")
	} else {
		rows := make([][]string, 0, len(p.Jobs))
		for _, job := range ciFailedFirst(p.Jobs) {
			rows = append(rows, []string{
				ciJobState(job), ciPlain(job.Name), dash(ciPlain(job.Stage)),
				ciSpan(job.StartedAt, job.CompletedAt), ciPlain(clicore.FirstString(job.JobID, job.Ref)),
			})
		}
		ciRenderTable(ioctx, []string{"STATE", "JOB", "STAGE", "DURATION", "ID"}, rows)
	}
	if p.JobsCounted > p.JobsKept {
		ioctx.Stdout(fmt.Sprintf("… Cloud kept %d of the pipeline's %d jobs.", p.JobsKept, p.JobsCounted))
	}

	if len(answer.Failures) == 0 && p.Conclusion != "" && p.Conclusion != "success" && p.Conclusion != "skipped" && p.Conclusion != "neutral" {
		ioctx.Stdout("")
		ioctx.Stdout("No job Cloud kept explains it: the CI says " + ciPlain(clicore.FirstString(p.ProviderConclusion, p.Conclusion)) + ".")
	}
	for i := range answer.Failures {
		ioctx.Stdout("")
		ciRenderPipelineJob(ioctx, p.Ref, &answer.Failures[i], answer.Capabilities)
	}
	if answer.OmittedFailures > 0 {
		ioctx.Stdout("")
		ioctx.Stdout(fmt.Sprintf("… %d more failed jobs; read one with --job <id>.", answer.OmittedFailures))
	}
}

func ciRenderPipelineJob(ioctx clicore.IO, ref string, answer *CIPipelineJobDetail, capabilities []string) {
	job := &answer.Job
	ioctx.Stdout("Job " + ciPlain(job.Name) + ": " + ciJobState(*job))
	if len(answer.Steps) == 0 && job.FailedStep != "" {
		ioctx.Stdout("  failed step: " + ciPlain(job.FailedStep))
	}
	for i := range answer.Steps {
		step := &answer.Steps[i]
		if step.Conclusion == "failure" || step.Conclusion == "timed_out" {
			ioctx.Stdout(fmt.Sprintf("  failed step %d: %s", step.Number, ciPlain(step.Name)))
		}
	}
	switch {
	case answer.Excerpt != nil:
		ioctx.Stdout(fmt.Sprintf("  log, lines %d to %d:", answer.Excerpt.FromLine, answer.Excerpt.ToLine))
		for _, line := range ciLogLines([]byte(answer.Excerpt.Text)) {
			ioctx.Stdout("    " + ciPlain(line))
		}
	case job.ExcerptState == "pending":
		ioctx.Stdout("  log excerpt: not read yet; run the command again in a minute")
	case job.ExcerptState == "unavailable":
		ioctx.Stdout("  log excerpt: not available " + ciParenthesized(ciPlain(clicore.FirstString(job.ExcerptReason, "no reason given"))))
	case job.failed():
		ioctx.Stdout("  log excerpt: none kept")
	}
	for _, capability := range capabilities {
		if capability == "jobLogs" {
			ioctx.Stdout("  full log: putnami cloud ci logs --pipeline " + ciPlain(ref) + " --job " + ciPlain(clicore.FirstString(job.JobID, job.Ref)))
			return
		}
	}
	if job.URL != "" {
		ioctx.Stdout("  page: " + ciPlain(job.URL))
	}
}

// --- logs ---

func ciPipelineLogs(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	if ciRunID(args) != "" {
		return clicore.NewError("pass a run id or --pipeline, not both", clicore.ExitUsage)
	}
	if err := ciRefuseFlags(params, "--pipeline", "follow", "phase", "project", "q", "level", "from", "to", "cursor", "limit"); err != nil {
		return err
	}
	ctx, err := resolvePipelineContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	ref := ciPipelineRef(params)
	detail, err := ciGetPipeline(reqCtx, ctx, "logs", ref)
	if err != nil {
		return err
	}
	job, err := ciLogJob(detail.Pipeline.Jobs, clicore.ScalarStringParam(params, "job"))
	if err != nil {
		return err
	}
	log, tail, err := ciGetPipelineLog(reqCtx, ctx, ref, job.Ref)
	if err != nil {
		if page := clicore.FirstString(job.URL, detail.Pipeline.URL); page != "" && reqCtx.Err() == nil {
			ciStderr(ioctx)("note: the job's page is " + ciPlain(page))
		}
		return err
	}
	if tail {
		ciStderr(ioctx)("note: this is the end of the log only; Cloud does not read the start of a log this long")
	}
	structured, raw := clicore.StructuredOutput(params), clicore.Truthy(clicore.Param(params, "raw"))
	lines := ciLogLines(log)
	for i, line := range lines {
		switch {
		case structured:
			ioctx.Stdout(ciCompactJSON(ciPipelineLogLine{Line: i + 1, Text: line}))
		case raw:
			ioctx.Stdout(line)
		default:
			ioctx.Stdout(ciPlain(line))
		}
	}
	if len(lines) == 0 {
		ciStderr(ioctx)("note: the log of " + ciPlain(job.Name) + " is empty")
	}
	return nil
}

// ciLogJob is the job whose log `logs` reads: the selected one, else the only
// one that fails the pipeline, else the only one.
func ciLogJob(jobs []CIPipelineJob, selector string) (*CIPipelineJob, error) {
	if selector != "" {
		return ciSelectJob(jobs, selector)
	}
	var failed []*CIPipelineJob
	for i := range jobs {
		if jobs[i].failsThePipeline() {
			failed = append(failed, &jobs[i])
		}
	}
	switch {
	case len(failed) == 1:
		return failed[0], nil
	case len(jobs) == 1:
		return &jobs[0], nil
	}
	return nil, ciJobChoiceError("the pipeline has several jobs", jobs)
}

// ciLogLines splits a log into lines, without the line ends. Bytes that are no
// text become the replacement character.
func ciLogLines(log []byte) []string {
	if len(log) == 0 {
		return nil
	}
	lines := bytes.Split(bytes.TrimSuffix(log, []byte("\n")), []byte("\n"))
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = strings.ToValidUTF8(string(bytes.TrimSuffix(line, []byte("\r"))), "�")
	}
	return out
}

// --- text ---

// ciTerminalSequence matches a terminal control sequence: CSI, then the string
// sequences (OSC, DCS, SOS, PM, APC), then the other escapes.
var ciTerminalSequence = regexp.MustCompile("(?:\x1b\\[|\u009b)[0-?]*[ -/]*[@-~]" +
	"|\x1b[\\]PX^_][^\x07\x1b]*(?:\x07|\x1b\\\\)" +
	"|\x1b[ -/]*[0-Z\\\\^-~]")

// ciPlain is a text a CI wrote, safe on a terminal: no control sequence, no
// control character but the tab, and no character that reorders the line. Job
// names and logs come from whoever can open a change.
func ciPlain(text string) string {
	text = ciTerminalSequence.ReplaceAllString(strings.ToValidUTF8(text, "�"), "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return r
		case r == '\r' || r == '\n':
			return ' '
		case r < 0x20, r >= 0x7f && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
			r == 0x2028, r == 0x2029:
			return -1
		}
		return r
	}, text)
}

// ciStderr is the stderr sink, or a sink that drops the line.
func ciStderr(ioctx clicore.IO) func(string) {
	if ioctx.Stderr == nil {
		return func(string) {}
	}
	return ioctx.Stderr
}

func ciStateOf(status, conclusion string) string {
	return clicore.FirstString(conclusion, status)
}

// ciStateLine is the state, with the CI's own word when it says more.
func ciStateLine(status, conclusion, providerConclusion string) string {
	state := ciPlain(ciStateOf(status, conclusion))
	if own := ciPlain(providerConclusion); own != "" && !strings.EqualFold(own, state) {
		state += " " + ciParenthesized(own)
	}
	return state
}

func ciJobState(job CIPipelineJob) string {
	state := dash(ciStateLine(job.Status, job.Conclusion, job.ProviderConclusion))
	switch {
	case job.Retried:
		state += ", retried"
	case job.failed() && job.AllowedFailure:
		state += ", allowed"
	}
	return state
}

func ciPipelineEvent(p *CIPipeline) string {
	event := ciPlain(p.Event)
	if p.Change > 0 {
		event = strings.TrimSpace(event + " #" + strconv.FormatInt(p.Change, 10))
	}
	if p.Fork {
		event = strings.TrimSpace(event + " (fork)")
	}
	return event
}

func ciParenthesized(text string) string {
	if text == "" {
		return ""
	}
	return "(" + text + ")"
}

func ciSpan(from, to *time.Time) string {
	if from == nil || to == nil || to.Before(*from) {
		return "-"
	}
	return ciFormatDurationSeconds(int(to.Sub(*from) / time.Second))
}
