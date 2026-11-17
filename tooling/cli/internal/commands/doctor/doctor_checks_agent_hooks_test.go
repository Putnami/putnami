package doctor

import (
	"strings"
	"testing"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

const (
	agentHooksFeature     = "cli/diagnostics-results"
	agentHooksRequirement = "doctor-names-unreachable-agent-hooks"
)

func withAgentHookGaps(gaps ...shared.HostHookGap) workstationProbe {
	probe := fakeWorkstation()
	probe.agentHookGaps = func() []shared.HostHookGap { return gaps }
	return probe
}

// A host whose user hook file lacks the workspace's hooks is one advisory,
// workspace-scoped finding that names the file, the events and the fix.
func TestCheckWorkstation_AHostWithoutUserHooksIsReported(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, agentHooksRequirement, "doctor-reports-a-host-without-user-hooks")
	probe := withAgentHookGaps(shared.HostHookGap{
		Kind: shared.HostHookGapFile, Host: "claude-code", File: "/elsewhere/.claude/settings.json",
		Events: []string{"SessionStart", "Stop"}, Reason: "the file does not exist",
	})
	findings := checkWorkstation(doctor.ProfileProduction, probe)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want one", findings)
	}
	finding := findings[0]
	if finding.Code != doctor.CheckAgentHooksUnreachable || finding.Severity != doctor.SeverityWarning || finding.Project != doctorWorkspaceScope {
		t.Fatalf("finding = %#v", finding)
	}
	if finding.Evidence[0].Path != "/elsewhere/.claude/settings.json" || finding.Evidence[0].Field != "SessionStart,Stop" {
		t.Fatalf("evidence = %#v", finding.Evidence)
	}
	if !strings.Contains(finding.Message, "Claude Code would run no Putnami hook") || !strings.Contains(finding.Message, "the file does not exist") {
		t.Fatalf("message = %q", finding.Message)
	}
	if !strings.Contains(finding.Remediation, "putnami install") || !strings.Contains(finding.Remediation, "setup script") || !strings.Contains(finding.Remediation, "/hooks") {
		t.Fatalf("remediation = %q", finding.Remediation)
	}
}

// putnami off the PATH is reported once, for every host, and so is a host
// whose file cannot be located.
func TestCheckWorkstation_PutnamiOffThePathIsReported(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, agentHooksRequirement, "doctor-reports-putnami-off-the-path")
	probe := withAgentHookGaps(
		shared.HostHookGap{Kind: shared.HostHookGapFile, Host: "codex", Reason: "CODEX_HOME is not an absolute path"},
		shared.HostHookGap{Kind: shared.HostHookGapPath, Reason: "putnami is not on the PATH, so the user's hooks cannot start it"},
		shared.HostHookGap{Kind: shared.HostHookGapRepository, Reason: "no putnami command recorded this repository"},
	)
	findings := checkWorkstation(doctor.ProfileDev, probe)
	if len(findings) != 3 {
		t.Fatalf("findings = %#v, want three", findings)
	}
	if findings[2].Evidence[0].Path != "." || !strings.Contains(findings[2].Message, "do not serve this repository") {
		t.Fatalf("repository finding = %#v", findings[2])
	}
	if findings[0].Evidence[0].Path != "Codex" || !strings.Contains(findings[0].Message, "CODEX_HOME") {
		t.Fatalf("host finding = %#v", findings[0])
	}
	if findings[1].Evidence[0].Path != "PATH" || findings[1].Evidence[0].Field != "putnami" || findings[1].Severity != doctor.SeverityInfo {
		t.Fatalf("PATH finding = %#v", findings[1])
	}
}

// Hooks that reach the host leave nothing to report.
func TestCheckWorkstation_HooksThatReachTheHostAreQuiet(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, agentHooksRequirement, "doctor-is-quiet-when-hooks-reach-the-host")
	if findings := checkWorkstation(doctor.ProfileProduction, withAgentHookGaps()); len(findings) != 0 {
		t.Fatalf("findings = %#v, want none", findings)
	}
}

func TestDisplayHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := displayHomePath(home + "/.claude/settings.json"); got != "~/.claude/settings.json" {
		t.Fatalf("displayHomePath = %q", got)
	}
	if got := displayHomePath("/etc/x.json"); got != "/etc/x.json" {
		t.Fatalf("displayHomePath = %q", got)
	}
}
