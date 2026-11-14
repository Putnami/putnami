// Package collabcli is the collaboration provider of @putnami/cloud: the
// tasks and proposals contracts of go.putnami.dev/protocol/collaboration,
// served by Putnami Source through the Putnami GitHub App of the repository
// connected to the linked workspace. It needs no GitHub token on the machine:
// every call presents the signed-in Putnami session.
//
// A write is sent once. When its outcome is unknown (the connection broke
// after the request left, the deadline passed, or Source answered a server
// error), the operation answers unresolved with the read that settles it, and
// never sends it again.
package collabcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
)

// cloudTokenVar is the machine bearer a hosted run may provide. A stored
// session wins over it.
const cloudTokenVar = "PUTNAMI_CLOUD_TOKEN"

// Provider-owned reasons.
const (
	reasonSettings   = "settings.invalid"
	reasonUnlinked   = "workspace.unlinked"
	reasonSignIn     = "session.missing"
	reasonNotFound   = "source.not_found"
	reasonRefused    = "source.refused"
	reasonInvalid    = "source.invalid"
	reasonTransport  = "source.unreachable"
	reasonRateLimit  = "source.rate_limited"
	reasonUnanswered = "source.outcome_unknown"
)

// Provider answers the operations Putnami Source serves.
type Provider struct {
	// Env is the environment credentials and the Cloud link resolve from;
	// nil reads the process environment.
	Env map[string]string
	// IO carries the HTTP client and the clock of the session refresh and of
	// the Source calls. A nil Client or Now takes the default.
	IO clicore.IO
	// Connect returns the Source surface of a resolved workspace.
	Connect func(workspace *clicore.WorkspaceContext) (Source, error)
	// Budget bounds one operation, below the manifest's provider timeout so
	// the provider answers before the orchestrator gives up. Default 100s.
	Budget time.Duration

	mu      sync.Mutex
	secrets []string
}

// New returns a provider on the process environment.
func New() *Provider {
	return &Provider{Connect: connectSource}
}

func key(contract, operation string) collab.OperationKey {
	return collab.OperationKey{Contract: contract, Version: 1, Operation: operation}
}

// Handlers is the dispatch table collab.Serve routes on.
func (p *Provider) Handlers() map[collab.OperationKey]collab.Handler {
	const (
		proposalRead   = "read the proposal with proposals.find for its base and head"
		proposalStatus = "read the proposal with proposals.status"
		reviewRepeat   = "repeat proposals.review with the same idempotency key, or read the reviews with proposals.status"
		taskRead       = "read the task with tasks.get"
	)
	return map[collab.OperationKey]collab.Handler{
		key(collab.ContractProposals, collab.OperationFind):      served(p, "", Source.FindProposals),
		key(collab.ContractProposals, collab.OperationUpsert):    served(p, proposalRead, Source.UpsertProposal),
		key(collab.ContractProposals, collab.OperationStatus):    served(p, "", Source.ProposalStatus),
		key(collab.ContractProposals, collab.OperationComments):  served(p, "", Source.ProposalComments),
		key(collab.ContractProposals, collab.OperationReview):    served(p, reviewRepeat, Source.ReviewProposal),
		key(collab.ContractProposals, collab.OperationAutoMerge): served(p, proposalStatus, Source.AutoMergeProposal),
		key(collab.ContractProposals, collab.OperationMerge):     notServed,
		key(collab.ContractTasks, collab.OperationGet):           served(p, "", Source.GetTask),
		key(collab.ContractTasks, collab.OperationTransition):    served(p, taskRead, Source.TransitionTask),
		key(collab.ContractTasks, collab.OperationAssign):        served(p, taskRead, Source.AssignTask),
		key(collab.ContractTasks, collab.OperationFind):          notServed,
		key(collab.ContractTasks, collab.OperationCreate):        notServed,
		key(collab.ContractTasks, collab.OperationUpdate):        notServed,
		key(collab.ContractTasks, collab.OperationLink):          notServed,
	}
}

// notServed answers an operation Putnami Source does not serve. It sends
// nothing.
func notServed(_ context.Context, call collab.Call) (any, *collab.Failure) {
	return nil, collab.Fail(collab.OutcomeUnsupported, collab.ReasonOperationUnsupported,
		"Putnami Source does not serve %s.%s", call.Contract, call.Operation)
}

// served is the handler of one operation Source serves. A write names the
// read that settles it in reconcile; a read names none.
func served[I, R any](p *Provider, reconcile string, send func(Source, context.Context, *I) (*R, error)) collab.Handler {
	return func(ctx context.Context, call collab.Call) (any, *collab.Failure) {
		input, ok := call.Input.(*I)
		if !ok || input == nil {
			return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the request is a %T", call.Input)
		}
		if failure := checkSettings(call.Settings); failure != nil {
			return nil, failure
		}
		ctx, cancel := p.budget(ctx)
		defer cancel()
		source, failure := p.open(ctx, call.Request.WorkspaceRoot)
		if failure != nil {
			return nil, failure
		}
		result, err := send(source, ctx, input)
		if err != nil {
			return nil, failureOf(err, reconcile)
		}
		if result == nil {
			return nil, failureOf(errors.New("Putnami Source answered no document"), reconcile)
		}
		return result, nil
	}
}

// checkSettings accepts no member: the workspace, the repository and the
// credential all come from the Cloud link and the session.
func checkSettings(raw json.RawMessage) *collab.Failure {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &members); err != nil {
		return collab.Fail(collab.OutcomeInvalid, reasonSettings, "the binding settings are not an object")
	}
	if len(members) > 0 {
		return collab.Fail(collab.OutcomeInvalid, reasonSettings,
			"@putnami/cloud takes no binding settings: the workspace comes from putnami cloud setup and the credential from putnami cloud login")
	}
	return nil
}

func (p *Provider) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := p.Budget
	if budget <= 0 {
		budget = 100 * time.Second
	}
	return context.WithTimeout(ctx, budget)
}

// open resolves the linked workspace and its bearer, and connects Source.
// Nothing is sent to Source before it returns.
func (p *Provider) open(ctx context.Context, workspaceRoot string) (Source, *collab.Failure) {
	env := p.environment()
	if token := strings.TrimSpace(env[cloudTokenVar]); token != "" {
		p.remember(token)
	}
	link, err := clicore.ReadCloudLink(workspaceRoot)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnlinked, "read the Putnami Cloud link of the workspace: %v", err)
	}
	if clicore.ResolveWorkspaceID(nil, link) == "" {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnlinked,
			"the workspace is not linked to Putnami Cloud: run putnami cloud setup")
	}
	workspace, err := p.workspace(ctx, link, env)
	if err != nil {
		if clicore.ExitCode(err) == clicore.ExitAuth {
			return nil, collab.Fail(collab.OutcomeDenied, reasonSignIn, "no Putnami session can call Putnami Source: %v", err)
		}
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonTransport, "resolve the linked workspace: %v", err)
	}
	p.remember(bearerValue(workspace.AuthToken))
	refresh := workspace.RefreshAuth
	if refresh != nil {
		workspace.RefreshAuth = func() (clicore.Bearer, error) {
			fresh, err := refresh()
			if err == nil {
				p.remember(bearerValue(fresh))
			}
			return fresh, err
		}
	}
	connect := p.Connect
	if connect == nil {
		connect = connectSource
	}
	source, err := connect(workspace)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonTransport, "connect Putnami Source: %v", err)
	}
	return source, nil
}

// workspace resolves the linked workspace under the stored session first:
// PUTNAMI_CLOUD_TOKEN is presented only when no stored session gives a
// bearer, so a valid session always wins over it.
func (p *Provider) workspace(ctx context.Context, link map[string]any, env map[string]string) (*clicore.WorkspaceContext, error) {
	io := p.io(ctx)
	sessionEnv := make(map[string]string, len(env))
	for name, value := range env {
		if name != cloudTokenVar {
			sessionEnv[name] = value
		}
	}
	workspace, err := clicore.ResolveWorkspaceContextFromLink(nil, link, sessionEnv, io)
	if err == nil || strings.TrimSpace(env[cloudTokenVar]) == "" {
		return workspace, err
	}
	fallback, tokenErr := clicore.ResolveWorkspaceContextFromLink(nil, link, env, io)
	if tokenErr != nil {
		return nil, tokenErr
	}
	return fallback, nil
}

func (p *Provider) io(ctx context.Context) clicore.IO {
	io := p.IO
	if io.Client == nil {
		io.Client = http.DefaultClient
	}
	if io.Now == nil {
		io.Now = time.Now
	}
	io.Context = ctx
	return io
}

func (p *Provider) environment() map[string]string {
	if p.Env != nil {
		return p.Env
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

func bearerValue(bearer clicore.Bearer) string {
	return strings.TrimPrefix(bearer.Authorization(), "Bearer ")
}

func (p *Provider) remember(secret string) {
	if strings.TrimSpace(secret) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secrets = append(p.secrets, secret)
}

// Redact replaces every bearer this provider presented or read from the
// environment in what it is about to write.
func (p *Provider) Redact(data []byte) []byte {
	p.mu.Lock()
	secrets := append([]string(nil), p.secrets...)
	p.mu.Unlock()
	for _, secret := range secrets {
		data = bytes.ReplaceAll(data, []byte(secret), []byte("[REDACTED]"))
	}
	return data
}

// failureOf maps a failed Source call to its outcome. A refusal below 500
// wrote nothing. A request that never left wrote nothing. Anything else is
// unknown: a write answers unresolved and is never sent again, a read
// answers unavailable and may be retried.
func failureOf(err error, reconcile string) *collab.Failure {
	var refusal *Refusal
	if errors.As(err, &refusal) && refusal.Status > 0 && refusal.Status < 500 {
		return refused(refusal)
	}
	if wasNotSent(err) {
		return collab.Fail(collab.OutcomeUnavailable, reasonTransport, "Putnami Source was not called: %v", err)
	}
	if reconcile != "" {
		failure := collab.Fail(collab.OutcomeUnresolved, reasonUnanswered,
			"Putnami Source did not confirm the write, and it was not sent again: %v", err)
		failure.Error.Reconcile = reconcile
		return failure
	}
	failure := collab.Fail(collab.OutcomeUnavailable, reasonTransport, "Putnami Source did not answer: %v", err)
	failure.Error.Retryable = true
	return failure
}

func refused(refusal *Refusal) *collab.Failure {
	outcome, reason := collab.OutcomeUnavailable, reasonRefused
	retryable := false
	switch refusal.Status {
	case http.StatusNotFound:
		outcome, reason = collab.OutcomeNotFound, reasonNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		outcome, reason = collab.OutcomeConflict, collab.ReasonRevisionConflict
	case http.StatusUnauthorized, http.StatusForbidden:
		outcome, reason = collab.OutcomeDenied, reasonRefused
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		outcome, reason = collab.OutcomeInvalid, reasonInvalid
	case http.StatusTooManyRequests:
		reason, retryable = reasonRateLimit, true
	}
	if refusal.Reason != "" {
		reason = refusal.Reason
	}
	failure := collab.Fail(outcome, reason, "Putnami Source refused the request (%s): %s",
		clicore.StatusLine(refusal.Status), refusal.Error())
	failure.Error.Retryable = retryable
	if outcome == collab.OutcomeConflict {
		failure.Error.Current = refusal.Current
	}
	return failure
}
