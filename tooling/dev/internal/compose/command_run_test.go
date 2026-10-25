package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// The command tests run `putnami compose` as the dev runtime does, against a
// fixture workspace whose members' serve runs are the fake CLI. They pin the
// start, exit and failure documents byte for byte against the goldens the
// core command recorded, after normalizing what differs between runs: the
// composition id, ports and durations.
//
// PUTNAMI_UPDATE_COMPOSE_GOLDENS=1 rewrites the goldens.

const composeGoldenDir = "testdata/serve-run"

var updateComposeGoldens = os.Getenv("PUTNAMI_UPDATE_COMPOSE_GOLDENS") == "1"

// documentLines is a writer that hands each complete line to a channel.
type documentLines struct {
	mu      sync.Mutex
	pending bytes.Buffer
	lines   chan string
}

func newDocumentLines() *documentLines {
	return &documentLines{lines: make(chan string, 16)}
}

func (d *documentLines) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending.Write(p)
	for {
		line, err := d.pending.ReadString('\n')
		if err != nil {
			d.pending.Reset()
			d.pending.WriteString(line)
			return len(p), nil
		}
		d.lines <- strings.TrimSuffix(line, "\n")
	}
}

// commandRun is one Run of the command in a goroutine.
type commandRun struct {
	stdout *documentLines
	stderr *lockedBuffer
	code   chan int
	cancel context.CancelFunc
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startCommand(t *testing.T, fixture *compositionFixture, mode protocolcli.OutputMode, args ...string) *commandRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &commandRun{stdout: newDocumentLines(), stderr: &lockedBuffer{}, code: make(chan int, 1), cancel: cancel}
	go func() {
		run.code <- Run(Request{
			Ctx: ctx, Workspace: fixture.ws, Args: args, Output: mode,
			Executable: fixture.executable, Stdout: run.stdout, Stderr: run.stderr,
		})
	}()
	t.Cleanup(cancel)
	return run
}

// line returns the next stdout line, failing the test when none arrives.
func (r *commandRun) line(t *testing.T) string {
	t.Helper()
	select {
	case line := <-r.stdout.lines:
		return line
	case code := <-r.code:
		r.code <- code
		select {
		case line := <-r.stdout.lines:
			return line
		default:
		}
		t.Fatalf("compose exited %d before the next document\nstderr:\n%s", code, r.stderr.String())
	case <-time.After(hangDetector):
		t.Fatalf("no document on stdout within %s\nstderr:\n%s", hangDetector, r.stderr.String())
	}
	return ""
}

// wait returns the exit code and the stdout lines not read yet.
func (r *commandRun) wait(t *testing.T) (int, []string) {
	t.Helper()
	var code int
	select {
	case code = <-r.code:
	case <-time.After(hangDetector):
		t.Fatalf("compose did not return within %s\nstderr:\n%s", hangDetector, r.stderr.String())
	}
	var rest []string
	for {
		select {
		case line := <-r.stdout.lines:
			rest = append(rest, line)
		default:
			return code, rest
		}
	}
}

// The normalizers replace what changes between runs. A port of 0 stays: the
// exit document reports it for a stopped backend. Every timing is replaced,
// 0 included: a member can claim readiness within a millisecond.
var (
	loopbackPort  = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
	compositionID = regexp.MustCompile(`\b[0-9a-f]{16}\b`)
	portMember    = regexp.MustCompile(`"(backendPort|port)":[1-9][0-9]*`)
	timingMember  = regexp.MustCompile(`"(readyMs|durationMs|pid)":[0-9]+`)
)

// normalizeDocument rewrites, in the document's own bytes, what differs
// between two runs of the same composition: the composition id, ports,
// durations and the temporary root. Member order and key order stay as the
// command wrote them.
func normalizeDocument(t *testing.T, root, line string) string {
	t.Helper()
	if !json.Valid([]byte(line)) {
		t.Fatalf("stdout line is not one JSON document:\n%s", line)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		line = strings.ReplaceAll(line, resolved, "<root>")
	}
	line = strings.ReplaceAll(line, root, "<root>")
	line = loopbackPort.ReplaceAllString(line, "127.0.0.1:<port>")
	line = compositionID.ReplaceAllString(line, "<composition>")
	line = portMember.ReplaceAllString(line, `"$1":"<port>"`)
	return timingMember.ReplaceAllString(line, `"$1":"<$1>"`)
}

// assertGolden compares a run's exit code and normalized documents with the
// golden named name.
func assertGolden(t *testing.T, name string, code int, documents []string) {
	t.Helper()
	got := fmt.Sprintf("exit %d\n", code) + strings.Join(documents, "\n") + "\n"
	path := filepath.Join(composeGoldenDir, name+".golden")
	if updateComposeGoldens {
		writeFile(t, path, got)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: PUTNAMI_UPDATE_COMPOSE_GOLDENS=1 ./putnamiw test --projects /tooling/dev --run Command --no-cache --no-enforce-coverage)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s drifted from its golden.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// A stopped composition writes its start document, then its exit document
// with the clean teardown, and exits 0. Each member's environment document
// travels on its serve run's stdin alone: no argument and no file the
// composition wrote holds it.
func TestRun_TheStartAndExitDocumentsKeepTheirBytes(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", serveRunRequirement, "the-start-exit-and-verdict-documents-keep-their-bytes")
	spectest.Proves(t, "tooling/workload-qualification", serveRunRequirement, "the-environment-document-reaches-no-session-file")
	fixture := newCompositionFixture(t,
		fixtureProject{id: "/provider", name: "provider"},
		fixtureProject{id: "/app", name: "app", runsWith: []string{"provider"}},
	)
	writeFile(t, filepath.Join(fixture.root, "provider", "schema", "openapi.json"),
		`{"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:acme:items"},"credentials":{}}}`)
	run := startCommand(t, fixture, protocolcli.OutputJSONL, "app", "--port", "0", "--no-watch", "--ready-timeout", "60s")
	start := run.line(t)

	var envelope struct {
		Data Status `json:"data"`
	}
	if err := json.Unmarshal([]byte(start), &envelope); err != nil || len(envelope.Data.Members) != 2 {
		t.Fatalf("start document = %s (%v)\nstderr:\n%s", start, err, run.stderr.String())
	}
	if code, body := get(t, envelope.Data.Members[1].ProxyURL+"/whoami"); code != 200 || !strings.HasPrefix(body, "/app host=127.0.0.1:") {
		t.Errorf("the target's proxy answered %d %q", code, body)
	}
	if record := fixture.record(t, "/app"); len(record.ServiceKeys) != 2 || record.ConfigSections[0] != SectionClients {
		t.Errorf("the target's document = %+v, want the clients section naming its provider", record)
	}

	run.cancel()
	code, rest := run.wait(t)
	if code != 0 || len(rest) != 1 {
		t.Fatalf("exit %d with %d more documents %q\nstderr:\n%s", code, len(rest), rest, run.stderr.String())
	}
	assertGolden(t, "start-and-exit", code, []string{normalizeDocument(t, fixture.root, start), normalizeDocument(t, fixture.root, rest[0])})
	if dirs := leaseDirs(t, fixture.root); len(dirs) != 0 {
		t.Errorf("lease directories left after a clean stop: %v", dirs)
	}

	for _, id := range []string{"/provider", "/app"} {
		argv := fixture.argv(t, id)
		if !strings.Contains(strings.Join(argv, " "), "--serve-env -") {
			t.Errorf("%s started as %v, without its document on stdin", id, argv)
		}
		assertHoldsNoDocument(t, filepath.Join(fixture.root, strings.TrimPrefix(id, "/"), fakeCLIArgsFile))
	}
	_ = filepath.WalkDir(filepath.Join(fixture.root, ".putnami"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			assertHoldsNoDocument(t, path)
		}
		return nil
	})
}

// assertHoldsNoDocument fails when the file at path holds the environment
// document: its CONFIG_DATA variable or a section of the document.
func assertHoldsNoDocument(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, needle := range []string{ConfigDataEnv, `"clients":{"services"`} {
		if bytes.Contains(data, []byte(needle)) {
			t.Errorf("%s holds %s of the environment document", path, needle)
		}
	}
}

func TestRun_TheFailureDocumentsKeepTheirBytes(t *testing.T) {
	spectest.Proves(t, "tooling/workload-qualification", serveRunRequirement, "the-start-exit-and-verdict-documents-keep-their-bytes")
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"ready-timeout", fixtureNeverReady},
		{"member-exited", fixtureExitEarly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompositionFixture(t,
				fixtureProject{id: "/provider", name: "provider", mode: tc.mode},
				fixtureProject{id: "/app", name: "app", runsWith: []string{"provider"}},
			)
			run := startCommand(t, fixture, protocolcli.OutputJSONL, "app", "--port", "0", "--no-watch", "--ready-timeout", "2s")
			code, documents := run.wait(t)
			if code == 0 || len(documents) != 1 {
				t.Fatalf("exit %d with documents %q\nstderr:\n%s", code, documents, run.stderr.String())
			}
			assertGolden(t, tc.name, code, []string{normalizeDocument(t, fixture.root, documents[0])})
			if want := "putnami: compose." + strings.ReplaceAll(tc.name, "-", "_") + ": /provider: "; !strings.Contains(run.stderr.String(), want) {
				t.Errorf("stderr = %q, want the error line %q", run.stderr.String(), want)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "app", "record.json")); err == nil {
				t.Error("the target started although the member it runs with never became ready")
			}
			if dirs := leaseDirs(t, fixture.root); len(dirs) != 0 {
				t.Errorf("lease directories left after a failed start: %v", dirs)
			}
		})
	}
}

// A refused invocation exits 2 with its usage error before anything starts;
// in text mode the error is the one line on stderr.
func TestRun_RefusesAnInvocationBeforeAnythingStarts(t *testing.T) {
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app"})
	cases := map[string]struct {
		args []string
		want string
	}{
		"no project":         {nil, "compose takes exactly one project: putnami compose <project> [--port <n>] [--no-watch] [--ready-timeout <duration>]"},
		"two projects":       {[]string{"app", "lib"}, "compose takes exactly one project"},
		"unknown project":    {[]string{"nope"}, `compose: "nope" names no project of this workspace`},
		"bad port":           {[]string{"app", "--port", "web"}, `--port "web" is not a TCP port`},
		"bad ready timeout":  {[]string{"app", "--ready-timeout", "-5s"}, `--ready-timeout "-5s" is not a positive duration`},
		"unknown flag":       {[]string{"app", "--watch"}, "unknown flag --watch for `putnami compose`"},
		"flag without value": {[]string{"app", "--port"}, "flag --port requires a value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(Request{Ctx: t.Context(), Workspace: fixture.ws, Args: tc.args, Executable: fixture.executable, Stdout: &stdout, Stderr: &stderr})
			if code != protocolcli.ExitUsage || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "putnami: "+tc.want) {
				t.Errorf("exit %d, stdout %q, stderr %q, want 2 and %q", code, stdout.String(), stderr.String(), tc.want)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	code := Run(Request{Ctx: t.Context(), Workspace: fixture.ws, Output: protocolcli.OutputJSON, Executable: fixture.executable, Stdout: &stdout, Stderr: &stderr})
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || code != protocolcli.ExitUsage || envelope.Command != CommandPath ||
		envelope.Error == nil || envelope.Error.Code != "usage" {
		t.Errorf("json refusal: exit %d, envelope %s (%v)", code, stdout.String(), err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "app", fakeCLIArgsFile)); err == nil {
		t.Error("a refused invocation started a serve run")
	}
}

// A target that stops on its own fails the run: in text mode the exit table
// follows the start table, then the member's output tail and the error.
func TestRun_ATargetThatExitsFailsTheRunInText(t *testing.T) {
	fixture := newCompositionFixture(t, fixtureProject{id: "/app", name: "app", mode: fixtureListenThenExit})
	run := startCommand(t, fixture, "", "app", "--port", "0", "--no-watch")
	table := run.line(t)
	if !strings.HasPrefix(table, "  composition ") || !strings.HasSuffix(table, ": /app (isolation none)") {
		t.Fatalf("start table = %q", table)
	}
	member := run.line(t)
	proxyURL := strings.Fields(member)[1]
	if code, _ := get(t, proxyURL+"/exit"); code != 200 {
		t.Fatalf("GET /exit answered %d", code)
	}
	code, rest := run.wait(t)
	if code != protocolcli.ExitFailure || len(rest) != 1 || !strings.HasSuffix(rest[0], "stopped: cleanup clean") {
		t.Fatalf("exit %d with %q\nstderr:\n%s", code, rest, run.stderr.String())
	}
	stderr := run.stderr.String()
	for _, want := range []string{"last output of /app:\n", "putnami: compose.member_exited: /app: "} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}
