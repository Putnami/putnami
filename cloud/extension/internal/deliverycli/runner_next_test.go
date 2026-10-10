package deliverycli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runnerNextFixture(t *testing.T) (string, string) {
	t.Helper()
	header := filepath.Join(t.TempDir(), "root-header")
	if err := os.WriteFile(header, []byte("Authorization: Bearer original-machine-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return header, `{"workspaceId":"workspace-one","machineRunId":"` + strings.Repeat("a", 64) + `","completedRunId":"` + strings.Repeat("b", 64) + `"}`
}

func TestRunnerNextUsesGeneratedProviderContract(t *testing.T) {
	header, request := runnerNextFixture(t)
	env := "CI_RUN_ID=" + strings.Repeat("c", 64) + "\nPUTNAMI_WORKSPACE_ID=workspace-one\nCI_INGEST_CREDENTIAL=next-secret\nLITERAL=$(do-not-execute) '\"\\\n"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/api/ci/runner/next" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("generated request route/content type: %s %s %#v", r.Method, r.URL, r.Header)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer original-machine-secret" {
			t.Errorf("forwarded initial bearer = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 3 || body["workspaceId"] != "workspace-one" || body["machineRunId"] != strings.Repeat("a", 64) || body["completedRunId"] != strings.Repeat("b", 64) {
			t.Errorf("generated client lost request fields: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"env": env})
	}))
	defer server.Close()
	var output bytes.Buffer
	if code := RunnerNext([]string{server.URL + "/api/ci/runner/next", "2", header}, strings.NewReader(request), &output); code != 0 {
		t.Fatalf("helper exit = %d, output=%q", code, output.String())
	}
	if output.String() != env || calls.Load() != 1 {
		t.Fatalf("literal output or request count: %q, calls=%d", output.String(), calls.Load())
	}
}

func TestRunnerNextNoJobAndFailuresNeverPrintProviderBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"no job", 200, `{"env":""}`, runnerNextEmpty},
		{"unauthorized", 401, `{"message":"original-machine-secret"}`, runnerNextDenied},
		{"forbidden", 403, `{"message":"next-secret"}`, runnerNextDenied},
		{"server failure", 500, `{"message":"original-machine-secret"}`, runnerNextFailure},
		{"invalid JSON", 200, `original-machine-secret`, runnerNextFailure},
		{"missing env", 200, `{}`, runnerNextFailure},
		{"NUL", 200, `{"env":"VALUE=secret\u0000"}`, runnerNextFailure},
		{"CR", 200, `{"env":"VALUE=secret\r"}`, runnerNextFailure},
		{"oversize", 200, `{"env":"` + strings.Repeat("x", runnerNextMaxEnv+1) + `"}`, runnerNextFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, request := runnerNextFixture(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var output bytes.Buffer
			if code := RunnerNext([]string{server.URL + "/api/ci/runner/next", "2", header}, strings.NewReader(request), &output); code != tc.code || output.Len() != 0 || calls.Load() != 1 {
				t.Fatalf("code=%d want=%d, stdout=%q, calls=%d", code, tc.code, output.String(), calls.Load())
			}
		})
	}
}

func TestRunnerNextRejectsRedirectAndBoundsProviderWait(t *testing.T) {
	header, request := runnerNextFixture(t)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			start := time.Now()
			code := RunnerNext([]string{server.URL + "/api/ci/runner/next", "1", header}, strings.NewReader(request), &output)
			// A server need not observe the disconnect while its request body
			// remains unread. Release it independently of client cancellation so
			// Close cannot hang after the deadline assertion has already passed.
			close(release)
			if code != runnerNextFailure || output.Len() != 0 || redirected.Load() != 0 || time.Since(start) > 3*time.Second {
				t.Fatalf("code=%d output=%q redirected=%d elapsed=%s", code, output.String(), redirected.Load(), time.Since(start))
			}
		})
	}
}

func TestRunnerNextRejectsInvalidHostInputBeforeCalling(t *testing.T) {
	header, request := runnerNextFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	endpoint := server.URL + "/api/ci/runner/next"
	for _, tc := range []struct {
		args []string
		body string
	}{
		{nil, request},
		{[]string{endpoint, "0", header}, request},
		{[]string{endpoint, "11", header}, request},
		{[]string{endpoint, "NaN", header}, request},
		{[]string{endpoint + "?token=secret", "1", header}, request},
		{[]string{endpoint + "#fragment", "1", header}, request},
		{[]string{"http://delivery.example/api/ci/runner/next", "1", header}, request},
		{[]string{endpoint, "1", header + ".missing"}, request},
		{[]string{endpoint, "1", header}, `{}`},
		{[]string{endpoint, "1", header}, strings.ReplaceAll(request, strings.Repeat("a", 64), "bad")},
		{[]string{endpoint, "1", header}, strings.Repeat(" ", 4097)},
	} {
		var output bytes.Buffer
		if code := RunnerNext(tc.args, strings.NewReader(tc.body), &output); code != runnerNextFailure || output.Len() != 0 || calls.Load() != 0 {
			t.Fatalf("accepted invalid input: args=%v code=%d output=%q calls=%d", tc.args, code, output.String(), calls.Load())
		}
	}
	for _, bad := range []string{"Bearer secret", "Authorization: Bearer \n", "Authorization: Bearer secret\r\nInjected: secret", strings.Repeat("x", 8193)} {
		if err := os.WriteFile(header, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := RunnerNext([]string{endpoint, "1", header}, strings.NewReader(request), io.Discard); code != runnerNextFailure || calls.Load() != 0 {
			t.Fatalf("accepted malformed authority: code=%d calls=%d", code, calls.Load())
		}
	}
}
