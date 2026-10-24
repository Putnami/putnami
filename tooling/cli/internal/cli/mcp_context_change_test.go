package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
