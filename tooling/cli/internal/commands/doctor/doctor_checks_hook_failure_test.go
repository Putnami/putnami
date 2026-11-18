package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/dirlink"
)

const hookFailureRequirement = "doctor-names-the-last-agent-hook-failure"

// hookLine encodes one hook log line as the hook writes it, so a path with a
// backslash stays a valid JSON string.
func hookLine(t *testing.T, fields map[string]string) string {
	t.Helper()
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// writeHookLog writes lines to a hook log under a fresh Putnami home and
// answers its path.
func writeHookLog(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := hookLogPath("", func(key string) string {
		if key == "PUTNAMI_HOME" {
			return filepath.Join(t.TempDir(), "home")
		}
		return ""
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckWorkstation_TheLastHookFailureOfTheWorkspaceIsReported(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, hookFailureRequirement, "doctor-reports-the-last-hook-failure-of-the-workspace")
	root := filepath.Join(t.TempDir(), "ws")
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	path := writeHookLog(t, "hook.log",
		hookLine(t, map[string]string{"at": "2026-11-18T09:00:00Z", "root": root, "engine": "claude-code", "event": "stop", "error": "an earlier failure"}),
		`not json`,
		hookLine(t, map[string]string{"at": "2026-11-18T10:00:00Z", "root": root + string(filepath.Separator), "engine": "claude-code", "event": "stop", "error": "missions.sync answered denied (auth.unavailable): run putnami cloud login"}),
		hookLine(t, map[string]string{"at": "2026-11-18T11:00:00Z", "root": filepath.Join(t.TempDir(), "another"), "engine": "codex", "event": "stop", "error": "another workspace's failure"}),
		hookLine(t, map[string]string{"at": "2026-11-18T11:30:00Z", "engine": "codex", "event": "stop", "error": "a line without a root"}),
	)
	failure, ok := readLastHookFailure(path, root, now)
	if !ok || failure.Error != "missions.sync answered denied (auth.unavailable): run putnami cloud login" || failure.Path != path {
		t.Fatalf("failure = %+v, %v; want the last one of the workspace", failure, ok)
	}

	probe := fakeWorkstation()
	probe.lastHookFailure = func() (hookFailure, bool) { return failure, true }
	findings := checkWorkstation(doctor.ProfileProduction, probe)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want one", findings)
	}
	finding := findings[0]
	if finding.Code != doctor.CheckAgentHookFailed || finding.Severity != doctor.SeverityWarning || finding.Project != doctorWorkspaceScope {
		t.Fatalf("finding = %#v", finding)
	}
	for _, want := range []string{"2026-11-18T10:00:00Z", "stop", "claude-code", "auth.unavailable"} {
		if !strings.Contains(finding.Message, want) {
			t.Fatalf("message %q does not name %q", finding.Message, want)
		}
	}
	if !strings.Contains(finding.Remediation, "putnami cloud login") {
		t.Fatalf("remediation = %q", finding.Remediation)
	}
}

// A hook that ran in the workspace through a directory link logs the linked
// path; doctor, which reads the resolved root, still reports its failure, in
// either direction.
func TestCheckWorkstation_AHookFailureThroughADirectoryLinkIsReported(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, hookFailureRequirement, "doctor-reports-the-last-hook-failure-of-the-workspace")
	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(root, link); err != nil {
		t.Fatalf("link the workspace: %v", err)
	}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for logged, workspace := range map[string]string{link: root, root: link} {
		path := writeHookLog(t, "hook.log", hookLine(t, map[string]string{"at": "2026-11-18T11:00:00Z", "root": logged, "engine": "claude-code", "event": "stop", "error": "linked"}))
		if failure, ok := readLastHookFailure(path, workspace, now); !ok || failure.Error != "linked" {
			t.Errorf("a failure logged under %s, read for %s: %+v, %v", logged, workspace, failure, ok)
		}
	}
}

func TestCheckWorkstation_NoRecentHookFailureOfTheWorkspaceIsQuiet(t *testing.T) {
	spectest.Proves(t, agentHooksFeature, hookFailureRequirement, "doctor-is-quiet-without-a-recent-hook-failure-of-the-workspace")
	root := filepath.Join(t.TempDir(), "ws")
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for name, path := range map[string]string{
		"older than a day":  writeHookLog(t, "hook.log", hookLine(t, map[string]string{"at": "2026-11-17T11:59:00Z", "root": root, "engine": "claude-code", "event": "stop", "error": "old"})),
		"another workspace": writeHookLog(t, "hook.log", hookLine(t, map[string]string{"at": "2026-11-18T11:00:00Z", "root": filepath.Join(t.TempDir(), "elsewhere"), "engine": "claude-code", "event": "stop", "error": "x"})),
		"no root":           writeHookLog(t, "hook.log", hookLine(t, map[string]string{"at": "2026-11-18T11:00:00Z", "engine": "claude-code", "event": "stop", "error": "x"})),
		"no log":            filepath.Join(t.TempDir(), "hook.log"),
	} {
		if failure, ok := readLastHookFailure(path, root, now); ok {
			t.Errorf("%s: reported %+v", name, failure)
		}
	}

	rotated := writeHookLog(t, "hook.log.1", hookLine(t, map[string]string{"at": "2026-11-18T11:00:00Z", "root": root, "engine": "codex", "event": "stop", "error": "rotated"}))
	if failure, ok := readLastHookFailure(rotated, root, now); !ok || failure.Error != "rotated" || failure.Path != rotated+".1" {
		t.Errorf("a failure in the rotated log: %+v, %v", failure, ok)
	}

	probe := fakeWorkstation()
	probe.lastHookFailure = func() (hookFailure, bool) { return hookFailure{}, false }
	if findings := checkWorkstation(doctor.ProfileDev, probe); len(findings) != 0 {
		t.Fatalf("findings = %#v, want none", findings)
	}
}
