//go:build unix

package compose

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// The serve-run tests drive `putnami compose` through the shipped CLI binary
// in an on-disk workspace whose one extension serves every project with this
// test binary in fixture mode. They pin the start, exit and failure documents
// byte for byte against goldens, after normalizing what differs between runs:
// the composition id, ports and durations.
//
// PUTNAMI_UPDATE_COMPOSE_GOLDENS=1 rewrites the goldens.

const composeGoldenDir = "testdata/serve-run"

var updateComposeGoldens = os.Getenv("PUTNAMI_UPDATE_COMPOSE_GOLDENS") == "1"

// diskMember is one project of an on-disk composition workspace.
type diskMember struct {
	name     string
	mode     string
	runsWith []string
	// contract, when set, publishes a client contract for the service id.
	contract string
}

// diskWorkspace writes the workspace and returns its root.
func diskWorkspace(t *testing.T, members ...diskMember) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	root := t.TempDir()
	includes := []string{"fixture-extension"}
	for _, member := range members {
		includes = append(includes, member.name)
	}
	manifest, _ := json.Marshal(struct {
		Name     string   `json:"name"`
		Includes []string `json:"includes"`
	}{"compose-serve-run", includes})
	writeFile(t, filepath.Join(root, "putnami.workspace.json"), string(manifest))
	writeFile(t, filepath.Join(root, "fixture-extension", "putnami.json"), `{"name":"@putnami/compose-fixture"}`)
	command, _ := json.Marshal(executable)
	writeFile(t, filepath.Join(root, "fixture-extension", "putnami.extension.json"), fmt.Sprintf(`{
  "name": "@putnami/compose-fixture",
  "version": "0.1.0",
  "cliContract": 11,
  "commands": {
    "serve": {
      "description": "Serve the compose fixture workload.",
      "traits": {"workload": "serve"},
      "flags": {
        "port": {"type": "number", "default": 3000},
        "watch": {"type": "boolean", "default": true}
      },
      "run": [{"id": "serve", "task": "serve-run"}]
    }
  },
  "tasks": {
    "serve-run": {
      "description": "Long-lived fixture workload that announces typed readiness.",
      "kind": "command",
      "command": %s,
      "env": {%q: %q},
      "cache": false,
      "timeoutMs": -1,
      "declares": {"effects": ["network", "process"]}
    }
  }
}`, command, fixtureModeEnv, fixtureDiskMode))
	for _, member := range members {
		project, _ := json.Marshal(struct {
			Name       string   `json:"name"`
			Extensions []string `json:"extensions"`
			RunsWith   []string `json:"runsWith,omitempty"`
		}{member.name, []string{"@putnami/compose-fixture"}, member.runsWith})
		writeFile(t, filepath.Join(root, member.name, "putnami.json"), string(project))
		fixture, _ := json.Marshal(diskFixture{
			Mode: member.mode, Member: "/" + member.name, Record: filepath.Join(root, member.name, "record.json"),
		})
		writeFile(t, filepath.Join(root, member.name, "fixture.json"), string(fixture))
		if member.contract != "" {
			writeFile(t, filepath.Join(root, member.name, "schema", "openapi.json"), fmt.Sprintf(
				`{"x-putnami-client":{"protocolVersion":1,"service":{"id":%q,"audience":"urn:acme:%s"},"credentials":{}}}`,
				member.contract, member.contract))
		}
	}
	return root
}

// cliRun is one `putnami` process a test started.
type cliRun struct {
	cmd    *exec.Cmd
	lines  chan string
	stderr *syncBuffer
	done   chan struct{}
	err    error
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startCLI starts the built CLI in root with args. Its stdout lines arrive on
// run.lines; the channel closes when stdout does.
func startCLI(t *testing.T, root string, args ...string) *cliRun {
	t.Helper()
	cmd := exec.Command(fixtureproc.CLI(t), args...)
	cmd.Dir = root
	cmd.Env = fixtureproc.CLIEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	run := &cliRun{cmd: cmd, lines: make(chan string, 16), stderr: &syncBuffer{}, done: make(chan struct{})}
	cmd.Stderr = run.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start putnami %s: %v", strings.Join(args, " "), err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			run.lines <- scanner.Text()
		}
		close(run.lines)
		run.err = cmd.Wait()
		close(run.done)
	}()
	t.Cleanup(func() {
		select {
		case <-run.done:
		default:
			_ = cmd.Process.Kill()
			<-run.done
		}
	})
	return run
}

// line returns the next stdout line, failing the test when none arrives.
func (r *cliRun) line(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-r.lines:
		if !ok {
			t.Fatalf("putnami closed stdout before the next document\nstderr:\n%s", r.stderr.String())
		}
		return line
	case <-time.After(hangDetector):
		t.Fatalf("no document on stdout within %s\nstderr:\n%s", hangDetector, r.stderr.String())
	}
	return ""
}

// wait returns the exit code once the process ended and stdout closed, and the
// stdout lines it had not read yet.
func (r *cliRun) wait(t *testing.T) (int, []string) {
	t.Helper()
	var rest []string
	for {
		select {
		case line, ok := <-r.lines:
			if ok {
				rest = append(rest, line)
				continue
			}
			select {
			case <-r.done:
			case <-time.After(hangDetector):
				t.Fatalf("putnami did not exit within %s\nstderr:\n%s", hangDetector, r.stderr.String())
			}
			var exitErr *exec.ExitError
			switch {
			case r.err == nil:
				return 0, rest
			case errors.As(r.err, &exitErr):
				return exitErr.ExitCode(), rest
			}
			t.Fatalf("wait putnami: %v", r.err)
		case <-time.After(hangDetector):
			t.Fatalf("putnami did not close stdout within %s\nstderr:\n%s", hangDetector, r.stderr.String())
		}
	}
}

var (
	loopbackPort  = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
	compositionID = regexp.MustCompile(`\b[0-9a-f]{16}\b`)
	portMember    = regexp.MustCompile(`"(backendPort|port)":[1-9][0-9]*`)
	timingMember  = regexp.MustCompile(`"(readyMs|durationMs|pid)":[1-9][0-9]*`)
)

// normalizeDocument rewrites, in the document's own bytes, what differs
// between two runs of the same composition: the composition id, ports,
// durations and the temporary root. Member order and key order stay as the
// CLI wrote them.
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
		t.Fatalf("read golden %s: %v (regenerate with: PUTNAMI_UPDATE_COMPOSE_GOLDENS=1 ./putnamiw test --projects /tooling/cli --run ServeRun --no-cache --no-enforce-coverage)", path, err)
	}
	if string(want) != got {
		t.Errorf("%s drifted from its golden.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func TestServeRun_TheStartAndExitDocumentsKeepTheirBytes(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement, "the-start-exit-and-verdict-documents-keep-their-bytes")
	spectest.Proves(t, "cli/workload-qualification", serveRunRequirement, "the-environment-document-reaches-no-session-file")
	root := diskWorkspace(t,
		diskMember{name: "provider", mode: fixtureServe, contract: "items"},
		diskMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}},
	)
	run := startCLI(t, root, "compose", "app", "--port", "0", "--no-watch", "--ready-timeout", "60s", "--output=jsonl", "--no-color")
	start := run.line(t)

	var envelope struct {
		Data Status `json:"data"`
	}
	if err := json.Unmarshal([]byte(start), &envelope); err != nil || len(envelope.Data.Members) != 2 {
		t.Fatalf("start document = %s (%v)\nstderr:\n%s", start, err, run.stderr.String())
	}
	app := envelope.Data.Members[1]
	if code, body := get(t, app.ProxyURL+"/whoami"); code != 200 || !strings.HasPrefix(body, "/app host=127.0.0.1:") {
		t.Errorf("the target's proxy answered %d %q", code, body)
	}
	for _, id := range []string{"/provider", "/app"} {
		record := readDiskRecord(t, root, id)
		if record.Port != "0" || record.NodeEnv != "production" {
			t.Errorf("member %s ran with PORT=%q NODE_ENV=%q, want 0 and production", id, record.Port, record.NodeEnv)
		}
	}
	if record := readDiskRecord(t, root, "/app"); !slices.Equal(record.ConfigSections, []string{SectionClients}) ||
		!slices.Equal(record.ServiceKeys, []string{"items", "provider"}) {
		t.Errorf("the target's document = %+v, want the clients section naming its provider", record)
	}
	// Each member is a serve run of the CLI: the lease holds its group and its
	// serve step's, each with its leader's start time.
	lease, ok := readLease(filepath.Join(Root(root), envelope.Data.ID))
	if !ok || len(lease.PGIDs) != 4 || len(lease.Groups) != 4 {
		t.Errorf("lease = %+v (%v), want the serve run and the serve step of each member", lease, ok)
	}
	for _, group := range lease.Groups {
		if group.LeaderStart == "" || !processGroupAlive(group.PGID) {
			t.Errorf("lease group %+v is not a running group with an identity", group)
		}
	}

	if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, rest := run.wait(t)
	if code != 0 || len(rest) != 1 {
		t.Fatalf("exit %d with %d more documents %q\nstderr:\n%s", code, len(rest), rest, run.stderr.String())
	}
	assertGolden(t, "start-and-exit", code, []string{normalizeDocument(t, root, start), normalizeDocument(t, root, rest[0])})
	if dirs := leaseDirs(t, root); len(dirs) != 0 {
		t.Errorf("lease directories left after a clean stop: %v", dirs)
	}
	for _, group := range lease.Groups {
		if processGroupAlive(group.PGID) {
			t.Errorf("process group %d survived the stop", group.PGID)
		}
	}
	assertSessionsHoldNoDocument(t, root, 2)
}

// assertSessionsHoldNoDocument checks that the workspace recorded one session
// per member's serve run, and that no file the runs wrote under .putnami holds
// the environment document: neither its CONFIG_DATA variable nor a section of
// the document.
func assertSessionsHoldNoDocument(t *testing.T, root string, members int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".putnami", "sessions"))
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	sessions := 0
	for _, entry := range entries {
		if entry.IsDir() {
			sessions++
		}
	}
	if sessions != members {
		t.Errorf("sessions = %d, want one per member's serve run (%d)", sessions, members)
	}
	_ = filepath.WalkDir(filepath.Join(root, ".putnami"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, needle := range []string{ConfigDataEnv, `"clients":{"services"`} {
			if bytes.Contains(data, []byte(needle)) {
				t.Errorf("%s holds %s of the environment document", path, needle)
			}
		}
		return nil
	})
}

func TestServeRun_TheFailureDocumentsKeepTheirBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"ready-timeout", fixtureNeverReady},
		{"member-exited", fixtureExitEarly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := diskWorkspace(t,
				diskMember{name: "provider", mode: tc.mode},
				diskMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}},
			)
			run := startCLI(t, root, "compose", "app", "--port", "0", "--no-watch", "--ready-timeout", "2s", "--output=jsonl", "--no-color")
			code, documents := run.wait(t)
			if code == 0 || len(documents) != 1 {
				t.Fatalf("exit %d with documents %q\nstderr:\n%s", code, documents, run.stderr.String())
			}
			assertGolden(t, tc.name, code, []string{normalizeDocument(t, root, documents[0])})
			if _, err := os.Stat(filepath.Join(root, "app", "record.json")); err == nil {
				t.Error("the target started although the member it runs with never became ready")
			}
			if dirs := leaseDirs(t, root); len(dirs) != 0 {
				t.Errorf("lease directories left after a failed start: %v", dirs)
			}
		})
	}
}

func readDiskRecord(t *testing.T, root, id string) fixtureRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(id, "/"), "record.json"))
	if err != nil {
		t.Fatalf("member %s left no record: %v", id, err)
	}
	var record fixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode record of %s: %v", id, err)
	}
	return record
}
