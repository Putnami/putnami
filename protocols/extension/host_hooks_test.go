package extension

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// hostHooksFixture is a manifest stamped 16 that declares host hooks for both
// hosts. It lives outside fixtures/valid, whose tests hold every valid
// fixture to the base contract and no agent content.
var hostHooksFixture = filepath.Join("testdata", "host-hooks", ManifestFilename)

// hostHooksInvalidFixtureCodes maps each host-hooks counter-example to the
// exact diagnostic codes FullValidateManifest reports, in order.
var hostHooksInvalidFixtureCodes = map[string][]string{
	// The opt-in that enables the hooks is the opt-in into the content.
	"host-hooks-without-agent-content.json": {"host-hooks-without-agent-content"},
	// The contract is a lowercase token.
	"host-hooks-contract.json": {"invalid-host-hooks-contract"},
	// Only an interactive subcommand hands its exit code to the host.
	"host-hooks-command.json": {"invalid-host-hooks-command"},
	// A host lists hooks, and the hosts are a closed list.
	"host-hooks-hosts.json": {"empty-host-hooks", "unknown-host-hooks-host"},
	// Events per host, matchers on PostToolUse only and always there,
	// shell-safe arguments, a bounded timeout, wake on Claude Code only, no
	// repeated hook.
	"host-hooks-entries.json": {
		"unknown-host-hook-event",
		"invalid-host-hook-matcher",
		"invalid-host-hook-matcher",
		"invalid-host-hook-args",
		"invalid-host-hook-timeout",
		"duplicate-host-hook",
		"unknown-host-hook-event",
		"invalid-host-hook-wake",
		"invalid-host-hook-timeout",
		"invalid-host-hook-matcher",
	},
}

func TestHostHooks_InvalidFixturesReportExactCodes(t *testing.T) {
	for name, codes := range hostHooksInvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			diags := FullValidateManifest(m)
			if got := diagCodes(diags); !reflect.DeepEqual(got, codes) {
				t.Fatalf("codes = %v, want %v (%v)", got, codes, diags)
			}
		})
	}
}

// The valid fixture declares every event of both hosts, a matcher of host and
// MCP tools, and a wake entry, and passes strict validation.
func TestHostHooks_ValidFixtureValidates(t *testing.T) {
	m := loadFixtureManifest(t, hostHooksFixture)
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("the host-hooks fixture must validate: %v", diags)
	}
	if !DeclaresHostHooks(m) {
		t.Fatal("DeclaresHostHooks = false for the host-hooks fixture")
	}
	if got := m.HostHooks.Command; !reflect.DeepEqual(got, []string{"agent", "hook"}) {
		t.Fatalf("command = %v", got)
	}
	if got := len(m.HostHooks.Hosts[HostClaudeCode]); got != 5 {
		t.Fatalf("claude-code hooks = %d, want 5", got)
	}
}

// A manifest that declares host hooks requires rung 16 from every origin, and
// the stamped fixture loads.
func TestHostHooks_RequireTheirContract(t *testing.T) {
	m := loadFixtureManifest(t, hostHooksFixture)
	if got := RequiredLoadContract(m); got != protocolcli.HostHooksContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.HostHooksContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.HostHooksContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.HostHooksContract)
	}
	loaded, err := LoadManifest(hostHooksFixture)
	if err != nil {
		t.Fatalf("a manifest stamped 16 must load: %v", err)
	}
	if loaded.HostHooks == nil || loaded.HostHooks.Requires.Contract != "missions" {
		t.Fatalf("loaded host hooks = %+v", loaded.HostHooks)
	}
}

// Stamped below 16, a host-hooks manifest is refused for its hooks, with the
// remedy that moves it. An older reader would install the extension without
// the hooks the workspace opted into.
func TestHostHooks_BelowTheirContractAreRefused(t *testing.T) {
	for _, stamp := range []int{protocolcli.AgentContentContract, protocolcli.HostHooksContract - 1} {
		m := loadFixtureManifest(t, hostHooksFixture)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest that declares host hooks", stamp)
		}
		for _, want := range []string{"requires 16 for its host hooks", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// A manifest without the member keeps its exact verdict and its stamp.
func TestHostHooks_InertWithoutTheMember(t *testing.T) {
	m := loadFixtureManifest(t, "fixtures/valid/agent-content.json")
	if diags := ValidateHostHooks(m); len(diags) != 0 {
		t.Fatalf("host hooks validation spoke about a manifest without the member: %v", diags)
	}
	if got := RequiredCLIContract(m); got != protocolcli.AgentContentContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.AgentContentContract)
	}
}

// A member the vocabulary does not name, such as a probe command, fails the
// strict parse rather than being dropped.
func TestHostHooks_StrictParseRefusesUnknownMembers(t *testing.T) {
	for _, body := range []string{
		`{"name":"@x/y","hostHooks":{"requires":{"contract":"missions"},"command":["a","b"],"hosts":{},"probe":["a"]}}`,
		`{"name":"@x/y","hostHooks":{"requires":{"contract":"missions"},"command":["a","b"],"hosts":{"codex":[{"event":"Stop","timeoutSeconds":1,"probe":true}]}}}`,
	} {
		if _, diags := ParseManifest([]byte(body)); !hasCode(diags, "parse-error") {
			t.Errorf("strict parse accepted %s: %v", body, diags)
		}
	}
}

// The command path may descend into nested subcommands of the interactive one,
// and every word must exist.
func TestHostHooks_CommandPathResolvesNestedSubcommands(t *testing.T) {
	m := loadFixtureManifest(t, hostHooksFixture)
	hook := m.CommandGroups["agent"].Subcommands["hook"]
	hook.Subcommands = map[string]SubcommandDefinition{"run": {Description: "Run."}}
	m.CommandGroups["agent"].Subcommands["hook"] = hook

	m.HostHooks.Command = []string{"agent", "hook", "run"}
	if diags := ValidateHostHooks(m); diag.HasErrors(diags) {
		t.Fatalf("a nested path under an interactive subcommand must validate: %v", diags)
	}
	for _, path := range [][]string{
		{"agent"},
		{"agent", "hook", "missing"},
		{"other", "hook"},
		{"agent", "Hook"},
	} {
		m.HostHooks.Command = path
		if !hasCode(ValidateHostHooks(m), "invalid-host-hooks-command") {
			t.Errorf("command %v validated", path)
		}
	}
}

// The closed lists the writer reads are the ones the validator enforces.
func TestHostHooks_ClosedLists(t *testing.T) {
	if got := HostHookHosts(); !reflect.DeepEqual(got, []string{HostClaudeCode, HostCodex}) {
		t.Fatalf("hosts = %v", got)
	}
	if got := HostHookEvents(HostClaudeCode); !reflect.DeepEqual(got, []string{"SessionStart", "PostToolUse", "Stop", "StopFailure"}) {
		t.Fatalf("claude-code events = %v", got)
	}
	if got := HostHookEvents(HostCodex); !reflect.DeepEqual(got, []string{"SessionStart", "PostToolUse", "Stop"}) {
		t.Fatalf("codex events = %v", got)
	}
	if HostHookEvents("cursor") != nil {
		t.Fatal("an unknown host has events")
	}
	events := HostHookEvents(HostCodex)
	events[0] = "changed"
	if HostHookEvents(HostCodex)[0] != "SessionStart" {
		t.Fatal("HostHookEvents hands out the shared list")
	}
}
