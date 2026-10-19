package extension

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// commandKindManifest declares one command and one task, at contract, with the
// command's traits and the task's batchable policy written as given. An empty
// fragment declares nothing.
func commandKindManifest(contract int, traits, batchable string) string {
	if traits != "" {
		traits = `"traits": ` + traits + `, `
	}
	if batchable != "" {
		batchable = `, "batchable": ` + batchable
	}
	return fmt.Sprintf(`{
		"name": "@acme/lang",
		"cliContract": %d,
		"commands": {"check": {%s"run": [{"id": "check", "task": "check"}]}},
		"tasks": {"check": {"kind": "command", "command": "echo", "timeoutMs": -1%s}}
	}`, contract, traits, batchable)
}

// Discovery refuses a manifest that declares a command kind, a workload,
// rewritesSources, consumesReleaseSet, or a batchable kind or phase below
// contract 11, with the reason, and loads it at 11. Each field raises the
// floor alone, and traits that declare none of them do not.
func TestDiscovery_AManifestDeclaringACommandKindRequiresContract11(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "command-kinds-and-traits", "a-manifest-declaring-a-kind-or-new-trait-requires-contract-11")
	for name, fields := range map[string][2]string{
		"kind":               {`{"kind": "verification"}`, ""},
		"workload":           {`{"requiresRunnable": true, "workload": "serve"}`, ""},
		"rewritesSources":    {`{"rewritesSources": true}`, ""},
		"consumesReleaseSet": {`{"consumesReleaseSet": true}`, ""},
		"batchable kind":     {"", `{"tool": "echo", "kind": "diagnostics"}`},
		"batchable phase":    {"", `{"tool": "echo", "phase": "lint"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeExtensionManifest(t, filepath.Join(dir, "below"), commandKindManifest(protocolcli.SourceBoundCacheContract, fields[0], fields[1]))
			writeExtensionManifest(t, filepath.Join(dir, "at"), strings.Replace(
				commandKindManifest(protocolcli.CommandKindContract, fields[0], fields[1]), "@acme/lang", "@acme/kinds", 1))
			result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"below", "at"})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason.Error(), "requires 11 for its command kinds") {
				t.Fatalf("skipped = %v, want the contract-10 manifest refused for its command kinds", result.Skipped)
			}
			if FindExtensionByName(result.Extensions, "@acme/lang") != nil || FindExtensionByName(result.Extensions, "@acme/kinds") == nil {
				t.Fatalf("loaded %d extensions, want only the contract-11 one", len(result.Extensions))
			}
		})
	}

	t.Run("traits without them", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeExtensionManifest(t, filepath.Join(dir, "lang"),
			commandKindManifest(protocolcli.CurrentContract, `{"heavy": true}`, `{"tool": "echo", "maxProjects": 4}`))
		result, err := DiscoverExtensionsDetailed(dir, &wsproto.Config{}, []string{"lang"})
		if err != nil || len(result.Skipped) != 0 || FindExtensionByName(result.Extensions, "@acme/lang") == nil {
			t.Fatalf("a contract-4 manifest without the fields did not load: %+v, %v", result.Skipped, err)
		}
	})
}
