package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	proto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestMain isolates the machine-global putnami stores for the whole binary:
// engine runs resolve ~/.putnami/store (and trigger opportunistic GC there)
// unless PUTNAMI_STORE_DIR overrides it. Setting the override once, before any
// test runs, keeps every test — parallel ones included — out of the real home
// directory without per-test t.Setenv (which forbids t.Parallel).
func TestMain(m *testing.M) {
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-mcp-test-")
	if err == nil {
		os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	_ = dir.Remove()
	fixtureproc.Remove()
	os.Exit(code)
}

// trueCommand places a program that exits 0 and returns its path: a tool
// command registration finds on every platform.
func trueCommand(t *testing.T) string {
	t.Helper()
	return fixtureproc.Write(t, filepath.Join(t.TempDir(), "true"), fixtureproc.Program{})
}

// toolResult is the output of a tool that answers text.
func toolResult(text string) string {
	return `{"content":[{"type":"text","text":"` + text + `"}]}`
}

// identityToolEnv makes this test binary the identity tool
// (TestIdentityToolHelperProcess), the pattern extension_tool_workspace_test.go
// explains. Its value is the file the tool writes its request to; the file
// beside it with an ".env" suffix gets the agent identity environment.
const identityToolEnv = "PUTNAMI_MCP_IDENTITY_TOOL_HELPER"

// TestIdentityToolHelperProcess is the identity tool. It does nothing unless
// a tool call spawned it.
func TestIdentityToolHelperProcess(t *testing.T) {
	payloadPath := os.Getenv(identityToolEnv)
	if payloadPath == "" {
		t.Skip("helper process: not invoked as an extension tool")
	}
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	identity := os.Getenv(proto.AgentHarnessEnv) + "\n" + os.Getenv(proto.AgentModelEnv) + "\n" + os.Getenv(proto.AgentUserAgentEnv) + "\n"
	if os.WriteFile(payloadPath, payload, 0o644) != nil || os.WriteFile(payloadPath+".env", []byte(identity), 0o644) != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString(toolResult("ok"))
	// Exit before the testing package prints its own PASS line: a tool's
	// stdout carries one document.
	os.Exit(0)
}

// identityToolDefinition is the identity tool, writing its request to
// payloadPath and the agent identity environment to payloadPath+".env".
func identityToolDefinition(t *testing.T, payloadPath string) extension.ToolDefinition {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate this test binary: %v", err)
	}
	def := testExtensionToolDefinition(executable)
	def.Args = []string{"-test.run=^TestIdentityToolHelperProcess$"}
	def.Env = map[string]string{identityToolEnv: payloadPath}
	return def
}

// fixtureWorkspace writes a minimal two-project workspace and returns its root.
func fixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app","packages/lib"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("packages/lib/putnami.json", `{"name":"lib","type":"library"}`)
	workspace.InvalidateLoadCache(dir)
	recordWorkspaceIndex(t, dir)
	return dir
}

// recordWorkspaceIndex writes the recorded workspace view a real workspace has
// after `putnami projects sync`.
//
// Project identity and dependency edges are a projection of the provider
// answers, and a graph tool fails closed when no copy exists rather than
// answering an empty-but-well-formed graph. A fixture with no index would
// therefore be testing the refusal in every test — which is what
// TestGraphToolsFailClosedWithoutARecordedView is for, and nothing else should
// be.
func recordWorkspaceIndex(t *testing.T, dir string) {
	t.Helper()
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load fixture workspace: %v", err)
	}
	if _, err := workspace.RefreshSnapshot(ws, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("record fixture workspace index: %v", err)
	}
	workspace.InvalidateLoadCache(dir)
}

func newFixtureServer(t *testing.T) *Server {
	t.Helper()
	dir := fixtureWorkspace(t)
	return NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})
}

// runSession marshals each request to a JSON line, runs one Serve loop over
// them, and returns the parsed response objects (one per output line).
func runSession(t *testing.T, srv *Server, requests ...any) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, r := range requests {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		in.Write(b)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), &in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return parseResponses(t, &out)
}

func parseResponses(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var responses []map[string]any
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal response %q: %v", line, err)
		}
		responses = append(responses, m)
	}
	return responses
}

func req(id int, method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

// resultOf returns the "result" object of a response, failing if it carries an
// error instead.
func resultOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if e, ok := resp["error"]; ok {
		t.Fatalf("expected result, got error: %v", e)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}
	return res
}

// callResultJSON returns the decoded JSON object from a tools/call text result.
func callResultJSON(t *testing.T, resp map[string]any) (map[string]any, bool) {
	t.Helper()
	res := resultOf(t, resp)
	isErr, _ := res["isError"].(bool)
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call result has no content: %v", res)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		// Tool error text is a plain message, not JSON; surface it as empty.
		return map[string]any{"_text": text}, isErr
	}
	return decoded, isErr
}

func TestInitializeEchoesProtocolVersion(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
	}))
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	res := resultOf(t, resps[0])
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want echoed 2025-03-26", res["protocolVersion"])
	}
	srvInfo, _ := res["serverInfo"].(map[string]any)
	if srvInfo["name"] != serverName {
		t.Errorf("serverInfo.name = %v, want %q", srvInfo["name"], serverName)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}
	if _, ok := res["capabilities"].(map[string]any)["resources"]; !ok {
		t.Error("capabilities.resources missing")
	}
}

func TestInitializeDefaultsProtocolVersion(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "initialize", struct{}{}))
	res := resultOf(t, resps[0])
	if res["protocolVersion"] != defaultProtocolVersion {
		t.Errorf("protocolVersion = %v, want default %q", res["protocolVersion"], defaultProtocolVersion)
	}
}

func TestInitializeCarriesAutomaticRoutingAndExactLockedGuidance(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "live-extension-guidance", "initialize-is-self-contained-and-carries-exact-locked-guidance")
	const exact = "# Cloud bytes\n\nExact locked instructions.\n"
	srv := NewServer(Options{
		WorkspaceRoot: t.TempDir(), ServerVersion: "test",
		ExtensionGuidance: []ExtensionGuidance{{Name: "@putnami/cloud", Version: "0.1.0-locked", Content: exact}},
	})
	resps := runSession(t, srv, req(1, "initialize", struct{}{}))
	result := resultOf(t, resps[0])
	instructions, _ := result["instructions"].(string)
	preamble := srv.initializeRoutingPreamble()
	if len(preamble) > 512 {
		t.Fatalf("initialize routing preamble is %d bytes, want at most 512", len(preamble))
	}
	for _, want := range []string{initializeWorkspaceIdentity(srv.opts.WorkspaceRoot), "active worktree/cwd", "analysis/resume/subagents", "mismatch => local Putnami CLI, not MCP", "one advertised Intelligence tool", "no workspace arg", "freshness=indexed only", "local fallback this turn", "no second tool", "putnami.go_docs/typescript_docs"} {
		if !strings.Contains(instructions[:min(len(instructions), 512)], want) {
			t.Errorf("first 512 instruction bytes missing %q: %q", want, instructions[:min(len(instructions), 512)])
		}
	}
	if !strings.Contains(instructions, exact) {
		t.Fatalf("initialize instructions do not carry exact guidance bytes: %q", instructions)
	}
	clearRoot := "/" + strings.Repeat("w", 149)
	clearPreamble := (&Server{opts: Options{WorkspaceRoot: clearRoot}}).initializeRoutingPreamble()
	if len(clearPreamble) > 512 || !strings.Contains(clearPreamble, clearRoot) || strings.Contains(clearPreamble, "sha256:") {
		t.Fatalf("150-byte root is not clear and bounded: len=%d %q", len(clearPreamble), clearPreamble)
	}
	longRoot := "/" + strings.Repeat("long-worktree/", 80)
	longRootServer := &Server{opts: Options{WorkspaceRoot: longRoot}}
	longRootPreamble := longRootServer.initializeRoutingPreamble()
	if len(longRootPreamble) > 512 || !strings.Contains(longRootPreamble, "MCP root=sha256:") {
		t.Fatalf("long-root preamble is not bounded identity: len=%d %q", len(longRootPreamble), longRootPreamble)
	}
	for _, want := range []string{"active worktree/cwd", "Intelligence tool", "no workspace arg", "freshness=indexed", "local fallback this turn"} {
		if !strings.Contains(longRootPreamble, want) {
			t.Errorf("long-root preamble missing %q: %q", want, longRootPreamble)
		}
	}
	if instructions := longRootServer.initializeInstructions(); !strings.Contains(instructions, longRoot) || !strings.Contains(instructions, "Full MCP root") {
		t.Fatalf("long-root instructions do not provide the full comparison path: %q", instructions)
	}
}

func TestExtensionGuidanceResourcesPreserveBytesAndUnavailableVersion(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "live-extension-guidance", "versioned-resources-serve-exact-bytes-or-an-explicit-offline-status")
	const exact = "# Exact extension guide\n\nDo not normalize these bytes.  \n"
	srv := NewServer(Options{
		WorkspaceRoot: t.TempDir(), ServerVersion: "test",
		ExtensionGuidance: []ExtensionGuidance{{Name: "@putnami/cloud", Version: "1.2.3", Content: exact}},
		GuidanceIssues:    []GuidanceIssue{{Name: "@putnami/go", Version: "4.5.6", Reason: "offline cache miss"}},
	})

	availableURI := extensionGuidanceURI("@putnami/cloud", "1.2.3")
	unavailableURI := extensionGuidanceURI("@putnami/go", "4.5.6")
	resps := runSession(t, srv,
		req(1, "resources/list", nil),
		req(2, "resources/read", readResourceParams{URI: availableURI}),
		req(3, "resources/read", readResourceParams{URI: unavailableURI}),
	)
	listedRaw, err := json.Marshal(resultOf(t, resps[0]))
	if err != nil {
		t.Fatal(err)
	}
	var listed resourcesListResult
	if err := json.Unmarshal(listedRaw, &listed); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, resource := range listed.Resources {
		seen[resource.URI] = true
	}
	if !seen[availableURI] || !seen[unavailableURI] {
		t.Fatalf("guidance resources missing from list: %v", seen)
	}
	availableRaw, _ := json.Marshal(resultOf(t, resps[1]))
	var available readResourceResult
	if err := json.Unmarshal(availableRaw, &available); err != nil {
		t.Fatal(err)
	}
	if got := available.Contents[0].Text; got != exact {
		t.Fatalf("available guidance bytes = %q, want %q", got, exact)
	}
	unavailableRaw, _ := json.Marshal(resultOf(t, resps[2]))
	var unavailable readResourceResult
	if err := json.Unmarshal(unavailableRaw, &unavailable); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(unavailable.Contents[0].Text), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != "unavailable" || status.Version != "4.5.6" || status.Reason != "offline cache miss" {
		t.Fatalf("unavailable guidance status = %v", status)
	}
}

func TestUnavailableLockedExtensionCannotExposeAmbientTools(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "failed-exact-preparation-never-exposes-an-ambient-extension")
	srv := NewServer(Options{WorkspaceRoot: t.TempDir(), ServerVersion: "test", UnavailableExtensions: []string{"@putnami/cloud"}})
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name: "@putnami/cloud", Path: t.TempDir(),
		Tools: map[string]extension.ToolDefinition{"putnami.search": testExtensionToolDefinition("present")},
	}})
	if _, ok := srv.byName["putnami.search"]; ok {
		t.Fatal("ambient tool was exposed after exact locked preparation failed")
	}
}

func TestInitializeRetainsAndPropagatesOptedInAgentIdentity(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	extRoot := t.TempDir()
	payloadPath := filepath.Join(t.TempDir(), "request.json")
	envPath := payloadPath + ".env"
	definition := identityToolDefinition(t, payloadPath)
	definition.Env = map[string]string{
		identityToolEnv:         payloadPath,
		proto.AgentHarnessEnv:   "manifest-must-not-win",
		proto.AgentModelEnv:     "manifest-must-not-win",
		proto.AgentUserAgentEnv: "manifest-must-not-win",
	}
	srv := NewServer(Options{
		WorkspaceRoot:          dir,
		Config:                 wsproto.Load(dir),
		ServerVersion:          "v1.2.3",
		PropagateAgentIdentity: true,
		AgentModel:             "haiku",
	})
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name:  "@putnami/identity",
		Path:  extRoot,
		Tools: map[string]extension.ToolDefinition{"putnami.identity": definition},
	}})

	resps := runSession(t, srv,
		req(1, "initialize", map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "claude-cli", "version": "1.0"},
		}),
		req(2, "tools/call", map[string]any{"name": "putnami.identity"}),
	)
	if _, isErr := callResultJSON(t, resps[1]); isErr {
		t.Fatalf("identity tool failed: %v", resps[1])
	}

	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	var request proto.ToolCallRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode extension request: %v", err)
	}
	if request.Agent == nil {
		t.Fatal("extension request did not include agent identity")
	}
	if request.Agent.ClientName != "claude-cli" || request.Agent.ClientVersion != "1.0" {
		t.Errorf("captured client = %q %q", request.Agent.ClientName, request.Agent.ClientVersion)
	}
	if request.Agent.Harness != "claude-cli/1.0" || request.Agent.Model != "haiku" {
		t.Errorf("agent identity = %#v", request.Agent)
	}
	wantUA := "putnami-mcp/v1.2.3 (harness=claude-cli/1.0; model=haiku)"
	if request.Agent.UserAgent != wantUA {
		t.Errorf("agent User-Agent = %q, want %q", request.Agent.UserAgent, wantUA)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := "claude-cli/1.0\nhaiku\n" + wantUA + "\n"
	if string(envData) != wantEnv {
		t.Errorf("identity environment = %q, want %q", envData, wantEnv)
	}
}

func TestAgentIdentityOptOutPreservesExtensionToolRequest(t *testing.T) {
	t.Setenv(proto.AgentHarnessEnv, "inherited-harness")
	t.Setenv(proto.AgentModelEnv, "inherited-model")
	t.Setenv(proto.AgentUserAgentEnv, "inherited-user-agent")
	dir := fixtureWorkspace(t)
	extRoot := t.TempDir()
	payloadPath := filepath.Join(t.TempDir(), "request.json")
	envPath := payloadPath + ".env"
	definition := identityToolDefinition(t, payloadPath)
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		Config:        wsproto.Load(dir),
		ServerVersion: "v1.2.3",
		AgentModel:    "configured-but-disabled",
	})
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name:  "@putnami/identity",
		Path:  extRoot,
		Tools: map[string]extension.ToolDefinition{"putnami.identity": definition},
	}})

	resps := runSession(t, srv,
		req(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "cursor", "version": "1.0"}}),
		req(2, "tools/call", map[string]any{"name": "putnami.identity"}),
	)
	if _, isErr := callResultJSON(t, resps[1]); isErr {
		t.Fatalf("identity tool failed: %v", resps[1])
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	var request proto.ToolCallRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	if request.Agent != nil {
		t.Errorf("opted-out request included agent identity: %#v", request.Agent)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(envData) != "\n\n\n" {
		t.Errorf("opted-out identity environment leaked: %q", envData)
	}

	// initialize.clientInfo is still retained for the lifetime of the server,
	// even when propagation is disabled.
	srv.identityMu.RLock()
	captured := srv.identity
	srv.identityMu.RUnlock()
	if captured.ClientName != "cursor" || captured.ClientVersion != "1.0" {
		t.Errorf("captured client = %#v", captured)
	}
}

func TestToolsListAdvertisesExpectedTools(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-description", "every-tool-advertises-description-schema-and-annotations")
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "tools/list", nil))
	res := resultOf(t, resps[0])
	tools, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("tools is not an array: %v", res["tools"])
	}
	got := map[string]bool{}
	descriptions := map[string]string{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		got[name] = true
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("tool %q missing inputSchema", name)
		}
		if d, _ := tool["description"].(string); d == "" {
			t.Errorf("tool %q missing description", name)
		} else {
			descriptions[name] = d
		}
	}
	for _, want := range commandmeta.CoreMCPToolNames() {
		if !got[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
	if len(tools) != len(commandmeta.CoreMCPToolNames()) {
		t.Errorf("tools count = %d, want %d", len(tools), len(commandmeta.CoreMCPToolNames()))
	}
	for name, phrases := range map[string][]string{
		"run_jobs":        {"same planner and engine as the CLI", "dryRun=true", "refuses long-lived serve mode", "mutate external systems", "get_diagnostics"},
		"get_diagnostics": {"most recent executed run_jobs", "dry run does not replace", "without rerunning"},
	} {
		for _, phrase := range phrases {
			if !strings.Contains(descriptions[name], phrase) {
				t.Errorf("tool %q description missing %q", name, phrase)
			}
		}
	}
}

func TestNewServerDiscoversAndCallsExtensionTool(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	extRoot := filepath.Join(dir, "extensions", "search")
	if err := os.MkdirAll(filepath.Join(extRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	search := fixtureproc.Write(t, filepath.Join(extRoot, "bin", "search"), fixtureproc.Program{Stdout: toolResult("extension search result")})
	manifest := `{
  "name":"@putnami/search",
  "cliContract": 4,
  "commands":{"search-support":{"run":[{"id":"noop","task":"noop"}]}},
  "tasks":{"noop":{"kind":"command","command":"true"}},
  "tools":{"putnami.search":{
    "description":"Search the hosted index.",
    "inputSchema":{"type":"object","properties":{},"additionalProperties":false},
    "annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":true},
    "_meta":{"putnami.dev/contract":{"access":"read","readOnly":true,"supportsDryRun":false}},
    "command":"{extensionRoot}/bin/` + filepath.Base(search) + `"
  }}
}`
	if err := os.WriteFile(filepath.Join(extRoot, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"name":"fixture","includes":["packages/app","packages/lib"],"extensions":["/extensions/search"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(dir)
	srv := NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"})

	resps := runSession(t, srv, req(1, "tools/list", nil), req(2, "tools/call", map[string]any{"name": "putnami.search", "arguments": map[string]any{"query": "MCP"}}))
	if len(resps) != 2 {
		t.Fatalf("responses = %v, want tools/list and tools/call", resps)
	}
	tools := resultOf(t, resps[0])["tools"].([]any)
	var got map[string]any
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "putnami.search" {
			got = tool
			break
		}
	}
	if got == nil {
		t.Fatalf("tools/list did not include extension tool: %v", tools)
	}
	if got["description"] != "Search the hosted index." {
		t.Errorf("description = %v", got["description"])
	}
	if got["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Errorf("annotations = %v", got["annotations"])
	}
	result, isErr := callResultJSON(t, resps[1])
	if isErr || result["_text"] != "extension search result" {
		t.Errorf("extension result = %v, isError = %v", result, isErr)
	}
}

func TestExtensionToolPreflightUsesResolvedCwd(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	extRoot := t.TempDir()
	binDir := filepath.Join(extRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	search := fixtureproc.Write(t, filepath.Join(binDir, "search"), fixtureproc.Program{Stdout: toolResult("relative command result")})
	definition := testExtensionToolDefinition("bin/" + filepath.Base(search))
	definition.Cwd = "{extensionRoot}"
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name:  "@putnami/relative",
		Path:  extRoot,
		Tools: map[string]extension.ToolDefinition{"putnami.relative": definition},
	}})

	resps := runSession(t, srv,
		req(1, "tools/list", nil),
		req(2, "tools/call", map[string]any{"name": "putnami.relative"}),
	)
	tools := resultOf(t, resps[0])["tools"].([]any)
	advertised := false
	for _, raw := range tools {
		if raw.(map[string]any)["name"] == "putnami.relative" {
			advertised = true
			break
		}
	}
	if !advertised {
		t.Fatalf("tools/list did not include relative extension tool: %v", tools)
	}
	result, isErr := callResultJSON(t, resps[1])
	if isErr || result["_text"] != "relative command result" {
		t.Errorf("relative extension result = %v, isError = %v", result, isErr)
	}
}

// The preflight asks exec.Command's question: a command with a directory part
// runs from the tool's Cwd, so it is probed there and never looked up on PATH.
// On Windows both separators make a directory part, and a command without an
// extension runs the PATHEXT program, so a declared bin/tool is bin\tool.exe.
func TestExtensionToolPreflightProbesARelativeCommandInItsCwd(t *testing.T) {
	t.Parallel()
	extRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := "tool"
	commands := []string{"bin/tool", filepath.Join(extRoot, "bin", "tool")}
	if runtime.GOOS == "windows" {
		file = "tool.exe"
		commands = append(commands, `bin\tool`, "bin/tool.exe", `bin\tool.exe`)
	}
	if err := os.WriteFile(filepath.Join(extRoot, "bin", file), []byte("tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	ext := &extension.ExtensionDescription{Name: "@putnami/relative", Path: extRoot}
	for _, command := range commands {
		definition := testExtensionToolDefinition(command)
		definition.Cwd = "{extensionRoot}"
		if !extensionToolExecutablePresent(t.TempDir(), ext, definition) {
			t.Errorf("%s was not found in the tool's Cwd", command)
		}
		if filepath.IsAbs(command) {
			continue
		}
		definition.Cwd = ""
		if extensionToolExecutablePresent(t.TempDir(), ext, definition) {
			t.Errorf("%s was found although the workspace root, its Cwd, lacks it", command)
		}
	}
	if runtime.GOOS == "windows" {
		// A program Windows cannot start is not advertised: no PATHEXT match.
		if err := os.WriteFile(filepath.Join(extRoot, "bin", "data"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		definition := testExtensionToolDefinition("bin/data")
		definition.Cwd = "{extensionRoot}"
		if extensionToolExecutablePresent(t.TempDir(), ext, definition) {
			t.Error("bin/data has no PATHEXT extension, which exec.Command cannot start, yet it was advertised")
		}
	}
}

func TestExtensionToolCollisionsAndMissingBinariesAreIsolated(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	definition := testExtensionToolDefinition(trueCommand(t))
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{
		{Name: "@putnami/one", Path: t.TempDir(), Tools: map[string]extension.ToolDefinition{"putnami.same": definition}},
		{Name: "@putnami/two", Path: t.TempDir(), Tools: map[string]extension.ToolDefinition{"putnami.same": definition}},
		{Name: "@putnami/missing", Path: t.TempDir(), Tools: map[string]extension.ToolDefinition{"putnami.missing": testExtensionToolDefinition("{extensionRoot}/not-present")}},
	})
	resps := runSession(t, srv, req(1, "tools/list", nil), req(2, "tools/call", map[string]any{"name": "list_projects"}))
	tools := resultOf(t, resps[0])["tools"].([]any)
	for _, raw := range tools {
		name := raw.(map[string]any)["name"]
		if name == "putnami.same" || name == "putnami.missing" {
			t.Errorf("isolated tool %q should not be advertised", name)
		}
	}
	if _, isErr := callResultJSON(t, resps[1]); isErr {
		t.Error("core tool failed after invalid extension tools")
	}
}

func TestExtensionToolFailureDoesNotEndSession(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	dir := t.TempDir()
	bin := fixtureproc.Write(t, filepath.Join(dir, "fail"), fixtureproc.Program{Stderr: "remote service unavailable\n", Exit: 1})
	srv.registerExtensionToolDefinitions([]*extension.ExtensionDescription{{
		Name:  "@putnami/fail",
		Path:  dir,
		Tools: map[string]extension.ToolDefinition{"putnami.fail": testExtensionToolDefinition(bin)},
	}})
	resps := runSession(t, srv,
		req(1, "tools/call", map[string]any{"name": "putnami.fail"}),
		req(2, "ping", nil),
	)
	if len(resps) != 2 {
		t.Fatalf("responses = %v", resps)
	}
	failed, isErr := callResultJSON(t, resps[0])
	if !isErr || failed["_text"] != "extension tool putnami.fail failed: remote service unavailable" {
		t.Errorf("failed tool result = %v, isError = %v", failed, isErr)
	}
	if got := resultOf(t, resps[1]); len(got) != 0 {
		t.Errorf("ping after extension failure = %v, want empty object", got)
	}
}

func testExtensionToolDefinition(command string) extension.ToolDefinition {
	trueVal, falseVal := true, false
	return extension.ToolDefinition{
		Description: "Test extension tool.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: &extension.ToolAnnotations{
			ReadOnlyHint:    &trueVal,
			DestructiveHint: &falseVal,
			IdempotentHint:  &trueVal,
			OpenWorldHint:   &trueVal,
		},
		Meta: map[string]any{"putnami.dev/contract": map[string]any{
			"access": "read", "readOnly": true, "supportsDryRun": false,
		}},
		Command: command,
	}
}

func TestResourcesListAndReadWorkspaceContext(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "resources/list", nil))
	res := resultOf(t, resps[0])
	resources, ok := res["resources"].([]any)
	if !ok || len(resources) != 1 {
		t.Fatalf("resources = %v, want one resource", res["resources"])
	}
	first := resources[0].(map[string]any)
	if first["uri"] != workspaceContextURI {
		t.Fatalf("resource uri = %v, want %s", first["uri"], workspaceContextURI)
	}

	resps = runSession(t, srv, req(2, "resources/read", map[string]any{"uri": workspaceContextURI}))
	res = resultOf(t, resps[0])
	contents, ok := res["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %v, want one content item", res["contents"])
	}
	item := contents[0].(map[string]any)
	text, _ := item["text"].(string)
	var ctx map[string]any
	if err := json.Unmarshal([]byte(text), &ctx); err != nil {
		t.Fatalf("resource text is not JSON: %v\n%s", err, text)
	}
	if ctx["name"] != "fixture" {
		t.Errorf("context name = %v, want fixture", ctx["name"])
	}
	projects, _ := ctx["projects"].([]any)
	if len(projects) != 2 {
		t.Errorf("context projects = %d, want 2", len(projects))
	}
	order, _ := ctx["topologicalOrder"].([]any)
	if len(order) != 2 || order[0] != "/packages/lib" || order[1] != "/packages/app" {
		t.Errorf("topologicalOrder = %v, want lib before app", order)
	}
}

func TestPingReturnsEmptyResult(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "ping", nil))
	res := resultOf(t, resps[0])
	if len(res) != 0 {
		t.Errorf("ping result = %v, want empty object", res)
	}
}

func TestToolsCallListProjects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "listing-and-description-serve-the-loaded-graph")
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{"name": "list_projects"}))
	decoded, isErr := callResultJSON(t, resps[0])
	if isErr {
		t.Fatalf("list_projects returned error: %v", decoded)
	}
	if decoded["count"].(float64) != 2 {
		t.Errorf("count = %v, want 2", decoded["count"])
	}
	projects, _ := decoded["projects"].([]any)
	names := map[string]bool{}
	for _, p := range projects {
		names[p.(map[string]any)["name"].(string)] = true
	}
	if !names["app"] || !names["lib"] {
		t.Errorf("projects = %v, want app and lib", names)
	}
}

func TestToolsCallDescribeProject(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "listing-and-description-serve-the-loaded-graph")
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{
		"name":      "describe_project",
		"arguments": map[string]any{"project": "app"},
	}))
	decoded, isErr := callResultJSON(t, resps[0])
	if isErr {
		t.Fatalf("describe_project returned error: %v", decoded)
	}
	if decoded["id"] != "/packages/app" {
		t.Errorf("id = %v, want /packages/app", decoded["id"])
	}
	if decoded["type"] != "application" {
		t.Errorf("type = %v, want application", decoded["type"])
	}
}

func TestToolsCallDescribeProjectMissingArg(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{"name": "describe_project"}))
	_, isErr := callResultJSON(t, resps[0])
	if !isErr {
		t.Error("describe_project without project should be an isError result")
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{"name": "nope"}))
	errObj, ok := resps[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error for unknown tool, got %v", resps[0])
	}
	if int(errObj["code"].(float64)) != codeInvalidParams {
		t.Errorf("error code = %v, want %d", errObj["code"], codeInvalidParams)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	resps := runSession(t, srv, req(1, "does/not/exist", nil))
	errObj, ok := resps[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got %v", resps[0])
	}
	if int(errObj["code"].(float64)) != codeMethodNotFound {
		t.Errorf("error code = %v, want %d", errObj["code"], codeMethodNotFound)
	}
}

func TestNotificationProducesNoResponse(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	// A notification (no id) must not be answered; the following ping must.
	resps := runSession(t, srv,
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		req(7, "ping", nil),
	)
	if len(resps) != 1 {
		t.Fatalf("expected exactly 1 response (ping only), got %d: %v", len(resps), resps)
	}
	if resps[0]["id"].(float64) != 7 {
		t.Errorf("response id = %v, want 7 (the ping)", resps[0]["id"])
	}
}

func TestParseErrorReturnsNullID(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), bytes.NewReader([]byte("{not json}\n")), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	resps := parseResponses(t, &out)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	if resps[0]["id"] != nil {
		t.Errorf("parse-error id = %v, want null", resps[0]["id"])
	}
	errObj, ok := resps[0]["error"].(map[string]any)
	if !ok || int(errObj["code"].(float64)) != codeParseError {
		t.Errorf("expected parse error code %d, got %v", codeParseError, resps[0]["error"])
	}
}

func TestBlankLinesAreSkipped(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	var out bytes.Buffer
	in := bytes.NewReader([]byte("\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n"))
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	resps := parseResponses(t, &out)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d: %v", len(resps), resps)
	}
}

// fillReader streams remaining bytes of a single non-newline byte without ever
// materializing them, so a test can hand the server a frame far larger than the
// cap without allocating one itself (which would defeat the point of measuring
// what the server allocates).
type fillReader struct{ remaining int }

func (r *fillReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

// assertFrameRejection pins the whole wire line an oversized frame gets back.
// The rejection is a fixed shape, so it is asserted verbatim rather than walked
// as a decoded object: a null id (the frame was never parsed, so there is no id
// to echo) and one bounded, fixed-length error whose size cannot be influenced
// by the request that caused it.
func assertFrameRejection(t *testing.T, line string) {
	t.Helper()
	want := fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":%q}}`,
		codeInvalidRequest, errFrameTooLarge.Error())
	if line != want {
		t.Errorf("rejection line =\n\t%s\nwant\n\t%s", line, want)
	}
}

func TestOversizedFrameIsRejectedAndSessionSurvives(t *testing.T) {
	t.Parallel()
	ping := `{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n"
	cases := []struct {
		name string
		// size is the payload written before the frame's terminating newline.
		size int
	}{
		// The cap is crossed by the read that also carries the terminator, so
		// the frame is already fully consumed and needs no resynchronization.
		{name: "overflow at terminator", size: maxFrameBytes},
		// The cap is crossed mid-frame: the remainder has to be discarded
		// without being buffered before the next frame can be read.
		{name: "overflow mid frame", size: maxFrameBytes + 256*1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFixtureServer(t)
			var out bytes.Buffer
			in := io.MultiReader(
				&fillReader{remaining: tc.size},
				strings.NewReader("\n"+ping),
			)
			if err := srv.Serve(context.Background(), in, &out); err != nil {
				t.Fatalf("Serve: %v", err)
			}
			raw := out.String()
			resps := parseResponses(t, &out)
			if len(resps) != 2 {
				t.Fatalf("expected 2 responses (rejection + ping), got %d: %v", len(resps), resps)
			}
			assertFrameRejection(t, strings.SplitN(raw, "\n", 2)[0])
			// The session must survive one bad frame: the well-behaved request
			// that follows it is answered normally.
			if resps[1]["id"].(float64) != 9 {
				t.Errorf("second response id = %v, want 9 (the ping)", resps[1]["id"])
			}
			resultOf(t, resps[1])
		})
	}
}

func TestUnterminatedOversizedStreamStaysBounded(t *testing.T) {
	// Sixteen times the cap, never terminated: the exact shape that made the
	// old reader allocate until the process died.
	const streamed = 16 * maxFrameBytes
	srv := newFixtureServer(t)
	var out bytes.Buffer

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := srv.Serve(context.Background(), &fillReader{remaining: streamed}, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	runtime.ReadMemStats(&after)

	raw := out.String()
	resps := parseResponses(t, &out)
	if len(resps) != 1 {
		t.Fatalf("expected exactly 1 rejection, got %d: %v", len(resps), resps)
	}
	assertFrameRejection(t, strings.SplitN(raw, "\n", 2)[0])

	// Without a cap the reader grew a single buffer to hold the whole stream
	// (and then copied it), so allocation tracked `streamed`. Reading against a
	// bounded counter keeps it near the cap instead. The 8x threshold absorbs
	// slice-growth doubling and unrelated allocations while still failing hard
	// if a 64 MiB frame is ever buffered.
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(8 * maxFrameBytes); allocated > limit {
		t.Errorf("Serve allocated %d bytes for a %d-byte unterminated frame, want <= %d",
			allocated, streamed, limit)
	}
}

func TestLargeFrameUnderCapIsAccepted(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)
	// A frame just under the cap stands in for the largest legitimate request
	// (a tool call whose arguments enumerate a big workspace): the cap must not
	// break it.
	line := `{"jsonrpc":"2.0","id":5,"method":"ping","params":{"pad":"` +
		strings.Repeat("a", maxFrameBytes-1024) + `"}}` + "\n"
	if len(line) > maxFrameBytes {
		t.Fatalf("test frame is %d bytes, which exceeds the %d-byte cap", len(line), maxFrameBytes)
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(line), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	resps := parseResponses(t, &out)
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d: %v", len(resps), resps)
	}
	if resps[0]["id"].(float64) != 5 {
		t.Errorf("response id = %v, want 5", resps[0]["id"])
	}
	resultOf(t, resps[0])
}

func TestInitializeBoundsTotalGuidanceAndPointsAtTheResource(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "live-extension-guidance", "initialize-instructions-are-bounded-across-every-extension")
	t.Parallel()
	const small = "# Small exact guide\n"
	oversized := strings.Repeat("x", maxInitializeGuidanceBytes+1)
	srv := NewServer(Options{
		WorkspaceRoot: t.TempDir(), ServerVersion: "test",
		ExtensionGuidance: []ExtensionGuidance{
			{Name: "@putnami/a-small", Version: "1.0.0", Content: small},
			{Name: "@putnami/b-huge", Version: "2.0.0", Content: oversized},
		},
	})

	instructions := srv.initializeInstructions()
	if !strings.Contains(instructions, small) {
		t.Fatal("a guide within budget was dropped")
	}
	if strings.Contains(instructions, oversized) {
		t.Fatalf("an oversized guide entered initialize instructions (%d bytes)", len(instructions))
	}
	if len(instructions) > maxInitializeGuidanceBytes+4096 {
		t.Fatalf("initialize instructions = %d bytes, want a bounded document", len(instructions))
	}
	for _, want := range []string{
		"exact extension guidance omitted: @putnami/b-huge@2.0.0",
		extensionGuidanceURI("@putnami/b-huge", "2.0.0"),
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("omitted guidance does not name %q:\n%s", want, instructions)
		}
	}

	// The complete bytes must still be readable, so nothing is actually lost.
	resps := runSession(t, srv, req(1, "resources/read",
		readResourceParams{URI: extensionGuidanceURI("@putnami/b-huge", "2.0.0")}))
	raw, _ := json.Marshal(resultOf(t, resps[0]))
	var read readResourceResult
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || read.Contents[0].Text != oversized {
		t.Fatal("the omitted guide is not served in full as a resource")
	}
}

func TestGuidanceStatusResourceNameOmitsAMissingVersion(t *testing.T) {
	t.Parallel()
	if got := guidanceStatusResourceName(GuidanceIssue{Name: "@putnami/cloud"}); got != "@putnami/cloud AI guidance status (no lock pin)" {
		t.Errorf("unpinned status name = %q", got)
	}
	if got := guidanceStatusResourceName(GuidanceIssue{Name: "@putnami/cloud", Version: "1.2.3"}); got != "@putnami/cloud@1.2.3 AI guidance status" {
		t.Errorf("pinned status name = %q", got)
	}
}
