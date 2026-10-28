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

func installFakeAgentHost(t *testing.T, e *env, name string, rejectRemove bool) fakeAgentHost {
	t.Helper()
	return writeFakeAgentHost(t, e.home, e.pathDirs[0], filepath.Join(e.installDir, "putnami"), name, rejectRemove)
}

// writeFakeAgentHost writes a fake agent-host CLI called name into binDir. It
// records its calls and keeps its MCP definition in files under stateDir. It
// has no `mcp add`: a call to it is an unexpected command and exits 2, so a
// test sees it in the call log. `mcp remove` deletes the definition, unless
// rejectRemove makes the host refuse as a managed policy would. binary is the
// launcher a definition seeded by seedInstallerWritten names.
func writeFakeAgentHost(t *testing.T, stateDir, binDir, binary, name string, rejectRemove bool) fakeAgentHost {
	t.Helper()
	host := fakeAgentHost{
		name:      name,
		log:       filepath.Join(stateDir, name+"-calls.log"),
		state:     filepath.Join(stateDir, name+"-mcp-state.txt"),
		jsonState: filepath.Join(stateDir, name+"-mcp-state.json"),
	}
	reject := "false"
	if rejectRemove {
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
if [ "$1" = mcp ] && [ "$2" = remove ] && [ "$3" = --scope ] && [ "$4" = user ] && [ "$5" = putnami ]; then
  %s && exit 23
  rm -f %s
  exit 0
fi
exit 2
`, shellQuote(host.log), shellQuote(host.state), shellQuote(host.state), shellQuote(host.state), reject, shellQuote(host.state))
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
if [ "$1" = mcp ] && [ "$2" = remove ] && [ "$3" = putnami ]; then
  %s && exit 23
  rm -f %s %s
  exit 0
fi
exit 2
`, shellQuote(host.log), shellQuote(host.state), shellQuote(host.jsonState), shellQuote(host.jsonState), shellQuote(host.state), reject, shellQuote(host.state), shellQuote(host.jsonState))
	default:
		t.Fatalf("unsupported fake host %q", name)
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return host
}

// seedInstallerWritten stores the definition an earlier installer registered
// through `mcp add`, launching binary. connected says whether Claude Code
// reports the server as connected.
func (h fakeAgentHost) seedInstallerWritten(t *testing.T, binary string, connected bool) {
	t.Helper()
	var state, jsonState string
	switch h.name {
	case "claude":
		status := "\u2714 Connected"
		if !connected {
			status = "\u2718 Failed to connect"
		}
		state = fmt.Sprintf("putnami:\n  Status: %s\n  Command: %s\n  Args: mcp\n  Environment:\n    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}\n", status, binary)
	case "codex":
		state = fmt.Sprintf("putnami\n  enabled: true\n  transport: stdio\n  command: %s\n  args: mcp\n  cwd: -\n  env: -\n", binary)
		jsonState = fmt.Sprintf("{\n  \"name\": \"putnami\",\n  \"enabled\": true,\n  \"disabled_reason\": null,\n  \"transport\": {\n    \"type\": \"stdio\",\n    \"command\": \"%s\",\n    \"args\": [\n      \"mcp\"\n    ],\n    \"env\": null,\n    \"env_vars\": [],\n    \"cwd\": null\n  },\n  \"enabled_tools\": null,\n  \"disabled_tools\": null,\n  \"startup_timeout_sec\": null,\n  \"tool_timeout_sec\": null\n}\n", binary)
	default:
		t.Fatalf("unsupported fake host %q", h.name)
	}
	if err := os.WriteFile(h.state, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if jsonState != "" {
		if err := os.WriteFile(h.jsonState, []byte(jsonState), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readHostCalls(t *testing.T, host fakeAgentHost) string {
	t.Helper()
	data, err := os.ReadFile(host.log)
	if err != nil {
		t.Fatalf("read %s calls: %v", host.name, err)
	}
	return string(data)
}

func hostHasDefinition(host fakeAgentHost) bool {
	_, err := os.Stat(host.state)
	return err == nil
}

// The installer registers no MCP server: with neither host defining one it
// calls no `mcp add` and no `mcp remove`, and says nothing about either host.
func TestInstallRegistersNoAgentHostServer(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "host-registration-removal", "the-installer-registers-no-host-server")
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("install failed (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, host := range []fakeAgentHost{claude, codex} {
		calls := readHostCalls(t, host)
		if strings.Contains(calls, "mcp add") || strings.Contains(calls, "mcp remove") {
			t.Fatalf("the installer changed the %s configuration:\n%s", host.name, calls)
		}
		if hostHasDefinition(host) {
			t.Fatalf("the installer left a %s definition", host.name)
		}
	}
	for _, unwanted := range []string{"Connecting supported agent hosts", "MCP configuration", "Removed the Putnami MCP server"} {
		if res.contains(unwanted) {
			t.Fatalf("the installer reported %q with no registration present:\n%s", unwanted, res.output)
		}
	}
	e.assertNeverEscalated(t)
}

// A registration that is exactly the one an earlier installer wrote is removed
// from each host, and a repeat install finds nothing left to remove.
func TestInstallRemovesTheRegistrationItWrote(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "host-registration-removal", "a-registration-the-installer-wrote-is-removed")
	requireBash(t)
	for _, tc := range []struct {
		name      string
		connected bool
	}{
		{"connected", true},
		{"not connected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := defaultRegistry(t)
			e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
			claude := installFakeAgentHost(t, e, "claude", false)
			codex := installFakeAgentHost(t, e, "codex", false)
			binary := filepath.Join(e.installDir, "putnami")
			claude.seedInstallerWritten(t, binary, tc.connected)
			codex.seedInstallerWritten(t, binary, true)

			first := e.run(t, "--install-dir", e.installDir)
			if first.exitCode != 0 {
				t.Fatalf("first install failed (exit %d):\n%s", first.exitCode, first.output)
			}
			if calls := readHostCalls(t, claude); !strings.Contains(calls, "mcp remove --scope user putnami") || strings.Contains(calls, "mcp add") {
				t.Fatalf("Claude Code was not asked to remove the user-scope server:\n%s", calls)
			}
			if calls := readHostCalls(t, codex); !strings.Contains(calls, "mcp remove putnami") || strings.Contains(calls, "mcp add") {
				t.Fatalf("Codex was not asked to remove the server:\n%s", calls)
			}
			for _, host := range []fakeAgentHost{claude, codex} {
				if hostHasDefinition(host) {
					t.Fatalf("the %s definition survived the install", host.name)
				}
			}
			for _, want := range []string{
				"Removed the Putnami MCP server that an earlier install registered in Claude Code",
				"Removed the Putnami MCP server that an earlier install registered in Codex",
			} {
				if !first.contains(want) {
					t.Fatalf("missing the removal result %q:\n%s", want, first.output)
				}
			}

			second := e.run(t, "--install-dir", e.installDir)
			if second.exitCode != 0 {
				t.Fatalf("repeat install failed (exit %d):\n%s", second.exitCode, second.output)
			}
			for _, host := range []fakeAgentHost{claude, codex} {
				if got := strings.Count(readHostCalls(t, host), "mcp remove "); got != 1 {
					t.Fatalf("%s removal count = %d, want one across repeat install", host.name, got)
				}
			}
			e.assertNeverEscalated(t)
		})
	}
}

func TestInstallKeepsCustomizedHostDefinitions(t *testing.T) {
	spectest.Proves(t, "cli/context-discovery", "host-registration-removal", "a-customized-registration-is-kept")
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)
	claudeHuman := "putnami:\n  Status: \u2714 Connected\n  Command: /human/claude-putnami\n  Args: mcp --custom\n  Environment:\n    HUMAN=value\n# human-json-sentinel\n"
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
	for _, host := range []fakeAgentHost{claude, codex} {
		if calls := readHostCalls(t, host); strings.Contains(calls, "mcp remove") || strings.Contains(calls, "mcp add") {
			t.Fatalf("the installer changed the customized %s definition:\n%s", host.name, calls)
		}
	}
	if data, _ := os.ReadFile(claude.state); string(data) != claudeHuman {
		t.Fatal("existing Claude configuration bytes changed")
	}
	if data, _ := os.ReadFile(codex.state); string(data) != codexHuman {
		t.Fatal("existing Codex TOML/comment bytes changed")
	}
	for _, want := range []string{
		"Claude Code defines a putnami MCP server that Putnami did not register as is; it was kept.",
		"Codex defines a putnami MCP server that Putnami did not register as is; it was kept.",
		"claude mcp remove putnami",
		"codex mcp remove putnami",
	} {
		if !res.contains(want) {
			t.Fatalf("keep warning missing %q:\n%s", want, res.output)
		}
	}
	e.assertNeverEscalated(t)
}

func TestInstallDoesNotMistakeLookalikeHostDefinitionsForItsOwn(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", false)
	codex := installFakeAgentHost(t, e, "codex", false)
	binary := filepath.Join(e.installDir, "putnami")
	claudeLookalike := "putnami:\n  Status: \u2714 Connected\n  Command: " + binary + "-other\n  Args: mcp\n  Environment:\n    PUTNAMI_AGENT_WORKSPACE=${CLAUDE_PROJECT_DIR:-.}\n"
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
	if res.contains("Removed the Putnami MCP server") {
		t.Fatalf("lookalike definitions were reported as removed:\n%s", res.output)
	}
	for _, host := range []fakeAgentHost{claude, codex} {
		if calls := readHostCalls(t, host); strings.Contains(calls, "mcp remove") {
			t.Fatalf("a lookalike %s definition was removed:\n%s", host.name, calls)
		}
		if !hostHasDefinition(host) {
			t.Fatalf("the lookalike %s definition is gone", host.name)
		}
	}
}

func TestInstallDoesNotClaimRemovalWhenPolicyRejectsIt(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	claude := installFakeAgentHost(t, e, "claude", true)
	codex := installFakeAgentHost(t, e, "codex", true)
	binary := filepath.Join(e.installDir, "putnami")
	claude.seedInstallerWritten(t, binary, true)
	codex.seedInstallerWritten(t, binary, true)

	res := e.run(t, "--install-dir", e.installDir)
	if res.exitCode != 0 {
		t.Fatalf("host policy should not invalidate the verified CLI install (exit %d):\n%s", res.exitCode, res.output)
	}
	for _, host := range []string{"Claude Code", "Codex"} {
		if !res.contains("Could not remove the Putnami MCP server from " + host) {
			t.Fatalf("missing %s policy warning:\n%s", host, res.output)
		}
	}
	if res.contains("Removed the Putnami MCP server") {
		t.Fatalf("installer claimed a removal after the hosts refused:\n%s", res.output)
	}
	e.assertNeverEscalated(t)
}

func TestInstallSkipsAgentHostsWhenDeclined(t *testing.T) {
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
			binary := filepath.Join(e.installDir, "putnami")
			claude.seedInstallerWritten(t, binary, true)
			codex.seedInstallerWritten(t, binary, true)

			res := decline.run(e)
			if res.exitCode != 0 {
				t.Fatalf("declined install failed (exit %d):\n%s", res.exitCode, res.output)
			}
			if !res.contains("Skipping agent-host cleanup") {
				t.Fatalf("installer did not report the declined step:\n%s", res.output)
			}
			for _, host := range []fakeAgentHost{claude, codex} {
				if data, err := os.ReadFile(host.log); err == nil && len(data) != 0 {
					t.Fatalf("declined install still called %s: %s", host.name, data)
				}
				if !hostHasDefinition(host) {
					t.Fatalf("declined install removed the %s definition", host.name)
				}
			}
			// The CLI itself must still be fully installed.
			if !res.contains("Integrity verified") {
				t.Fatalf("declining the host step changed the CLI install:\n%s", res.output)
			}
		})
	}
}

func TestInstallRecognizesAReformattedCodexDefinitionAsItsOwn(t *testing.T) {
	requireBash(t)
	server, _ := defaultRegistry(t)
	e := newEnv(t).set("PUTNAMI_REGISTRY_URL", server.URL)
	codex := installFakeAgentHost(t, e, "codex", false)
	binary := filepath.Join(e.installDir, "putnami")

	// A plausible future Codex release: four-space indent, reordered keys, a new
	// field, and the optional defaults omitted entirely. Semantically this is
	// still exactly the launcher the installer wrote.
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
	if !res.contains("Removed the Putnami MCP server that an earlier install registered in Codex") {
		t.Fatalf("a reformatted Putnami definition was not recognized as its own:\n%s", res.output)
	}
	if res.contains("did not register as is") {
		t.Fatalf("a reformatted Putnami definition was mistaken for a human override:\n%s", res.output)
	}
	if hostHasDefinition(codex) {
		t.Fatal("the recognized definition was not removed")
	}
}
