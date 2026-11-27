package collabcli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"go.putnami.dev/client"
	agencyapiclient "go.putnami.dev/cloud/clients/agency-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
)

// Agency-owned reasons.
const (
	reasonAgencyRejected   = "auth.rejected"
	reasonAgencyRateLimit  = "rate.limited"
	reasonAgencyUnanswered = "agency.outcome_unknown"
	reasonAgencyTransport  = "agency.unreachable"
	reasonWorkspaceOther   = "workspace.other"
)

// agencyOwnerOnly is the reason agency-api refuses a write to another
// person's mission with.
const agencyOwnerOnly = "owner_only"

// Agency is the part of agency-api's generated client the memory and
// missions providers call: one method per served operation, each one
// request to a route under /v1/workspaces/{workspace}/agency/.
type Agency interface {
	CreateV1WorkspacesAgencyMemoryContext(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMemoryContextInput) (*agencyapiclient.MemoryListResult, error)
	CreateV1WorkspacesAgencyMemoryMission(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMemoryMissionInput) (*agencyapiclient.MemoryRecordResult, error)
	CreateV1WorkspacesAgencyMemoryCheckpoint(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMemoryCheckpointInput) (*agencyapiclient.MemoryCheckpointResult, error)
	CreateV1WorkspacesAgencyAgentSessionsSync(context.Context, agencyapiclient.CreateV1WorkspacesAgencyAgentSessionsSyncInput) (*agencyapiclient.SessionSyncResult, error)
	CreateV1WorkspacesAgencyDecisions(context.Context, agencyapiclient.CreateV1WorkspacesAgencyDecisionsInput) (*agencyapiclient.DecisionResult, error)
	CreateV1WorkspacesAgencyMessagesReply(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMessagesReplyInput) (*agencyapiclient.MessageResult, error)
	CreateV1WorkspacesAgencyMissionsStatus(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMissionsStatusInput) (*agencyapiclient.MissionsStatusResult, error)
	CreateV1WorkspacesAgencyMissionsStart(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMissionsStartInput) (*agencyapiclient.MissionsRunResult, error)
	CreateV1WorkspacesAgencyMissionsReport(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMissionsReportInput) (*agencyapiclient.MissionsRunResult, error)
	CreateV1WorkspacesAgencyMissionsRuns(context.Context, agencyapiclient.CreateV1WorkspacesAgencyMissionsRunsInput) (*agencyapiclient.MissionsRunsResult, error)
}

// connectAgency binds agency-api's generated client to the workspace's
// control plane, which routes /v1/workspaces/{workspace}/agency/ to it.
func connectAgency(workspace *clicore.WorkspaceContext) (Agency, error) {
	return clicore.NewServiceClient[agencyapiclient.AgencyClient](agencyapiclient.RegisterAgencyClient, workspace.ServiceBinding())
}

// agencyCall is one operation's call to agency-api: the linked workspace,
// the connected client and the call's context.
type agencyCall struct {
	workspace *clicore.WorkspaceContext
	api       Agency
}

// agencyHandler is the handler of one operation agency-api serves: it checks
// the input and the binding settings, bounds the call, resolves the linked
// workspace and its session, connects agency-api, then runs serve. Nothing is
// sent before serve.
func agencyHandler[I any](p *Provider, serve func(ctx context.Context, call collab.Call, agency agencyCall, input *I) (any, *collab.Failure)) collab.Handler {
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
		workspace, failure := p.linked(ctx, call.Request.WorkspaceRoot, "Putnami Agency")
		if failure != nil {
			return nil, failure
		}
		connect := p.ConnectAgency
		if connect == nil {
			connect = connectAgency
		}
		api, err := connect(workspace)
		if err != nil {
			return nil, collab.Fail(collab.OutcomeUnavailable, reasonAgencyTransport, "connect Putnami Agency: %v", err)
		}
		return serve(ctx, call, agencyCall{workspace: workspace, api: api}, input)
	}
}

// send runs one generated call under the workspace's session, re-minted once
// when agency-api answers 401.
func send[T any](ctx context.Context, agency agencyCall, call func(context.Context) (*T, error)) (*T, error) {
	return clicore.CallWithSession(ctx, agency.workspace, call)
}

// agencyRefusal is an answer agency-api sent: its status, and the reason and
// current revision it named.
type agencyRefusal struct {
	status  int
	reason  string
	current string
	// field and problem are the member and the rule an invalid request broke.
	field, problem string
}

// agencyRefusalOf reads the answer a call failed with; ok is false when no
// answer came back, as after a transport failure or a timeout.
func agencyRefusalOf(err error) (agencyRefusal, bool) {
	var remote *client.RemoteError
	if !errors.As(err, &remote) {
		return agencyRefusal{}, false
	}
	// The reason is the one agency-api's refusal details name: the stable
	// code of the envelope (conflict, forbidden) only repeats the status.
	found := agencyRefusal{status: remote.StatusCode}
	var details agencyapiclient.Refusal
	if len(remote.Payload) > 0 && json.Unmarshal(remote.Payload, &details) == nil {
		if details.Reason != nil && *details.Reason != "" {
			found.reason = *details.Reason
		}
		if details.Current != nil {
			found.current = *details.Current
		}
		found.field, found.problem = value(details.Field), value(details.Problem)
	}
	return found, true
}

// agencyFailure maps a failed call to agency-api. A refusal below 500 wrote
// nothing. Anything else leaves the outcome unknown: a write (reconcile set)
// answers unresolved and is not sent again, a read answers a retryable
// unavailable. conflict names what a 409 means for this operation.
func agencyFailure(err error, operation, reconcile, conflict string) *collab.Failure {
	answer, ok := agencyRefusalOf(err)
	if !ok || answer.status >= 500 || answer.status < 400 {
		if reconcile != "" {
			failure := collab.Fail(collab.OutcomeUnresolved, reasonAgencyUnanswered,
				"Putnami Agency did not confirm the %s; it may have been recorded: %v", operation, err)
			failure.Error.Reconcile = reconcile
			return failure
		}
		failure := collab.Fail(collab.OutcomeUnavailable, collab.ReasonProviderUnavailable, "Putnami Agency is unavailable; try again shortly: %v", err)
		failure.Error.Retryable = true
		return failure
	}
	reason := func(fallback string) string {
		if answer.reason != "" {
			return answer.reason
		}
		return fallback
	}
	switch answer.status {
	case http.StatusUnauthorized:
		return collab.Fail(collab.OutcomeDenied, reasonAgencyRejected, "%s", agencyRejected())
	case http.StatusForbidden:
		if operation == collab.OperationReply && answer.reason == agencyOwnerOnly {
			return collab.Fail(collab.OutcomeDenied, collab.ReasonMessageDenied, "the message is on another person's mission")
		}
		return collab.Fail(collab.OutcomeDenied, reason("denied"), "%s", agencyRefused(answer.reason))
	case http.StatusNotFound:
		if operation == collab.OperationReply {
			return collab.Fail(collab.OutcomeNotFound, collab.ReasonMessageNotFound, "the workspace holds no such message")
		}
		return collab.Fail(collab.OutcomeNotFound, reason("mission.missing"), "the workspace holds no such record")
	case http.StatusConflict:
		if operation == collab.OperationReply {
			return collab.Fail(collab.OutcomeConflict, collab.ReasonMessageClosed, "the message already holds another reply")
		}
		failure := collab.Fail(collab.OutcomeConflict, reason(conflict), "Putnami Agency refused the %s as a conflict", operation)
		failure.Error.Current = answer.current
		return failure
	case http.StatusTooManyRequests:
		failure := collab.Fail(collab.OutcomeUnavailable, reasonAgencyRateLimit, "Putnami Agency is rate limiting this caller; try again shortly")
		failure.Error.Retryable = true
		return failure
	}
	// The member a refusal names reaches the caller, so a sync refused for a
	// declared member can go once without it (collaboration ADR 0013).
	if answer.field != "" {
		return collab.Fail(collab.OutcomeInvalid, reason(collab.ReasonRequestInvalid), "Putnami Agency refused the %s as invalid: %s: %s",
			operation, answer.field, answer.problem)
	}
	return collab.Fail(collab.OutcomeInvalid, reason(collab.ReasonRequestInvalid), "Putnami Agency refused the %s as invalid", operation)
}

// agencyRejected says why agency-api answered 401: the session is no longer
// valid. A delegated session needs a new approval through the connection
// that approved it.
func agencyRejected() string {
	if delegation, ok := clicore.SentDelegation(); ok {
		return "Putnami Agency rejected the session an MCP connection approved (actor " + delegation.Actor +
			"); it expired or was revoked: approve a new `putnami cloud login` through the MCP connection"
	}
	return "Putnami Agency rejected the credential; run `putnami cloud login`"
}

// agencyRefused says why agency-api answered 403. For a delegated session it
// says that a new login does not change it.
func agencyRefused(reason string) string {
	if reason != "" && reason != "denied" {
		return "Putnami Agency refused the request (" + reason + ")"
	}
	if delegation, ok := clicore.SentDelegation(); ok {
		return "Putnami Agency refused the session an MCP connection approved (actor " + delegation.Actor +
			") on this route: the route does not take it, or the person may not act on this workspace; a new login does not change that"
	}
	return "Putnami Agency refused the request"
}

// invalidAgencyAnswer is an answer the provider cannot read. After a write,
// the write may have happened.
func invalidAgencyAnswer(operation, reconcile string) *collab.Failure {
	if reconcile != "" {
		failure := collab.Fail(collab.OutcomeUnresolved, reasonAgencyUnanswered,
			"Putnami Agency returned an answer the provider cannot read after the %s; it may have been recorded", operation)
		failure.Error.Reconcile = reconcile
		return failure
	}
	failure := collab.Fail(collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse, "Putnami Agency returned an invalid answer")
	failure.Error.Retryable = true
	return failure
}

// convert copies a document between the contract's types and the generated
// client's, which share one JSON form.
func convert(from, to any) error {
	encoded, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, to)
}

func optional(text string) *string {
	if text == "" {
		return nil
	}
	return &text
}

func value[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func list[T any](items *[]T) []T {
	if items == nil {
		return nil
	}
	return *items
}
