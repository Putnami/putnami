// Package publish posts what observe found on the pull request or merge
// request its job ran for: a commit status, on the pull request's head commit
// or on the commit of the merge request pipeline, and one summary note that
// the next run updates in place.
//
// GitHub Actions publishes with GITHUB_TOKEN, GitLab CI with a project or
// personal access token in PUTNAMI_GITLAB_TOKEN, because the GitLab job token
// cannot write notes. Every outcome other than success is a diagnostic: a
// publication never fails the job.
package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.putnami.dev/protocol/codehost"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const (
	// Marker is the first line of the summary note: the next run finds the
	// note by it. Markdown renders it as nothing. The codehost protocol owns
	// it, so every reader finds the same note.
	Marker = codehost.NoteMarker
	// StatusName is the context (GitHub) or name (GitLab) of the commit
	// status.
	StatusName = "putnami/observe"
	// GitHubTokenEnv holds the GitHub token.
	GitHubTokenEnv = "GITHUB_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential
	// GitLabTokenEnv holds the GitLab project or personal access token.
	GitLabTokenEnv = "PUTNAMI_GITLAB_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential
	// Timeout bounds one publication.
	Timeout = time.Minute

	// CodeSkipped is the info of a job that has nothing to publish on.
	CodeSkipped = "observe.publish_skipped"
	// CodeTokenMissing is the warning of a job without the code host's token.
	CodeTokenMissing = "observe.publish_token_missing"
	// CodeFailed is the warning of a call the code host refused or that
	// failed.
	CodeFailed = "observe.publish_failed"

	// requestTimeout bounds one HTTP request.
	requestTimeout = 20 * time.Second
)

// Request is one publication.
type Request struct {
	Observation observe.Observation
	// Getenv reads the environment.
	Getenv func(string) string
	// Client sends the requests; nil uses a client with a per-request
	// timeout. Redirects are never followed, whatever the client says.
	Client *http.Client
	// Output receives one line per published item.
	Output io.Writer
	// Timeout bounds the publication; zero is Timeout.
	Timeout time.Duration
}

// host is one code host's API for one pull or merge request.
type host interface {
	// statusSHA is the commit the status goes on, "" when the CI names none.
	statusSHA() string
	setStatus(ctx context.Context, sha string, s status) error
	// findNote returns the id of the first note whose body starts with
	// Marker, "" when none does.
	findNote(ctx context.Context) (string, error)
	createNote(ctx context.Context, body string) (string, error)
	updateNote(ctx context.Context, id, body string) (string, error)
	// requestLabel names the request in a message: pull request #4.
	requestLabel() string
}

// status is the commit status observe sets.
type status struct {
	Description string
	TargetURL   string
}

// Run publishes the observation and returns its diagnostics, nil when both
// the status and the note were published.
func Run(ctx context.Context, req Request) []diag.Diagnostic {
	o := req.Observation
	output := req.Output
	if output == nil {
		output = io.Discard
	}
	cr := o.Subject.ChangeRequest
	var tokenEnv string
	switch o.Subject.Provider {
	case ci.ProviderGitHub:
		tokenEnv = GitHubTokenEnv
	case ci.ProviderGitLab:
		tokenEnv = GitLabTokenEnv
	default:
		return skipped("this CI is neither GitHub Actions nor GitLab CI, so --publish has no pull or merge request to publish on")
	}
	if cr == nil || cr.Number <= 0 {
		return skipped("this job runs for no pull request or merge request, so --publish published nothing")
	}
	token := strings.TrimSpace(req.Getenv(tokenEnv))
	if token == "" {
		return []diag.Diagnostic{diag.Warningf(CodeTokenMissing, tokenEnv,
			"%s is unset, so --publish published nothing on %s", tokenEnv, label(o.Subject.Provider, cr.Number))}
	}
	client := apiClient(req.Client)
	var h host
	var err error
	switch o.Subject.Provider {
	case ci.ProviderGitHub:
		h, err = newGitHub(client, req.Getenv, token, o)
	default:
		h, err = newGitLab(client, req.Getenv, token, o)
	}
	if err != nil {
		return failed("%v; nothing was published", err)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(what string, err error) diag.Diagnostic {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return diag.Warningf(CodeFailed, "publish", "%s on %s ran past %s", what, h.requestLabel(), timeout)
		}
		return diag.Warningf(CodeFailed, "publish", "%s on %s failed: %v", what, h.requestLabel(), err)
	}

	var diags []diag.Diagnostic
	if sha := h.statusSHA(); sha == "" {
		diags = append(diags, diag.Warningf(CodeFailed, "publish",
			"the CI names no commit of %s, so no commit status was set", h.requestLabel()))
	} else if err := h.setStatus(ctx, sha, status{Description: Description(o), TargetURL: runURL(o.CI.URL)}); err != nil {
		diags = append(diags, fail("setting the commit status "+StatusName, err))
	} else {
		fmt.Fprintf(output, "putnami observe: set the commit status %s on %s\n", StatusName, short(sha))
	}

	body := Note(o)
	id, err := h.findNote(ctx)
	if err != nil {
		return append(diags, fail("reading the notes", err))
	}
	verb := "updated"
	var where string
	if id == "" {
		verb = "created"
		where, err = h.createNote(ctx, body)
	} else {
		where, err = h.updateNote(ctx, id, body)
	}
	if err != nil {
		return append(diags, fail("writing the summary note", err))
	}
	if where == "" {
		where = h.requestLabel()
	}
	fmt.Fprintf(output, "putnami observe: %s the summary note %s\n", verb, where)
	return diags
}

func skipped(message string) []diag.Diagnostic {
	return []diag.Diagnostic{{Severity: diag.Info, Code: CodeSkipped, Field: "publish", Message: message}}
}

func failed(format string, args ...any) []diag.Diagnostic {
	return []diag.Diagnostic{diag.Warningf(CodeFailed, "publish", format, args...)}
}

func label(provider string, number int) string {
	return codehost.ChangeRequestLabel(provider, number)
}

// runURL keeps an https run page without credentials, the only link a
// status carries.
func runURL(raw string) string {
	if _, err := apiBase(raw); err != nil {
		return ""
	}
	return raw
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
