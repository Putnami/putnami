package collabcli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"

	agencyapiclient "go.putnami.dev/cloud/clients/agency-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	collab "go.putnami.dev/protocol/collaboration"
)

// workspaceManifestMax bounds the workspace manifest read for its name.
const workspaceManifestMax = 4 << 20

// checkpointRead is how a caller settles a checkpoint whose outcome is
// unknown.
const checkpointRead = "read memory.mission, then repeat the checkpoint with the same idempotency key"

// memoryHandlers serves memory version 1 from agency-api: context, mission and
// checkpoint each make one call to the workspace's memory routes. memory.search
// is not served: Serve answers it unsupported.
func (p *Provider) memoryHandlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMemory, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationContext):    agencyHandler(p, p.memoryContext),
		key(collab.OperationMission):    agencyHandler(p, p.memoryMission),
		key(collab.OperationCheckpoint): agencyHandler(p, p.memoryCheckpoint),
	}
}

func (p *Provider) memoryContext(ctx context.Context, call collab.Call, agency agencyCall, in *collab.MemoryContextInput) (any, *collab.Failure) {
	if other(call, in.Identity) {
		return &collab.MemoryListResult{Items: []collab.MemoryRecord{}}, nil
	}
	var body agencyapiclient.MemoryContextInput
	if err := convert(in, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the memory.context request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MemoryListResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMemoryContext(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMemoryContextInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMemoryContextPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationContext, "", collab.ReasonRevisionConflict)
	}
	result := &collab.MemoryListResult{}
	if answer == nil || convert(answer, result) != nil {
		return nil, invalidAgencyAnswer(collab.OperationContext, "")
	}
	if result.Items == nil {
		result.Items = []collab.MemoryRecord{}
	}
	return result, nil
}

func (p *Provider) memoryMission(ctx context.Context, call collab.Call, agency agencyCall, in *collab.MemoryMissionInput) (any, *collab.Failure) {
	if other(call, in.Identity) {
		return nil, collab.Fail(collab.OutcomeNotFound, "mission.missing", "the workspace holds no record of mission %q", in.Mission)
	}
	request := *in
	request.Identity = p.withRepository(call, in.Identity)
	var body agencyapiclient.MemoryMissionInput
	if err := convert(request, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the memory.mission request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MemoryRecordResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMemoryMission(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMemoryMissionInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMemoryMissionPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationMission, "", collab.ReasonRevisionConflict)
	}
	result := &collab.MemoryRecordResult{}
	if answer == nil || convert(answer, result) != nil {
		return nil, invalidAgencyAnswer(collab.OperationMission, "")
	}
	return result, nil
}

func (p *Provider) memoryCheckpoint(ctx context.Context, call collab.Call, agency agencyCall, in *collab.MemoryCheckpointInput) (any, *collab.Failure) {
	if other(call, in.Identity) {
		return nil, collab.Fail(collab.OutcomeDenied, reasonWorkspaceOther,
			"the checkpoint names workspace %q; this provider serves %q", in.Identity.Workspace, workspaceName(call.Request.WorkspaceRoot))
	}
	request := *in
	request.Identity = p.withRepository(call, in.Identity)
	var body agencyapiclient.MemoryCheckpointInput
	if err := convert(request, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the memory.checkpoint request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MemoryCheckpointResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMemoryCheckpoint(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMemoryCheckpointInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMemoryCheckpointPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationCheckpoint, checkpointRead, collab.ReasonRevisionConflict)
	}
	result := &collab.MemoryCheckpointResult{}
	if answer == nil || convert(answer, result) != nil {
		return nil, invalidAgencyAnswer(collab.OperationCheckpoint, checkpointRead)
	}
	return result, nil
}

// other reports whether an identity names another workspace than the one
// whose manifest is at the call's workspace root.
func other(call collab.Call, identity collab.MemoryIdentity) bool {
	return identity.Workspace != "" && identity.Workspace != workspaceName(call.Request.WorkspaceRoot)
}

// withRepository names the checkout's repository in an identity that names
// none: the GitHub repository of its origin remote.
func (p *Provider) withRepository(call collab.Call, identity collab.MemoryIdentity) collab.MemoryIdentity {
	if identity.Repository != "" {
		return identity
	}
	origin := p.OriginRemote
	if origin == nil {
		origin = gitOriginRemote
	}
	remote, err := origin(call.Request.WorkspaceRoot)
	if err != nil {
		return identity
	}
	if _, repository, err := clicore.RepositoryKeyFromRemote(remote); err == nil {
		identity.Repository = repository
	}
	return identity
}

// gitOriginRemote is the URL of the origin remote of the checkout at dir.
func gitOriginRemote(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// workspaceName is the name of the workspace manifest at root, or "".
func workspaceName(root string) string {
	path := clicore.ManifestPath(root)
	if path == "" {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(file, workspaceManifestMax)).Decode(&manifest); err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.Name)
}
