//go:build !windows

package sessionreporter

import (
	"context"
	"os"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// The detached replay is this executable's `sessions replay` of the exact
// session, in the workspace root, in a session of its own, with the captured
// selector and token added back to its environment.
func TestTheDetachedReplayIsTheCLIsOwnSessionsReplay(t *testing.T) {
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
	t.Setenv(protocolcli.LogReporterEnv, "")
	t.Setenv(protocolcli.LogReporterTokenEnv, "")
	ctx := Capture(context.Background())
	root := t.TempDir()
	cmd, err := replayCommand(ctx, root, "20261127-120000-abcdef")
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if cmd.Path != self || !slices.Equal(cmd.Args[1:], []string{"sessions", "replay", "--session", "20261127-120000-abcdef"}) || cmd.Dir != root {
		t.Fatalf("replay command %s %v in %s", cmd.Path, cmd.Args, cmd.Dir)
	}
	for _, entry := range []string{protocolcli.SessionReporterEnv + "=@test/reporter", protocolcli.SessionReporterTokenEnv + "=reporter-test-secret"} {
		if !slices.Contains(cmd.Env, entry) {
			t.Errorf("the replay environment lacks %s", entry)
		}
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the replay does not start in a session of its own")
	}
}
