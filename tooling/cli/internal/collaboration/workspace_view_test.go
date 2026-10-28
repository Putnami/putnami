package collaboration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The workspace view a provider tool that declares workspaceSelection
// receives.
//
// Every assertion reads what the provider PROCESS read on stdin (the
// fixture's recorded request), not what this package intended to send. A test
// that asserted the Go value would pass for a member that never survived
// encoding, and the whole point of the wire is what crosses it.

// fixtureSelectionResolver is the resolver the CLI shell injects, reduced to
// what a fixture needs: the whole workspace, or the ids AttachWorkspaceView
// already resolved. The production closure runs shared.ResolveProjectSelection;
// this package cannot import it (no edge into the command subtree), which is
// why the production wiring is proven by the collaboration command tests in
// internal/cli instead.
func fixtureSelectionResolver(
	ws *workspace.Workspace, selection ProjectSelection,
) (*proto.ToolSelection, error) {
	if selection.Projects == "" && !selection.Impacted {
		ids := make([]string, 0, len(ws.Projects))
		for _, project := range ws.Projects {
			ids = append(ids, project.ID)
		}
		return &proto.ToolSelection{Mode: proto.ToolSelectionModeAll, ProjectIDs: ids}, nil
	}
	return &proto.ToolSelection{
		Mode:       proto.ToolSelectionModeProjects,
		Scoped:     true,
		ProjectIDs: strings.Split(selection.Projects, ","),
	}, nil
}

// fixtureWorkspaceView attaches the view the way the command line does: it
// loads the workspace and resolves through AttachWorkspaceView.
func fixtureWorkspaceView(root string) func(context.Context, *proto.ToolCallRequest, json.RawMessage) error {
	return func(_ context.Context, request *proto.ToolCallRequest, args json.RawMessage) error {
		ws, err := workspace.Load(root)
		if err != nil {
			return fmt.Errorf("load workspace: %w", err)
		}
		return AttachWorkspaceView(ws, request, args, fixtureSelectionResolver)
	}
}

// TestWorkspaceSelectionPutsTheResolvedViewOnTheRequest: memory.context
// declares workspaceSelection, so its provider receives the complete
// membership and the projection, in canonical order. A partial membership is
// the failure mode that produces a well-formed wrong answer rather than an
// error.
func TestWorkspaceSelectionPutsTheResolvedViewOnTheRequest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-discovery", "extension-tool-workspace-facts", "a-declaring-tool-receives-the-resolved-view")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	expectOutcome(t, fixture.call(t, collab.ContractMemory, collab.OperationContext, `{}`), collab.OutcomeOK, "")

	request := fixture.recorded(t)
	if len(request.WorkspaceProjects) != 2 {
		t.Fatalf("workspaceProjects = %v, want both fixture projects", request.WorkspaceProjects)
	}
	if request.WorkspaceProjects[0].ID != "/packages/app" || request.WorkspaceProjects[1].ID != "/packages/lib" {
		t.Errorf("membership is not in canonical project-id order: %v", request.WorkspaceProjects)
	}
	// The resolved graph travels with the membership: an entry with no edge list
	// declares none, which is only readable when the producer resolved the graph.
	if got := request.WorkspaceProjects[0].Dependencies; len(got) != 1 || got[0] != "/packages/lib" {
		t.Errorf("app dependencies = %v, want the resolved id of lib", got)
	}
	if request.WorkspaceProjects[0].Type != "application" {
		t.Errorf("app type = %q, want the resolved classification", request.WorkspaceProjects[0].Type)
	}
	// The authored putnami.json travels raw, because the members a tool reads out
	// of it are facts about somebody ELSE's project.
	if len(request.WorkspaceProjects[0].Config) == 0 {
		t.Errorf("no authored config traveled for %s; a tool cannot read a sibling's bin or featureAuthority",
			request.WorkspaceProjects[0].ID)
	}
	if request.Selection == nil {
		// Say how much of the view DID arrive: a request carrying the membership
		// and no selection is a different bug from one carrying neither.
		t.Fatalf("no selection traveled beside %d workspace projects", len(request.WorkspaceProjects))
	}
	if request.Selection.Mode != proto.ToolSelectionModeAll || request.Selection.Scoped {
		t.Errorf("selection = %+v, want the unscoped whole-workspace projection", request.Selection)
	}
	if len(request.Selection.ProjectIDs) != 2 {
		t.Errorf("selection projects = %v, want both", request.Selection.ProjectIDs)
	}
}

// TestWorkspaceSelectionNarrowsFromTheRequest pins that the orchestrator
// RESOLVES rather than forwards: the provider receives ids, never the selector
// the caller typed.
func TestWorkspaceSelectionNarrowsFromTheRequest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-discovery", "extension-tool-workspace-facts", "a-declaring-tool-receives-the-resolved-view")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	expectOutcome(t, fixture.call(t, collab.ContractMemory, collab.OperationContext, `{"projects":["app"]}`), collab.OutcomeOK, "")

	request := fixture.recorded(t)
	if request.Selection == nil || !request.Selection.Scoped {
		t.Fatalf("selection = %+v, want a scoped narrowing", request.Selection)
	}
	if len(request.Selection.ProjectIDs) != 1 || request.Selection.ProjectIDs[0] != "/packages/app" {
		t.Errorf("selection projects = %v, want the resolved id of app", request.Selection.ProjectIDs)
	}
	// The MEMBERSHIP is deliberately NOT narrowed. A tool that reports on one
	// project still resolves identities against the whole workspace, which is the
	// same two-tier split the job wire carries.
	if len(request.WorkspaceProjects) != 2 {
		t.Errorf("workspaceProjects = %d, want the complete membership beside a narrowed selection",
			len(request.WorkspaceProjects))
	}
}

// TestWorkspaceSelectionRefusesAnUnknownSelector keeps the resolution honest:
// a selector naming no project is an error, never a quietly smaller answer,
// and an entry path that cannot resolve a selection refuses instead of handing
// the provider an absent selection it would read as "the whole workspace".
// Nothing reaches the provider in either case.
func TestWorkspaceSelectionRefusesAnUnknownSelector(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-discovery", "extension-tool-workspace-facts", "unknown-selectors-and-missing-resolvers-fail-closed")
	fixture := newCollabFixture(t, "ok", bindAll, nil)

	unknown := fixture.call(t, collab.ContractMemory, collab.OperationContext, `{"projects":["nope"]}`)
	expectOutcome(t, unknown, collab.OutcomeInvalid, collab.ReasonRequestInvalid)
	if !strings.Contains(unknown.Error.Message, "projects not found: nope") {
		t.Errorf("message = %q, want the orchestrator's own refusal", unknown.Error.Message)
	}

	withoutView := fixture.env(t)
	withoutView.AttachView = nil
	refused := CallProviderOperation(context.Background(), withoutView, collab.ContractMemory, collab.OperationContext, json.RawMessage(`{}`))
	if refused.Outcome == collab.OutcomeOK || refused.Error == nil ||
		!strings.Contains(refused.Error.Message, "cannot resolve the workspace selection") {
		t.Errorf("an entry path without a view answered %s %+v, want a refusal", refused.Outcome, refused.Error)
	}
	if _, err := os.Stat(fixture.record); !os.IsNotExist(err) {
		t.Error("a selection nobody could resolve reached the provider")
	}
}

// TestAttachWorkspaceViewFailsClosed drives the two guards AttachWorkspaceView
// owns, which the contract's own request validation may answer first on the
// routed path.
func TestAttachWorkspaceViewFailsClosed(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-discovery", "extension-tool-workspace-facts", "unknown-selectors-and-missing-resolvers-fail-closed")
	root := fixtureWorkspace(t)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, args, want string
		resolve          func(*workspace.Workspace, ProjectSelection) (*proto.ToolSelection, error)
	}{
		{"an unknown selector", `{"projects":["nope"]}`, "projects not found: nope", fixtureSelectionResolver},
		{"two ways to select", `{"projects":["app"],"impacted":true}`, "pass one", fixtureSelectionResolver},
		{"no resolver", ``, "selection resolver", nil},
	} {
		request := proto.ToolCallRequest{Name: "fixture.memory.context"}
		err := AttachWorkspaceView(ws, &request, json.RawMessage(tc.args), tc.resolve)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if request.Selection != nil || request.WorkspaceProjects != nil {
			t.Errorf("%s: a refused view still attached selection=%v projects=%v", tc.name, request.Selection, request.WorkspaceProjects)
		}
	}
}

// TestAToolThatDidNotAskGetsTheRequestItAlwaysGot is the additive half of the
// contract: a provider tool that declares no workspaceSelection sees no view
// members, and the entry path resolves none for it.
func TestAToolThatDidNotAskGetsTheRequestItAlwaysGot(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-discovery", "extension-tool-workspace-facts", "a-non-declaring-tool-receives-neither")
	fixture := newCollabFixture(t, "ok", bindAll, nil)
	if fixture.tools["fixture.tasks.find"].WorkspaceSelection {
		t.Fatal("the fixture's tasks.find declares workspaceSelection; this case needs a tool that does not")
	}
	env := fixture.env(t)
	env.AttachView = func(context.Context, *proto.ToolCallRequest, json.RawMessage) error {
		t.Error("the entry path resolved a view for a tool that declared no workspaceSelection")
		return nil
	}
	expectOutcome(t, CallProviderOperation(context.Background(), env, collab.ContractTasks, collab.OperationFind, json.RawMessage(`{}`)),
		collab.OutcomeOK, "")

	request := fixture.recorded(t)
	if request.WorkspaceProjects != nil || request.Selection != nil {
		t.Errorf("an undeclaring tool received workspaceProjects=%v selection=%v; both members are opt-in",
			request.WorkspaceProjects, request.Selection)
	}
}
