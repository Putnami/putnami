// putnami cloud evidence-report — upload a change evidence document
// (go.putnami.dev/protocol/evidence v1) to Delivery, as the evidence reporter
// `putnami observe --upload` hands it to.
//
// The command opens the run on POST /v1/delivery/records with the CI's
// provenance and the pull or merge request, then posts two report batches on
// POST /v1/delivery/records/reports under the same submission id: the tests and
// coverage of each component, then the components, the changed-line coverage
// and the dependency changes. The second batch rides apart because the route
// refuses a body over 1 MiB whole. The bearer is the workspace token in
// PUTNAMI_CLOUD_TOKEN; the workspace comes out of it, never out of the body.
//
// The run also names, under operational, the tree the job tested and the merge
// base its diff started from. Only the job's checkout knows them, so the
// command reads them with Git in the directory it was invoked from.
package deliverycli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// evidenceTokenEnv holds the workspace token of scope delivery.ingest.
const evidenceTokenEnv = "PUTNAMI_CLOUD_TOKEN"

// maxEvidenceBytes bounds the document the command reads.
const maxEvidenceBytes = 64 << 20

// evidenceStreamVersion names the source of a batch in its stream stats.
const evidenceStreamVersion = "evidence/1"

// evidenceInvocationDirEnv names the directory the command was invoked from.
// The CLI sets it for an interactive extension command; Git reads the job's
// checkout there, and in the working directory when it is unset.
const evidenceInvocationDirEnv = "PUTNAMI_INVOCATION_DIR"

// The operational members the command fills, as the records ingest names them.
const (
	evidenceTestedTree        = "testedTree"
	evidenceTestedHeadTree    = "testedHeadTree"
	evidenceBaselineMergeBase = "baselineMergeBase"
)

// evidenceSubmitRequest is the records ingest body of an external run. Its
// keys are the delivery API's SubmitRequest keys.
type evidenceSubmitRequest struct {
	Provider      string                 `json:"provider"`
	Repo          string                 `json:"repo"`
	SHA           string                 `json:"sha"`
	Branch        string                 `json:"branch,omitempty"`
	Provenance    string                 `json:"provenance"`
	ChangeRequest *evidenceChangeRequest `json:"changeRequest,omitempty"`
	PullRequest   int                    `json:"pullRequest,omitempty"`
	SubmissionID  string                 `json:"submissionId"`
	Status        string                 `json:"status"`
	Conclusion    string                 `json:"conclusion"`
	Operational   *evidenceOperational   `json:"operational,omitempty"`
}

// evidenceOperational is the part of the run's operational metrics an observed
// job knows. TestedTree and TestedHeadTree are exclusive: the first is the tree
// of a merge the CI built, the second the tree of a commit as it was pushed.
type evidenceOperational struct {
	TestedTree        string `json:"testedTree,omitempty"`
	TestedHeadTree    string `json:"testedHeadTree,omitempty"`
	BaselineMergeBase string `json:"baselineMergeBase,omitempty"`
}

// evidenceChangeRequest is the provider-neutral pull or merge request.
type evidenceChangeRequest struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Number   int    `json:"number"`
}

// evidenceReportBatch is the report-batch body. Tests and coverage are keyed by
// component id; the three evidence members are the document's own JSON.
type evidenceReportBatch struct {
	SubmissionID        string                        `json:"submissionId"`
	Stream              reportBatchStream             `json:"stream"`
	Tests               *evidenceTestsReport          `json:"tests,omitempty"`
	Coverage            *evidenceCoverageReport       `json:"coverage,omitempty"`
	Components          []evidence.Component          `json:"components,omitempty"`
	ChangedLineCoverage *evidence.ChangedLineCoverage `json:"changedLineCoverage,omitempty"`
	DependencyChanges   []evidence.DependencyChange   `json:"dependencyChanges,omitempty"`
}

type evidenceTestsReport struct {
	Projects []evidenceProjectTests `json:"projects"`
}

type evidenceProjectTests struct {
	Project      string                `json:"project"`
	Passed       int                   `json:"passed"`
	Failed       int                   `json:"failed"`
	Skipped      int                   `json:"skipped"`
	Total        int                   `json:"total"`
	FailingCases []evidenceFailingCase `json:"failingCases,omitempty"`
}

type evidenceFailingCase struct {
	Name    string `json:"name"`
	File    string `json:"file,omitempty"`
	Message string `json:"message,omitempty"`
}

type evidenceCoverageReport struct {
	Projects []evidenceProjectCoverage `json:"projects"`
}

type evidenceProjectCoverage struct {
	Project           string  `json:"project"`
	CoveredStatements int     `json:"coveredStatements"`
	TotalStatements   int     `json:"totalStatements"`
	Percentage        float64 `json:"percentage"`
}

// EvidenceReport uploads the evidence document --file names.
func EvidenceReport(params map[string]any, env map[string]string, ioctx clicore.IO) error {
	doc, err := readEvidenceDocument(clicore.StringParam(params, "file"))
	if err != nil {
		return err
	}
	run, err := evidenceRun(doc)
	if err != nil {
		return err
	}
	token := clicore.EnvGet(env, evidenceTokenEnv)
	if token == "" {
		return clicore.NewError("cloud evidence-report needs "+evidenceTokenEnv+": a workspace token of scope delivery.ingest", clicore.ExitAuth)
	}
	if fields := strings.Fields(token); len(fields) != 1 || fields[0] != token {
		return clicore.NewError(evidenceTokenEnv+" must contain exactly one bearer token", clicore.ExitAuth)
	}
	controlURL := clicore.ControlPlaneBaseURL(params, env, "")
	var leftOut map[string]string
	run.Operational, leftOut = evidenceTested(doc, env, clicore.EnvGet(env, evidenceInvocationDirEnv))
	for _, field := range []string{evidenceTestedTree, evidenceTestedHeadTree, evidenceBaselineMergeBase} {
		if reason, ok := leftOut[field]; ok {
			ioctx.Stderr("warning: cloud evidence-report sends no " + field + ": " + reason)
		}
	}

	resp, refused, err := openEvidenceRun(ioctx, controlURL, token, run)
	if err != nil && refused == http.StatusBadRequest && run.Operational != nil {
		// Delivery refuses operational on a control plane that predates
		// self-reported trees, or on a commit another provenance already
		// recorded. The evidence still matters: open the run without it, under
		// the same submission id.
		refusal := err.Error()
		run.Operational = nil
		if resp, _, err = openEvidenceRun(ioctx, controlURL, token, run); err == nil {
			ioctx.Stderr("warning: cloud evidence-report sends no operational: " + refusal)
			if leftOut == nil {
				leftOut = map[string]string{}
			}
			leftOut["operational"] = refusal
		}
	}
	if err != nil {
		return err
	}

	var stored, already, failures []string
	for _, batch := range evidenceBatches(doc, run.SubmissionID) {
		answer, err := postReport(ioctx, controlURL, recordsReportsIngestPath, token, batch,
			func(callCtx context.Context, api *deliveryapiclient.DeliveryClient, raw []byte) (*deliveryapiclient.ReportBatchResponse, error) {
				var in deliveryapiclient.CreateV1DeliveryRecordsReportsInput
				if err := json.Unmarshal(raw, &in.Body); err != nil {
					return nil, err
				}
				return api.CreateV1DeliveryRecordsReports(callCtx, in)
			})
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		stored = append(stored, deref(answer.Stored)...)
		already = append(already, deref(answer.Idempotent)...)
		if dropped := deref(answer.Dropped); len(dropped) > 0 {
			ioctx.Stderr("warning: Putnami Cloud dropped " + strings.Join(dropped, ", ") + " from the evidence of run " + deref(resp.RunId))
		}
	}

	result := map[string]any{
		"run_id":         deref(resp.RunId),
		"subject_id":     deref(resp.SubjectId),
		"provenance":     deref(resp.Provenance),
		"idempotent":     deref(resp.Idempotent),
		"repo":           run.Repo,
		"sha":            run.SHA,
		"submission_id":  run.SubmissionID,
		"stored":         stored,
		"already_stored": already,
	}
	if run.Operational != nil {
		result["operational"] = run.Operational
	}
	if len(leftOut) > 0 {
		result["operational_left_out"] = leftOut
	}
	if len(failures) > 0 {
		result["failed"] = failures
	}
	summary := fmt.Sprintf("Uploaded the evidence of %s@%s to Putnami Cloud: run %s (%s, self-reported), stored %s",
		doc.Subject.Repository, shortSHA(run.SHA), deref(resp.RunId), deref(resp.Provenance), storedLabel(stored))
	if len(already) > 0 {
		summary += "; already stored " + strings.Join(already, ", ")
	}
	clicore.WriteResult(result, params, ioctx, summary+".")
	if len(failures) > 0 {
		return clicore.NewError("cloud evidence-report: a report batch was not stored: "+strings.Join(failures, "; "), clicore.ExitAPI)
	}
	return nil
}

// readEvidenceDocument reads and validates the document at path, resolved
// against the working directory.
func readEvidenceDocument(path string) (*evidence.Document, error) {
	if path == "" || path == "true" {
		return nil, clicore.NewError("cloud evidence-report needs --file <path> to a change evidence document", clicore.ExitUsage)
	}
	if !filepath.IsAbs(path) {
		if dir, err := os.Getwd(); err == nil {
			path = filepath.Join(dir, path)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, clicore.NewError("cloud evidence-report cannot read "+path+": "+err.Error(), clicore.ExitUsage)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxEvidenceBytes+1))
	if err != nil {
		return nil, clicore.NewError("cloud evidence-report cannot read "+path+": "+err.Error(), clicore.ExitUsage)
	}
	if len(data) > maxEvidenceBytes {
		return nil, clicore.NewError(fmt.Sprintf("cloud evidence-report: %s is over %d MiB", path, maxEvidenceBytes>>20), clicore.ExitUsage)
	}
	doc, problems := evidence.ParseAndValidate(data)
	if diag.HasErrors(problems) || doc == nil {
		return nil, clicore.NewError("cloud evidence-report: "+path+" is not a valid change evidence document: "+diag.ErrorText(problems), clicore.ExitUsage)
	}
	return doc, nil
}

// evidenceRun derives the records ingest body of the document's run.
func evidenceRun(doc *evidence.Document) (evidenceSubmitRequest, error) {
	provenance := ""
	switch doc.CI.System {
	case evidence.CISystemGitHubActions:
		provenance = "gha"
	case evidence.CISystemGitLabCI:
		provenance = "gitlab"
	default:
		return evidenceSubmitRequest{}, clicore.NewError("cloud evidence-report uploads the evidence of a GitHub Actions or GitLab CI job; Putnami Cloud has no provenance for the CI "+strconv.Quote(doc.CI.System), clicore.ExitUsage)
	}
	if doc.Subject.ProjectID == "" {
		return evidenceSubmitRequest{}, clicore.NewError("cloud evidence-report: the document names no numeric project id for its repository", clicore.ExitUsage)
	}
	submissionID, err := evidenceSubmissionID(doc, provenance)
	if err != nil {
		return evidenceSubmitRequest{}, err
	}
	run := evidenceSubmitRequest{
		Provider:     doc.Subject.Provider,
		Repo:         doc.Subject.ProjectID,
		SHA:          doc.Subject.SHA,
		Branch:       doc.Subject.Branch,
		Provenance:   provenance,
		SubmissionID: submissionID,
		Status:       "completed",
		Conclusion:   evidenceConclusion(doc),
	}
	if ref := doc.Subject.ChangeRequest; ref != nil {
		if doc.Subject.Host != "" {
			run.ChangeRequest = &evidenceChangeRequest{Provider: doc.Subject.Provider, Host: doc.Subject.Host, Number: ref.Number}
		} else {
			run.PullRequest = ref.Number
		}
	}
	return run, nil
}

// openEvidenceRun opens the run on the records route. It also returns the
// status of the last refused call, 0 when there was none.
func openEvidenceRun(ioctx clicore.IO, controlURL, token string, run evidenceSubmitRequest) (*deliveryapiclient.SubmitResponse, int, error) {
	refused := 0
	resp, err := postReport(ioctx, controlURL, recordsIngestPath, token, run,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient, raw []byte) (*deliveryapiclient.SubmitResponse, error) {
			var in deliveryapiclient.CreateV1DeliveryRecordsInput
			if err := json.Unmarshal(raw, &in.Body); err != nil {
				return nil, err
			}
			resp, err := api.CreateV1DeliveryRecords(callCtx, in)
			refused = clicore.ServiceStatus(err)
			return resp, err
		})
	return resp, refused, err
}

// evidenceTested reads, with Git in dir, the tree the job tested and the merge
// base of the document's base and commit. It returns nil when it reads no
// tested tree, and the reason of each member it leaves out, keyed by its name.
func evidenceTested(doc *evidence.Document, env map[string]string, dir string) (*evidenceOperational, map[string]string) {
	where := "the working directory"
	if dir != "" {
		where = dir
	}
	var operational evidenceOperational
	leftOut := map[string]string{}
	field := evidenceTestedHeadTree
	if evidenceTestedMerge(doc, env) {
		field = evidenceTestedTree
	}
	tree, err := evidenceGit(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", doc.Subject.SHA+"^{tree}")
	switch {
	case err != nil || tree == "":
		leftOut[field] = fmt.Sprintf("the commit %s is not in the Git checkout of %s", shortSHA(doc.Subject.SHA), where)
	case field == evidenceTestedTree:
		operational.TestedTree = tree
	default:
		operational.TestedHeadTree = tree
	}
	switch base := doc.Subject.BaseSHA; {
	case base == "":
		leftOut[evidenceBaselineMergeBase] = "the document names no base for its change"
	default:
		mergeBase, err := evidenceGit(dir, "merge-base", "--end-of-options", base, doc.Subject.SHA)
		if err == nil && mergeBase != "" {
			operational.BaselineMergeBase = mergeBase
			break
		}
		reason := fmt.Sprintf("Git finds no merge base of %s and %s in the checkout of %s", shortSHA(base), shortSHA(doc.Subject.SHA), where)
		if shallow, _ := evidenceGit(dir, "rev-parse", "--is-shallow-repository"); shallow == "true" {
			reason += ": the clone is shallow and does not reach it"
		}
		leftOut[evidenceBaselineMergeBase] = reason
	}
	if operational.TestedTree == "" && operational.TestedHeadTree == "" {
		// Delivery reads a merge base only beside the tree it was measured
		// for, and refuses one alone.
		if operational.BaselineMergeBase != "" {
			leftOut[evidenceBaselineMergeBase] = "no tested tree goes with it"
		}
		return nil, leftOut
	}
	if len(leftOut) == 0 {
		leftOut = nil
	}
	return &operational, leftOut
}

// evidenceGitLocating are the variables that point Git at another repository
// or index. The command reads the checkout of the directory it runs Git in, as
// putnami observe does when it writes the document.
var evidenceGitLocating = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
	"GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true,
}

// evidenceGit runs one Git command in dir, the working directory when empty,
// and returns its trimmed stdout. It never prompts.
func evidenceGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !evidenceGitLocating[name] {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// evidenceTestedMerge reports whether the document's commit is the merge the CI
// built rather than a commit that was pushed: the merge ref of a GitHub pull
// request, or the merge commit of a GitLab merged results or merge train
// pipeline. The commit must be the one the CI names, so a job that checked out
// another one tested that one as it was pushed.
func evidenceTestedMerge(doc *evidence.Document, env map[string]string) bool {
	switch doc.CI.System {
	case evidence.CISystemGitHubActions:
		return pullRequestMergeRef(clicore.EnvGet(env, "GITHUB_REF")) &&
			strings.EqualFold(clicore.EnvGet(env, "GITHUB_SHA"), doc.Subject.SHA)
	case evidence.CISystemGitLabCI:
		switch clicore.EnvGet(env, "CI_MERGE_REQUEST_EVENT_TYPE") {
		case "merged_result", "merge_train":
			return strings.EqualFold(clicore.EnvGet(env, "CI_COMMIT_SHA"), doc.Subject.SHA)
		}
	}
	return false
}

// pullRequestMergeRef reports whether ref is refs/pull/<n>/merge.
func pullRequestMergeRef(ref string) bool {
	number, ok := strings.CutPrefix(ref, "refs/pull/")
	if !ok {
		return false
	}
	number, ok = strings.CutSuffix(number, "/merge")
	if !ok || number == "" {
		return false
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// evidenceSubmissionID names the run after the CI job that observed it, so
// uploading the same job again opens no second run. A document without a run id
// gets a fresh id. GitHub Actions names a job by its key, which every leg of a
// matrix shares, so a gha id also carries the digest of what the leg measured:
// two legs open two runs, and the same leg uploaded again opens none.
func evidenceSubmissionID(doc *evidence.Document, provenance string) (string, error) {
	if doc.CI.RunID == "" {
		id, err := generateSubmissionID()
		if err != nil {
			return "", err
		}
		return "observe-" + id, nil
	}
	parts := []string{"observe", provenance, doc.Subject.ProjectID, doc.CI.RunID}
	if doc.CI.Attempt != nil {
		parts = append(parts, strconv.Itoa(*doc.CI.Attempt))
	}
	if doc.CI.JobID != "" {
		parts = append(parts, doc.CI.JobID)
	}
	if provenance == "gha" {
		leg, err := evidenceLegDigest(doc)
		if err != nil {
			return "", err
		}
		parts = append(parts, leg)
	}
	return strings.Join(parts, "-"), nil
}

// evidenceLegDigest is the first 12 hex digits of the SHA-256 of what a job
// measured: its components, tests, coverage, changed-line coverage and
// dependency changes. It leaves out the time the document was written.
func evidenceLegDigest(doc *evidence.Document) (string, error) {
	data, err := json.Marshal([]any{doc.Components, doc.Tests, doc.Coverage, doc.ChangedLineCoverage, doc.DependencyChanges})
	if err != nil {
		return "", clicore.NewError("cloud evidence-report: digest the evidence: "+err.Error(), clicore.ExitAPI)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6]), nil
}

// evidenceConclusion is failure when a test failed, success otherwise.
func evidenceConclusion(doc *evidence.Document) string {
	for _, tests := range doc.Tests {
		if tests.Failed > 0 {
			return "failure"
		}
	}
	return "success"
}

// evidenceBatches returns the report batches of the document, in the order they
// are posted: tests and coverage, then the change evidence members.
func evidenceBatches(doc *evidence.Document, submissionID string) []evidenceReportBatch {
	stream := reportBatchStream{Version: evidenceStreamVersion}
	var batches []evidenceReportBatch
	reports := evidenceReportBatch{SubmissionID: submissionID, Stream: stream}
	if len(doc.Tests) > 0 {
		reports.Tests = &evidenceTestsReport{Projects: make([]evidenceProjectTests, 0, len(doc.Tests))}
		for _, tests := range doc.Tests {
			project := evidenceProjectTests{
				Project: tests.Component,
				Passed:  tests.Passed,
				Failed:  tests.Failed,
				Skipped: tests.Skipped,
				Total:   tests.Passed + tests.Failed + tests.Skipped,
			}
			for _, failing := range tests.FailingCases {
				project.FailingCases = append(project.FailingCases, evidenceFailingCase{Name: failing.Name, File: failing.File, Message: failing.Message})
			}
			reports.Tests.Projects = append(reports.Tests.Projects, project)
		}
	}
	if len(doc.Coverage) > 0 {
		reports.Coverage = &evidenceCoverageReport{Projects: make([]evidenceProjectCoverage, 0, len(doc.Coverage))}
		for _, coverage := range doc.Coverage {
			reports.Coverage.Projects = append(reports.Coverage.Projects, evidenceProjectCoverage{
				Project:           coverage.Component,
				CoveredStatements: coverage.CoveredLines,
				TotalStatements:   coverage.TotalLines,
				Percentage:        percentage(coverage.CoveredLines, coverage.TotalLines),
			})
		}
	}
	if reports.Tests != nil || reports.Coverage != nil {
		batches = append(batches, reports)
	}
	members := evidenceReportBatch{
		SubmissionID:        submissionID,
		Stream:              stream,
		Components:          doc.Components,
		ChangedLineCoverage: doc.ChangedLineCoverage,
		DependencyChanges:   doc.DependencyChanges,
	}
	if len(members.Components) > 0 || members.ChangedLineCoverage != nil || len(members.DependencyChanges) > 0 {
		batches = append(batches, members)
	}
	return batches
}

// percentage is covered over total, in percent with two decimals; 0 for no
// line.
func percentage(covered, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(covered)*10000/float64(total)) / 100
}

func storedLabel(stored []string) string {
	if len(stored) == 0 {
		return "no report"
	}
	return strings.Join(stored, ", ")
}
