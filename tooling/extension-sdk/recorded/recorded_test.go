package recorded

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fatalTB turns Fatal into a recoverable panic, so a test can assert that the
// helper refuses a broken recording instead of answering with it.
type fatalTB struct {
	*testing.T
}

type fatal string

func (f fatalTB) Fatal(args ...any)                 { panic(fatal(fmt.Sprint(args...))) }
func (f fatalTB) Fatalf(format string, args ...any) { panic(fatal(fmt.Sprintf(format, args...))) }

func refuses(t *testing.T, want string, call func(tb testing.TB)) {
	t.Helper()
	defer func() {
		t.Helper()
		got, ok := recover().(fatal)
		if !ok {
			t.Fatalf("the helper accepted what it must refuse (want a failure naming %q)", want)
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("failure = %q, want it to name %q", got, want)
		}
	}()
	call(fatalTB{t})
}

func get(t *testing.T, url string, header http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// The recording is the Put registry's anonymous refusal, copied from
// tooling/cli/testdata/recorded/put-registry.
func TestHTTPServesTheRecordedStatusHeadersAndBody(t *testing.T) {
	refusal := HTTP(t, "testdata/put-registry-anonymous.401.http")
	if refusal.Status() != http.StatusUnauthorized {
		t.Fatalf("status = %d", refusal.Status())
	}
	if got := string(refusal.Body()); got != "{\"error\":\"authentication required\"}\n" {
		t.Fatalf("body = %q", got)
	}

	server := NewServer(t, nil, refusal)
	resp, body := get(t, server.URL+"/putnami/go/download", http.Header{"Authorization": {"Bearer x"}})
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Server") != "Google Frontend" {
		t.Fatalf("served %d with headers %v", resp.StatusCode, resp.Header)
	}
	if !bytes.Equal(body, refusal.Body()) {
		t.Fatalf("served body %q, want the recorded one", body)
	}
	// One recording answers every request.
	if again, _ := get(t, server.URL, nil); again.StatusCode != http.StatusUnauthorized {
		t.Fatalf("second request = %d, want the last recording again", again.StatusCode)
	}
	requests := server.Requests()
	if len(requests) != 2 || requests[0].URL.Path != "/putnami/go/download" || requests[0].Header.Get("Authorization") != "Bearer x" {
		t.Fatalf("requests = %v", requests)
	}
}

func TestServerHandsOverToTheHandlerOnceTheRecordingsAreSpent(t *testing.T) {
	fault := HTTP(t, "testdata/chunked.502.http")
	server := NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("archive"))
	}), fault)

	first, body := get(t, server.URL, nil)
	if first.StatusCode != http.StatusBadGateway || string(body) != "reset" || first.Header.Get("X-Kept") != "yes" {
		t.Fatalf("first = %d %q %v", first.StatusCode, body, first.Header)
	}
	if first.Close {
		t.Fatal("the recorded Connection: close was replayed; framing belongs to the server")
	}
	second, body := get(t, server.URL, nil)
	if second.StatusCode != http.StatusOK || string(body) != "archive" {
		t.Fatalf("second = %d %q, want the handler", second.StatusCode, body)
	}
}

// notExist returns the text the host gives for opening a missing path:
// "no such file or directory" on Unix, "The system cannot find the file
// specified." on Windows.
func notExist(t *testing.T, path string) string {
	t.Helper()
	_, err := os.Open(path)
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open %s = %v, want a missing file", path, err)
	}
	return pathErr.Err.Error()
}

func TestHTTPRefusesABrokenRecording(t *testing.T) {
	refuses(t, notExist(t, "testdata/missing.http"), func(tb testing.TB) { HTTP(tb, "testdata/missing.http") })
	refuses(t, "not an HTTP/1.x response", func(tb testing.TB) { HTTP(tb, "testdata/garbage.http") })
	refuses(t, "body shorter than its headers declare", func(tb testing.TB) { HTTP(tb, "testdata/truncated.http") })
	refuses(t, "needs a response or a handler", func(tb testing.TB) { NewServer(tb, nil) })
}

func TestExecutableReplaysTheRecordedExchange(t *testing.T) {
	refusal := Command(t, "testdata/refusal")
	if string(refusal.Stdout()) != "partial out\n" || string(refusal.Stderr()) != "it's refused\n" || refusal.ExitCode() != 3 {
		t.Fatalf("exchange = %q %q %d", refusal.Stdout(), refusal.Stderr(), refusal.ExitCode())
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(Executable(t, refusal), "any", "argument")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit = %v, want 3", err)
	}
	if stdout.String() != "partial out\n" || stderr.String() != "it's refused\n" {
		t.Fatalf("replayed %q / %q", stdout.String(), stderr.String())
	}
}

func TestExecutableTakesTheBranchItsEnvironmentSelects(t *testing.T) {
	refusal := Command(t, "testdata/refusal")
	settled := Exchange{stdout: []byte("pkt_settled\n")}
	program := Executable(t, refusal, Branch{Env: "RECORDED_BRANCH", Value: "it's on", Exchange: settled})

	run := func(env ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(program)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	if out, code := run("RECORDED_BRANCH=it's on"); out != "pkt_settled\n" || code != 0 {
		t.Fatalf("selected branch = %q exit %d", out, code)
	}
	if out, code := run("RECORDED_BRANCH=other"); out != "partial out\n" || code != 3 {
		t.Fatalf("default = %q exit %d", out, code)
	}
	refuses(t, "is not a shell variable name", func(tb testing.TB) {
		Executable(tb, refusal, Branch{Env: "NOT-A-NAME", Exchange: settled})
	})
}

func TestCommandRefusesABrokenRecording(t *testing.T) {
	refuses(t, "want a status from 0 to 255", func(tb testing.TB) { Command(tb, "testdata/truncated") })
	for _, missing := range []string{"stdout", "stderr", "exit"} {
		dir := t.TempDir()
		for _, name := range []string{"stdout", "stderr", "exit"} {
			if name != missing {
				if err := os.WriteFile(dir+"/"+name, []byte("0"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		refuses(t, missing, func(tb testing.TB) { Command(tb, dir) })
	}
}
