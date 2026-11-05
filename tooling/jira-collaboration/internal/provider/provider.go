// Package provider implements the tasks contract of
// go.putnami.dev/protocol/collaboration on Jira Cloud: tasks are the issues of
// one site, found and created in one project.
//
// Every operation sends its requests through the Jira Cloud REST API v2 with
// the credential JIRA_EMAIL and JIRA_API_TOKEN name. A write is sent once.
// When its outcome is unknown — the connection broke after the request left,
// the deadline passed, or Jira answered 5xx — the operation reads Jira back
// and answers from what it finds; if the read cannot establish the outcome,
// it answers unresolved and never repeats the write itself.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// Provider answers operations.
type Provider struct {
	// Getenv reads the process environment: the credential and the loopback
	// API override.
	Getenv func(string) string
	// Client tunes every Jira client.
	Client jira.Options
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
	clients map[string]*jira.Client
}

// New returns a provider on the process environment.
func New() *Provider {
	return &Provider{Getenv: os.Getenv}
}

// Handlers is the dispatch table collab.Serve routes on.
func (p *Provider) Handlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractTasks, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationFind):       p.handler((*session).find),
		key(collab.OperationGet):        p.handler((*session).get),
		key(collab.OperationCreate):     p.handler((*session).create),
		key(collab.OperationUpdate):     p.handler((*session).update),
		key(collab.OperationTransition): p.handler((*session).transition),
	}
}

// Redact removes every credential this provider resolved, and every string
// shaped like an Atlassian API token, from what it is about to write.
func (p *Provider) Redact(data []byte) []byte {
	p.mu.Lock()
	secrets := make([]string, 0, 2*len(p.clients))
	for _, client := range p.clients {
		secrets = append(secrets, client.Secrets()...)
	}
	p.mu.Unlock()
	return []byte(jira.Redact(string(data), secrets))
}

// Settings are a tasks binding's settings.
type Settings struct {
	// Site is the Jira Cloud host, such as acme.atlassian.net.
	Site string `json:"site"`
	// Project is the key of the project find and create serve.
	Project string `json:"project"`
	// IssueType is the issue type create opens; Task unless set.
	IssueType string `json:"issueType,omitempty"`
}

const settingNames = "site, project and issueType"

var projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}$`)

// session is what every operation of one call shares.
type session struct {
	p        *Provider
	ctx      context.Context
	client   *jira.Client
	settings Settings
	// source qualifies every reference this site issues.
	source  string
	account string
}

func (p *Provider) handler(run func(*session, any) (any, *collab.Failure)) collab.Handler {
	return func(ctx context.Context, call collab.Call) (any, *collab.Failure) {
		var settings Settings
		if failure := decodeSettings(call.Settings, &settings); failure != nil {
			return nil, failure
		}
		ctx, cancel := p.budget(ctx)
		defer cancel()
		s, failure := p.open(ctx, settings)
		if failure != nil {
			return nil, failure
		}
		return run(s, call.Input)
	}
}

// decodeSettings decodes a binding's settings strictly: an unknown setting is
// a configuration error, never ignored.
func decodeSettings(raw json.RawMessage, into *Settings) *collab.Failure {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "the Jira provider's settings are %s: %v", settingNames, err)
	}
	switch {
	case !collabhelper.ValidHost(into.Site):
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.site must name the Jira Cloud host, such as acme.atlassian.net, got %q", into.Site)
	case !projectPattern.MatchString(into.Project):
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.project must be a Jira project key, such as ENG, got %q", into.Project)
	case into.IssueType == "":
		into.IssueType = "Task"
	case utf8.RuneCountInString(into.IssueType) > 255 || strings.TrimSpace(into.IssueType) != into.IssueType:
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.issueType must name an issue type, got %q", into.IssueType)
	}
	return nil
}

// open resolves the client of a call.
func (p *Provider) open(ctx context.Context, settings Settings) (*session, *collab.Failure) {
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint, err := jira.ResolveEndpoint(settings.Site, getenv)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "%v", err)
	}
	key := strings.ToLower(endpoint.Site) + " " + endpoint.REST.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*jira.Client{}
	}
	client, cached := p.clients[key]
	if !cached {
		client = jira.New(endpoint, jira.NewCredential(getenv), p.Client)
		p.clients[key] = client
	}
	return &session{p: p, ctx: ctx, client: client, settings: settings, source: "jira:" + strings.ToLower(settings.Site)}, nil
}

// budget bounds one operation.
func (p *Provider) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := p.Budget
	if budget <= 0 {
		budget = 100 * time.Second
	}
	return context.WithTimeout(ctx, budget)
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

// Reasons this provider reports, beside the contract's own.
const (
	reasonSettings        = "settings.invalid"
	reasonUnauthenticated = "jira.unauthenticated"
	reasonUnreachable     = "jira.unreachable"
	reasonUnavailable     = "jira.unavailable"
	reasonRateLimited     = "jira.rate_limited"
	reasonUnauthorized    = "jira.unauthorized"
	reasonForbidden       = "jira.forbidden"
	reasonNotFound        = "jira.not_found"
	reasonJiraConflict    = "jira.conflict"
	reasonRejected        = "jira.rejected"
	reasonUncertain       = "jira.uncertain"
	reasonIncomplete      = "jira.incomplete"
	reasonStateUnmapped   = "state.unmapped"
	reasonNoTransition    = "transition.unavailable"
	reasonReservedLabel   = "label.reserved"
	reasonKeyUnverifiable = "idempotency.unverifiable"
)

// fail renders a failed request as the contract outcome it proves. A request
// that failed with an unknown outcome is a read's failure here: a write
// reconciles before it reports one.
func fail(err *jira.Error, format string, args ...any) *collab.Failure {
	message := fmt.Sprintf(format, args...) + ": " + err.Message
	switch err.Kind {
	case jira.NoCredential:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthenticated, "%s", err.Message)
	case jira.NotSent:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnreachable, "%s", message))
	case jira.Uncertain:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "%s", message))
	}
	switch {
	case err.RateLimited:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonRateLimited, "%s", message))
	case err.Status == 401:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthorized, "%s", message)
	case err.Status == 403:
		return collab.Fail(collab.OutcomeDenied, reasonForbidden, "%s", message)
	case err.Status == 404:
		return collab.Fail(collab.OutcomeNotFound, reasonNotFound, "%s", message)
	case err.Status == 409:
		return collab.Fail(collab.OutcomeConflict, reasonJiraConflict, "%s", message)
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

// myself returns the account id of the credential's account.
func (s *session) myself() (string, *collab.Failure) {
	if s.account != "" {
		return s.account, nil
	}
	var user jiraUser
	if err := s.client.Get(s.ctx, "/myself", nil, &user); err != nil {
		return "", fail(err, "read the authenticated account")
	}
	if user.AccountID == "" {
		return "", collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "Jira named no authenticated account")
	}
	s.account = user.AccountID
	return s.account, nil
}

// id reads the issue id a reference names. ok is false for a reference
// another source issued or an id that is not a positive number.
func (s *session) id(ref collab.Ref) (string, bool) {
	if !strings.EqualFold(ref.Source, s.source) {
		return "", false
	}
	number, err := strconv.ParseInt(ref.ID, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != ref.ID {
		return "", false
	}
	return ref.ID, true
}

// token keeps a value the contract accepts as a token: printable, without
// whitespace, bounded.
func token(value string) string {
	if value == "" || utf8.RuneCountInString(value) > collab.MaxTokenLength || strings.ContainsAny(value, " \t\r\n") {
		return ""
	}
	return value
}

// jiraTime is the layout of Jira's timestamps: 2026-09-24T08:00:00.000+0000.
const jiraTime = "2006-01-02T15:04:05.000-0700"

// timestamp renders a Jira timestamp in RFC 3339; empty when it is not one.
func timestamp(value string) string {
	parsed, err := time.Parse(jiraTime, value)
	if err != nil {
		return ""
	}
	return parsed.Format("2006-01-02T15:04:05.000Z07:00")
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
