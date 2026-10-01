package jobs

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Python has no Putnami framework of its own: this extension gains typed
// readiness purely from the shared forwarder in the extension SDK, which is the
// point of putting the recognition there instead of in each serve wrapper. Any
// Python app that writes the reserved marker on a structured log line therefore
// reports readiness with no extension change at all.

// captureServeStdout runs fn with os.Stdout redirected, returning what the
// emitter wrote. The SDK emitter binds os.Stdout at emit time.
func captureServeStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = orig

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

const pythonListeningRecord = `{"severity":"INFO","message":"listening http://localhost:3000",` +
	`"putnami.ready":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":3000}]}}`

// readyHelperEnv makes the test binary a served program that writes
// pythonListeningRecord and exits 0, on every host: no shell is needed.
const readyHelperEnv = "PUTNAMI_PYTHON_SERVE_READY_HELPER"

// TestReadyHelperProcess is not a test: it is the served program of the
// readiness tests, and it returns at once unless a test started it as one.
func TestReadyHelperProcess(t *testing.T) {
	if os.Getenv(readyHelperEnv) != "1" {
		return
	}
	_, _ = os.Stdout.WriteString(pythonListeningRecord + "\n")
	os.Exit(0)
}

// readyHelper is the command line and environment that run the readiness
// helper through spawnServer.
func readyHelper() ([]string, []string) {
	return []string{os.Args[0], "-test.run=^TestReadyHelperProcess$"}, append(os.Environ(), readyHelperEnv+"=1")
}

func countReadyEvents(t *testing.T, out string) int {
	t.Helper()
	events, err := runtimeproto.DecodeAll(strings.NewReader(out))
	if err != nil {
		t.Fatalf("serve output is not a runtime event stream: %v\n%s", err, out)
	}
	ready := 0
	for _, event := range events {
		if event.Type == runtimeproto.EventReady {
			ready++
		}
	}
	return ready
}

// TestSpawnServer_ReemitsReadinessAcrossRestarts covers the watch-mode restart
// loop in Serve: a source change stops the process and calls spawnServer again
// with the SAME emitter, so readiness must be announced once per iteration.
func TestSpawnServer_ReemitsReadinessAcrossRestarts(t *testing.T) {
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion2)
	args, env := readyHelper()
	dir := t.TempDir()

	out := captureServeStdout(t, func() {
		for iteration := 0; iteration < 2; iteration++ {
			proc, err := spawnServer(emit, args, dir, env)
			if err != nil {
				t.Fatalf("iteration %d: %v", iteration, err)
			}
			proc.wait()
		}
	})

	if got := countReadyEvents(t, out); got != 2 {
		t.Fatalf("2 serve iterations emitted %d ready events, want 2:\n%s", got, out)
	}
}

// TestSpawnServer_NoReadinessLeakAtV1 is the no-leak guard: a CLI that never
// advertised v2 acceptance sees the stream it always did, with the marker
// consumed rather than echoed into the log context.
func TestSpawnServer_NoReadinessLeakAtV1(t *testing.T) {
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion)
	args, env := readyHelper()

	out := captureServeStdout(t, func() {
		proc, err := spawnServer(emit, args, t.TempDir(), env)
		if err != nil {
			t.Fatal(err)
		}
		proc.wait()
	})

	if got := countReadyEvents(t, out); got != 0 {
		t.Fatalf("a v1 stream carried %d ready events, want 0:\n%s", got, out)
	}
	if strings.Contains(out, runtimeproto.ReadyLogKey) {
		t.Errorf("the reserved marker leaked into the forwarded log context:\n%s", out)
	}
}
