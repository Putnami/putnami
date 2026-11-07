// Package ci reads, from the environment of a CI job, which CI ran it, which
// repository and commit it built, and which commit the change is measured
// against.
//
// The evidence protocol's markers name the CI. GitHub Actions and GitLab CI
// are read in full; any other CI is named alone and takes its base from a
// flag. The repository key is
// the code host and its numeric project id, which survive a rename or a
// transfer; the repository path is for display only.
package ci

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.putnami.dev/protocol/codehost"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// Code hosts: the slugs of the codehost protocol.
const (
	ProviderGitHub  = codehost.GitHub
	ProviderGitLab  = codehost.GitLab
	ProviderGeneric = codehost.Generic
)

// CI systems: the slugs of the evidence protocol.
const (
	SystemGitHubActions = evidence.CISystemGitHubActions
	SystemGitLabCI      = evidence.CISystemGitLabCI
	SystemGeneric       = evidence.CISystemGeneric
)

// Base sources: where the base candidate came from.
const (
	BaseFromFlag         = "flag"
	BaseFromMergeRequest = "merge-request"
	BaseFromPullRequest  = "pull-request"
	BaseFromPush         = "push"
)

// Diagnostic codes.
const (
	CodeProjectIDMissing = "observe.project_id_missing"
	CodeBaseMissing      = "observe.base_missing"
	CodeEventUnreadable  = "observe.event_unreadable"
)

// Subject names the change: the repository and the commit.
type Subject struct {
	Provider      string         `json:"provider"`
	Host          string         `json:"host,omitempty"`
	ProjectID     string         `json:"projectId,omitempty"`
	Repository    string         `json:"repository"`
	SHA           string         `json:"sha,omitempty"`
	BaseSHA       string         `json:"baseSha,omitempty"`
	Branch        string         `json:"branch,omitempty"`
	ChangeRequest *ChangeRequest `json:"changeRequest,omitempty"`
}

// ChangeRequest is a merge request (GitLab) or a pull request (GitHub).
type ChangeRequest struct {
	Number int `json:"number"`
	// HeadSHA is the last commit of the request's source branch, where a
	// commit status shows on the request. On a GitHub pull request the job
	// builds a merge commit, so it differs from the subject's commit.
	HeadSHA string `json:"headSha,omitempty"`
}

// Run names the CI job.
type Run struct {
	System  string `json:"system"`
	RunID   string `json:"runId,omitempty"`
	JobID   string `json:"jobId,omitempty"`
	Attempt *int   `json:"attempt,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Base is the commit or branch a change is measured against, before Git
// resolves it. SHA names a commit; Ref names a branch of the origin remote.
// When both are set, SHA is the base and Ref is the branch that holds it.
type Base struct {
	SHA    string `json:"sha,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Source string `json:"source"`
}

// Detection is what the environment says about the job.
type Detection struct {
	Subject Subject
	Run     Run
	// Head is the revision that was built: the --head flag, or HEAD.
	Head string
	// Base is the base candidate, nil when neither a flag nor the CI names one.
	Base        *Base
	Diagnostics []diag.Diagnostic
}

// Overrides are the flags that replace what the CI says.
type Overrides struct {
	Base string
	Head string
}

// Env looks up one environment variable; "" means unset.
type Env func(string) string

// ReadFile reads a file the CI names, such as the GitHub event payload.
type ReadFile func(string) ([]byte, error)

// Detect reads the job from env. readFile reads the GitHub event payload; nil
// uses os.ReadFile.
func Detect(env Env, readFile ReadFile, overrides Overrides) Detection {
	if readFile == nil {
		readFile = os.ReadFile
	}
	var d Detection
	switch system, _ := evidence.DetectCI(env); system {
	case SystemGitHubActions:
		d = detectGitHub(env, readFile)
	case SystemGitLabCI:
		d = detectGitLab(env)
	default:
		d = Detection{
			Subject: Subject{Provider: ProviderGeneric},
			Run:     Run{System: system},
		}
	}
	d.Head = "HEAD"
	if overrides.Head != "" {
		d.Head = overrides.Head
		d.Subject.SHA = ""
	}
	if overrides.Base != "" {
		d.Base = baseFromFlag(overrides.Base)
	}
	switch {
	case d.Base != nil:
	case d.Subject.Provider == ProviderGeneric:
		d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeBaseMissing, "base",
			"no CI names the base of this change: pass --base <commit or branch>"))
	default:
		d.Diagnostics = append(d.Diagnostics, diag.Warningf(CodeBaseMissing, "base",
			"the CI names no base for this job, neither a pull or merge request nor a previous commit: pass --base <commit or branch> to measure the change"))
	}
	return d
}

func detectGitHub(env Env, readFile ReadFile) Detection {
	host := hostOf(env("GITHUB_SERVER_URL"), "github.com")
	repository := env("GITHUB_REPOSITORY")
	d := Detection{
		Subject: Subject{
			Provider:   ProviderGitHub,
			Host:       host,
			Repository: repository,
			SHA:        env("GITHUB_SHA"),
			Branch:     firstOf(env("GITHUB_HEAD_REF"), env("GITHUB_REF_NAME")),
		},
		Run: Run{
			System:  SystemGitHubActions,
			RunID:   env("GITHUB_RUN_ID"),
			JobID:   env("GITHUB_JOB"),
			Attempt: positive(env("GITHUB_RUN_ATTEMPT")),
		},
	}
	d.Subject.ProjectID, d.Diagnostics = projectID(env("GITHUB_REPOSITORY_ID"), "GITHUB_REPOSITORY_ID", d.Diagnostics)
	if serverURL := env("GITHUB_SERVER_URL"); serverURL != "" && repository != "" && d.Run.RunID != "" {
		d.Run.URL = strings.TrimRight(serverURL, "/") + "/" + repository + "/actions/runs/" + d.Run.RunID
	}
	if number := pullRequestNumber(env("GITHUB_REF")); number > 0 {
		d.Subject.ChangeRequest = &ChangeRequest{Number: number}
	}
	event, diags := readEvent(env("GITHUB_EVENT_PATH"), readFile)
	d.Diagnostics = append(d.Diagnostics, diags...)
	if event.PullRequest != nil {
		if d.Subject.ChangeRequest == nil && event.PullRequest.Number > 0 {
			d.Subject.ChangeRequest = &ChangeRequest{Number: event.PullRequest.Number}
		}
		if d.Subject.ChangeRequest != nil && commitID(event.PullRequest.Head.SHA) {
			d.Subject.ChangeRequest.HeadSHA = strings.ToLower(event.PullRequest.Head.SHA)
		}
	}
	switch {
	case env("GITHUB_BASE_REF") != "":
		d.Base = &Base{Ref: env("GITHUB_BASE_REF"), SHA: pullRequestBaseSHA(event), Source: BaseFromPullRequest}
	case commitID(event.Before) && !zeroSHA(event.Before):
		d.Base = &Base{SHA: event.Before, Source: BaseFromPush}
	}
	return d
}

func detectGitLab(env Env) Detection {
	d := Detection{
		Subject: Subject{
			Provider:   ProviderGitLab,
			Host:       strings.ToLower(firstOf(env("CI_SERVER_HOST"), hostOf(env("CI_SERVER_URL"), ""))),
			Repository: env("CI_PROJECT_PATH"),
			SHA:        env("CI_COMMIT_SHA"),
			Branch:     firstOf(env("CI_MERGE_REQUEST_SOURCE_BRANCH_NAME"), env("CI_COMMIT_BRANCH"), env("CI_COMMIT_REF_NAME")),
		},
		Run: Run{
			System: SystemGitLabCI,
			RunID:  env("CI_PIPELINE_ID"),
			JobID:  env("CI_JOB_ID"),
			URL:    env("CI_PIPELINE_URL"),
		},
	}
	d.Subject.ProjectID, d.Diagnostics = projectID(env("CI_PROJECT_ID"), "CI_PROJECT_ID", d.Diagnostics)
	if number := positive(env("CI_MERGE_REQUEST_IID")); number != nil {
		// A merged results pipeline builds a merge commit and names the
		// source branch's commit in CI_MERGE_REQUEST_SOURCE_BRANCH_SHA.
		head := ""
		for _, sha := range []string{env("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA"), env("CI_COMMIT_SHA")} {
			if commitID(sha) {
				head = strings.ToLower(sha)
				break
			}
		}
		d.Subject.ChangeRequest = &ChangeRequest{Number: *number, HeadSHA: head}
	}
	switch base := env("CI_MERGE_REQUEST_DIFF_BASE_SHA"); {
	case commitID(base):
		d.Base = &Base{SHA: strings.ToLower(base), Ref: env("CI_MERGE_REQUEST_TARGET_BRANCH_NAME"), Source: BaseFromMergeRequest}
	case commitID(env("CI_COMMIT_BEFORE_SHA")) && !zeroSHA(env("CI_COMMIT_BEFORE_SHA")):
		d.Base = &Base{SHA: env("CI_COMMIT_BEFORE_SHA"), Source: BaseFromPush}
	}
	return d
}

// baseFromFlag reads --base: a full commit id is a commit, anything else a
// revision Git resolves, read as a branch of origin when it names none.
func baseFromFlag(value string) *Base {
	if commitID(value) {
		return &Base{SHA: strings.ToLower(value), Source: BaseFromFlag}
	}
	return &Base{Ref: value, Source: BaseFromFlag}
}

// event is the part of the GitHub event payload observe reads.
type event struct {
	Before      string `json:"before"`
	PullRequest *struct {
		Number int `json:"number"`
		Base   struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// maxEventBytes bounds the event payload observe reads.
const maxEventBytes = 25 << 20

var errTooLarge = errors.New("larger than 25 MiB")

func readEvent(path string, readFile ReadFile) (event, []diag.Diagnostic) {
	if path == "" {
		return event{}, nil
	}
	data, err := readFile(path)
	if err == nil && len(data) > maxEventBytes {
		err = errTooLarge
	}
	var e event
	if err == nil {
		err = json.Unmarshal(data, &e)
	}
	if err != nil {
		return event{}, []diag.Diagnostic{diag.Warningf(CodeEventUnreadable, "GITHUB_EVENT_PATH",
			"the GitHub event payload %s cannot be read: %v", path, err)}
	}
	return e, nil
}

func pullRequestBaseSHA(e event) string {
	if e.PullRequest == nil || !commitID(e.PullRequest.Base.SHA) {
		return ""
	}
	return e.PullRequest.Base.SHA
}

// projectID keeps value when it is a positive decimal number, and reports a
// missing or malformed one.
func projectID(value, variable string, diags []diag.Diagnostic) (string, []diag.Diagnostic) {
	if positive(value) != nil {
		return value, diags
	}
	return "", append(diags, diag.Warningf(CodeProjectIDMissing, variable,
		"%s is %s: the repository has no stable key, so a rename or a transfer makes it another repository", variable, describe(value)))
}

func describe(value string) string {
	if value == "" {
		return "unset"
	}
	return "not a numeric id (" + strconv.Quote(value) + ")"
}

// pullRequestNumber reads refs/pull/<n>/merge or refs/pull/<n>/head.
func pullRequestNumber(ref string) int {
	rest, ok := strings.CutPrefix(ref, "refs/pull/")
	if !ok {
		return 0
	}
	number, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0
	}
	if n := positive(number); n != nil {
		return *n
	}
	return 0
}

// hostOf returns the hostname of a server URL, or fallback when it has none.
func hostOf(serverURL, fallback string) string {
	if serverURL == "" {
		return fallback
	}
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Hostname() == "" {
		return fallback
	}
	return strings.ToLower(parsed.Hostname())
}

// positive parses a positive decimal integer without sign or leading zero.
func positive(value string) *int {
	if value == "" || value[0] < '1' || value[0] > '9' {
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// commitID reports whether value is a full SHA-1 or SHA-256 object id.
func commitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range strings.ToLower(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// zeroSHA reports the all-zero id a CI gives for a push that created its
// branch.
func zeroSHA(value string) bool {
	return strings.Trim(value, "0") == ""
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
