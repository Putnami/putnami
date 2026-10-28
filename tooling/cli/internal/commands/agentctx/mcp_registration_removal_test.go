package agentctx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// writtenMCPJSON is the file earlier releases created from nothing in a
// consumer workspace, byte for byte; command is putnami or ./putnamiw.
func writtenMCPJSON(command string) string {
	return "{\n  \"mcpServers\": {\n    \"putnami\": {\n      \"args\": [\n        \"mcp\"\n      ],\n      \"command\": \"" + command + "\",\n      \"env\": {\n        \"PUTNAMI_AGENT_WORKSPACE\": \"${CLAUDE_PROJECT_DIR:-.}\"\n      }\n    }\n  }\n}\n"
}

func writeMCPJSON(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, mcpConfigPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The entry Putnami wrote, under either launcher command, is removed: a
// workspace may have switched between the two since the write.
func TestRemoveMCPRegistrationRemovesAPutnamiWrittenEntry(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "mcp-registration-removal", "a-putnami-written-entry-is-removed")
	merged := `{"customKey":42,"mcpServers":{"other":{"args":["serve"],"command":"other-bin"},"putnami":{"args":["mcp"],"command":"%s","env":{"PUTNAMI_AGENT_WORKSPACE":"${CLAUDE_PROJECT_DIR:-.}"}}}}`
	for _, command := range []string{"putnami", "./putnamiw"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			path := writeMCPJSON(t, dir, strings.Replace(merged, "%s", command, 1)+"\n")
			var warn bytes.Buffer
			if got := RemoveMCPRegistration(dir, &warn); got != MCPRemovalEntry {
				t.Fatalf("RemoveMCPRegistration = %v, want the entry removed", got)
			}
			if warn.Len() != 0 {
				t.Fatalf("removing Putnami's own entry warned: %q", warn.String())
			}
			want := "{\n  \"customKey\": 42,\n  \"mcpServers\": {\n    \"other\": {\n      \"args\": [\n        \"serve\"\n      ],\n      \"command\": \"other-bin\"\n    }\n  }\n}\n"
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("file after removal = %q (%v), want %q", got, err, want)
			}
		})
	}
}

// A file that held nothing but the entry was Putnami's alone: it goes.
func TestRemoveMCPRegistrationDeletesTheFileWhenNothingElseRemains(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "mcp-registration-removal", "the-file-goes-when-nothing-else-remains")
	for _, command := range []string{"putnami", "./putnamiw"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			path := writeMCPJSON(t, dir, writtenMCPJSON(command))
			if got := RemoveMCPRegistration(dir, nil); got != MCPRemovalFile {
				t.Fatalf("RemoveMCPRegistration = %v, want the file removed", got)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("%s still exists: stat err %v", mcpConfigPath, err)
			}
		})
	}

	// An unknown root key is the user's: the file stays, without the entry.
	dir := t.TempDir()
	path := writeMCPJSON(t, dir, strings.Replace(writtenMCPJSON("putnami"), "{\n  \"mcpServers\"", "{\n  \"customKey\": 1,\n  \"mcpServers\"", 1))
	if got := RemoveMCPRegistration(dir, nil); got != MCPRemovalEntry {
		t.Fatalf("RemoveMCPRegistration = %v, want only the entry removed", got)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "{\n  \"customKey\": 1,\n  \"mcpServers\": {}\n}\n" {
		t.Fatalf("file after removal = %q (%v)", got, err)
	}
}

// An entry that differs from the written one was changed on purpose: it stays
// byte for byte, with one warning that names the file.
func TestRemoveMCPRegistrationKeepsAChangedEntryWithAWarning(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "mcp-registration-removal", "a-changed-entry-is-kept-with-a-warning")
	for name, content := range map[string]string{
		"another command": `{"mcpServers":{"putnami":{"command":"/stale/path/putnami","args":["mcp"]},"other":{"command":"x"}}}` + "\n",
		"a missing env":   `{"mcpServers":{"putnami":{"command":"./putnamiw","args":["mcp"]}}}` + "\n",
		"an extra field":  `{"mcpServers":{"putnami":{"args":["mcp"],"command":"putnami","env":{"PUTNAMI_AGENT_WORKSPACE":"${CLAUDE_PROJECT_DIR:-.}"},"disabled":true}}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeMCPJSON(t, dir, content)
			var warn bytes.Buffer
			if got := RemoveMCPRegistration(dir, &warn); got != MCPRemovalNone {
				t.Fatalf("RemoveMCPRegistration = %v, want nothing removed", got)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != content {
				t.Fatalf("a changed entry was rewritten: %q (%v)", got, err)
			}
			if strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "warning") ||
				!strings.Contains(warn.String(), mcpConfigPath) || !strings.Contains(warn.String(), "by hand") {
				t.Fatalf("warning = %q, want one line that names %s and says to remove the entry by hand", warn.String(), mcpConfigPath)
			}
		})
	}
}

func TestRemoveMCPRegistrationWarnsAndLeavesAnUnparseableFile(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "mcp-registration-removal", "an-unparseable-file-is-a-warning-and-stays-untouched")
	dir := t.TempDir()
	content := `{not json`
	path := writeMCPJSON(t, dir, content)
	var warn bytes.Buffer
	if got := RemoveMCPRegistration(dir, &warn); got != MCPRemovalNone {
		t.Fatalf("RemoveMCPRegistration = %v over an unparseable file", got)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != content {
		t.Fatalf("an unparseable file changed: %q (%v)", got, err)
	}
	if !strings.Contains(warn.String(), "warning") || !strings.Contains(warn.String(), mcpConfigPath) {
		t.Fatalf("warning = %q, want it to name %s", warn.String(), mcpConfigPath)
	}
}

// A missing file, or one with no putnami entry, is not Putnami's to report.
func TestRemoveMCPRegistrationIsSilentWithoutAPutnamiEntry(t *testing.T) {
	for name, content := range map[string]string{
		"no file":            "",
		"other servers":      `{"mcpServers":{"other":{"command":"x"}}}` + "\n",
		"no servers":         `{}` + "\n",
		"servers not a map":  `{"mcpServers":["array"]}` + "\n",
		"a null document":    `null` + "\n",
		"an empty servers":   `{"mcpServers":{}}` + "\n",
		"a non-object entry": `{"mcpServers":{"other":1}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if content != "" {
				writeMCPJSON(t, dir, content)
			}
			var warn bytes.Buffer
			if got := RemoveMCPRegistration(dir, &warn); got != MCPRemovalNone || warn.Len() != 0 {
				t.Fatalf("RemoveMCPRegistration = %v, warning %q; want nothing and silence", got, warn.String())
			}
			if content == "" {
				return
			}
			if got, err := os.ReadFile(filepath.Join(dir, mcpConfigPath)); err != nil || string(got) != content {
				t.Fatalf("the file changed: %q (%v)", got, err)
			}
		})
	}
}

// Acceptance criterion: `putnami context generate` neither creates .mcp.json
// nor touches an existing one, in either direction.
func TestContextGenerateNeverWritesMCPConfig(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "generated-context", "context-generation-writes-no-mcp-registration")
	dir := t.TempDir()
	wsConfig := `{"name":"t","extensions":["@putnami/go"]}`
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(wsConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := wsproto.Load(dir)

	if err := ContextGenerate(dir, cfg, nil); err != nil {
		t.Fatalf("ContextGenerate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, mcpConfigPath)); !os.IsNotExist(err) {
		t.Fatalf("context generation created %s: stat err %v", mcpConfigPath, err)
	}

	// A registration written by an older release is the workspace's data until
	// an explicit install or upgrade removes it: generation does not touch it.
	legacy := writtenMCPJSON("putnami")
	writeMCPJSON(t, dir, legacy)
	if err := ContextGenerate(dir, cfg, nil); err != nil {
		t.Fatalf("ContextGenerate over an existing registration: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, mcpConfigPath))
	if err != nil || string(after) != legacy {
		t.Fatalf("context generation rewrote %s:\n%s\n(%v)", mcpConfigPath, after, err)
	}
}
