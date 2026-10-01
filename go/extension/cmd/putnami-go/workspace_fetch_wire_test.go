package main

import (
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// fetchWireBearer is the bearer the wire tests hand workspace-fetch. No event
// and no line of standard error may carry it.
const fetchWireBearer = "pkt_fetch-wire-bearer-3c9e1a7f"

// fetchWireEnv is the environment of a workspace-fetch the engine starts on a
// hosted run, with no go command on PATH: the empty workspace the tests fetch
// needs none, and none may run.
func fetchWireEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	origin := "file:///nonexistent-proxy"
	return jobtest.Env(t, origin, jobtest.CacheRoot(t), append([]string{
		"PATH=" + t.TempDir(),
		extensionproto.OfflineDependenciesEnv + "=1",
		runtimeproto.AdvertisedVersionEnv(runtimeproto.ProtocolVersion2),
	}, extra...)...)
}

// A descriptor variable the job cannot read fails workspace-fetch before it
// runs anything: a hosted run whose credential hand-off broke must not fetch
// with whatever other credential the machine holds.
func TestWorkspaceFetchWire_FailsClosedOnAnUnreadableDescriptor(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"workspace-fetch-fails-closed-on-an-unreadable-credential")
	ws := jobtest.RealTempDir(t)
	context := writeLifecycleContext(t, ws, "workspace-fetch", `{}`)
	env := fetchWireEnv(t, extensionproto.JobCredentialFDEnv+"=not-a-descriptor")

	events, stderr, code := runPutnamiGo(t, env, "workspace-fetch", "--putnamiContext", context)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr:\n%s", code, stderr)
	}
	requireJobStream(t, events, "workspace-fetch", runtimeproto.ProtocolVersion2, runtimeproto.ResultFailed)
	found := false
	for _, event := range events {
		if event.Type == runtimeproto.EventDiagnostic &&
			strings.HasPrefix(event.Message, "Cannot read the registry credential the engine handed workspace-fetch: ") {
			found = true
		}
		if event.Type == runtimeproto.EventPhase {
			t.Errorf("a phase ran after the credential read failed: %s", eventLines(events))
		}
	}
	if !found {
		t.Errorf("no diagnostic names the unreadable credential:\n%s", eventLines(events))
	}
}
