// Package provider implements the tasks contract of
// go.putnami.dev/protocol/collaboration on GitLab: tasks are the issues of
// one project.
//
// Every operation sends its requests through the GitLab REST API v4 with the
// credential glab would use. A write is sent once. When its outcome is
// unknown — the connection broke after the request left, the deadline passed,
// or GitLab answered 5xx — the operation reads the project back and answers
// from what it finds; if the read cannot establish the outcome, it answers
// unresolved and never repeats the write itself.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// Provider answers operations.
type Provider struct {
	// Getenv reads the process environment: credentials and the loopback
	// API override.
	Getenv func(string) string
	// TokenCommand asks the glab CLI for a stored token when no variable
	// names one.
	TokenCommand gitlab.TokenCommand
	// Client tunes every GitLab client.
	Client gitlab.Options
	// ReconcileAttempts is how many times a write whose outcome is unknown is
	// read back before the operation answers unresolved. Default 3.
	ReconcileAttempts int
	// ReconcilePause waits before read-back attempt n (1-based) after the
	// first. The default waits 1s, then 2s.
	ReconcilePause func(ctx context.Context, attempt int) error
	// Budget bounds one operation, every request included. Default 100s,
	// below the manifest's provider timeout, so the provider answers before
	// the orchestrator gives up on it.
	Budget time.Duration

	mu      sync.Mutex
	clients map[string]*gitlab.Client
}

// New returns a provider on the process environment and the glab CLI.
func New() *Provider {
	return &Provider{Getenv: os.Getenv, TokenCommand: gitlab.GlabConfigToken}
}

// Handlers is the dispatch table collab.Serve routes on.
func (p *Provider) Handlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractTasks, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationFind):       p.taskHandler((*taskSession).find),
		key(collab.OperationGet):        p.taskHandler((*taskSession).get),
		key(collab.OperationCreate):     p.taskHandler((*taskSession).create),
		key(collab.OperationUpdate):     p.taskHandler((*taskSession).update),
		key(collab.OperationTransition): p.taskHandler((*taskSession).transition),
		key(collab.OperationAssign):     p.taskHandler((*taskSession).assign),
	}
}

// Redact removes every credential this provider resolved, and every string
// shaped like a GitLab token, from what it is about to write.
func (p *Provider) Redact(data []byte) []byte {
	p.mu.Lock()
	secrets := make([]string, 0, len(p.clients))
	for _, client := range p.clients {
		secrets = append(secrets, client.Secrets()...)
	}
	p.mu.Unlock()
	return []byte(gitlab.Redact(string(data), secrets))
}

// inputOf returns a call's validated request document. collab.Serve decodes
// it into the operation's input type, so any other type is a dispatch defect.
func inputOf[T any](input any) (T, *collab.Failure) {
	typed, ok := input.(T)
	if !ok {
		return typed, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the request is a %T", input)
	}
	return typed, nil
}

// session is what every operation of one call shares.
type session struct {
	p       *Provider
	ctx     context.Context
	client  *gitlab.Client
	project project
	me      *glUser
}

// open resolves the project and the client of a call.
func (p *Provider) open(ctx context.Context, settings TaskSettings) (*session, *collab.Failure) {
	project, failure := parseProject(settings.Project, settings.Host)
	if failure != nil {
		return nil, failure
	}
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint, err := gitlab.ResolveEndpoint(project.host, getenv)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "%v", err)
	}
	key := endpoint.Host + " " + endpoint.REST.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*gitlab.Client{}
	}
	client, cached := p.clients[key]
	if !cached {
		client = gitlab.New(endpoint, gitlab.NewCredential(endpoint.Host, getenv, p.TokenCommand), p.Client)
		p.clients[key] = client
	}
	return &session{p: p, ctx: ctx, client: client, project: project}, nil
}

// budget bounds one operation.
func (p *Provider) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := p.Budget
	if budget <= 0 {
		budget = 100 * time.Second
	}
	return context.WithTimeout(ctx, budget)
}

// Reasons this provider reports, beside the contract's own.
const (
	reasonSettings        = "settings.invalid"
	reasonUnauthenticated = "gitlab.unauthenticated"
	reasonUnreachable     = "gitlab.unreachable"
	reasonUnavailable     = "gitlab.unavailable"
	reasonRateLimited     = "gitlab.rate_limited"
	reasonUnauthorized    = "gitlab.unauthorized"
	reasonForbidden       = "gitlab.forbidden"
	reasonNotFound        = "gitlab.not_found"
	reasonGitLabConflict  = "gitlab.conflict"
	reasonRejected        = "gitlab.rejected"
	reasonUncertain       = "gitlab.uncertain"
	reasonIncomplete      = "gitlab.incomplete"
	reasonStateUnmapped   = "state.unmapped"
	reasonStateLabel      = "label.state"
	reasonLabelComma      = "label.comma"
	reasonUnknownUser     = "assignee.unknown"
	reasonKeyUnverifiable = "idempotency.unverifiable"
)

// fail renders a failed request as the contract outcome it proves. A request
// that failed with an unknown outcome is a read's failure here: a write
// reconciles before it reports one.
func fail(err *gitlab.Error, format string, args ...any) *collab.Failure {
	message := fmt.Sprintf(format, args...) + ": " + err.Message
	switch err.Kind {
	case gitlab.NoCredential:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthenticated, "%s", err.Message)
	case gitlab.NotSent:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnreachable, "%s", message))
	case gitlab.Uncertain:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "%s", message))
	}
	switch {
	case err.RateLimited:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonRateLimited, "%s", message))
	case err.Status == 401:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthorized, "%s", message)
	case err.Status == 403:
		return collab.Fail(collab.OutcomeDenied, reasonForbidden, "%s", message)
	case err.Status == 404 || err.Status == 410:
		return collab.Fail(collab.OutcomeNotFound, reasonNotFound, "%s", message)
	case err.Status == 409 || err.Status == 412:
		return collab.Fail(collab.OutcomeConflict, reasonGitLabConflict, "%s", message)
	}
	return collab.Fail(collab.OutcomeInvalid, reasonRejected, "%s", message)
}

func retryable(failure *collab.Failure) *collab.Failure {
	failure.Error.Retryable = true
	return failure
}

// unresolved is the answer to a write whose outcome could not be
// established. It is never retryable; reconcile names what settles it.
func unresolved(reconcile, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeUnresolved, reasonUncertain, format, args...)
	failure.Error.Reconcile = reconcile
	return failure
}

func conflictAt(current, reason, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeConflict, reason, format, args...)
	failure.Error.Current = current
	return failure
}

// reconcile reads back a write whose outcome is unknown: settled runs one
// read and reports whether it established the outcome. It is tried up to
// ReconcileAttempts times; a read that fails counts as not established.
func (s *session) reconcile(settled func() (bool, *collab.Failure)) bool {
	attempts := s.p.ReconcileAttempts
	if attempts <= 0 {
		attempts = 3
	}
	pause := s.p.ReconcilePause
	if pause == nil {
		pause = collabhelper.ReconcilePause
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && pause(s.ctx, attempt-1) != nil {
			return false
		}
		if done, failure := settled(); failure == nil && done {
			return true
		}
	}
	return false
}

// authenticated returns the credential's account.
func (s *session) authenticated() (*glUser, *collab.Failure) {
	if s.me != nil {
		return s.me, nil
	}
	var user glUser
	if _, err := s.client.Get(s.ctx, "/user", nil, &user); err != nil {
		return nil, fail(err, "read the authenticated account")
	}
	if user.ID <= 0 || user.Username == "" {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "GitLab named no authenticated account")
	}
	s.me = &user
	return s.me, nil
}

// decodeSettings decodes a binding's settings strictly: an unknown setting is
// a configuration error, never ignored.
func decodeSettings(raw json.RawMessage, into any, names string) *collab.Failure {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "the GitLab provider's settings are %s: %v", names, err)
	}
	return nil
}

// project is the project a binding serves.
type project struct {
	host string
	// path is the full path as the settings spell it.
	path string
	// source qualifies every reference this project issues.
	source string
}

// segmentPattern is one namespace or project path segment.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]{0,254}$`)

// maxDepth bounds the segments of a project path: GitLab nests at most 20
// groups.
const maxDepth = 21

func parseProject(value, host string) (project, *collab.Failure) {
	segments := strings.Split(value, "/")
	valid := len(segments) >= 2 && len(segments) <= maxDepth
	for _, segment := range segments {
		if !segmentPattern.MatchString(segment) || segment == "." || segment == ".." ||
			strings.HasSuffix(segment, ".git") || strings.HasSuffix(segment, ".atom") {
			valid = false
		}
	}
	if !valid {
		return project{}, collab.Fail(collab.OutcomeInvalid, reasonSettings,
			"settings.project must name the GitLab project by its full path, namespace/name, got %q", value)
	}
	if host == "" {
		host = gitlab.DefaultHost
	}
	if !collabhelper.ValidHost(host) {
		return project{}, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.host %q is not a bare host name", host)
	}
	source := "gitlab:" + strings.ToLower(host) + "/" + strings.ToLower(value)
	if len(source) > collab.MaxTokenLength {
		return project{}, collab.Fail(collab.OutcomeInvalid, reasonSettings,
			"the references of %s on %s would be longer than %d characters", value, host, collab.MaxTokenLength)
	}
	return project{host: host, path: value, source: source}, nil
}

// apiPath is a REST path under this project.
func (p project) apiPath(segments ...string) string {
	return gitlab.Path(append([]string{"projects", p.path}, segments...)...)
}

// iid reads the issue iid of a reference to this project. The source must be
// exactly the lowercase one this project issues. ok is false for any other
// reference.
func (p project) iid(ref collab.Ref) (int, bool) {
	if ref.Source != p.source {
		return 0, false
	}
	iid, err := strconv.Atoi(ref.ID)
	if err != nil || iid <= 0 || strconv.Itoa(iid) != ref.ID {
		return 0, false
	}
	return iid, true
}

// missing explains why a reference names nothing here, and names this
// project's source when the reference spells it in another casing.
func (p project) missing(ref collab.Ref) string {
	message := fmt.Sprintf("no task %s in source %s", ref.ID, ref.Source)
	if ref.Source != p.source && strings.EqualFold(ref.Source, p.source) {
		message += fmt.Sprintf("; this provider issues the source as %s", p.source)
	}
	return message
}

func (p project) ref(iid int) collab.Ref {
	return collab.Ref{Source: p.source, ID: strconv.Itoa(iid)}
}

// revisionOf is a revision token over the content a caller can see and
// change: it changes whenever that content changes, and not when only
// comments or reactions do.
func revisionOf(value any) string {
	return "v" + collabhelper.Digest(value)[:32]
}

// token keeps a value the contract accepts as a token: printable, without
// whitespace, bounded.
func token(value string) string {
	if value == "" || utf8.RuneCountInString(value) > collab.MaxTokenLength || strings.ContainsAny(value, " \t\r\n") {
		return ""
	}
	return value
}

// boundedLogins keeps the usernames the contract carries as assignees, in
// order: at most MaxListMembers distinct tokens. The list is never nil.
func boundedLogins(logins []string) []string {
	names := []string{}
	for _, login := range logins {
		if login = token(login); login != "" && len(names) < collab.MaxListMembers && !slices.Contains(names, login) {
			names = append(names, login)
		}
	}
	return names
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// glUser is a GitLab user as the API answers it.
type glUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// glIssue is a GitLab issue as the API answers it.
type glIssue struct {
	ID          int64    `json:"id"`
	IID         int      `json:"iid"`
	ProjectID   int64    `json:"project_id"`
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	Assignees   []glUser `json:"assignees"`
	Author      *glUser  `json:"author"`
	WebURL      string   `json:"web_url"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Links       struct {
		ClosedAsDuplicateOf *string `json:"closed_as_duplicate_of"`
	} `json:"_links"`
}

// closed reports whether GitLab closed the issue.
func (i *glIssue) closed() bool { return i.State == "closed" }

// duplicate reports whether GitLab closed the issue as a duplicate of another.
func (i *glIssue) duplicate() bool {
	return i.closed() && deref(i.Links.ClosedAsDuplicateOf) != ""
}

func (i *glIssue) labelNames() []string {
	if i.Labels == nil {
		return []string{}
	}
	return slices.Clone(i.Labels)
}

func (i *glIssue) assigneeLogins() []string {
	logins := make([]string, 0, len(i.Assignees))
	for _, user := range i.Assignees {
		logins = append(logins, user.Username)
	}
	return logins
}

// createdMillis is the issue's creation time in milliseconds, 0 when GitLab
// gave none it can read.
func (i *glIssue) createdMillis() int64 {
	created, err := time.Parse(time.RFC3339Nano, i.CreatedAt)
	if err != nil {
		return 0
	}
	return created.UnixMilli()
}
