// Package provider implements the tasks and proposals contracts of
// go.putnami.dev/protocol/collaboration on GitHub: tasks are the issues of one
// repository, proposals are its pull requests.
//
// Every operation sends its requests through the GitHub REST API (and GraphQL
// for the draft flag of a pull request and to learn which numbers
// discussions hold) with the credential the gh CLI would use. A write is sent once. When its outcome is unknown — the connection
// broke after the request left, the deadline passed, or GitHub answered 5xx —
// the operation reads the repository back and answers from what it finds; if
// the read cannot establish the outcome, it answers unresolved and never
// repeats the write itself.
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
)

// Provider answers operations.
type Provider struct {
	// Getenv reads the process environment: credentials and the loopback
	// API override.
	Getenv func(string) string
	// TokenCommand asks the gh CLI for a stored token when no variable names
	// one.
	TokenCommand github.TokenCommand
	// Client tunes every GitHub client.
	Client github.Options
	// ReconcileAttempts is how many times a write whose outcome is unknown is
	// read back before the operation answers unresolved. Default 3.
	ReconcileAttempts int
	// ReconcilePause waits before read-back attempt n (1-based) after the
	// first. The default waits 1s, then 2s.
	ReconcilePause func(ctx context.Context, attempt int) error
	// HeadLagPause waits before re-read n (1-based) of a pull request whose
	// head is behind the branch it was pushed to. The default waits 1s, 2s,
	// 3s, then 4s.
	HeadLagPause func(ctx context.Context, attempt int) error
	// Budget bounds one operation, every request included. Default 100s,
	// below the manifest's tool timeout, so the provider answers before the
	// orchestrator gives up on it.
	Budget time.Duration

	mu      sync.Mutex
	clients map[string]*github.Client
}

// New returns a provider on the process environment and the gh CLI.
func New() *Provider {
	return &Provider{Getenv: os.Getenv, TokenCommand: github.GHAuthToken}
}

// Handlers is the dispatch table collab.Serve routes on.
func (p *Provider) Handlers() map[collab.OperationKey]collab.Handler {
	key := func(contract, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.ContractTasks, collab.OperationFind):       p.taskHandler((*taskSession).find),
		key(collab.ContractTasks, collab.OperationGet):        p.taskHandler((*taskSession).get),
		key(collab.ContractTasks, collab.OperationCreate):     p.taskHandler((*taskSession).create),
		key(collab.ContractTasks, collab.OperationUpdate):     p.taskHandler((*taskSession).update),
		key(collab.ContractTasks, collab.OperationTransition): p.taskHandler((*taskSession).transition),
		key(collab.ContractTasks, collab.OperationAssign):     p.taskHandler((*taskSession).assign),
		key(collab.ContractTasks, collab.OperationLink):       p.taskHandler((*taskSession).link),
		key(collab.ContractProposals, collab.OperationFind):   p.proposalHandler((*proposalSession).find),
		key(collab.ContractProposals, collab.OperationUpsert): p.proposalHandler((*proposalSession).upsert),
		key(collab.ContractProposals, collab.OperationStatus): p.proposalHandler((*proposalSession).status),
		key(collab.ContractProposals, collab.OperationReview): p.proposalHandler((*proposalSession).publish),
		key(collab.ContractProposals, collab.OperationMerge):  p.proposalHandler((*proposalSession).merge),
	}
}

// Redact removes every credential this provider resolved, and every string
// shaped like a GitHub token, from what it is about to write.
func (p *Provider) Redact(data []byte) []byte {
	p.mu.Lock()
	secrets := make([]string, 0, len(p.clients))
	for _, client := range p.clients {
		secrets = append(secrets, client.Secrets()...)
	}
	p.mu.Unlock()
	return []byte(github.Redact(string(data), secrets))
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
	p      *Provider
	ctx    context.Context
	client *github.Client
	repo   repository
	login  string
}

// open resolves the repository and the client of a call.
func (p *Provider) open(ctx context.Context, settings repositorySettings) (*session, *collab.Failure) {
	repo, failure := parseRepository(settings.Repository, settings.Host)
	if failure != nil {
		return nil, failure
	}
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	endpoint, err := github.ResolveEndpoint(repo.host, getenv)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "%v", err)
	}
	key := endpoint.Host + " " + endpoint.REST.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*github.Client{}
	}
	client, cached := p.clients[key]
	if !cached {
		client = github.New(endpoint, github.NewCredential(endpoint.Host, getenv, p.TokenCommand), p.Client)
		p.clients[key] = client
	}
	return &session{p: p, ctx: ctx, client: client, repo: repo}, nil
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
	reasonUnauthenticated = "github.unauthenticated"
	reasonUnreachable     = "github.unreachable"
	reasonUnavailable     = "github.unavailable"
	reasonRateLimited     = "github.rate_limited"
	reasonUnauthorized    = "github.unauthorized"
	reasonForbidden       = "github.forbidden"
	reasonNotFound        = "github.not_found"
	reasonGitHubConflict  = "github.conflict"
	reasonRejected        = "github.rejected"
	reasonUncertain       = "github.uncertain"
	reasonIncomplete      = "github.incomplete"
	reasonStateUnmapped   = "state.unmapped"
	reasonStateLabel      = "label.state"
	reasonHeadMoved       = "head.moved"
	reasonHeadLagging     = "head.lagging"
	reasonHeadMissing     = "head.missing"
	reasonNotMergeable    = "merge.not_mergeable"
	reasonMergeDisabled   = "merge.disabled"
	reasonRepository      = "repository.other"
	reasonPullRequest     = "task.is_pull_request"
	reasonTooManyReviews  = "reviews.unbounded"
	reasonKeyUnverifiable = "idempotency.unverifiable"
)

// fail renders a failed request as the contract outcome it proves. A request
// that failed with an unknown outcome is a read's failure here: a write
// reconciles before it reports one.
func fail(err *github.Error, format string, args ...any) *collab.Failure {
	message := fmt.Sprintf(format, args...) + ": " + err.Message
	switch err.Kind {
	case github.NoCredential:
		return collab.Fail(collab.OutcomeDenied, reasonUnauthenticated, "%s", err.Message)
	case github.NotSent:
		return retryable(collab.Fail(collab.OutcomeUnavailable, reasonUnreachable, "%s", message))
	case github.Uncertain:
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
		return collab.Fail(collab.OutcomeConflict, reasonGitHubConflict, "%s", message)
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

// authenticated returns the login of the credential's account.
func (s *session) authenticated() (string, *collab.Failure) {
	if s.login != "" {
		return s.login, nil
	}
	var user ghUser
	if _, err := s.client.Get(s.ctx, "/user", nil, &user); err != nil {
		return "", fail(err, "read the authenticated account")
	}
	if user.Login == "" {
		return "", collab.Fail(collab.OutcomeUnavailable, reasonUnavailable, "GitHub named no authenticated account")
	}
	s.login = user.Login
	return s.login, nil
}

// repositorySettings are the settings both contracts share.
type repositorySettings struct {
	// Repository is the owner/name of the repository served.
	Repository string `json:"repository"`
	// Host is the GitHub host, github.com unless set.
	Host string `json:"host,omitempty"`
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
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "the GitHub provider's settings are %s: %v", names, err)
	}
	return nil
}

// repository is the repository a binding serves.
type repository struct {
	host  string
	owner string
	name  string
	// display is the owner/name as the settings spell it; proposals name it.
	display string
	// source qualifies every reference this repository issues.
	source string
}

var repositoryPattern = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)

func parseRepository(value, host string) (repository, *collab.Failure) {
	match := repositoryPattern.FindStringSubmatch(value)
	if match == nil || match[2] == "." || match[2] == ".." {
		return repository{}, collab.Fail(collab.OutcomeInvalid, reasonSettings,
			"settings.repository must name the GitHub repository as owner/name, got %q", value)
	}
	if host == "" {
		host = github.DefaultHost
	}
	if !github.ValidHost(host) {
		return repository{}, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.host %q is not a bare host name", host)
	}
	locator := strings.ToLower(value)
	if !strings.EqualFold(host, github.DefaultHost) {
		locator = strings.ToLower(host) + "/" + locator
	}
	return repository{host: host, owner: match[1], name: match[2], display: value, source: "github:" + locator}, nil
}

// named checks a request's repository against the one the binding serves and
// returns the spelling every answer echoes: the request's own, or the
// configured one.
func (r repository) named(requested string) (string, *collab.Failure) {
	if requested == "" {
		return r.display, nil
	}
	if !strings.EqualFold(requested, r.display) {
		return "", collab.Fail(collab.OutcomeInvalid, reasonRepository,
			"this binding serves %s; repository %s is not served", r.display, requested)
	}
	return requested, nil
}

// path is a REST path under this repository.
func (r repository) path(segments ...string) string {
	return github.Path(append([]string{"repos", r.owner, r.name}, segments...)...)
}

// number reads the issue or pull request number of a reference to this
// repository; the source compares case-insensitively, as GitHub owner and
// repository names do. ok is false for any other reference.
func (r repository) number(ref collab.Ref) (int, bool) {
	if !strings.EqualFold(ref.Source, r.source) {
		return 0, false
	}
	number, err := strconv.Atoi(ref.ID)
	if err != nil || number <= 0 || strconv.Itoa(number) != ref.ID {
		return 0, false
	}
	return number, true
}

func (r repository) ref(number int) collab.Ref {
	return collab.Ref{Source: r.source, ID: strconv.Itoa(number)}
}

// sameRepository reports whether an API URL of a repository names this one.
func (r repository) sameRepository(apiURL string) bool {
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), strings.ToLower("/repos/"+r.owner+"/"+r.name))
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

// revisionOf is a revision token over the content a caller can see and
// change: it changes whenever that content changes, and not when only
// comments or reactions do.
func revisionOf(value any) string {
	return "v" + digest(value)[:32]
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

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func commit(value string) string {
	if commitPattern.MatchString(value) {
		return value
	}
	return ""
}

// sameCommit compares two commits where either may be abbreviated.
func sameCommit(a, b string) bool {
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
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

// boundedLogins keeps the logins the contract carries as assignees, in
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
