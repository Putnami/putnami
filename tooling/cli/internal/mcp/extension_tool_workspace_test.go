package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The two additions made to the extension-tool path: the workspace view a
// tool may ask for, and the prepared runtime a tool may name.
//
// Both are measured against what the SUBPROCESS receives, not against what this
// package intended to send. A test that asserted the Go value would pass for a
// member that never survived encoding, and the whole point of the wire is what
// crosses it.

// The echo fixture is this TEST BINARY re-executed, not a script.
//
// A fixture that shells out depends on whatever the host happens to have. The
// first version of this file ran python3 from a /bin/sh wrapper: it passed on
// every developer machine and failed on the CI runner, which has no python3 on
// PATH. The self-exec helper-process pattern — internal/workspace's
// exec probe and internal/store's lease tests use the same one — depends on
// nothing but the Go toolchain already building this test: no interpreter, no
// shebang, and no executable bit on a temp file.
const echoHelperEnv = "PUTNAMI_MCP_ECHO_TOOL_HELPER"

// TestEchoToolHelperProcess is the fixture tool itself. It is a real test so
// `go test` builds it into this binary, and it does nothing at all unless the
// parent invoked it as the child of a tool call.
//
// It writes the raw ToolCallRequest bytes it read on stdin back out inside a
// result envelope. The request is deliberately NOT decoded and re-encoded here:
// the tests read this text to assert what crossed the wire, and a round trip
// through Go values would launder the very encoding faults they exist to catch.
func TestEchoToolHelperProcess(t *testing.T) {
	if os.Getenv(echoHelperEnv) == "" {
		t.Skip("helper process: not invoked as an extension tool")
	}
	request, err := io.ReadAll(os.Stdin)
	if err != nil {
		echoHelperFail("read the tool call request: " + err.Error())
	}
	encoded, err := json.Marshal(proto.ToolCallResult{
		Content: []proto.ToolContent{{Type: "text", Text: string(request)}},
	})
	if err != nil {
		echoHelperFail("encode the result: " + err.Error())
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		echoHelperFail("write the result: " + err.Error())
	}
	// Exit before the testing package prints its own PASS line: a tool's stdout
	// carries ONE document and the orchestrator rejects anything after it. The
	// child is spawned without -test.paniconexit0, so this is not the early exit
	// the harness fails a test for.
	os.Exit(0)
}

// echoHelperFail reports a child-side failure on stderr, which is the one
// channel a broken fixture has back to the assertions: callExtensionTool folds a
// failed tool's stderr into the result text it returns.
func echoHelperFail(message string) {
	os.Stderr.WriteString("echo tool: " + message + "\n") //nolint:errcheck // child process diagnostics
	os.Exit(2)
}

// echoToolDefinition is a tool whose executable writes the request it was
// handed back out as its result text, so a test reads exactly what the
// extension process would. It returns the definition and the extension root to
// register it under.
func echoToolDefinition(t *testing.T, workspaceSelection bool) (extension.ToolDefinition, string) {
	t.Helper()
	// os.Executable rather than argv[0]: registration stats the command before
	// it advertises the tool, and it resolves a relative one against the
	// fixture workspace rather than against this test's working directory.
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate this test binary: %v", err)
	}
	def := testExtensionToolDefinition(executable)
	def.Args = []string{"-test.run=^TestEchoToolHelperProcess$"}
	def.Env = map[string]string{echoHelperEnv: "1"}
	def.WorkspaceSelection = workspaceSelection
	return def, t.TempDir()
}

// echoCallParams is the tools/call params object, as a declared shape. The
// arguments member stays raw: it is the tool's own vocabulary, written here as
// the JSON a client sends rather than as a Go value the test would have to
// re-encode.
type echoCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// echoCallResult is the tools/call result this file reads back.
type echoCallResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// detail renders everything the result has to say, and every failure message in
// this file carries it.
//
// A tool call that fails reports its cause in the result text: callExtensionTool
// folds the subprocess's own stderr in there (boundedToolStderr), along with a
// timeout, a non-zero exit, or output that was not one JSON document. Reporting
// only "the tool call failed" throws that away — which is what happened when
// one CI cycle produced no usable signal for a failure that never reproduced
// locally.
func (r echoCallResult) detail() string {
	var out strings.Builder
	fmt.Fprintf(&out, "isError=%v", r.IsError)
	if len(r.Content) == 0 {
		out.WriteString(" (no content)")
	}
	for i, item := range r.Content {
		fmt.Fprintf(&out, "\ncontent[%d]: %s", i, item.Text)
	}
	return out.String()
}

// callEcho runs one tool call and returns the decoded result.
func callEcho(t *testing.T, srv *Server, name, arguments string) echoCallResult {
	t.Helper()
	params := echoCallParams{Name: name}
	if arguments != "" {
		params.Arguments = json.RawMessage(arguments)
	}
	resps := runSession(t, srv, req(1, "tools/call", params))
	encoded, err := json.Marshal(resultOf(t, resps[0]))
	if err != nil {
		t.Fatalf("re-encode the result: %v", err)
	}
	var result echoCallResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("decode the result: %v\n%s", err, encoded)
	}
	if len(result.Content) == 0 {
		t.Fatalf("tools/call %s returned no content; the whole result was: %s", name, encoded)
	}
	return result
}

// callEchoTool runs one tool call and returns the ToolCallRequest the
// subprocess read on stdin, plus the failure detail — empty when the call
// succeeded, so no caller can report a failure without its cause.
func callEchoTool(t *testing.T, srv *Server, name, arguments string) (proto.ToolCallRequest, string) {
	t.Helper()
	result := callEcho(t, srv, name, arguments)
	if result.IsError {
		return proto.ToolCallRequest{}, result.detail()
	}
	var request proto.ToolCallRequest
	if err := json.Unmarshal([]byte(result.Content[0].Text), &request); err != nil {
		t.Fatalf("the echoed request is not a ToolCallRequest: %v\n%s", err, result.detail())
	}
	return request, ""
}

// callEchoToolError runs one tool call that must fail and returns its message.
func callEchoToolError(t *testing.T, srv *Server, name, arguments string) string {
	t.Helper()
	result := callEcho(t, srv, name, arguments)
	if !result.IsError {
		t.Fatalf("tools/call %s succeeded; this case exists for its failure: %s", name, result.detail())
	}
	return result.Content[0].Text
}

// TestWorkspaceSelectionPutsTheResolvedViewOnTheRequest is the wire addition,
// end to end.
//
// An extension has no workspace loader and must never grow one, so a tool that
// declares `workspaceSelection` is handed the membership and the projection the
// orchestrator already resolved. This asserts BOTH arrive, complete, and in
// canonical order — a partial membership is the failure mode that produces a
// well-formed wrong answer rather than an error.
func TestWorkspaceSelectionPutsTheResolvedViewOnTheRequest(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "extension-tool-workspace-facts", "a-declaring-tool-receives-the-resolved-view")
	srv := newFixtureServer(t)
	srv.opts.ResolveSelection = fixtureSelectionResolver
	def, dir := echoToolDefinition(t, true)
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/echo", Path: dir,
		Tools: map[string]extension.ToolDefinition{"putnami.echo": def},
	}})

	request, failure := callEchoTool(t, srv, "putnami.echo", "")
	if failure != "" {
		t.Fatalf("the tool call failed: %s", failure)
	}
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

// TestWorkspaceSelectionNarrowsFromTheToolArguments pins that the orchestrator
// RESOLVES rather than forwards: the extension receives ids, never the selector
// the caller typed.
func TestWorkspaceSelectionNarrowsFromTheToolArguments(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "extension-tool-workspace-facts", "a-declaring-tool-receives-the-resolved-view")
	srv := newFixtureServer(t)
	srv.opts.ResolveSelection = fixtureSelectionResolver
	def, dir := echoToolDefinition(t, true)
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/echo", Path: dir,
		Tools: map[string]extension.ToolDefinition{"putnami.echo": def},
	}})

	request, failure := callEchoTool(t, srv, "putnami.echo", `{"projects":["app"]}`)
	if failure != "" {
		t.Fatalf("the narrowed tool call failed: %s", failure)
	}
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
// a selector naming no project is an error, never a quietly smaller answer.
func TestWorkspaceSelectionRefusesAnUnknownSelector(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "extension-tool-workspace-facts", "unknown-selectors-and-missing-resolvers-fail-closed")
	srv := newFixtureServer(t)
	srv.opts.ResolveSelection = fixtureSelectionResolver
	def, dir := echoToolDefinition(t, true)
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/echo", Path: dir,
		Tools: map[string]extension.ToolDefinition{"putnami.echo": def},
	}})

	if got := callEchoToolError(t, srv, "putnami.echo",
		`{"projects":["nope"]}`); !strings.Contains(got, "projects not found: nope") {
		t.Errorf("message = %q, want the core tools' own refusal", got)
	}
	if got := callEchoToolError(t, srv, "putnami.echo",
		`{"projects":["app"],"impacted":true}`); !strings.Contains(got, "pass one") {
		t.Errorf("message = %q, want the two-ways-to-say-selection refusal", got)
	}
}

// TestAToolThatDidNotAskGetsTheRequestItAlwaysGot is the additive half of the
// contract: an existing extension's tool must see no new members, so the
// addition cannot change any behavior nobody opted into.
func TestAToolThatDidNotAskGetsTheRequestItAlwaysGot(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "extension-tool-workspace-facts", "a-non-declaring-tool-receives-neither")
	srv := newFixtureServer(t)
	srv.opts.ResolveSelection = fixtureSelectionResolver
	def, dir := echoToolDefinition(t, false)
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/echo", Path: dir,
		Tools: map[string]extension.ToolDefinition{"putnami.echo": def},
	}})

	request, failure := callEchoTool(t, srv, "putnami.echo", `{"projects":["nope"]}`)
	if failure != "" {
		t.Fatalf("a tool that declared no workspaceSelection had its arguments rejected"+
			" as project selectors: %s", failure)
	}
	if request.WorkspaceProjects != nil || request.Selection != nil {
		t.Errorf("an undeclaring tool received workspaceProjects=%v selection=%v; both members are opt-in",
			request.WorkspaceProjects, request.Selection)
	}
}

// TestWorkspaceSelectionWithoutAResolverFailsClosed states the answer to a nil
// injection: refuse, rather than hand the tool an absent selection it would read
// as "the whole workspace".
func TestWorkspaceSelectionWithoutAResolverFailsClosed(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "extension-tool-workspace-facts", "unknown-selectors-and-missing-resolvers-fail-closed")
	srv := newFixtureServer(t)
	def, dir := echoToolDefinition(t, true)
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/echo", Path: dir,
		Tools: map[string]extension.ToolDefinition{"putnami.echo": def},
	}})

	if got := callEchoToolError(t, srv, "putnami.echo", ""); !strings.Contains(got, "selection resolver") {
		t.Errorf("message = %q, want a refusal naming the missing resolver", got)
	}
}

// fixtureSelectionResolver is the resolver the CLI shell injects, reduced to
// what a fixture needs: the whole workspace, or the ids the adapter already
// resolved. The production closure runs shared.ResolveProjectSelection; this
// package cannot import it (no edge into the command subtree), which is why the
// production wiring is proven by the parity test in internal/cli instead.
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

// TestAToolMayNameThePreparedRuntime pins the resolution {extensionRuntime}
// gets on this path.
//
// A manifest-declared tool names the extension's prepared runtime the same way
// every task does, and BuildTemplateVars deliberately does not carry that token
// — the orchestrator supplies it only where it owns one. Without this, the four
// SDD tools would expand to the literal string "{extensionRuntime}", fail the
// registration probe, and vanish from tools/list with no message anywhere.
func TestAToolMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	srv.opts.ResolveSelection = fixtureSelectionResolver
	def, dir := echoToolDefinition(t, true)
	executable := def.Command
	def.Command = "{" + proto.TemplateVarExtensionRuntime + "}"
	description := &extension.ExtensionDescription{
		Name: "@putnami/echo", Path: dir,
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/echo"},
		// Already synchronized, which is what a second call in a session sees.
		// Preparing one here would compile a Go module inside a unit test.
		RuntimeExecutable: executable,
		Tools:             map[string]extension.ToolDefinition{"putnami.echo": def},
	}
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{description})

	request, failure := callEchoTool(t, srv, "putnami.echo", "")
	if failure != "" {
		t.Fatalf("a tool naming {extensionRuntime} did not run: %s", failure)
	}
	if request.Name != "putnami.echo" {
		t.Errorf("request name = %q", request.Name)
	}
}

// TestAToolEnvMayNameThePreparedRuntime extends the resolution to env values:
// a tool that names the prepared runtime only in its env gets the executable,
// not the literal "{extensionRuntime}".
func TestAToolEnvMayNameThePreparedRuntime(t *testing.T) {
	t.Parallel()
	token := "{" + proto.TemplateVarExtensionRuntime + "}"
	def := testExtensionToolDefinition("tool")
	def.Env = map[string]string{"TOOL_RUNTIME": token}
	executable := filepath.Join(t.TempDir(), "compiled", "tool")
	candidate := extensionToolCandidate{name: "putnami.tool", def: def, ext: &extension.ExtensionDescription{
		Name: "@putnami/tool", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/tool"},
		// Already synchronized, which is what a second call in a session sees.
		RuntimeExecutable: executable,
	}}

	vars, err := extensionToolTemplateVars(context.Background(), t.TempDir(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := extension.ExpandTemplateVars(token, vars); got != executable {
		t.Fatalf("an env value naming {extensionRuntime} expands to %q, want the prepared runtime %q", got, executable)
	}
}

// TestAToolNamingAnUndeclaredRuntimeIsNotAdvertised is the registration half:
// {extensionRuntime} is answered from the DECLARATION because there is nothing
// to stat before the first call — but an extension that declares no runtime has
// no answer at all, and its tool must not reach tools/list.
func TestAToolNamingAnUndeclaredRuntimeIsNotAdvertised(t *testing.T) {
	t.Parallel()
	def := testExtensionToolDefinition("{" + proto.TemplateVarExtensionRuntime + "}")
	withRuntime := &extension.ExtensionDescription{
		Name: "@putnami/with", Path: t.TempDir(),
		Runtime: &proto.RuntimeDefinition{Executable: "compiled/tool"},
	}
	without := &extension.ExtensionDescription{Name: "@putnami/without", Path: t.TempDir()}

	if !extensionToolExecutablePresent(withRuntime.Path, withRuntime, def) {
		t.Error("a tool naming the prepared runtime of an extension that declares one must be advertised")
	}
	if extensionToolExecutablePresent(without.Path, without, def) {
		t.Error("a tool naming a runtime its extension never declares must not be advertised")
	}
}
