package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// selectReporter selects the fixture's session reporter again: a run
// captures and removes the selector and the token from the environment.
func selectReporter(t *testing.T) {
	t.Helper()
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
}

// A terminal run returns with its reporting at a durable checkpoint and hands
// the rest to a detached replay. The first run's replay never delivers, as
// one a crash, an interruption or an offline machine stopped: the next run
// starts a replay for its own session and for the one the first left, and
// the receiver ends with every frame of both, each sequence once.
func TestADetachedRunReturnsBeforeItsUploadAndALaterRunDeliversItOnce(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "detached-delivery", "a-local-run-returns-before-its-upload-and-a-detached-replay-delivers")
	spectest.Proves(t, "cli/native-session-reporting", "detached-delivery", "an-interrupted-delivery-reaches-the-receiver-on-a-later-run-exactly-once")
	for _, name := range []string{"CI", "JENKINS_URL", "TF_BUILD", "TEAMCITY_VERSION"} {
		t.Setenv(name, "")
	}
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
	req.DetachedReporting = true

	var lost []string
	req.startReplay = func(_ context.Context, wsRoot, sessionID string) error {
		if wsRoot != root {
			t.Errorf("replay of %s started in %s, want the workspace root", sessionID, wsRoot)
		}
		lost = append(lost, sessionID)
		return nil
	}
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("first run: exit=%d err=%v", result.ExitCode, err)
	}
	first := workspace_state.NewSessionStore(root).LatestID()
	if !slices.Equal(lost, []string{first}) {
		t.Fatalf("first run started replays of %v, want only its session %s", lost, first)
	}
	// The run returned before its delivery ended: session.json leaves only
	// once the graph ended, and the run stopped its worker instead.
	if complete, _ := readCheckpoint(t, root, first, "reporting.json"); complete {
		t.Fatal("the first run drained its reporting before it returned")
	}

	// A session killed before it was finalized is never owed: no replay
	// accepts it. It sorts after the first session, as a later one would.
	killed := first[:len(first)-6] + "zzzzzz"
	killedDir := filepath.Join(root, ".putnami", "sessions", killed)
	if err := os.MkdirAll(killedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(killedDir, "reporting.json"), []byte(`{"version":2,"provider":"@test/reporter","sessionId":"`+killed+`","complete":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(killedDir, "reporting.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	selectReporter(t)
	var replayed []string
	req.startReplay = func(ctx context.Context, wsRoot, sessionID string) error {
		replayed = append(replayed, sessionID)
		return New().ReplaySession(ctx, wsRoot, req.Config, sessionID)
	}
	stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("second run: exit=%d err=%v", result.ExitCode, err)
	}
	if want := "putnami: 1 earlier session(s) not delivered by their reporters; replaying them in the background (putnami sessions inspect " + first + ")"; !strings.Contains(stderr, want) {
		t.Fatalf("second run output lacks %q:\n%s", want, stderr)
	}
	second := workspace_state.NewSessionStore(root).LatestID()
	if second == first || !slices.Equal(replayed, []string{second, first}) {
		t.Fatalf("second run started replays of %v, want its session %s, then %s", replayed, second, first)
	}
	for _, id := range []string{first, second} {
		if complete, pending := readCheckpoint(t, root, id, "reporting.json"); !complete || pending != nil {
			t.Fatalf("session %s: delivery incomplete after the later run", id)
		}
		receivedOnce(t, root, id)
	}
	if runs := fixtureproc.Runs(t, filepath.Join(root, "executions")); len(runs) != 2 {
		t.Fatalf("job executions = %d, want one per run", len(runs))
	}
}

// receivedOnce fails t unless the receiver's trace holds every artifact the
// session reporter sends of sessionID, byte for byte, as one contiguous run
// of sequences that ends with its final marker, where a resent sequence
// carries the same bytes: the receiver stores each frame once.
func receivedOnce(t *testing.T, root, sessionID string) {
	t.Helper()
	frames := map[string]map[int64]*protocolcli.SessionReportingChunk{}
	for _, line := range traceLines(t, filepath.Join(root, "trace.jsonl")) {
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.SessionID != sessionID {
			continue
		}
		if frames[chunk.Artifact] == nil {
			frames[chunk.Artifact] = map[int64]*protocolcli.SessionReportingChunk{}
		}
		if seen := frames[chunk.Artifact][chunk.Sequence]; seen != nil {
			if seen.Offset != chunk.Offset || seen.Final != chunk.Final || !bytes.Equal(seen.Data, chunk.Data) {
				t.Fatalf("%s %s sequence %d was resent with other bytes", sessionID, chunk.Artifact, chunk.Sequence)
			}
			continue
		}
		frames[chunk.Artifact][chunk.Sequence] = chunk
	}
	for _, artifact := range []string{"plan.json", "events.jsonl", "session.json"} {
		want, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, artifact))
		if err != nil {
			t.Fatal(err)
		}
		var got []byte
		final := false
		for sequence := int64(0); sequence < int64(len(frames[artifact])); sequence++ {
			chunk := frames[artifact][sequence]
			if chunk == nil || final || chunk.Offset != int64(len(got)) {
				t.Fatalf("%s %s: sequence %d missing, out of place or after the final marker", sessionID, artifact, sequence)
			}
			got = append(got, chunk.Data...)
			final = chunk.Final
		}
		if !final || !bytes.Equal(got, want) {
			t.Fatalf("%s %s: the receiver holds %d bytes (final %v), want the %d recorded", sessionID, artifact, len(got), final, len(want))
		}
	}
}

// A run in CI drains before it returns, even from the terminal adapter: its
// machine may be reclaimed once it exits.
func TestACIRunDrainsItsReportingBeforeItReturns(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "detached-delivery", "hosted-and-ci-runs-drain-before-they-return")
	t.Setenv("CI", "true")
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
	req.DetachedReporting = true
	req.startReplay = func(_ context.Context, _, sessionID string) error {
		t.Errorf("a CI run started a detached replay of %s", sessionID)
		return nil
	}
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("engine exit=%d err=%v", result.ExitCode, err)
	}
	verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
}
