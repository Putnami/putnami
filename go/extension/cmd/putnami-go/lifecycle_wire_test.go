package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/localrun"
)

// mainHelperArgs holds, as a JSON array, the arguments TestMainHelperProcess
// runs main with.
const mainHelperArgs = "PUTNAMI_GO_MAIN_HELPER_ARGS"

// TestMainHelperProcess is not a test: it is the putnami-go binary, run with
// the arguments in mainHelperArgs as the CLI runs it, and it skips unless a
// test started it as one. It exits as main does, so nothing but the
// job's event stream reaches its standard output.
func TestMainHelperProcess(t *testing.T) {
	encoded := os.Getenv(mainHelperArgs)
	if encoded == "" {
		t.Skip("not the helper process")
	}
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		os.Exit(90)
	}
	os.Args = append([]string{"putnami-go"}, args...)
	main()
	os.Exit(0)
}

// runPutnamiGo runs the putnami-go binary with args and env, and returns its
// decoded event stream, its standard error and its exit code.
func runPutnamiGo(t *testing.T, env []string, args ...string) ([]*runtimeproto.Event, string, int) {
	t.Helper()
	return runPutnamiGoWithFiles(t, env, nil, args...)
}

// runPutnamiGoWithFiles is runPutnamiGo with files inherited as descriptors 3
// and up, the way the engine hands a job a descriptor. The binary keeps the
// hosted-run variables of env (localrun.StartedEntry).
func runPutnamiGoWithFiles(t *testing.T, env []string, files []*os.File, args ...string) ([]*runtimeproto.Event, string, int) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelperProcess$")
	cmd.Env = slices.Concat(env, []string{mainHelperArgs + "=" + string(encoded), localrun.StartedEntry})
	cmd.ExtraFiles = files
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run putnami-go: %v", err)
		}
		code = exitErr.ExitCode()
	}
	events, err := runtimeproto.DecodeAll(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("putnami-go wrote something other than a runtime event stream: %v\n%s", err, stdout.String())
	}
	return events, stderr.String(), code
}

// writeDepsUpgradeContext writes the job context the CLI hands deps-upgrade
// for workspace ws with params, and returns its path.
func writeDepsUpgradeContext(t *testing.T, ws, params string) string {
	t.Helper()
	return writeLifecycleContext(t, ws, "deps-upgrade", params)
}

// writeLifecycleContext writes the job context the CLI hands the lifecycle job
// for workspace ws with params, and returns its path.
func writeLifecycleContext(t *testing.T, ws, job, params string) string {
	t.Helper()
	document := `{"workspaceRoot":` + quote(t, ws) + `,"outputPath":` + quote(t, filepath.Join(ws, ".out")) +
		`,"cacheRoot":` + quote(t, t.TempDir()) + `,"workspace":{},"project":{},` +
		`"extension":{"name":"@putnami/go"},"job":{"name":` + quote(t, job) + `},"params":` + params + `}`
	return jobtest.WriteFile(t, t.TempDir(), "context.json", document)
}

func quote(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// requireStream checks what every stream of the job owes the CLI: it passes
// the protocol's stream validation, each event carries the negotiated
// version, the first is the meta event naming the extension and the job, and
// the last is the one result, with status.
func requireStream(t *testing.T, events []*runtimeproto.Event, version int, status runtimeproto.ResultStatus) {
	t.Helper()
	requireJobStream(t, events, "deps-upgrade", version, status)
}

// requireJobStream is requireStream for the lifecycle job named job.
func requireJobStream(t *testing.T, events []*runtimeproto.Event, job string, version int, status runtimeproto.ResultStatus) {
	t.Helper()
	if diagnostics := runtimeproto.ValidateEventStream(events); len(diagnostics) != 0 {
		t.Errorf("the event stream is not valid: %v\n%s", diagnostics, eventLines(events))
	}
	if len(events) < 2 {
		t.Fatalf("stream has %d events, want at least meta and result", len(events))
	}
	for _, event := range events {
		if event.V != version {
			t.Errorf("%s event has v %d, want the negotiated %d", event.Type, event.V, version)
		}
	}
	var meta runtimeproto.MetaData
	if first := events[0]; first.Type != runtimeproto.EventMeta || json.Unmarshal(first.Data, &meta) != nil ||
		meta.Extension != "@putnami/go" || meta.Job != job {
		t.Errorf("first event = %s %s, want the %s meta event", first.Type, first.Data, job)
	}
	var result runtimeproto.ResultData
	if last := events[len(events)-1]; last.Type != runtimeproto.EventResult || json.Unmarshal(last.Data, &result) != nil ||
		result.Status != status {
		t.Errorf("last event = %s %s, want the result %q", last.Type, last.Data, status)
	}
}

// deps-upgrade runs through the same entry point the CLI starts: a dry run on
// a workspace with no Go project resolves an exact release and succeeds, and a
// declared proxy chain that is not a list fails, each with the event stream
// and the exit code the CLI reads.
func TestDepsUpgradeWireThroughTheEntryPoint(t *testing.T) {
	goBinary := jobtest.RequireGo(t)
	origin := "file:///nonexistent-proxy"
	env := jobtest.Env(t, origin, jobtest.CacheRoot(t),
		"PATH="+filepath.Dir(goBinary),
		"PUTNAMI_GO_MODULE_PROXY="+origin,
		runtimeproto.AdvertisedVersionEnv(runtimeproto.ProtocolVersion2))

	t.Run("success", func(t *testing.T) {
		ws := jobtest.RealTempDir(t)
		context := writeDepsUpgradeContext(t, ws, `{"putnamiVersion":"0.1.0-aaaa1111","dryRun":true}`)
		events, stderr, code := runPutnamiGo(t, env, "deps-upgrade", "--putnamiContext", context)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
		}
		requireStream(t, events, runtimeproto.ProtocolVersion2, runtimeproto.ResultOK)
		if !hasEvent(events, runtimeproto.EventLog, "Dry run complete; no Go files were changed") {
			t.Errorf("missing the dry-run log:\n%s", eventLines(events))
		}
		if entries, err := os.ReadDir(ws); err != nil || len(entries) != 0 {
			t.Errorf("a dry run wrote into the workspace: %v %v", entries, err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		ws := jobtest.RealTempDir(t)
		context := writeDepsUpgradeContext(t, ws, `{"registries":{"go":{"proxy":"https://m.test"}}}`)
		events, stderr, code := runPutnamiGo(t, env, "deps-upgrade", "--putnamiContext", context)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1\nstderr:\n%s", code, stderr)
		}
		requireStream(t, events, runtimeproto.ProtocolVersion2, runtimeproto.ResultFailed)
		var diagnostic *runtimeproto.Event
		for _, event := range events {
			if event.Type == runtimeproto.EventDiagnostic {
				diagnostic = event
			}
		}
		if diagnostic == nil || diagnostic.Severity == nil || *diagnostic.Severity != runtimeproto.SeverityError ||
			diagnostic.Message != "registries.go.proxy is neither an array nor an object of proxy URLs" ||
			diagnostic.Location == nil || diagnostic.Location.File != context || diagnostic.Location.Line != 0 {
			t.Errorf("want one error diagnostic on the context file:\n%s", eventLines(events))
		}
	})
}

func hasEvent(events []*runtimeproto.Event, eventType runtimeproto.EventType, message string) bool {
	for _, event := range events {
		if event.Type == eventType && event.Message == message {
			return true
		}
	}
	return false
}

func eventLines(events []*runtimeproto.Event) string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		line, _ := json.Marshal(event)
		lines = append(lines, string(line))
	}
	return strings.Join(lines, "\n")
}
