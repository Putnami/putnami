package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hostrpc"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mcp"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The host RPC channel of an interactive extension subcommand: the calls the
// launched process makes into this process instead of running putnami again
// (go.putnami.dev/protocol/extension/hostrpc). One channel serves one launch,
// and answers from what that launch's dispatch already resolved.

// hostRPCBinding is what the methods of one launch's channel answer from: the
// workspace the command runs in, its configuration, and the extensions the
// dispatch discovered. A launch outside any workspace has an empty root.
type hostRPCBinding struct {
	root       string
	config     *wsproto.Config
	extensions []*extension.ExtensionDescription
}

// startHostRPC opens the channel of one launch. It returns the variables the
// launched process receives and the function that closes the channel, which
// the caller runs once the process exits. When the channel cannot open, the
// process receives no variable and runs anyway; a warning says why, except in
// a hosted run, which never opens one.
func startHostRPC(ctx context.Context, binding hostRPCBinding, stderr io.Writer) ([]string, func()) {
	server, err := hostrpc.Start(ctx, hostrpc.Methods{
		Collaboration:      binding.collaborationCall,
		VerificationReport: binding.verificationReport,
		VersionLines:       binding.versionLines,
		WorkspaceOwners:    binding.workspaceOwners,
	})
	return hostRPCLaunch(server, err, stderr)
}

// hostRPCLaunch is what a launch receives from the channel hostrpc.Start
// opened, or from its refusal err.
func hostRPCLaunch(server *hostrpc.Server, err error, stderr io.Writer) ([]string, func()) {
	if err != nil {
		if !errors.Is(err, hostrpc.ErrHosted) {
			iox.Fprintf(stderr, "putnami: warning: the host RPC channel did not open, so this command cannot call putnami: %v\n", err)
		}
		return nil, func() {}
	}
	return server.Env(), func() {
		if err := server.Stop(); err != nil {
			iox.Fprintf(stderr, "putnami: warning: close the host RPC channel: %v\n", err)
		}
	}
}

// dropInheritedHostRPC removes the channel variables this process inherited
// from the extension command that started it.
func dropInheritedHostRPC() {
	_ = os.Unsetenv(hostrpcproto.SocketEnv)
	_ = os.Unsetenv(hostrpcproto.TokenEnv)
}

// collaborationCall answers collaboration.call through the route
// `putnami <contract> <operation>` takes (collaborationEnvelope), and returns
// the envelope that command prints under --output=json. Where that command
// prints no envelope, the call is refused with the reason. The CLI routes a
// word to that command only when it is a contract and the workspace declares
// options.collaboration (isCollaborationCommand), so the call reaches the
// route under the same two conditions.
func (b hostRPCBinding) collaborationCall(ctx context.Context, call hostrpcproto.CollaborationCallParams) (json.RawMessage, error) {
	switch {
	case !collab.IsContract(call.Contract):
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeInvalidParams,
			Message: fmt.Sprintf("%q is not a collaboration contract; the contracts are %s", call.Contract, strings.Join(collab.ContractNames, ", "))}
	case b.root == "":
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so no collaboration contract is bound"}
	case !mcp.CollaborationDeclared(b.root):
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this workspace declares no options.collaboration in putnami.workspace.json, so no collaboration contract is bound"}
	}
	if owners := collaborationCommandOwners(b.extensions, call.Contract); len(owners) > 0 {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: fmt.Sprintf("%s is a collaboration contract of this workspace and a command of %s", call.Contract, strings.Join(owners, ", "))}
	}
	envelope := collaborationEnvelope(ctx, b.root, b.config, b.extensions, call.Contract, call.Operation, call.Input)
	text, err := mcp.EnvelopeText(envelope, false)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(text), nil
}

// verificationReport answers verification.report with what
// engine.VerifyCachedVerificationReport returns for the launch's workspace.
func (b hostRPCBinding) verificationReport(_ context.Context, ref hostrpcproto.VerificationReportParams) ([]byte, error) {
	if b.root == "" {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so there is no cache to read a verification report from"}
	}
	report, err := engine.VerifyCachedVerificationReport(b.root, features.VerificationReportRef{
		Project: ref.Project,
		Task:    ref.Task,
		Path:    ref.Path,
		Digest:  ref.Digest,
	})
	if err != nil {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeNotVerified, Message: err.Error()}
	}
	return report, nil
}

// workspaceOwners answers workspace.owners with what workspace.ProjectOwnersForPath
// returns for each path in the launch's workspace: the nearest project whose
// directory holds the path, plus every project that claims it as an asset.
// It loads the workspace once for the call. The server has already refused an
// empty list and a path that is not a workspace-relative io/fs path.
func (b hostRPCBinding) workspaceOwners(_ context.Context, params hostrpcproto.WorkspaceOwnersParams) (hostrpcproto.WorkspaceOwnersResult, error) {
	if b.root == "" {
		return hostrpcproto.WorkspaceOwnersResult{}, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so no project owns a path"}
	}
	ws, err := workspace.Load(b.root)
	if err != nil {
		return hostrpcproto.WorkspaceOwnersResult{}, err
	}
	result := hostrpcproto.WorkspaceOwnersResult{Owners: make([]hostrpcproto.PathOwners, 0, len(params.Paths))}
	for _, path := range params.Paths {
		owners := workspace.ProjectOwnersForPath(ws, path)
		projects := make([]hostrpcproto.OwningProject, 0, len(owners))
		for _, project := range owners {
			projects = append(projects, hostrpcproto.OwningProject{ID: project.ID, Path: project.Path})
		}
		slices.SortFunc(projects, func(a, b hostrpcproto.OwningProject) int { return strings.Compare(a.ID, b.ID) })
		result.Owners = append(result.Owners, hostrpcproto.PathOwners{Path: path, Projects: projects})
	}
	return result, nil
}

// versionLines answers version.lines with what `putnami version get` and
// `putnami version tag` compute for the launch's workspace, through the same
// functions. Like them, it first refreshes the dependency graph a support
// catalog reads. When the refresh fails it answers from the recorded view and
// names it in RecordedGraph, so a caller that releases can refuse as
// `version tag` does; without a usable view it fails, as both commands do. A
// scope that names no line is invalid params, and any other failure, such as
// a shallow clone, is the host's.
func (b hostRPCBinding) versionLines(ctx context.Context, params hostrpcproto.VersionLinesParams) (hostrpcproto.VersionLinesResult, error) {
	if b.root == "" {
		return hostrpcproto.VersionLinesResult{}, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this command runs outside any workspace, so there is no version line"}
	}
	var result hostrpcproto.VersionLinesResult
	if refresh := refreshVersionGraph(ctx, b.root, b.config); refresh.err != nil {
		if !refresh.recorded.Usable() {
			return hostrpcproto.VersionLinesResult{}, refresh.refusal()
		}
		result.RecordedGraph = &hostrpcproto.RecordedGraph{ObservedAt: refresh.recorded.ObservedAt, RefreshError: refresh.err.Error()}
	}
	specs, err := versioncmd.LineSpecs(b.root)
	if err != nil {
		return hostrpcproto.VersionLinesResult{}, err
	}
	selected, err := versioncmd.SelectLines(specs, params.Scope)
	if err != nil {
		return hostrpcproto.VersionLinesResult{}, &hostrpcproto.Error{Code: hostrpcproto.CodeInvalidParams, Message: err.Error()}
	}
	if result.SourceRevision, err = versioncmd.BoundRevision(b.root); err != nil {
		return hostrpcproto.VersionLinesResult{}, err
	}
	result.Lines = make([]hostrpcproto.VersionLine, 0, len(selected))
	for _, spec := range selected {
		report, err := versioncmd.ReportLine(b.root, spec)
		if err != nil {
			return hostrpcproto.VersionLinesResult{}, err
		}
		line := hostrpcproto.VersionLine{
			Line:       spec.ScopePath,
			TagPattern: spec.TagPattern,
			Version:    report.Info.Full,
			Tagged:     report.Info.Tagged,
			Tag:        report.Info.Tag,
			Commits:    make([]hostrpcproto.LineCommit, 0, len(report.Commits)),
		}
		if report.Release != nil {
			line.Release = &hostrpcproto.LineRelease{Version: report.Release.Version, Tag: report.Release.Tag}
		}
		for _, commit := range report.Commits {
			line.Commits = append(line.Commits, hostrpcproto.LineCommit{SHA: commit.SHA, Subject: commit.Subject, Body: commit.Body})
		}
		result.Lines = append(result.Lines, line)
	}
	return result, nil
}
