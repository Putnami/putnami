package installscript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

type fakeAgentHost struct {
	name      string
	log       string
	state     string
	jsonState string
}

func installFakeAgentHost(t *testing.T, e *env, name string, rejectAdd bool) fakeAgentHost {
	t.Helper()
	return writeFakeAgentHost(t, e.home, e.pathDirs[0], filepath.Join(e.installDir, "putnami"), name, rejectAdd)
}

// writeFakeAgentHost writes a fake agent-host CLI called name into binDir. It
// records its calls and keeps its MCP definition in files under stateDir, and
// the definition an `mcp add` stores launches binary.
func writeFakeAgentHost(t *testing.T, stateDir, binDir, binary, name string, rejectAdd bool) fakeAgentHost {
	t.Helper()
	host := fakeAgentHost{
		name:      name,
		log:       filepath.Join(stateDir, name+"-calls.log"),
		state:     filepath.Join(stateDir, name+"-mcp-state.txt"),
		jsonState: filepath.Join(stateDir, name+"-mcp-state.json"),
	}
	reject := "false"
	if rejectAdd {
		reject = "true"
	}

	var script string
	switch name {
	case "claude":
		script = fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
if [ "$1" = mcp ] && [ "$2" = get ] && [ "$3" = putnami ]; then
  [ -f %s ] || exit 1
  cat %s
  grep -Eq '^  Status: .*Connected$' %s || exit 17
  exit 0
fi
if [ "$1" = mcp ] && [ "$2" = add ]; then
  %s && exit 23
  status='✔ Connected'
  [ "${PUTNAMI_TEST_CLAUDE_DISCONNECTED:-}" = true ] && status='✘ Failed to connect'
  printf 'putnami:\n  Status: %%s\n  Command: %%s\n  Args: mcp\n  Environment:\n    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}\n' "$status" %s > %s
  exit 0
fi
exit 2
`, shellQuote(host.log), shellQuote(host.state), shellQuote(host.state), shellQuote(host.state), reject, shellQuote(binary), shellQuote(host.state))
	case "codex":
		script = fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
if [ "$1" = mcp ] && [ "$2" = get ] && [ "$3" = putnami ]; then
  [ -f %s ] || exit 1
  if [ "${4:-}" = --json ]; then
    [ -f %s ] || exit 1
    cat %s
    exit 0
  fi
  cat %s
  exit 0
fi
if [ "$1" = mcp ] && [ "$2" = add ]; then
  %s && exit 23
  printf 'putnami\n  enabled: true\n  transport: stdio\n  command: %%s\n  args: mcp\n  cwd: -\n  env: -\n' %s > %s
  printf '{\n  "name": "putnami",\n  "enabled": true,\n  "disabled_reason": null,\n  "transport": {\n    "type": "stdio",\n    "command": "%%s",\n    "args": [\n      "mcp"\n    ],\n    "env": null,\n    "env_vars": [],\n    "cwd": null\n  },\n  "enabled_tools": null,\n  "disabled_tools": null,\n  "startup_timeout_sec": null,\n  "tool_timeout_sec": null\n}\n' %s > %s
  exit 0
fi
exit 2
`, shellQuote(host.log), shellQuote(host.state), shellQuote(host.jsonState), shellQuote(host.jsonState), shellQuote(host.state), reject, shellQuote(binary), shellQuote(host.state), shellQuote(binary), shellQuote(host.jsonState))
	default:
		t.Fatalf("unsupported fake host %q", name)
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return host
}

func readHostCalls(t *testing.T, host fakeAgentHost) string {
	t.Helper()
	data, err := os.ReadFile(host.log)
	if err != nil {
		t.Fatalf("read %s calls: %v", host.name, err)
	}
	return string(data)
}

func TestInstallRegistersClaudeAndCodexWithStableSessionLaunchers(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "installer-registers-supported-hosts-with-a-stable-session-launcher")
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)

	first := e.run(t, "--install-dir", e.installDir)
	if first.exitCode != 0 {
		t.Fatalf("first install failed (exit %d):\n%s", first.exitCode, first.output)
	}
	binary := filepath.Join(e.installDir, "putnami")
	wantClaude := "mcp add --scope user --env PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.} --transport stdio putnami -- " + binary + " mcp"
	if calls := readHostCalls(t, claude); !strings.Contains(calls, wantClaude) {
		t.Fatalf("Claude registration did not carry the active-session root and stable binary:\n%s", calls)
	}
	wantCodex := "mcp add putnami -- " + binary + " mcp"
	if calls := readHostCalls(t, codex); !strings.Contains(calls, wantCodex) {
		t.Fatalf("Codex registration did not use the stable binary without a fixed cwd:\n%s", calls)
	}
	for _, want := range []string{
		"Claude Code MCP configuration and connection verified for new sessions",
		"Codex MCP configuration verified for new sessions",
	} {
		if !first.contains(want) {
			t.Fatalf("installer announced readiness without the expected verified host result %q:\n%s", want, first.output)
		}
	}

	second := e.run(t, "--install-dir", e.installDir)
	if second.exitCode != 0 {
		t.Fatalf("repeat install failed (exit %d):\n%s", second.exitCode, second.output)
	}
	if got := strings.Count(readHostCalls(t, claude), "mcp add "); got != 1 {
		t.Fatalf("Claude registration count = %d, want one across repeat install", got)
	}
	if got := strings.Count(readHostCalls(t, codex), "mcp add "); got != 1 {
		t.Fatalf("Codex registration count = %d, want one across repeat install", got)
	}
	e.assertNeverEscalated(t)
}

func TestInstallVerifiesExactClaudeConfigurationBeforeAWorkspaceCanConnect(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).
		set("PUTNAMI_REGISTRY_URL", server.URL).
		set("PUTNAMI_TEST_CLAUDE_DISCONNECTED", "true")
	claude := installFakeAgentHost(t, e, "claude", false)

	first := e.run(t, "--install-dir", e.installDir)
	if first.exitCode != 0 {
		t.Fatalf("install outside a workspace failed (exit %d):\n%s", first.exitCode, first.output)
	}
	for _, want := range []string{
		"Claude Code MCP configuration verified for new sessions",
		"MCP connection not yet verified; Claude Code will check it when a Putnami workspace opens.",
	} {
		if !first.contains(want) {
			t.Fatalf("missing deferred connection result %q:\n%s", want, first.output)
		}
	}
	for _, misleading := range []string{
		"did not report the expected Putnami launcher",
		"Inspect the active definition with: claude mcp get putnami",
	} {
		if first.contains(misleading) {
			t.Fatalf("exact disconnected configuration produced misleading diagnostic %q:\n%s", misleading, first.output)
		}
	}

	second := e.run(t, "--install-dir", e.installDir)
	if second.exitCode != 0 {
		t.Fatalf("repeat install outside a workspace failed (exit %d):\n%s", second.exitCode, second.output)
	}
	if got := strings.Count(readHostCalls(t, claude), "mcp add "); got != 1 {
		t.Fatalf("Claude registration count = %d, want one across disconnected repeat install", got)
	}
	e.assertNeverEscalated(t)
}

func TestInstallPreservesExistingHumanAgentHostDefinitions(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "existing-host-config-is-preserved")
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)
	claudeHuman := "putnami:\n  Status: ✔ Connected\n  Command: /human/claude-putnami\n  Args: mcp --custom\n  Environment:\n    HUMAN=value\n# human-json-sentinel\n"
	codexHuman := "putnami\n  enabled: true\n  transport: stdio\n  command: /human/codex-putnami\n  args: mcp --custom\n  cwd: /human/workspace\n  env: -\n# human-toml-comment\n"
	if err := os.WriteFile(claude.state, []byte(claudeHuman), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex.state, []byte(codexHuman), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if calls := readHostCalls(t, claude); strings.Contains(calls, "mcp add ") {
		t.Fatalf("installer replaced the existing Claude definition:\n%s", calls)
	}
	if calls := readHostCalls(t, codex); strings.Contains(calls, "mcp add ") {
		t.Fatalf("installer replaced the existing Codex definition:\n%s", calls)
	}
	if data, _ := os.ReadFile(claude.state); string(data) != claudeHuman {
		t.Fatal("existing Claude configuration bytes changed")
	}
	if data, _ := os.ReadFile(codex.state); string(data) != codexHuman {
		t.Fatal("existing Codex TOML/comment bytes changed")
	}
	for _, want := range []string{"human configuration was preserved", "human TOML and launcher were preserved"} {
		if !res.contains(want) {
			t.Fatalf("preservation warning missing %q:\n%s", want, res.output)
		}
	}
	e.assertNeverEscalated(t)
}

func TestInstallDoesNotMistakeLookalikeHostDefinitionsForTheContract(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)
	binary := filepath.Join(e.installDir, "putnami")
	claudeLookalike := "putnami:\n  Status: ✔ Connected\n  Command: " + binary + "-other\n  Args: mcp\n  Environment:\n    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}\n"
	codexLookalike := "putnami\n  enabled: true\n  transport: stdio\n  command: " + binary + "\n  args: mcp\n  cwd: -\n  env: -\n"
	codexLookalikeJSON := "{\n  \"name\": \"putnami\",\n  \"enabled\": true,\n  \"disabled_reason\": null,\n  \"transport\": {\n    \"type\": \"stdio\",\n    \"command\": \"" + binary + "\",\n    \"args\": [\n      \"mcp\",\n      \"--workspace\",\n      \"/first/repository\"\n    ],\n    \"env\": null,\n    \"env_vars\": [],\n    \"cwd\": null\n  },\n  \"enabled_tools\": null,\n  \"disabled_tools\": null,\n  \"startup_timeout_sec\": null,\n  \"tool_timeout_sec\": null\n}\n"
	if err := os.WriteFile(claude.state, []byte(claudeLookalike), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex.state, []byte(codexLookalike), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex.jsonState, []byte(codexLookalikeJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if res.contains("MCP configuration verified") || res.contains("configuration and connection verified") {
		t.Fatalf("lookalike definitions were reported as verified:\n%s", res.output)
	}
	if calls := readHostCalls(t, claude); strings.Contains(calls, "mcp add ") {
		t.Fatalf("lookalike Claude definition was replaced:\n%s", calls)
	}
	if calls := readHostCalls(t, codex); strings.Contains(calls, "mcp add ") {
		t.Fatalf("lookalike Codex definition was replaced:\n%s", calls)
	}
}

func TestInstallDoesNotClaimHostReadinessWhenPolicyRejectsRegistration(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	installFakeAgentHost(t, e, "claude", true)
	installFakeAgentHost(t, e, "codex", true)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("host policy should not invalidate the verified CLI install (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, host := range []string{"Claude Code", "Codex"} {
		if !res.contains("Could not register Putnami with " + host) {
			t.Fatalf("missing %s policy warning:\n%s", host, res.output)
		}
		if res.contains(host+" MCP configuration verified") || res.contains(host+" MCP configuration and connection verified") {
			t.Fatalf("installer claimed %s readiness after registration refusal:\n%s", host, res.output)
		}
	}
	e.assertNeverEscalated(t)
}

func TestInstallSkipsAgentHostsWhenDeclined(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "host-registration-can-be-declined")
	requireBash(t)
	for _, decline := range []struct {
		name string
		run  func(e *env) result
	}{
		{"flag", func(e *env) result { return e.run(t, "--install-dir", e.installDir, "--no-agent-hosts") }},
		{"environment", func(e *env) result {
			e.set("PUTNAMI_NO_AGENT_HOSTS", "1")
			return e.run(t, "--install-dir", e.installDir)
		}},
	} {
		t.Run(decline.name, func(t *testing.T) {
			server, _ := defaultRegistry(t)
			e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
			claude := installFakeAgentHost(t, e, "claude", false)
			codex := installFakeAgentHost(t, e, "codex", false)

			res := decline.run(e)
			if res.exitCode != 0 {
				t.Fatalf("declined install failed (exit %d):\n%s", res.exitCode, res.output)
			}
			if !res.contains("Skipping agent-host registration") {
				t.Fatalf("installer did not report the declined registration:\n%s", res.output)
			}
			for _, host := range []fakeAgentHost{claude, codex} {
				if data, err := os.ReadFile(host.log); err == nil && len(data) != 0 {
					t.Fatalf("declined install still called %s: %s", host.name, data)
				}
			}
			// The CLI itself must still be fully installed.
			if !res.contains("Integrity verified") {
				t.Fatalf("declining host registration changed the CLI install:\n%s", res.output)
			}
		})
	}
}

func TestInstallRecognizesAReformattedCodexDefinitionAsItsOwn(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "host-session-launcher", "host-definition-recognition-survives-host-formatting")
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	codex := installFakeAgentHost(t, e, "codex", false)
	binary := filepath.Join(e.installDir, "putnami")

	// A plausible future Codex release: four-space indent, reordered keys, a new
	// field, and the optional defaults omitted entirely. Semantically this is
	// still exactly the launcher the installer writes.
	reformatted := "{\n" +
		"    \"enabled\": true,\n" +
		"    \"name\": \"putnami\",\n" +
		"    \"startup_timeout_sec\": null,\n" +
		"    \"transport\": {\n" +
		"        \"args\": [ \"mcp\" ],\n" +
		"        \"command\": \"" + binary + "\",\n" +
		"        \"cwd\": null,\n" +
		"        \"type\": \"stdio\",\n" +
		"        \"unreleased_future_field\": \"whatever\"\n" +
		"    }\n" +
		"}\n"
	if err := os.WriteFile(codex.state, []byte("putnami\n  enabled: true\n  transport: stdio\n  command: "+binary+"\n  args: mcp\n  cwd: -\n  env: -\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex.jsonState, []byte(reformatted), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	if !res.contains("Codex MCP configuration verified for new sessions") {
		t.Fatalf("a reformatted Putnami definition was not recognized as its own:\n%s", res.output)
	}
	if res.contains("human TOML and launcher were preserved") {
		t.Fatalf("a reformatted Putnami definition was mistaken for a human override:\n%s", res.output)
	}
	if calls := readHostCalls(t, codex); strings.Contains(calls, "mcp add ") {
		t.Fatalf("a recognized definition was rewritten:\n%s", calls)
	}
}
