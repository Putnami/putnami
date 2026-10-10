// putnami cloud ci report — submit a delivery run's conclusion for the current
// pushed commit to the Delivery records ingest (control-plane
// POST /v1/delivery/records), plus the parsed JSONL report batch
// (POST /v1/delivery/records/reports) when a wrapped command emits one.
//
// The command is the client half of the delivery records contract: it resolves
// the subject (HEAD SHA + origin repository) from git, REFUSES to send anything
// for a dirty working tree or an unpushed HEAD (delivery records bind to
// resolvable, pushed commits only), derives provenance (local by default, gha
// under GitHub Actions), and POSTs a conclusion-only submission authenticated
// with the existing workspace-scoped token machinery. In the wrapper form
// (`putnami cloud ci report -- putnami … --output=jsonl`) the wrapped command's
// stdout is tee'd through internal/reportstream and the folded
// tests/coverage/builds batch is POSTed at run end. The batch is
// fail-soft by contract: a failed or partial report ingestion never changes
// the run's conclusion, which comes from the wrapped exit status alone.
package deliverycli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/client"
	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/reportstream"
	perrors "go.putnami.dev/errors"
)

// recordsIngestPath is the control-plane ingest endpoint for delivery run
// submissions. Keep in lockstep with the delivery API's records contract
// RecordsIngestPath — the CLI module deliberately stays off the server
// dependency tree (it is also compiled under GOWORK=off), so the path and the
// wire body below are local twins of the apis package contract.
const recordsIngestPath = "/v1/delivery/records"

// recordsReportsIngestPath is the sibling report-batch endpoint. Keep in
// lockstep with the delivery API's records contract RecordsReportsIngestPath.
const recordsReportsIngestPath = recordsIngestPath + "/reports"

// reportSubmitAttempts bounds the POST attempts for one invocation. Every
// attempt reuses the SAME submission id, so a retry after an ambiguous failure
// is idempotent on the server (re-POST returns the existing run).
const reportSubmitAttempts = 3

// reportRetrySleep delays between submit attempts; a package var so tests run
// without wall-clock waits.
var reportRetrySleep = func(attempt int) {
	time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
}

// reportGitRun is the git seam: it runs one git command in dir and returns its
// trimmed stdout. A package var so unit tests never depend on the ambient
// repository state.
var reportGitRun = func(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// reportRunWrapped is the wrapped-command seam: it runs `putnami cloud ci report
// -- <cmd…>`'s command and returns its exit code. tee, when non-nil, receives
// a copy of the wrapped command's stdout (the JSONL report stream) while the
// terminal still sees everything. A package var so unit tests derive
// conclusions without spawning processes.
var reportRunWrapped = func(argv []string, dir string, tee io.Writer) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: running the user's own wrapped command is the feature
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	if tee != nil {
		cmd.Stdout = io.MultiWriter(os.Stdout, tee)
	}
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// reportSubject is the resolved delivery subject: the pushed commit this
// submission is about.
type reportSubject struct {
	Provider string
	RepoKey  string
	SHA      string
	Branch   string
}

// reportSubmitRequest mirrors the delivery records ingest wire body
// (the delivery API's SubmitRequest). Keep the JSON keys in lockstep;
// TestReportSubmitRequestWireKeys pins them.
type reportSubmitRequest struct {
	Workspace    string `json:"workspace,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	Branch       string `json:"branch,omitempty"`
	Provenance   string `json:"provenance,omitempty"`
	SubmissionID string `json:"submissionId"`
	Status       string `json:"status,omitempty"`
	Conclusion   string `json:"conclusion,omitempty"`
}

// reportBatchRequest mirrors the delivery report-batch ingest wire body
// (the delivery API's ReportBatchRequest). Keep the JSON keys in
// lockstep; TestReportBatchWireKeys pins them, including the nested report
// payload twins in internal/reportstream.
type reportBatchRequest struct {
	Workspace    string                       `json:"workspace,omitempty"`
	SubmissionID string                       `json:"submissionId"`
	Stream       reportBatchStream            `json:"stream"`
	Tests        *reportstream.TestsReport    `json:"tests,omitempty"`
	Coverage     *reportstream.CoverageReport `json:"coverage,omitempty"`
	Builds       *reportstream.BuildsReport   `json:"builds,omitempty"`
}

// reportBatchStream mirrors apis.ReportStreamStats: the parse provenance the
// server uses to mark degraded ingestion (reports: partial).
type reportBatchStream struct {
	Version        string `json:"version,omitempty"`
	Lines          int    `json:"lines,omitempty"`
	MalformedLines int    `json:"malformedLines,omitempty"`
	UnknownEvents  int    `json:"unknownEvents,omitempty"`
	OversizedLines int    `json:"oversizedLines,omitempty"`
	ElidedLines    int    `json:"elidedLines,omitempty"`
}

// Report submits a delivery run conclusion for the current pushed commit.
//
//	putnami cloud ci report --conclusion success|failure|…   pre-captured outcome
//	putnami cloud ci report --exit-code <n>                  0 ⇒ success, else failure
//	putnami cloud ci report -- <cmd…>                        run cmd; its exit code decides
//
// The wrapper form additionally tees the wrapped command's stdout through the
// JSONL parser and, when the stream carried report data (`--output=jsonl`),
// POSTs the folded tests/coverage/builds batch after the run submission.
// Report ingestion is fail-soft: a failed batch POST warns on stderr and never
// changes the command's outcome or the run's conclusion.
//
// Client-side boundary (enforced, no request sent): the working tree must be
// clean and HEAD must be pushed to a remote. Provenance is `local` unless
// $GITHUB_ACTIONS is set (`gha`); v1 accepts no other value.
func Report(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	pre, wrapped, hasWrapped := splitWrappedCommand(args)
	if hasWrapped {
		scrubWrappedFlagParams(params, args, pre)
	}
	if clicore.FirstPositional(pre) == "help" {
		return reportHelp(params, ioctx)
	}

	// Resolve + refuse BEFORE running any wrapped command: a dirty tree or an
	// unpushed HEAD never sends a request and never burns a test run.
	subject, err := resolveReportSubject(workspaceRoot)
	if err != nil {
		return err
	}

	var stream *reportstream.Parser
	if hasWrapped {
		stream = reportstream.NewParser()
	}
	status := clicore.FirstString(clicore.StringParam(params, "status"), "completed")
	conclusion, wrappedExit, err := resolveReportConclusion(params, wrapped, hasWrapped, workspaceRoot, stream)
	if err != nil {
		return err
	}

	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return err
	}
	workspaceID := clicore.StringValue(link["workspace_id"])
	auth, err := clicore.WorkspaceAuth(params, env, ioctx, workspaceID)
	if err != nil {
		return err
	}
	controlURL := clicore.ControlPlaneBaseURL(params, env, clicore.StringValue(link["control_plane_url"]))

	submissionID, err := generateSubmissionID()
	if err != nil {
		return err
	}
	body := reportSubmitRequest{
		// The workspace travels as an assertion only — the server derives the
		// authoritative workspace from the token claim and rejects a mismatch.
		Workspace:    workspaceID,
		Provider:     subject.Provider,
		Repo:         subject.RepoKey,
		SHA:          subject.SHA,
		Branch:       subject.Branch,
		Provenance:   reportProvenance(env),
		SubmissionID: submissionID,
		Status:       status,
		Conclusion:   conclusion,
	}
	resp, err := postReport(ioctx, controlURL, recordsIngestPath, auth.AccessToken, body,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient, raw []byte) (*deliveryapiclient.SubmitResponse, error) {
			var in deliveryapiclient.CreateV1DeliveryRecordsInput
			if err := json.Unmarshal(raw, &in.Body); err != nil {
				return nil, err
			}
			return api.CreateV1DeliveryRecords(callCtx, in)
		})
	if err != nil {
		return err
	}

	// The run's conclusion is recorded — everything from here on is fail-soft.
	// The report batch is a side channel: a failed or partial ingestion warns
	// but never flips the reported outcome or this command's exit code.
	reportsState := postReportBatch(ioctx, controlURL, auth.AccessToken, workspaceID, submissionID, stream)

	result := map[string]any{
		"run_id":        deref(resp.RunId),
		"subject_id":    deref(resp.SubjectId),
		"status":        deref(resp.Status),
		"conclusion":    deref(resp.Conclusion),
		"provenance":    deref(resp.Provenance),
		"verified":      deref(resp.Verified),
		"idempotent":    deref(resp.Idempotent),
		"workspace_id":  workspaceID,
		"repo":          subject.RepoKey,
		"sha":           subject.SHA,
		"submission_id": submissionID,
		"reports":       reportsState,
	}
	clicore.WriteResult(result, params, ioctx, fmt.Sprintf(
		"Reported delivery run %s for %s@%s (%s).",
		deref(resp.Conclusion), subject.RepoKey, shortSHA(subject.SHA), deref(resp.Provenance)))

	// Preserve CI semantics for the wrapper form: the run was reported, but the
	// wrapped command failed, so `putnami cloud ci report -- <cmd…>` must fail too.
	if hasWrapped && wrappedExit != 0 {
		return clicore.NewError(fmt.Sprintf("wrapped command exited with code %d (delivery run reported as failure)", wrappedExit), wrappedExit)
	}
	return nil
}

func reportHelp(params map[string]any, ioctx clicore.IO) error {
	commands := []map[string]string{
		{"command": "cloud ci report --conclusion <success|failure|canceled|skipped|neutral|timed_out>", "description": "submit a pre-captured run conclusion for the current pushed commit"},
		{"command": "cloud ci report --exit-code <n>", "description": "derive the conclusion from an exit code (0 = success, else failure)"},
		{"command": "cloud ci report -- <cmd…>", "description": "run the command, report success/failure from its exit code, and propagate it; a `putnami … --output=jsonl` stream is tee'd and its test/coverage/build reports submitted"},
	}
	if clicore.StructuredOutput(params) {
		clicore.WriteResult(map[string]any{"commands": commands}, params, ioctx, "")
		return nil
	}
	ioctx.Stdout("@putnami/cloud ci report commands:")
	for _, c := range commands {
		ioctx.Stdout(fmt.Sprintf("  putnami %-80s %s", c["command"], c["description"]))
	}
	ioctx.Stdout("")
	ioctx.Stdout("Refuses (nothing sent) on a dirty working tree or an unpushed HEAD; provenance is local, or gha under GitHub Actions.")
	ioctx.Stdout("Report ingestion is fail-soft: a failed or partial report batch never changes the run's conclusion or this command's exit code.")
	return nil
}

// resolveReportSubject resolves the delivery subject from git and enforces the
// client-side boundary: a dirty working tree or an unpushed HEAD is refused
// with an actionable error BEFORE any request is prepared.
func resolveReportSubject(dir string) (*reportSubject, error) {
	sha, err := reportGitRun(dir, "rev-parse", "HEAD")
	if err != nil || sha == "" {
		return nil, clicore.NewError("cloud ci report requires a git repository with at least one commit ("+errText(err)+")", clicore.ExitUsage)
	}
	dirty, err := reportGitRun(dir, "status", "--porcelain")
	if err != nil {
		return nil, clicore.NewError("cloud ci report could not check the working tree: "+errText(err), clicore.ExitUsage)
	}
	if dirty != "" {
		return nil, clicore.NewError(
			"refusing to report: the working tree has uncommitted changes — delivery records bind to a pushed commit; commit (or stash) and push, then re-run `putnami cloud ci report`",
			clicore.ExitUsage)
	}
	remoteRefs, err := reportGitRun(dir, "branch", "-r", "--contains", sha)
	if err != nil || strings.TrimSpace(remoteRefs) == "" {
		return nil, clicore.NewError(fmt.Sprintf(
			"refusing to report: HEAD commit %s is not on any remote branch — push it (e.g. `git push`), then re-run `putnami cloud ci report`", shortSHA(sha)),
			clicore.ExitUsage)
	}
	branch, err := reportGitRun(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "HEAD" {
		branch = "" // detached HEAD: the subject is the SHA, no branch claim
	}
	remote, err := reportGitRun(dir, "remote", "get-url", "origin")
	if err != nil || remote == "" {
		return nil, clicore.NewError("cloud ci report requires an `origin` remote to resolve the repository ("+errText(err)+")", clicore.ExitUsage)
	}
	provider, repoKey, err := clicore.RepositoryKeyFromRemote(remote)
	if err != nil {
		return nil, clicore.NewError("cloud ci report could not derive owner/name from the origin remote: "+err.Error(), clicore.ExitUsage)
	}
	return &reportSubject{Provider: provider, RepoKey: repoKey, SHA: sha, Branch: branch}, nil
}

// resolveReportConclusion derives the run conclusion, preferring the wrapped
// command over pre-captured flags. Exactly one source must be provided. stream,
// when non-nil, tees the wrapped command's stdout (the JSONL report source).
func resolveReportConclusion(params map[string]any, wrapped []string, hasWrapped bool, dir string, stream *reportstream.Parser) (conclusion string, wrappedExit int, err error) {
	flagConclusion := strings.TrimSpace(clicore.StringParam(params, "conclusion"))
	flagExitCode := strings.TrimSpace(clicore.StringParam(params, "exit-code", "exitCode"))
	if hasWrapped {
		if flagConclusion != "" || flagExitCode != "" {
			return "", 0, clicore.NewError("cloud ci report: choose one conclusion source — a wrapped command after `--`, --conclusion, or --exit-code", clicore.ExitUsage)
		}
		if len(wrapped) == 0 {
			return "", 0, clicore.NewError("cloud ci report: no command after `--`", clicore.ExitUsage)
		}
		var tee io.Writer
		if stream != nil {
			tee = stream
		}
		code, runErr := reportRunWrapped(wrapped, dir, tee)
		if runErr != nil {
			return "", 0, clicore.NewError("cloud ci report: failed to run wrapped command: "+runErr.Error(), clicore.ExitUsage)
		}
		if code == 0 {
			return "success", 0, nil
		}
		return "failure", code, nil
	}
	if flagConclusion != "" && flagExitCode != "" {
		return "", 0, clicore.NewError("cloud ci report: --conclusion and --exit-code are mutually exclusive", clicore.ExitUsage)
	}
	if flagConclusion != "" {
		return flagConclusion, 0, nil
	}
	if flagExitCode != "" {
		code, parseErr := strconv.Atoi(flagExitCode)
		if parseErr != nil {
			return "", 0, clicore.NewError("cloud ci report: --exit-code must be an integer, got "+flagExitCode, clicore.ExitUsage)
		}
		if code == 0 {
			return "success", 0, nil
		}
		return "failure", 0, nil
	}
	return "", 0, clicore.NewError("cloud ci report requires a conclusion source: --conclusion <value>, --exit-code <n>, or a wrapped command after `--`", clicore.ExitUsage)
}

// reportProvenance derives the v1 provenance: gha under GitHub Actions, local
// otherwise. `ci` is reserved for the orchestrated runner and never sent.
func reportProvenance(env map[string]string) string {
	if clicore.EnvGet(env, "GITHUB_ACTIONS") != "" {
		return "gha"
	}
	return "local"
}

// postReportBatch folds the tee'd JSONL stream into a report batch and POSTs
// it, riding the same workspace token and submission id as the run submission
// (the server keys idempotency on submission id + report kind, so retries are
// safe). It is fail-soft by contract: any failure warns on stderr and returns
// a state string — it never returns an error, because report ingestion must
// not flip an already-reported run. Returns "none" when no stream was tee'd or
// it carried no report data.
func postReportBatch(ioctx clicore.IO, controlURL, token, workspaceID, submissionID string, stream *reportstream.Parser) string {
	if stream == nil {
		return "none"
	}
	batch := stream.Batch()
	if !batch.HasReports() {
		return "none"
	}
	body := reportBatchRequest{
		// Assertion only — the server derives the authoritative workspace from
		// the token claim and rejects a mismatch.
		Workspace:    workspaceID,
		SubmissionID: submissionID,
		Stream: reportBatchStream{
			Version:        batch.Version,
			Lines:          batch.Stats.Lines,
			MalformedLines: batch.Stats.MalformedLines,
			UnknownEvents:  batch.Stats.UnknownEvents,
			OversizedLines: batch.Stats.OversizedLines,
		},
		Tests:    batch.Tests,
		Coverage: batch.Coverage,
		Builds:   batch.Builds,
	}
	resp, err := postReport(ioctx, controlURL, recordsReportsIngestPath, token, body,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient, raw []byte) (*deliveryapiclient.ReportBatchResponse, error) {
			var in deliveryapiclient.CreateV1DeliveryRecordsReportsInput
			if err := json.Unmarshal(raw, &in.Body); err != nil {
				return nil, err
			}
			return api.CreateV1DeliveryRecordsReports(callCtx, in)
		})
	if err != nil {
		ioctx.Stderr("warning: delivery reports were not ingested (run conclusion is unaffected): " + err.Error())
		return "failed"
	}
	if deref(resp.Partial) {
		return "partial"
	}
	return "submitted"
}

// postReport POSTs a records ingest body and returns the typed response,
// retrying transient failures with the SAME submission id — the server treats a
// repeated submission id (and, for batches, submission id + kind) as idempotent,
// so a retry after an ambiguous failure can never double-record. Auth and usage
// failures are terminal (a retry cannot fix them). T is delivery-api's
// generated response (SubmitResponse or ReportBatchResponse); an empty answer
// reads as its zero value.
func postReport[T any](ioctx clicore.IO, controlURL, path, token string, body any,
	call func(context.Context, *deliveryapiclient.DeliveryClient, []byte) (*T, error),
) (*T, error) {
	url := controlURL + path
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, clicore.NewError("marshal request body: "+err.Error(), clicore.ExitAPI)
	}
	api, err := clicore.NewServiceClient[deliveryapiclient.DeliveryClient](deliveryapiclient.RegisterDeliveryClient, clicore.ServiceBindingFor(controlURL, ioctx.Client))
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < reportSubmitAttempts; attempt++ {
		if attempt > 0 {
			reportRetrySleep(attempt)
		}
		resp, err := call(client.WithForwardedUserToken(context.Background(), token), api, raw)
		if err == nil {
			if resp == nil {
				resp = new(T)
			}
			return resp, nil
		}
		lastErr = reportCallError(url, err)
		if code := clicore.ExitCode(lastErr); code == clicore.ExitAuth || code == clicore.ExitUsage {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// reportCallError renders a failed generated records call the way the CLI
// always has: "request failed for <url>: <reason>", ExitAuth on 401. The
// reason is the provider's own (records write it in the envelope message and
// the CLI binding carries it), else the status line.
func reportCallError(url string, err error) error {
	if status := clicore.ServiceStatus(err); status != 0 {
		message := clicore.FirstString(clicore.ServiceMessage(err), clicore.StatusLine(status))
		code := clicore.ExitAPI
		if status == http.StatusUnauthorized {
			code = clicore.ExitAuth
		}
		return clicore.NewError("request failed for "+url+": "+message, code)
	}
	if perrors.Is(err, client.CodeClientResponse) {
		return clicore.NewError("invalid JSON response from "+url, clicore.ExitAPI)
	}
	return clicore.NewError(fmt.Sprintf("request failed for %s: %s", url, err.Error()), clicore.ExitAPI)
}

// generateSubmissionID mints the per-invocation idempotency key: sub_ plus 32
// hex chars of crypto/rand bytes (mirrors the deploy CLI's release-id shape).
// One invocation reuses it across its own HTTP retries; a new invocation mints
// a new one, becoming a new run.
func generateSubmissionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", clicore.NewError("generate submission id: "+err.Error(), clicore.ExitAPI)
	}
	return "sub_" + hex.EncodeToString(buf), nil
}

// splitWrappedCommand splits args on the first literal "--" into the report's
// own flags and the wrapped command. The parent CLI's --putnamiContext plumbing
// pair is stripped from the wrapped segment if it landed there.
func splitWrappedCommand(args []string) (pre, wrapped []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], stripPutnamiContext(args[i+1:]), true
		}
	}
	return args, nil, false
}

func stripPutnamiContext(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--putnamiContext" {
			i++ // skip its value too
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// scrubWrappedFlagParams removes params that the dispatcher's whole-argv flag
// parse picked up from the wrapped command's own flags (everything after "--"),
// so `putnami cloud ci report -- npm test --json` never flips report's output mode
// or outcome flags. Context-provided params survive: only keys absent from the
// pre-separator parse whose value matches the polluted parse are dropped.
func scrubWrappedFlagParams(params map[string]any, args, pre []string) {
	polluted := clicore.ParseFlags(args)
	clean := clicore.ParseFlags(pre)
	for key, value := range polluted {
		if _, ok := clean[key]; ok {
			continue
		}
		if current, ok := params[key]; ok && current == value {
			delete(params, key)
		}
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func errText(err error) string {
	if err == nil {
		return "no output"
	}
	return err.Error()
}
