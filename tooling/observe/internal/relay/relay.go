// Package relay reads a change evidence document that another run kept, such
// as the run of a pull request from a fork, and prepares it for the upload
// and the publication of a GitHub Actions job that a workflow_run event
// started in the base repository.
//
// GitHub gives the run of a pull request from a fork no ID token and a
// read-only GITHUB_TOKEN, so that run keeps its document as an artifact, and
// a workflow_run job of the base repository, which holds both, relays it.
// The document is untrusted input written by the fork: the relay refuses one
// that names another repository than the job's or another pull request than
// the one the job resolves, bounds its size, and takes the repository, the
// pull request and the CI run from the job, never from the document. It
// keeps the document's analyses as the fork's claim.
package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/observe"
	"go.putnami.dev/tooling/observe/internal/publish"
)

const (
	// CodeRefused is the warning of a document the relay does not upload or
	// publish.
	CodeRefused = "observe.evidence_refused"
	// CodeUnresolved is the info of a run whose pull request the relay does
	// not find.
	CodeUnresolved = "observe.pull_request_unresolved"
	// MaxBytes bounds the document the relay reads, as the @putnami/cloud
	// reporter bounds the document it sends.
	MaxBytes = 64 << 20
)

// Request is one relay.
type Request struct {
	// Path is the absolute path of the kept document.
	Path string
	// Getenv reads the job's environment.
	Getenv func(string) string
	// ReadFile reads the event payload and the document; nil reads the
	// files, the document at most MaxBytes.
	ReadFile ci.ReadFile
	// Client sends the pull request lookup; nil uses a default client.
	Client *http.Client
}

// Result is what the relay prepared.
type Result struct {
	// Document is the document to upload, with the job's repository, pull
	// request and run; nil when the relay refused it.
	Document *evidence.Document
	// Observation is the document as an observation, for the publication:
	// its diagnostics are the document's, then the relay's.
	Observation observe.Observation
	// Run is the run that triggered the job; zero when the job is not a
	// workflow_run job.
	Run ci.WorkflowRun
	// Diagnostics are the relay's own, which name no text of the document.
	Diagnostics []diag.Diagnostic
}

// Load reads, checks and prepares the document req.Path names.
func Load(ctx context.Context, req Request) Result {
	var r Result
	run, err := ci.ReadWorkflowRun(req.Getenv, req.ReadFile)
	if err != nil {
		return r.refuse("--from-evidence relays only in a GitHub Actions job that a workflow_run event started: %v; nothing was uploaded or published", err)
	}
	r.Run = run
	if run.Event != "pull_request" {
		return r.refuse("the run that triggered this job ran for the event %q, not pull_request; nothing was uploaded or published", run.Event)
	}
	if strings.TrimSpace(req.Getenv(publish.GitHubTokenEnv)) == "" {
		return r.refuse("%s is unset, so the pull request and the commit of the document cannot be checked; nothing was uploaded or published", publish.GitHubTokenEnv)
	}
	detected := ci.Detect(ci.Env(req.Getenv), func(string) ([]byte, error) { return []byte("{}"), nil }, ci.Overrides{})
	job := detected.Subject
	if job.ProjectID == "" || job.Repository == "" {
		return r.refuse("GITHUB_REPOSITORY and GITHUB_REPOSITORY_ID name no repository with a numeric id, so the document cannot be checked against this job's repository; nothing was uploaded or published")
	}
	doc, diags := read(req.Path, req.ReadFile)
	if doc == nil {
		r.Diagnostics = append(r.Diagnostics, diags...)
		r.Observation = observation(job, run, 0, nil, r.Diagnostics)
		return r
	}
	if field := otherRepository(doc, job); field != "" {
		return r.refuse("the evidence document names another %s than this job's repository %s (project %s), so it was neither uploaded nor published", field, job.Repository, job.ProjectID)
	}
	fork := run.HeadRepositoryID != job.ProjectID
	number, unresolved := pullRequest(ctx, req, run, job)
	if unresolved != nil {
		if fork {
			return r.refuse("%s, and the run built a fork, so its document was neither uploaded nor published", unresolved.Message)
		}
		unresolved.Message += ", so the document is uploaded without a pull request and --publish publishes nothing"
	}
	if cr := doc.Subject.ChangeRequest; cr != nil && number > 0 && cr.Number != number {
		return r.refuse("the evidence document names another pull request than #%d, the one of the run that triggered this job, so it was neither uploaded nor published", number)
	}
	base, reason := bindCommit(ctx, req, job, run, number, doc)
	if reason != "" {
		return r.refuse("%s, so the document was neither uploaded nor published", reason)
	}
	if unresolved != nil {
		r.Diagnostics = append(r.Diagnostics, *unresolved)
	}
	doc.Subject.BaseSHA = base
	branch := run.HeadBranch
	if fork {
		// A fork names its own branches: the base repository keeps none.
		branch = ""
	}
	relayed := relay(doc, job, run, number, branch)
	if problems := evidence.ValidateDocument(relayed); diag.HasErrors(problems) {
		return r.refuse("the relayed evidence document does not validate (%s); nothing was uploaded or published", codes(problems))
	}
	r.Document = relayed
	r.Observation = observation(job, run, number, relayed, slices.Concat(relayed.Diagnostics, r.Diagnostics))
	return r
}

// refuse returns the result of a refused document.
func (r Result) refuse(format string, args ...any) Result {
	r.Document = nil
	r.Diagnostics = append(r.Diagnostics, diag.Warningf(CodeRefused, "from-evidence", format, args...))
	r.Observation = observation(ci.Subject{Provider: ci.ProviderGitHub}, r.Run, 0, nil, r.Diagnostics)
	return r
}

// read reads and validates the document at path, at most MaxBytes. Its
// diagnostics quote no text of the document: the validator's codes only.
func read(path string, readFile ci.ReadFile) (*evidence.Document, []diag.Diagnostic) {
	data, err := readBounded(path, readFile)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Warningf(CodeRefused, "from-evidence",
			"the evidence document %s cannot be read: %v; nothing was uploaded or published", path, err)}
	}
	doc, problems := evidence.ParseAndValidate(data)
	if doc == nil || diag.HasErrors(problems) {
		return nil, []diag.Diagnostic{diag.Warningf(CodeRefused, "from-evidence",
			"the evidence document %s does not validate against protocol/evidence v%d (%s); nothing was uploaded or published", path, evidence.ProtocolVersion, codes(problems))}
	}
	return doc, nil
}

var errTooLarge = fmt.Errorf("it is larger than %d MiB", MaxBytes>>20)

// readBounded reads at most MaxBytes of path, and refuses a larger file
// before it reads it whole when the file system says its size.
func readBounded(path string, readFile ci.ReadFile) ([]byte, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > MaxBytes {
		return nil, errTooLarge
	}
	if readFile == nil {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
		if err == nil && len(data) > MaxBytes {
			err = errTooLarge
		}
		return data, err
	}
	data, err := readFile(path)
	if err == nil && len(data) > MaxBytes {
		return nil, errTooLarge
	}
	return data, err
}

// codes lists the distinct codes of the error diagnostics, which the
// validator names, never the document.
func codes(problems []diag.Diagnostic) string {
	var list []string
	for _, d := range diag.Errors(problems) {
		if !slices.Contains(list, d.Code) {
			list = append(list, d.Code)
		}
	}
	if len(list) == 0 {
		return "no diagnostic"
	}
	return fmt.Sprintf("%d errors: %s", len(diag.Errors(problems)), strings.Join(list, ", "))
}

// otherRepository names the first member of the document's subject or CI
// that names another repository or CI than the job's, "" when none does.
func otherRepository(doc *evidence.Document, job ci.Subject) string {
	switch {
	case doc.CI.System != ci.SystemGitHubActions:
		return "CI"
	case doc.Subject.Provider != ci.ProviderGitHub:
		return "code host"
	case !strings.EqualFold(doc.Subject.Host, job.Host):
		return "host"
	case doc.Subject.ProjectID != job.ProjectID:
		return "repository id"
	case !strings.EqualFold(doc.Subject.Repository, job.Repository):
		return "repository"
	}
	return ""
}

// pullRequest returns the number of the pull request of the job's
// repository the run built: the one the payload lists at the run's head
// commit, else the one the GitHub API finds at the run's head owner, branch
// and commit. 0 with an info diagnostic when none is found.
func pullRequest(ctx context.Context, req Request, run ci.WorkflowRun, job ci.Subject) (int, *diag.Diagnostic) {
	for _, pr := range run.PullRequests {
		if pr.HeadSHA == run.HeadSHA && pr.BaseRepositoryID == job.ProjectID {
			return pr.Number, nil
		}
	}
	unresolved := func(reason string) (int, *diag.Diagnostic) {
		return 0, &diag.Diagnostic{Severity: diag.Info, Code: CodeUnresolved, Field: "from-evidence", Message: reason}
	}
	number, err := publish.FindPullRequest(ctx, publish.PullRequestQuery{
		Repository: job.Repository,
		HeadOwner:  run.HeadOwner,
		HeadBranch: run.HeadBranch,
		HeadSHA:    run.HeadSHA,
		Getenv:     req.Getenv,
		Client:     req.Client,
	})
	switch {
	case err != nil:
		return unresolved(fmt.Sprintf("no pull request of %s was found for the run: %v", job.Repository, err))
	case number == 0:
		return unresolved(fmt.Sprintf("no open pull request of %s has the run's head commit %s", job.Repository, short(run.HeadSHA)))
	}
	return number, nil
}

// bindCommit checks the document's commit against the run: the run's head
// commit, with a base the GitHub API answers as an ancestor of it, or the
// test merge commit GitHub keeps for the pull request, whose parents are
// the base, which must be the document's, and the run's head. It returns
// the base to keep, or the reason to refuse.
func bindCommit(ctx context.Context, req Request, job ci.Subject, run ci.WorkflowRun, number int, doc *evidence.Document) (base, refusal string) {
	q := publish.PullRequestQuery{Repository: job.Repository, Getenv: req.Getenv, Client: req.Client}
	sha, claimed := doc.Subject.SHA, doc.Subject.BaseSHA
	if sha == run.HeadSHA {
		if claimed == "" {
			return "", ""
		}
		mergeBase, err := publish.MergeBase(ctx, q, claimed, sha)
		switch {
		case err != nil:
			return "", fmt.Sprintf("the base of the evidence document cannot be checked in %s: %v", job.Repository, err)
		case mergeBase != claimed:
			return "", "the base of the evidence document is not an ancestor of the run's head commit"
		}
		return claimed, ""
	}
	if number == 0 {
		return "", fmt.Sprintf("the evidence document names a commit other than the run's head commit %s, and no pull request names its merge commit", short(run.HeadSHA))
	}
	merge, err := publish.MergeCommit(ctx, q, number)
	if err != nil {
		return "", fmt.Sprintf("the merge commit of pull request #%d cannot be read in %s: %v", number, job.Repository, err)
	}
	if merge == "" || sha != merge {
		return "", fmt.Sprintf("the evidence document names a commit that is neither the run's head commit %s nor the merge commit of pull request #%d", short(run.HeadSHA), number)
	}
	parents, err := publish.CommitParents(ctx, q, sha)
	switch {
	case err != nil:
		return "", fmt.Sprintf("the merge commit of pull request #%d cannot be read in %s: %v", number, job.Repository, err)
	case len(parents) != 2 || parents[1] != run.HeadSHA:
		return "", fmt.Sprintf("the merge commit of pull request #%d does not merge the run's head commit %s", number, short(run.HeadSHA))
	case claimed != "" && claimed != parents[0]:
		return "", "the base of the evidence document is not the first parent of the merge commit"
	}
	return claimed, ""
}

// relay returns a copy of doc with the job's repository, pull request and
// run.
func relay(doc *evidence.Document, job ci.Subject, run ci.WorkflowRun, number int, branch string) *evidence.Document {
	out := *doc
	out.Subject = evidence.Subject{
		Provider:   ci.ProviderGitHub,
		Host:       job.Host,
		ProjectID:  job.ProjectID,
		Repository: job.Repository,
		SHA:        doc.Subject.SHA,
		BaseSHA:    doc.Subject.BaseSHA,
		Branch:     bounded(branch),
	}
	if number > 0 {
		out.Subject.ChangeRequest = &evidence.ChangeRequest{Number: number}
	}
	out.CI = evidence.CI{
		System:  ci.SystemGitHubActions,
		RunID:   run.ID,
		JobID:   jobID(doc.CI.JobID),
		Attempt: run.Attempt,
		URL:     run.URL,
	}
	return &out
}

// observation is the relayed document as an observation; doc nil is a
// refused one, which carries the diagnostics only.
func observation(job ci.Subject, run ci.WorkflowRun, number int, doc *evidence.Document, diags []diag.Diagnostic) observe.Observation {
	o := observe.Observation{
		Subject:      job,
		CI:           ci.Run{System: ci.SystemGitHubActions, RunID: run.ID, Attempt: run.Attempt, URL: run.URL},
		ChangedFiles: []gitbase.ChangedFile{},
		Diagnostics:  diags,
	}
	o.Subject.ChangeRequest = nil
	if number > 0 {
		o.Subject.ChangeRequest = &ci.ChangeRequest{Number: number, HeadSHA: run.HeadSHA}
	}
	if o.Diagnostics == nil {
		o.Diagnostics = []diag.Diagnostic{}
	}
	if doc == nil {
		return o
	}
	o.Subject.SHA = doc.Subject.SHA
	o.Subject.BaseSHA = doc.Subject.BaseSHA
	o.Subject.Branch = doc.Subject.Branch
	o.CI.JobID = doc.CI.JobID // already checked by relay
	o.Producer = observe.Producer{Name: doc.Producer.Name, Version: doc.Producer.Version}
	for _, f := range doc.ChangedFiles {
		o.ChangedFiles = append(o.ChangedFiles, gitbase.ChangedFile{Path: f.Path, Status: f.Status, PreviousPath: f.PreviousPath})
	}
	o.Analysis.Components = doc.Components
	o.Analysis.Tests = doc.Tests
	o.Analysis.Coverage = doc.Coverage
	o.Analysis.Architecture = doc.Architecture
	o.Analysis.ChangedLineCoverage = doc.ChangedLineCoverage
	o.Analysis.DependencyChanges = doc.DependencyChanges
	o.Analysis.SBOM = doc.SBOM
	o.Analysis.Decisions = doc.Decisions
	o.Analysis.Declarations = doc.Declarations
	o.SpecVerification = doc.SpecVerification
	return o
}

// jobIDPattern is a GitHub Actions job key, the only job id the relay keeps
// from the document: it names the upload.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func jobID(id string) string {
	if jobIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// bounded cuts a branch name to the document's string bound.
func bounded(s string) string {
	if len(s) <= evidence.MaxStringBytes {
		return s
	}
	return strings.ToValidUTF8(s[:evidence.MaxStringBytes], "")
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// WriteText writes the relay's summary for a log: the run, the repository,
// the pull request and the counts the commit status shows, then diags. It
// writes no text of the document, so no line of it can be a workflow
// command.
func WriteText(w io.Writer, r Result, diags []diag.Diagnostic) error {
	var b strings.Builder
	o := r.Observation
	if r.Document == nil {
		fmt.Fprintln(&b, "putnami observe: relayed no evidence document")
	} else {
		fmt.Fprintf(&b, "putnami observe: relayed the evidence of GitHub Actions run %s to %s/%s (project %s)\n",
			o.CI.RunID, o.Subject.Host, o.Subject.Repository, o.Subject.ProjectID)
		if cr := o.Subject.ChangeRequest; cr != nil {
			fmt.Fprintf(&b, "  pull request #%d, head commit %s\n", cr.Number, short(cr.HeadSHA))
		} else {
			fmt.Fprintln(&b, "  no pull request")
		}
		fmt.Fprintf(&b, "  %s\n", publish.Description(o))
	}
	for _, d := range diags {
		fmt.Fprintf(&b, "  %s %s: %s\n", d.Severity, d.Code, d.Message)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
