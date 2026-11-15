package collabcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.putnami.dev/client"
	sourceapiclient "go.putnami.dev/cloud/clients/source-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
)

// connectSource binds Source's generated client to the workspace's control
// plane and bearer.
func connectSource(workspace *clicore.WorkspaceContext) (Source, error) {
	api, err := clicore.NewServiceClient[sourceapiclient.SourceClient](sourceapiclient.RegisterSourceClient, workspace.ServiceBinding())
	if err != nil {
		return nil, err
	}
	return &sourceClient{api: api, workspace: workspace}, nil
}

// sourceClient is the Source surface over the generated source-api client.
// Source speaks the collaboration documents, so each call carries the
// operation's input as the route's body and answers the route's document
// re-read as the collaboration result.
type sourceClient struct {
	api       *sourceapiclient.SourceClient
	workspace *clicore.WorkspaceContext
}

func (s *sourceClient) FindProposals(ctx context.Context, in *collab.ProposalFindInput) (*collab.ProposalListResult, error) {
	if in.Page != nil && in.Page.Cursor != "" {
		return nil, &invalidRequest{reason: "proposals.find takes no page cursor: Putnami Source answers one page"}
	}
	query := map[string]any{"base": in.Change.Base, "head": in.Change.Head}
	if in.Change.Repository != "" {
		query["repository"] = in.Change.Repository
	}
	if in.Page != nil && in.Page.Size > 0 {
		query["size"] = in.Page.Size
	}
	if len(in.States) > 0 {
		states := make([]string, len(in.States))
		for i, state := range in.States {
			states[i] = string(state)
		}
		query["state"] = strings.Join(states, ",")
	}
	return call[sourceapiclient.GetV1WorkspacesSourceProposalsInput, collab.ProposalListResult](ctx, s,
		map[string]any{"path": s.path(nil), "query": query}, s.api.GetV1WorkspacesSourceProposals)
}

func (s *sourceClient) UpsertProposal(ctx context.Context, in *collab.ProposalUpsertInput) (*collab.ProposalUpsertResult, error) {
	return call[sourceapiclient.CreateV1WorkspacesSourceProposalsInput, collab.ProposalUpsertResult](ctx, s,
		map[string]any{"path": s.path(nil), "body": in}, s.api.CreateV1WorkspacesSourceProposals)
}

func (s *sourceClient) ProposalStatus(ctx context.Context, in *collab.ProposalRefInput) (*collab.ProposalStatusResult, error) {
	return call[sourceapiclient.GetV1WorkspacesWorkspaceSourceProposalsNumberInput, collab.ProposalStatusResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"number": in.Ref.ID})}, s.api.GetV1WorkspacesWorkspaceSourceProposalsNumber)
}

func (s *sourceClient) ProposalComments(ctx context.Context, in *collab.ProposalCommentsInput) (*collab.ProposalCommentListResult, error) {
	query := map[string]any{}
	if in.Page != nil && in.Page.Cursor != "" {
		query["cursor"] = in.Page.Cursor
	}
	if in.Page != nil && in.Page.Size > 0 {
		query["size"] = in.Page.Size
	}
	return call[sourceapiclient.GetV1WorkspacesSourceProposalsCommentsInput, collab.ProposalCommentListResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"number": in.Ref.ID}), "query": query}, s.api.GetV1WorkspacesSourceProposalsComments)
}

func (s *sourceClient) ReviewProposal(ctx context.Context, in *collab.ProposalReviewInput) (*collab.ProposalReviewResult, error) {
	return call[sourceapiclient.CreateV1WorkspacesSourceProposalsReviewsInput, collab.ProposalReviewResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"number": in.Ref.ID}), "body": in}, s.api.CreateV1WorkspacesSourceProposalsReviews)
}

func (s *sourceClient) AutoMergeProposal(ctx context.Context, in *collab.ProposalAutoMergeInput) (*collab.ProposalResult, error) {
	return call[sourceapiclient.CreateV1WorkspacesSourceProposalsAutomergeInput, collab.ProposalResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"number": in.Ref.ID}), "body": in}, s.api.CreateV1WorkspacesSourceProposalsAutomerge)
}

func (s *sourceClient) GetTask(ctx context.Context, in *collab.TaskRefInput) (*collab.TaskResult, error) {
	return call[sourceapiclient.GetV1WorkspacesWorkspaceSourceTasksTaskInput, collab.TaskResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"task": in.Ref.ID})}, s.api.GetV1WorkspacesWorkspaceSourceTasksTask)
}

func (s *sourceClient) TransitionTask(ctx context.Context, in *collab.TaskTransitionInput) (*collab.TaskResult, error) {
	return call[sourceapiclient.CreateV1WorkspacesSourceTasksTransitionInput, collab.TaskResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"task": in.Ref.ID}), "body": in}, s.api.CreateV1WorkspacesSourceTasksTransition)
}

func (s *sourceClient) AssignTask(ctx context.Context, in *collab.TaskAssignInput) (*collab.TaskResult, error) {
	return call[sourceapiclient.CreateV1WorkspacesSourceTasksAssignInput, collab.TaskResult](ctx, s,
		map[string]any{"path": s.path(map[string]string{"task": in.Ref.ID}), "body": in}, s.api.CreateV1WorkspacesSourceTasksAssign)
}

// path is the route's path parameters: the linked workspace and extra.
func (s *sourceClient) path(extra map[string]string) map[string]string {
	path := map[string]string{"workspace": s.workspace.WorkspaceID}
	for name, value := range extra {
		path[name] = value
	}
	return path
}

// call sends one generated call. request is the call's input in the
// collaboration document's JSON members, decoded into the generated input I;
// the generated answer is decoded into the collaboration result R. A request
// that cannot be encoded is not sent. A provider refusal is a *Refusal; any
// other failure of the call leaves its outcome unknown.
func call[I, R any, G any](ctx context.Context, s *sourceClient, request map[string]any,
	send func(context.Context, I) (*G, error),
) (*R, error) {
	input, err := recode[I](request)
	if err != nil {
		return nil, NotSent(fmt.Errorf("encode the request for Putnami Source: %w", err))
	}
	answer, err := clicore.CallWithSession(ctx, s.workspace, func(callCtx context.Context) (*G, error) {
		return send(callCtx, *input)
	})
	if err != nil {
		return nil, refusalOf(err)
	}
	if answer == nil {
		return nil, errors.New("no document in the answer of Putnami Source")
	}
	result, err := recode[R](answer)
	if err != nil {
		return nil, fmt.Errorf("read the answer of Putnami Source: %w", err)
	}
	return result, nil
}

// recode re-reads value, through its JSON members, as a T.
func recode[T any](value any) (*T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// refusalOf turns a provider refusal into a *Refusal carrying its status,
// its message and the stable code Source declared for it; an undeclared code
// leaves the reason to the status. Any other error is returned as it is.
func refusalOf(err error) error {
	var remote *client.RemoteError
	if !errors.As(err, &remote) {
		return err
	}
	refusal := &Refusal{Status: remote.StatusCode, Message: remote.Message}
	if remote.RemoteCode != string(client.CodeClientRemote) {
		refusal.Reason = remote.RemoteCode
	}
	return refusal
}

// invalidRequest is a request the provider does not send because Source
// cannot answer it as asked.
type invalidRequest struct{ reason string }

func (e *invalidRequest) Error() string { return e.reason }
