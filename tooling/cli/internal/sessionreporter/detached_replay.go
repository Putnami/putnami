package sessionreporter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"go.putnami.dev/sdk/extension/procstat"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// Detaches reports whether a run that may return before its reporting
// delivered does (ADR 0115): it started a reporter, its session is
// finalized, so a replay accepts it, and it need not deliver before it
// exits. A hosted run hands its run credential to no later process, and a CI
// machine may be reclaimed once the run exits: both drain. Setting CI keeps
// any run's delivery in the foreground.
func Detaches(started, finalized bool) bool {
	return started && finalized && !runcredential.Hosted() && !inCI()
}

// ciMarkers are the variables a CI system sets on its jobs: CI, which most
// set, and those of the systems that do not.
var ciMarkers = []string{"JENKINS_URL", "TF_BUILD", "TEAMCITY_VERSION"}

// inCI reports whether the run is a CI job, whose machine may be reclaimed
// once it exits.
func inCI() bool {
	if cachekey.InCI() {
		return true
	}
	for _, name := range ciMarkers {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// errHostedReplay refuses a detached replay on a hosted run, which drains
// its reporting instead (Detaches).
var errHostedReplay = errors.New("a hosted run delivers its reporting before it exits")

// StartDetachedReplay starts `putnami sessions replay --session <id>` with
// this CLI's executable, outside every process tree of the run and in a
// session of its own, so that the run's end, its terminal and its signals
// leave it delivering. It writes nothing to the run's streams: the
// checkpoint and subscribers.json keep the delivery state.
func StartDetachedReplay(ctx context.Context, wsRoot, sessionID string) error {
	cmd, err := replayCommand(ctx, wsRoot, sessionID)
	if err != nil {
		return err
	}
	started, err := proctree.StartDetached(cmd)
	if err != nil {
		return err
	}
	// A detached replay outlives the run: its start is counted, its CPU never.
	procstat.Started(procstat.Control, cmd.Args)
	return started.Process.Release()
}

// replayCommand is the detached replay of sessionID: it inherits the run's
// environment with the captured selectors and tokens added back, as the pin
// relaunch does (Captured). The CLI it starts captures them again.
func replayCommand(ctx context.Context, wsRoot, sessionID string) (*exec.Cmd, error) {
	if runcredential.Hosted() {
		return nil, errHostedReplay
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "sessions", "replay", "--session", sessionID)
	cmd.Dir = wsRoot
	cmd.Env = append(os.Environ(), Captured(ctx)...)
	detachSession(cmd)
	return cmd, nil
}
