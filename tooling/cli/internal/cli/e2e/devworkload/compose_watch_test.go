//go:build unix

package devworkload

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// composeDocument is the part of a compose start or exit document this test
// reads.
type composeDocument struct {
	ExitCode int `json:"exitCode"`
	Data     struct {
		Members []struct {
			Project  string `json:"project"`
			ProxyURL string `json:"proxyUrl"`
		} `json:"members"`
		Cleanup *struct {
			State string `json:"state"`
		} `json:"cleanup"`
	} `json:"data"`
}

// TestComposeRestartsAWatchedTargetWithItsServeEnvironment runs a watched
// composition through the real CLI and the @putnami/dev executable: a change
// to the target's sources restarts its serve step, the proxy answers the new
// backend, the restarted step receives the environment document compose gave
// its serve run, and a signal still ends the composition with exit 0 and a
// clean exit document.
func TestComposeRestartsAWatchedTargetWithItsServeEnvironment(t *testing.T) {
	t.Parallel()
	requireFixtureTools(t)
	cli := fixtureproc.CLI(t)
	runtimePath := devRuntime(t)
	limitConcurrency(t)
	root := newFixture(t, runtimePath)
	starts := t.TempDir()

	cmd := exec.Command(cli, "compose", "app", "--port", "0", "--output=jsonl")
	cmd.Dir = root
	cmd.Env = append(runEnv(t), fixtureStartsEnv+"="+starts)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// The members hold the run's stdout and stderr: once putnami exits, Wait
	// stops waiting for them after this delay.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	t.Cleanup(func() {
		if exited {
			return
		}
		// A failed check leaves the composition running. SIGTERM lets it
		// stop its members; SIGKILL would orphan them.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	awaitOutput(t, stdout, regexp.MustCompile(`^\{.*"command":"compose".*\}\n`), done, stderr)
	start := parseComposeDocument(t, strings.SplitN(stdout.String(), "\n", 2)[0])
	proxies := map[string]string{}
	for _, member := range start.Data.Members {
		proxies[member.Project] = member.ProxyURL
	}
	if proxies["/app"] == "" || proxies["/provider"] == "" {
		t.Fatalf("the start document names no proxy of /app or /provider:\n%s", stdout.String())
	}
	first := awaitPID(t, proxies["/app"], 0, done, stderr)
	checkServeEnvironment(t, starts, first, proxies["/provider"])

	// The target's serve run snapshots the workspace once its step is ready,
	// so a file written before that snapshot is not a change. An attempt
	// writes a new file; the next one writes another only when the first
	// backend kept answering, so no restart was under way.
	var second int
	for attempt := 0; second == 0; attempt++ {
		if len(done) > 0 {
			t.Fatalf("compose exited before /app's serve step restarted\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		if attempt == 6 {
			t.Fatalf("no change to app/src restarted /app's serve step\nstderr:\n%s", stderr.String())
		}
		writeFixtureFile(t, root, "app/src/change-"+strconv.Itoa(attempt)+".txt", "change\n")
		pid, restarting := awaitPIDWithin(proxies["/app"], first, 20*time.Second, done)
		switch {
		case pid != 0:
			second = pid
		case restarting:
			second = awaitPID(t, proxies["/app"], first, done, stderr)
		}
	}
	checkServeEnvironment(t, starts, second, proxies["/provider"])

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-done:
		exited = true
	case <-time.After(3 * time.Minute):
		t.Fatalf("compose did not exit within 3m of SIGTERM\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if err != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		t.Fatalf("compose exited %d after SIGTERM, want 0\nstdout:\n%s\nstderr:\n%s", exitErr.ExitCode(), stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout holds %d documents, want the start and the exit document:\n%s", len(lines), stdout.String())
	}
	exit := parseComposeDocument(t, lines[1])
	if exit.ExitCode != 0 || exit.Data.Cleanup == nil || exit.Data.Cleanup.State != "clean" {
		t.Fatalf("the exit document is not a clean exit:\n%s", lines[1])
	}
}

func parseComposeDocument(t *testing.T, line string) composeDocument {
	t.Helper()
	var document composeDocument
	if err := json.Unmarshal([]byte(line), &document); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return document
}

// awaitPID returns the pid the backend behind proxy reports once it reports
// one other than previous, failing the test when compose exits first or none
// answers within a minute.
func awaitPID(t *testing.T, proxy string, previous int, done <-chan error, stderr *lockedBuffer) int {
	t.Helper()
	pid, _ := awaitPIDWithin(proxy, previous, time.Minute, done)
	if pid == 0 {
		t.Fatalf("no backend behind %s reported a pid other than %d\nstderr:\n%s", proxy, previous, stderr.String())
	}
	return pid
}

// awaitPIDWithin is awaitPID with a bound and without failing: it returns 0
// when compose exits or no other pid answers within limit. A request the
// proxy cannot pass, while the backend restarts, is retried; restarting
// reports that one did not reach the backend with previous.
func awaitPIDWithin(proxy string, previous int, limit time.Duration, done <-chan error) (pid int, restarting bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if len(done) > 0 {
			return 0, restarting
		}
		switch answered := backendPID(client, proxy); {
		case answered == 0:
			restarting = true
		case answered != previous:
			return answered, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, restarting
}

// backendPID is the pid the backend behind proxy reports on /pid, or 0.
func backendPID(client *http.Client, proxy string) int {
	response, err := client.Get(proxy + "/pid")
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		return 0
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		return 0
	}
	return pid
}

// checkServeEnvironment checks the start record of the /app serve step with
// the given pid: it received the ephemeral port and a CONFIG_DATA document
// that binds the provider's project name to its proxy.
func checkServeEnvironment(t *testing.T, starts string, pid int, providerProxy string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(starts, strconv.Itoa(pid)+".json"))
	if err != nil {
		t.Fatalf("the serve step %d recorded no start: %v", pid, err)
	}
	var record fixtureStart
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Member != "/app" || record.Port != "0" {
		t.Fatalf("serve step %d is %s on port %q, want /app on the ephemeral port 0", pid, record.Member, record.Port)
	}
	var document struct {
		Clients struct {
			Services map[string]struct {
				URL string `json:"url"`
			} `json:"services"`
		} `json:"clients"`
	}
	if err := json.Unmarshal([]byte(record.ConfigData), &document); err != nil {
		t.Fatalf("serve step %d received no CONFIG_DATA document (%v): %q", pid, err, record.ConfigData)
	}
	if got := document.Clients.Services["provider"].URL; got != providerProxy {
		t.Fatalf("serve step %d binds provider to %q, want its proxy %s", pid, got, providerProxy)
	}
}
