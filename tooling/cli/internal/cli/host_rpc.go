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
	diag "go.putnami.dev/protocol/diagnostic"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/collaboration"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hostrpc"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The host RPC channel of an interactive extension subcommand: the calls the
// launched process makes into this process instead of running putnami again
// (go.putnami.dev/protocol/extension/hostrpc). One channel serves one launch,
// and answers from what that launch's dispatch already resolved.

// hostRPCBinding is what the methods of one launch's channel answer from: the
// workspace the command runs in, its configuration, and the extensions the
// dispatch discovered. A launch outside any workspace has an empty root; its
// extensions are the user scope's, callerDir is the directory it runs in, and
// userScope is the synthetic workspace its runtimes are prepared in.
type hostRPCBinding struct {
	root       string
	config     *wsproto.Config
	extensions []*extension.ExtensionDescription
	callerDir  string
	userScope  *workspace.Workspace
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
		WorkspaceProbe:     binding.workspaceProbe,
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
	case !collaboration.Declared(b.root):
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this workspace declares no options.collaboration in putnami.workspace.json, so no collaboration contract is bound"}
	}
	if owners := collaborationCommandOwners(b.extensions, call.Contract); len(owners) > 0 {
		return nil, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: fmt.Sprintf("%s is a collaboration contract of this workspace and a command of %s", call.Contract, strings.Join(owners, ", "))}
	}
	envelope := collaborationEnvelope(ctx, b.root, b.config, b.extensions, call.Contract, call.Operation, call.Input)
	text, err := collaboration.EnvelopeText(envelope, false)
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

// workspaceProbe answers workspace.probe. Inside a workspace it answers the
// workspace's synchronized view, as every command prepares it. Outside one it
// runs the probes of the user scope's extensions on the caller's directory
// (probeDirectory). A probe failure is an error diagnostic of the
// answer, never a refused call.
func (b hostRPCBinding) workspaceProbe(ctx context.Context, _ hostrpcproto.WorkspaceProbeParams) (hostrpcproto.WorkspaceProbeResult, error) {
	switch {
	case b.root != "":
		return workspaceViewProbe(ctx, b.root, b.extensions)
	case b.callerDir != "" && b.userScope != nil:
		return probeDirectory(ctx, b.userScope, b.callerDir, b.extensions)
	default:
		return hostrpcproto.WorkspaceProbeResult{}, &hostrpcproto.Error{Code: hostrpcproto.CodeUnavailable,
			Message: "this launch names neither a workspace nor a caller directory to probe"}
	}
}

// workspaceViewProbe answers workspace.probe from the synchronized view of the
// workspace at root, whose extensions are the ones the dispatch discovered.
func workspaceViewProbe(ctx context.Context, root string, extensions []*extension.ExtensionDescription) (hostrpcproto.WorkspaceProbeResult, error) {
	ws, err := workspace.Load(root)
	if err != nil {
		return hostrpcproto.WorkspaceProbeResult{}, err
	}
	result := hostrpcproto.WorkspaceProbeResult{Root: root, Projects: []wsproto.MergedProject{}, Diagnostics: []diag.Diagnostic{}}
	outcome, err := engine.PrepareWorkspaceGraph(ctx, ws, &extension.DiscoveryResult{Extensions: extensions})
	if err != nil {
		failure := &wsproto.ProbeFailure{}
		if !errors.As(err, &failure) {
			return hostrpcproto.WorkspaceProbeResult{}, err
		}
		result.Diagnostics = append(append(result.Diagnostics, failure.Diagnostic()), failure.Diagnostics...)
		return result, nil
	}
	result.Projects = sortedMergedProjects(outcome.Merged)
	result.Diagnostics = append(result.Diagnostics, outcome.Diagnostics...)
	return result, nil
}

// probeDirectory answers workspace.probe for dir, a directory in no
// workspace: it runs the workspace probes of extensions there and merges their
// answers as a workspace view merges them, with no explicit project. ws is the
// user scope's synthetic workspace, where the providers' runtimes are
// prepared.
//
// The candidates are dir itself, as a workspace sync always asks about its
// root, and the directories under dir that carry one of the providers'
// markers. Nothing is written in dir: no snapshot and no lock. A provider that
// fails or answers outside its contract is an error diagnostic, and the other
// providers' projects stay in the answer; a merge conflict is an error
// diagnostic on the field it leaves empty. Only a directory that cannot be
// read fails the call.
func probeDirectory(
	ctx context.Context,
	ws *workspace.Workspace,
	dir string,
	extensions []*extension.ExtensionDescription,
) (hostrpcproto.WorkspaceProbeResult, error) {
	bindings := engine.WorkspaceProviderBindings(ctx, ws, dir, &extension.DiscoveryResult{Extensions: extensions}, nil)
	scopes := make([]workspace.ProviderScope, len(bindings))
	for i, binding := range bindings {
		scopes[i] = binding.Scope
	}
	if _, err := os.ReadDir(dir); err != nil {
		return hostrpcproto.WorkspaceProbeResult{}, err
	}
	scanned, err := workspace.ScanProjectPathsWithProviders(dir, scopes)
	if err != nil {
		return hostrpcproto.WorkspaceProbeResult{}, err
	}
	candidates := append([]string{wsproto.ProbeRootPath}, scanned...)
	slices.Sort(candidates)
	result := hostrpcproto.WorkspaceProbeResult{Root: dir, Diagnostics: []diag.Diagnostic{}}
	answers := make([]wsproto.ProbeResult, 0, len(bindings))
	for _, binding := range bindings {
		outcome, err := workspace.RunProbe([]workspace.ProbeProvider{binding.Provider}, wsproto.ProbeRequest{
			Reason:            wsproto.ProbeReasonLoad,
			Paths:             candidates,
			DependencySources: binding.Scope.DependencySources,
		}, nil)
		if err != nil {
			failure := &wsproto.ProbeFailure{}
			if !errors.As(err, &failure) {
				failure = wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, binding.Scope.Extension, "%v", err)
			}
			result.Diagnostics = append(append(result.Diagnostics, failure.Diagnostic()), failure.Diagnostics...)
			continue
		}
		answers = append(answers, outcome.Results...)
		result.Diagnostics = append(result.Diagnostics, outcome.Diagnostics...)
	}
	merged, conflicts := wsproto.MergeProbeResults(answers, nil)
	result.Projects = sortedMergedProjects(merged)
	result.Diagnostics = append(result.Diagnostics, conflicts...)
	return result, nil
}

// sortedMergedProjects lists a merged view's projects sorted by path, as an
// empty slice for an empty view.
func sortedMergedProjects(merged map[string]wsproto.MergedProject) []wsproto.MergedProject {
	projects := make([]wsproto.MergedProject, 0, len(merged))
	for _, project := range merged {
		projects = append(projects, project)
	}
	slices.SortFunc(projects, func(a, b wsproto.MergedProject) int { return strings.Compare(a.Path, b.Path) })
	return projects
}

// versionLines answers version.lines for the launch's workspace. It first
// refreshes the dependency graph a support catalog reads. When the refresh
// fails it answers from the recorded view and names it in RecordedGraph, so a
// caller that releases can refuse; without a usable view it fails. A scope
// that names no line is invalid params, and any other failure, such as a
// shallow clone, is the host's.
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
