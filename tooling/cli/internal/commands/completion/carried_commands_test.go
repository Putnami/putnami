package completion

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// writeCarriedCommandsWorkspace writes a workspace whose @acme/flow extension
// declares the group change, which carries the flat command change-verify
// (one level down, ship-check), and a public command lint-docs that no group
// carries.
func writeCarriedCommandsWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	task := `"tasks": {"t": {"kind": "command", "command": "echo", "cache": false}}`
	command := func(description string) string {
		return `{"description": "` + description + `", "run": [{"id": "r", "task": "t"}]}`
	}
	files := map[string]string{
		"putnami.workspace.json":      `{"name": "carried-completion-ws", "includes": ["flow"]}`,
		"flow/putnami.json":           `{"name": "@acme/flow"}`,
		"flow/putnami.extension.json": `{"name": "@acme/flow", "cliContract": 4, "commands": {"change-verify": ` + command("Verify.") + `, "ship-check": ` + command("Check.") + `, "lint-docs": ` + command("Lint docs.") + `}, ` + task + `, "commandGroups": {"change": {"description": "Manage changes", "subcommands": {"verify": {"description": "Verify.", "command": "change-verify"}, "ship": {"description": "Ship.", "subcommands": {"check": {"description": "Check.", "command": "ship-check"}}}}}}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGatherCompletionContext_OffersTheGroupNotTheCommandsItCarries(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "completion-skips-carried-commands", "completion-offers-groups-not-the-commands-they-carry")
	ctx := gatherCompletionContext(writeCarriedCommandsWorkspace(t), &wsproto.Config{}, nil)

	for _, carried := range []string{"change-verify", "ship-check"} {
		if sharedtest.Contains(ctx.commands, carried) {
			t.Errorf("top-level commands offer %s, which the group change carries and which fails typed directly (commands %v)", carried, ctx.commands)
		}
	}
	if !sharedtest.Contains(ctx.commands, "lint-docs") {
		t.Errorf("top-level commands miss lint-docs, which no group carries (commands %v)", ctx.commands)
	}
	if !sharedtest.Contains(ctx.structured, "change") {
		t.Errorf("structured roots miss the group change (roots %v)", ctx.structured)
	}
	for _, sub := range []string{"verify", "ship"} {
		if !sharedtest.Contains(ctx.subcommands["change"], sub) {
			t.Errorf("subcommands of change miss %s (subcommands %v)", sub, ctx.subcommands["change"])
		}
	}
	if !sharedtest.Contains(ctx.subcommands["change ship"], "check") {
		t.Errorf("subcommands of change ship miss check (subcommands %v)", ctx.subcommands["change ship"])
	}
}
