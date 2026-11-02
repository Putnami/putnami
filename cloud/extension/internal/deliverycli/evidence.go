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
package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
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

	resp, err := postReport(ioctx, controlURL, recordsIngestPath, token, run,
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

	var stored []string
	var failures []string
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
		stored = append(stored, deref(answer.Idempotent)...)
		if dropped := deref(answer.Dropped); len(dropped) > 0 {
			ioctx.Stderr("warning: Putnami Cloud dropped " + strings.Join(dropped, ", ") + " from the evidence of run " + deref(resp.RunId))
		}
	}

	result := map[string]any{
		"run_id":        deref(resp.RunId),
		"subject_id":    deref(resp.SubjectId),
		"provenance":    deref(resp.Provenance),
		"idempotent":    deref(resp.Idempotent),
		"repo":          run.Repo,
		"sha":           run.SHA,
		"submission_id": run.SubmissionID,
		"stored":        stored,
	}
	if len(failures) > 0 {
		result["failed"] = failures
	}
	clicore.WriteResult(result, params, ioctx, fmt.Sprintf(
		"Uploaded the evidence of %s@%s to Putnami Cloud: run %s (%s, self-reported), stored %s.",
		doc.Subject.Repository, shortSHA(run.SHA), deref(resp.RunId), deref(resp.Provenance), storedLabel(stored)))
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
	run := evidenceSubmitRequest{
		Provider:     doc.Subject.Provider,
		Repo:         doc.Subject.ProjectID,
		SHA:          doc.Subject.SHA,
		Branch:       doc.Subject.Branch,
		Provenance:   provenance,
		SubmissionID: evidenceSubmissionID(doc, provenance),
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

// evidenceSubmissionID names the run after the CI job that observed it, so
// uploading the same job again opens no second run. A document without a run id
// gets a fresh id.
func evidenceSubmissionID(doc *evidence.Document, provenance string) string {
	if doc.CI.RunID == "" {
		id, err := generateSubmissionID()
		if err == nil {
			return "observe-" + id
		}
	}
	parts := []string{"observe", provenance, doc.Subject.ProjectID, doc.CI.RunID}
	if doc.CI.Attempt != nil {
		parts = append(parts, strconv.Itoa(*doc.CI.Attempt))
	}
	if doc.CI.JobID != "" {
		parts = append(parts, doc.CI.JobID)
	}
	return strings.Join(parts, "-")
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
	if members.Components != nil || members.ChangedLineCoverage != nil || members.DependencyChanges != nil {
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
