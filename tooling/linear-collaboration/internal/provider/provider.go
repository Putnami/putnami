// Package provider implements the tasks contract of
// go.putnami.dev/protocol/collaboration on Linear: tasks are the issues of one
// team.
//
// Every operation sends its requests through Linear's GraphQL API with the key
// LINEAR_API_KEY holds. A mutation is sent once. When its outcome is unknown —
// the connection broke after the request left, the deadline passed, or Linear
// answered 5xx — the operation reads the issue back and answers from what it
// finds; if the read cannot establish the outcome, it answers unresolved and
// never repeats the mutation itself.
package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/linear-collaboration/internal/linear"
)

// Provider answers operations.
type Provider struct {
	// Getenv reads the process environment: the API key and the loopback
	// endpoint override.
	Getenv func(string) string
	// Client tunes the Linear client.
	Client linear.Options
	// ReconcileAttempts is how many times a mutation whose outcome is unknown
	// is read back before the operation answers unresolved. Default 3.
	ReconcileAttempts int
	// ReconcilePause waits before read-back attempt n (1-based) after the
	// first. The default waits 1s, then 2s.
	ReconcilePause func(ctx context.Context, attempt int) error
	// Budget bounds one operation, every request included. Default 100s,
	// below the manifest's provider timeout, so the provider answers before the
	// orchestrator gives up on it.
	Budget time.Duration

	mu     sync.Mutex
	client *linear.Client
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

// Redact removes the key this provider resolved, and every string shaped like
// a Linear key, from what it is about to write.
func (p *Provider) Redact(data []byte) []byte {
	p.mu.Lock()
	var secrets []string
	if p.client != nil {
		secrets = p.client.Secrets()
	}
	p.mu.Unlock()
	return []byte(linear.Redact(string(data), secrets))
}

// Settings are a tasks binding's settings.
type Settings struct {
	// Team is the key of the team served, such as ENG.
	Team string `json:"team"`
}

var teamKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// decodeSettings decodes a binding's settings strictly: an unknown setting is
// a configuration error, never ignored.
func decodeSettings(raw json.RawMessage) (Settings, *collab.Failure) {
	var settings Settings
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, collab.Fail(collab.OutcomeInvalid, reasonSettings, "the Linear provider's settings are team: %v", err)
	}
	if !teamKeyPattern.MatchString(settings.Team) {
		return settings, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.team must name the Linear team by its key, such as ENG, got %q", settings.Team)
	}
	return settings, nil
}

// session is one call: the client, the organization and the team served.
type session struct {
	p      *Provider
	ctx    context.Context
	client *linear.Client
	source string
	team   team
	// states caches the workflow states of a team, by team id.
	states map[string][]workflowState
}

func (p *Provider) handler(run func(*session, any) (any, *collab.Failure)) collab.Handler {
	return func(ctx context.Context, call collab.Call) (any, *collab.Failure) {
		settings, failure := decodeSettings(call.Settings)
		if failure != nil {
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

// open resolves the client, the organization and the team of a call.
func (p *Provider) open(ctx context.Context, settings Settings) (*session, *collab.Failure) {
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint, err := linear.ResolveEndpoint(getenv)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "%v", err)
	}
	p.mu.Lock()
	if p.client == nil {
		p.client = linear.New(endpoint, linear.NewCredential(getenv), p.Client)
	}
	client := p.client
	p.mu.Unlock()
	s := &session{p: p, ctx: ctx, client: client, states: map[string][]workflowState{}}
	var data struct {
		Organization struct {
			URLKey string `json:"urlKey"`
		} `json:"organization"`
		Teams struct {
			Nodes []team `json:"nodes"`
		} `json:"teams"`
	}
	if err := client.Query(ctx, "PutnamiServed", servedQuery, map[string]any{"key": settings.Team}, &data); err != nil {
		return nil, fail(err, "read the organization and team %s", settings.Team)
	}
	if !urlKeyPattern.MatchString(data.Organization.URLKey) {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "Linear named no organization for the credential")
	}
	s.source = "linear:" + strings.ToLower(data.Organization.URLKey)
	for _, candidate := range data.Teams.Nodes {
		if strings.EqualFold(candidate.Key, settings.Team) {
			s.team = candidate
		}
	}
	if s.team.ID == "" {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "the Linear organization %s has no team with key %s", data.Organization.URLKey, settings.Team)
	}
	return s, nil
}

var urlKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

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
	reasonUnauthenticated = "linear.unauthenticated"
	reasonUnreachable     = "linear.unreachable"
	reasonUnavailable     = "linear.unavailable"
	reasonRateLimited     = "linear.rate_limited"
	reasonUnauthorized    = "linear.unauthorized"
	reasonForbidden       = "linear.forbidden"
	reasonNotFound        = "linear.not_found"
	reasonRejected        = "linear.rejected"
	reasonUncertain       = "linear.uncertain"
	reasonIncomplete      = "linear.incomplete"
	reasonStateUnmapped   = "state.unmapped"
	reasonTaskMissing     = "task.missing"
)

// fail renders a failed request as the contract outcome it proves. A request
// that failed with an unknown outcome is a read's failure here: a mutation
// reconciles before it reports one.
func fail(err *linear.Error, format string, args ...any) *collab.Failure {
	message := fmt.Sprintf(format, args...) + ": " + err.Message
	switch err.Kind {
	case linear.NoCredential:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthenticated, "%s", err.Message)
	case linear.NotSent:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnreachable, "%s", message))
	case linear.Uncertain:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "%s", message))
	}
	switch {
	case err.RateLimited:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonRateLimited, "%s", message))
	case err.Status == 401 || err.Code == "AUTHENTICATION_ERROR":
		return collab.Fail(collab.OutcomeDenied, reasonUnauthorized, "%s", message)
	case err.Status == 403 || err.Code == "FORBIDDEN":
		return collab.Fail(collab.OutcomeDenied, reasonForbidden, "%s", message)
	case err.NotFound:
		return collab.Fail(collab.OutcomeNotFound, reasonNotFound, "%s", message)
	}
	return collab.Fail(collab.OutcomeInvalid, reasonRejected, "%s", message)
}

func retryable(failure *collab.Failure) *collab.Failure {
	failure.Error.Retryable = true
	return failure
}

// unresolved is the answer to a mutation whose outcome could not be
// established. It is never retryable; reconcile names what settles it.
func unresolved(reconcile, format string, args ...any) *collab.Failure {
	failure := collab.Fail(collab.OutcomeUnresolved, reasonUncertain, format, args...)
	failure.Error.Reconcile = reconcile
	return failure
}

// reconcile reads back a mutation whose outcome is unknown: settled runs one
// read and reports whether it established the outcome. It is tried up to
// ReconcileAttempts times; a read that fails counts as not established.
func (s *session) reconcile(settled func() (bool, *collab.Failure)) bool {
	attempts := s.p.ReconcileAttempts
	if attempts <= 0 {
		attempts = 3
	}
	pause := s.p.ReconcilePause
	if pause == nil {
		pause = reconcilePause
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

func reconcilePause(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// digest is the hex SHA-256 of a value's JSON encoding.
func digest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte(fmt.Sprint(value))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// displayURL keeps a link only when it is an absolute http or https URL
// without credentials.
func displayURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		len(value) > collab.MaxURLLength {
		return ""
	}
	return value
}

var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)

func timestamp(value string) string {
	if timestampPattern.MatchString(value) {
		return value
	}
	return ""
}

// oneLine fits a title into the contract: one line of at most limit
// characters, never empty.
func oneLine(value string, limit int, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		value = fallback
	}
	if utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		value = string(runes[:limit-1]) + "…"
	}
	return value
}

// boundedBody cuts a body to the contract's limit on a character boundary.
func boundedBody(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) <= collab.MaxBodyBytes {
		return value
	}
	cut := collab.MaxBodyBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// token keeps a value the contract accepts as a token: printable, without
// whitespace, bounded.
func token(value string) string {
	if value == "" || utf8.RuneCountInString(value) > collab.MaxTokenLength || strings.ContainsAny(value, " \t\r\n") {
		return ""
	}
	return value
}

// boundedLabels keeps the labels the contract carries, in order: at most
// MaxLabels distinct, non-empty names of at most MaxLabelLength characters.
// The list is never nil, so an answer reports a label-less item as [].
func boundedLabels(names []string) []string {
	labels := []string{}
	for _, label := range names {
		if len(labels) < collab.MaxLabels && label != "" && utf8.RuneCountInString(label) <= collab.MaxLabelLength && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}
