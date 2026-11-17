package agentctx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// withPutnamiOnPath answers the PATH lookup of putnami.
func withPutnamiOnPath(t *testing.T, found bool) {
	t.Helper()
	lookPath := hostHookLookPath
	hostHookLookPath = func(name string) (string, error) {
		if found {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { hostHookLookPath = lookPath })
}

// Before an install, each host's user file lacks every event the workspace
// asks for; after it, nothing is missing.
func TestUserHostHookGapsFollowTheUserFiles(t *testing.T) {
	home := userHostHookHome(t, nil)
	withPutnamiOnPath(t, true)
	ws := newHostHooksWorkspace(t, true, true)

	gaps := UserHostHookGaps(ws.root, ws.config())
	if len(gaps) != 3 || gaps[2].Kind != shared.HostHookGapRepository {
		t.Fatalf("gaps = %+v, want one per host and the unrecorded repository", gaps)
	}
	if gaps[0].File != filepath.Join(home, ".claude", "settings.json") || !slices.Equal(gaps[0].Events, []string{"SessionStart", "PostToolUse", "Stop"}) ||
		gaps[0].Reason != "the file does not exist" {
		t.Fatalf("Claude Code gap = %+v", gaps[0])
	}
	if !slices.Equal(gaps[1].Events, []string{"PostToolUse", "Stop"}) {
		t.Fatalf("Codex gap = %+v", gaps[1])
	}

	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	if gaps := UserHostHookGaps(ws.root, ws.config()); len(gaps) != 0 {
		t.Fatalf("gaps after the install = %+v", gaps)
	}

	// A person who deletes one event's hook loses that event only.
	claude := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `"SessionStart"`, `"SessionStartOff"`, 1)
	if err := os.WriteFile(claude, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if gaps := UserHostHookGaps(ws.root, ws.config()); len(gaps) != 1 || !slices.Equal(gaps[0].Events, []string{"SessionStart"}) {
		t.Fatalf("gaps = %+v", gaps)
	}
}

// putnami off the PATH is a gap of its own; a workspace with no active hooks,
// and a run in CI, have none.
func TestUserHostHookGapsPathAndQuietCases(t *testing.T) {
	userHostHookHome(t, nil)
	withPutnamiOnPath(t, false)
	ws := newHostHooksWorkspace(t, true, true)
	if _, warn := ws.register(t); warn != "" {
		t.Fatal(warn)
	}
	gaps := UserHostHookGaps(ws.root, ws.config())
	if len(gaps) != 1 || gaps[0].Kind != shared.HostHookGapPath || !strings.Contains(gaps[0].Reason, "PATH") {
		t.Fatalf("gaps = %+v", gaps)
	}

	inactive := newHostHooksWorkspace(t, false, true)
	if gaps := UserHostHookGaps(inactive.root, inactive.config()); len(gaps) != 0 {
		t.Fatalf("an inactive workspace has gaps %+v", gaps)
	}
	if gaps := UserHostHookGaps(ws.root, nil); len(gaps) != 0 {
		t.Fatalf("no config has gaps %+v", gaps)
	}

	userHostHookHome(t, map[string]string{"CI": "true"})
	if gaps := UserHostHookGaps(ws.root, ws.config()); len(gaps) != 0 {
		t.Fatalf("a CI run has gaps %+v", gaps)
	}
}

// A user file that is not a JSON object, or a relative config directory,
// leaves every event missing with the reason.
func TestUserHostHookGapsNameAnUnusableFile(t *testing.T) {
	home := userHostHookHome(t, map[string]string{"CODEX_HOME": "relative"})
	withPutnamiOnPath(t, true)
	ws := newHostHooksWorkspace(t, true, true)
	claude := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	gaps := UserHostHookGaps(ws.root, ws.config())
	if len(gaps) != 3 || gaps[0].Reason != "the file is not a JSON object" || len(gaps[0].Events) != 3 {
		t.Fatalf("gaps = %+v", gaps)
	}
	if gaps[1].File != "" || !strings.Contains(gaps[1].Reason, "CODEX_HOME") {
		t.Fatalf("Codex gap = %+v", gaps[1])
	}
}
