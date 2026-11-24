package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// promptHookFixture is the host-hooks fixture with a Claude Code hook at
// UserPromptSubmit that runs in the background, stamped 25.
func promptHookFixture(t *testing.T) *Manifest {
	t.Helper()
	m := loadFixtureManifest(t, hostHooksFixture)
	m.CLIContract = protocolcli.PromptHostHooksContract
	m.HostHooks.Hosts[HostClaudeCode] = append(m.HostHooks.Hosts[HostClaudeCode], HostHookEntry{
		Event: HostHookEventUserPromptSubmit, Args: []string{"--engine", "claude-code"}, Async: true, TimeoutSeconds: 30,
	})
	return m
}

// A hook at UserPromptSubmit, or any hook that declares async, requires rung
// 25 from every origin, and the stamped manifest validates and loads.
func TestPromptHostHooks_RequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "prompt-host-hooks-require-contract-25")
	m := promptHookFixture(t)
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a prompt hook must validate: %v", diags)
	}
	if !DeclaresPromptHostHooks(m) {
		t.Fatal("DeclaresPromptHostHooks = false for a hook at UserPromptSubmit")
	}
	if got := RequiredLoadContract(m); got != protocolcli.PromptHostHooksContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.PromptHostHooksContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.PromptHostHooksContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.PromptHostHooksContract)
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err != nil {
		t.Fatalf("a manifest stamped 25 must load: %v", err)
	}
	entries := loaded.HostHooks.Hosts[HostClaudeCode]
	if last := entries[len(entries)-1]; last.Event != HostHookEventUserPromptSubmit || !last.Async {
		t.Fatalf("loaded prompt hook = %+v", last)
	}

	// Either half of the vocabulary alone raises the floor.
	eventOnly := loadFixtureManifest(t, hostHooksFixture)
	eventOnly.HostHooks.Hosts[HostClaudeCode][0].Event = HostHookEventUserPromptSubmit
	asyncOnly := loadFixtureManifest(t, hostHooksFixture)
	asyncOnly.HostHooks.Hosts[HostClaudeCode][0].Async = true
	for name, manifest := range map[string]*Manifest{"event": eventOnly, "async": asyncOnly} {
		if got := RequiredLoadContract(manifest); got != protocolcli.PromptHostHooksContract {
			t.Errorf("%s: RequiredLoadContract = %d, want %d", name, got, protocolcli.PromptHostHooksContract)
		}
	}
	// The hooks of before keep their rung.
	if DeclaresPromptHostHooks(loadFixtureManifest(t, hostHooksFixture)) {
		t.Fatal("DeclaresPromptHostHooks = true for hooks without the event or async")
	}
}

// Stamped below 25, a manifest with a prompt hook is refused for it, with the
// remedy that moves it.
func TestPromptHostHooks_BelowTheirContractAreRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "prompt-host-hooks-below-contract-25-do-not-load")
	for _, stamp := range []int{protocolcli.HostHooksContract, protocolcli.PromptHostHooksContract - 1} {
		m := promptHookFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest with a prompt hook", stamp)
		}
		for _, want := range []string{"requires 25 for a host hook at the prompt or in the background", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// Only a Claude Code hook runs in the background, never one that wakes, and
// Codex runs no hook at UserPromptSubmit. async is part of what a hook does.
func TestPromptHostHooks_AsyncIsClaudeCodeOnly(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "host-hooks-are-closed-and-bounded", "only-a-claude-code-hook-runs-in-the-background")
	codex := promptHookFixture(t)
	codex.HostHooks.Hosts[HostCodex][0].Async = true
	if got := diagCodes(ValidateHostHooks(codex)); len(got) != 1 || got[0] != "invalid-host-hook-async" {
		t.Errorf("an async Codex hook: codes = %v, want [invalid-host-hook-async]", got)
	}

	wake := promptHookFixture(t)
	for index, entry := range wake.HostHooks.Hosts[HostClaudeCode] {
		if entry.Wake {
			wake.HostHooks.Hosts[HostClaudeCode][index].Async = true
		}
	}
	if got := diagCodes(ValidateHostHooks(wake)); len(got) != 1 || got[0] != "invalid-host-hook-async" {
		t.Errorf("an async wake hook: codes = %v, want [invalid-host-hook-async]", got)
	}

	prompt := promptHookFixture(t)
	prompt.HostHooks.Hosts[HostCodex] = append(prompt.HostHooks.Hosts[HostCodex], HostHookEntry{
		Event: HostHookEventUserPromptSubmit, Args: []string{"--engine", "codex"}, TimeoutSeconds: 30,
	})
	if !hasCode(ValidateHostHooks(prompt), "unknown-host-hook-event") {
		t.Error("Codex accepted a hook at UserPromptSubmit")
	}

	twice := promptHookFixture(t)
	entries := twice.HostHooks.Hosts[HostClaudeCode]
	foreground := entries[len(entries)-1]
	foreground.Async = false
	twice.HostHooks.Hosts[HostClaudeCode] = append(entries, foreground)
	if hasCode(ValidateHostHooks(twice), "duplicate-host-hook") {
		t.Error("a foreground and a background hook of the same command count as one")
	}
}
