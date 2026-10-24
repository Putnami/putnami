package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The real server, as the CLI shell wires it, reloads when the workspace adds
// an extension by path between two requests: it announces both lists before
// the response, the tool result says what changed, and tools/list carries the
// new extension's tool.
func TestMCPReloadsAnExtensionTheWorkspaceAddsBetweenRequests(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-context-change-notice", "the-lists-after-a-reload-follow-the-new-extension-set")
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app"]}`)
	write("packages/app/putnami.json", `{"name":"app","type":"application"}`)
	workspace.InvalidateLoadCache(root)
	srv := newMCPServer(root, wsproto.Load(root), "test")

	program := fixtureproc.Write(t, filepath.Join(root, "extensions", "notes", "bin", "notes"), fixtureproc.Program{Stdout: `{"content":[{"type":"text","text":"notes"}]}`})
	write("extensions/notes/putnami.extension.json", `{
  "name":"@putnami/notes",
  "cliContract": 4,
  "commands":{"notes-support":{"run":[{"id":"noop","task":"noop"}]}},
  "tasks":{"noop":{"kind":"command","command":"true"}},
  "tools":{"notes.read":{
    "description":"Read the notes.",
    "inputSchema":{"type":"object","properties":{},"additionalProperties":false},
    "annotations":{"readOnlyHint":true,"destructiveHint":false,"idempotentHint":true,"openWorldHint":false},
    "_meta":{"putnami.dev/contract":{"access":"read","readOnly":true,"supportsDryRun":false}},
    "command":"{extensionRoot}/bin/`+filepath.Base(program)+`"
  }}
}`)
	write("putnami.workspace.json", `{"name":"fixture","includes":["packages/app"],"extensions":["/extensions/notes"]}`)
	workspace.InvalidateLoadCache(root)

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_diagnostics","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"notes.read","arguments":{}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(requests), &out); err != nil {
		t.Fatal(err)
	}
	type message struct {
		Method string          `json:"method"`
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	var lines []message
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line message
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	if len(lines) != 5 || lines[0].Method != "notifications/tools/list_changed" || lines[1].Method != "notifications/resources/list_changed" || lines[2].ID != 1 {
		t.Fatalf("lines = %s, want the two notifications, then the three responses", out.String())
	}
	var call struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(lines[2].Result, &call); err != nil {
		t.Fatal(err)
	}
	if len(call.Content) < 2 {
		t.Fatalf("the tool result carries no notice: %s", lines[2].Result)
	}
	notice := call.Content[len(call.Content)-1].Text
	for _, want := range []string{
		"- extension extensions/notes now loads. Read extensions/notes/putnami.extension.json",
		"- Putnami reloaded its extension tools and guides; list the tools again.",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice lacks %q:\n%s", want, notice)
		}
	}
	if !strings.Contains(string(lines[3].Result), `"name":"notes.read"`) {
		t.Errorf("tools/list after the reload does not list notes.read: %s", lines[3].Result)
	}
	if err := json.Unmarshal(lines[4].Result, &call); err != nil || len(call.Content) != 1 || call.Content[0].Text != "notes" {
		t.Errorf("notes.read = %s, want the tool's answer alone", lines[4].Result)
	}
}

const (
	coldDecisionsOne = `{"protocolVersion":1,"decisions":[{"id":"D-001","statement":"every pull request names what it deletes","settled":"2026-09-03","settledBy":"fdumay","reviewOnly":true}]}`
	coldDecisionsTwo = `{"protocolVersion":1,"decisions":[{"id":"D-001","statement":"every pull request names what it deletes","settled":"2026-09-03","settledBy":"fdumay","reviewOnly":true},{"id":"D-002","statement":"every GitHub artifact is written in English","settled":"2026-09-11","settledBy":"fdumay","reviewOnly":true}]}`
)

// In a fresh worktree, the server starts before the workspace index exists,
// and the first tool call prepares it. The index appearing changes no
// decision: no tool result reports the decisions of a provider-described
// project as added, and a later edit to them is reported.
func TestMCPReportsNoDecisionAddedWhenTheIndexAppears(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-context-change-notice", "a-project-a-load-finds-later-joins-without-a-notice")
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename, `{"name":"cold","includes":["app"],"extensions":["/provider"]}`)
	write("app/package.json", `{"name":"@acme/app"}`)
	write("app/decisions.json", coldDecisionsOne)
	write("provider/putnami.extension.json", `{
  "name": "@putnami/test-provider",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {"executable": "runtime"},
  "workspace": {"markers": ["package.json"], "inputs": ["package.json"]}
}`)
	fixtureproc.Script(t, filepath.Join(root, "provider", "runtime"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' '{"extension":"@putnami/test-provider","version":"1.0.0","platform":"%s/%s","cliContract":4,"runtimeProtocol":2,"runtimeABI":1}'
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' '{"version":1,"extension":"@putnami/test-provider","projects":[{"path":"app","sourceName":"@acme/app"}]}'
  exit 0
fi
exit 2
`, runtime.GOOS, runtime.GOARCH), "__putnami", "runtime-info")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	srv := newMCPServerWithContext(context.Background(), root, wsproto.Load(root), "test")
	if _, err := os.Stat(workspace.SnapshotPath(root)); !os.IsNotExist(err) {
		t.Fatalf("the fixture is not a fresh worktree: an index exists: %v", err)
	}

	toolTexts := func(requests ...string) [][]string {
		t.Helper()
		var out bytes.Buffer
		if err := srv.Serve(context.Background(), strings.NewReader(strings.Join(requests, "\n")+"\n"), &out); err != nil {
			t.Fatal(err)
		}
		var results [][]string
		for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var line struct {
				Result struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error *testJSONRPCError `json:"error"`
			}
			if err := json.Unmarshal([]byte(raw), &line); err != nil || line.Error != nil {
				t.Fatalf("line %q is not a tool result: %v", raw, err)
			}
			texts := make([]string, 0, len(line.Result.Content))
			for _, block := range line.Result.Content {
				texts = append(texts, block.Text)
			}
			results = append(results, texts)
		}
		return results
	}
	listProjects := func(id int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`, id)
	}

	results := toolTexts(listProjects(1), listProjects(2))
	if len(results) != 2 || !strings.Contains(strings.Join(results[0], ""), "@acme/app") {
		t.Fatalf("results = %q, want two list_projects answers that find @acme/app", results)
	}
	for i, texts := range results {
		if len(texts) != 1 {
			t.Errorf("tool result %d carries a notice though nothing changed: %q", i+1, texts)
		}
	}

	write("app/decisions.json", coldDecisionsTwo)
	results = toolTexts(listProjects(3))
	if len(results) != 1 || len(results[0]) != 2 || !strings.Contains(results[0][1], "- D-002 added in app/decisions.json.") {
		t.Errorf("results = %q, want the edit to app/decisions.json reported", results)
	}
}
