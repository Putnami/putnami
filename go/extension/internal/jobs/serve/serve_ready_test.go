package serve

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/jsonl"
)

// captureServeStdout runs fn with os.Stdout redirected, returning what the
// emitter wrote. The SDK emitter binds os.Stdout at emit time, so redirecting
// it here captures the real serve output.
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

// listeningRecord is the JSON log line a Go framework server writes when it
// starts listening (go/framework/http/server.go): the human message plus the
// reserved readiness marker.
const listeningRecord = `{"severity":"INFO","message":"⚡️ listening http://localhost:8080","durationMs":12,` +
	`"putnami.ready":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":8080}],"durationMs":12}}`

// readyHelperEnv makes the test binary a served program that writes
// listeningRecord and exits 0, on every host: no shell is needed.
const readyHelperEnv = "PUTNAMI_GO_SERVE_READY_HELPER"

// TestReadyHelperProcess is not a test: it is the served program of the
// readiness tests, and it returns at once unless a test started it as one.
func TestReadyHelperProcess(t *testing.T) {
	if os.Getenv(readyHelperEnv) != "1" {
		return
	}
	_, _ = os.Stdout.WriteString(listeningRecord + "\n")
	os.Exit(0)
}

// readyHelper is the command line and environment that run the readiness
// helper through runCommand.
func readyHelper() ([]string, []string) {
	return []string{os.Args[0], "-test.run=^TestReadyHelperProcess$"}, append(os.Environ(), readyHelperEnv+"=1")
}

// TestRunCommand_ForwardsTypedReadinessFromWorkload is the Go extension's
// end-to-end proof: a real subprocess writes the framework's listening record,
// and the serve wrapper turns it into a typed readiness event on the job's
// event stream — without touching the human log line the substring probe still
// reads.
func TestRunCommand_ForwardsTypedReadinessFromWorkload(t *testing.T) {
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion2)
	args, env := readyHelper()

	out := captureServeStdout(t, func() {
		if code := runCommand(args, t.TempDir(), env, emit); code != 0 {
			t.Errorf("runCommand exit = %d, want 0", code)
		}
	})

	events, err := runtimeproto.DecodeAll(strings.NewReader(out))
	if err != nil {
		t.Fatalf("serve output is not a runtime event stream: %v\n%s", err, out)
	}

	var logged, ready int
	for _, event := range events {
		switch event.Type {
		case runtimeproto.EventLog:
			if strings.Contains(event.Message, "listening http://") {
				logged++
			}
		case runtimeproto.EventReady:
			ready++
			data, payloadErr := runtimeproto.ReadyPayload(event)
			if payloadErr != nil {
				t.Fatalf("readiness payload: %v", payloadErr)
			}
			if len(data.Endpoints) != 1 || data.Endpoints[0].URL() != "http://localhost:8080" {
				t.Errorf("readiness endpoints = %+v", data.Endpoints)
			}
		}
	}
	if logged != 1 {
		t.Errorf("forwarded %d listening log lines, want 1 — the substring probe still needs it", logged)
	}
	if ready != 1 {
		t.Fatalf("emitted %d ready events, want 1:\n%s", ready, out)
	}
}

// TestRunCommand_ReemitsReadinessAcrossRestarts covers the watch-mode restart
// loop (runWithWatch): each iteration spawns a fresh process and forwards it
// through the SAME emitter, so readiness must be announced once per iteration.
// A watcher that armed only on the first one would miss every restart.
func TestRunCommand_ReemitsReadinessAcrossRestarts(t *testing.T) {
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion2)
	args, env := readyHelper()
	dir := t.TempDir()

	out := captureServeStdout(t, func() {
		for iteration := 0; iteration < 2; iteration++ {
			if code := runCommand(args, dir, env, emit); code != 0 {
				t.Errorf("iteration %d exit = %d, want 0", iteration, code)
			}
		}
	})

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
	if ready != 2 {
		t.Fatalf("2 serve iterations emitted %d ready events, want 2:\n%s", ready, out)
	}
}

// TestRunCommand_NoReadinessLeakAtV1 is the no-leak guard for the serve path: a
// CLI that never advertised v2 acceptance sees exactly the stream it always
// did, with the workload's readiness marker consumed rather than echoed.
func TestRunCommand_NoReadinessLeakAtV1(t *testing.T) {
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion)
	args, env := readyHelper()

	out := captureServeStdout(t, func() {
		if code := runCommand(args, t.TempDir(), env, emit); code != 0 {
			t.Errorf("runCommand exit = %d, want 0", code)
		}
	})

	events, err := runtimeproto.DecodeAll(strings.NewReader(out))
	if err != nil {
		t.Fatalf("serve output is not a runtime event stream: %v\n%s", err, out)
	}
	for _, event := range events {
		if event.Type == runtimeproto.EventReady {
			t.Fatalf("a v1 stream must carry no readiness event:\n%s", out)
		}
		if event.V != runtimeproto.ProtocolVersion {
			t.Errorf("event %q v = %d, want %d", event.Type, event.V, runtimeproto.ProtocolVersion)
		}
	}
	if strings.Contains(out, runtimeproto.ReadyLogKey) {
		t.Errorf("the reserved marker leaked into the forwarded log context:\n%s", out)
	}
}
