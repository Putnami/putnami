package deliverycli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
)

const (
	fakeRunnerNextPath         = "/api/ci/runner/next"
	fakeRunnerNextMaxBody      = 4 << 10
	fakeRunnerNextMaxEnv       = 64 << 10
	fakeRunnerStatusCompleted  = "completed"
	fakeRunnerStatusInProgress = "in_progress"
)

// fakeRunnerRun is the part of a delivery run record the runner-next route reads.
type fakeRunnerRun struct {
	ID          string
	WorkspaceID string
	Status      string
}

// fakeRunnerCredential binds a bearer to its run and purpose. Only a
// host-purpose alias may ask for the next job; a job's ingest credential and
// its replay aliases may not.
type fakeRunnerCredential struct {
	runID string
	host  bool
}

// fakeRunnerNextProvider stands in for delivery-api's POST /api/ci/runner/next.
// It keeps the route's contract: the bearer must be a host-purpose alias of
// the machine run, the body is the generated client's RunnerNextRequest with
// no unknown or trailing fields, and the machine and workspace must match the
// credential. Missing or unknown credentials get 401, wrong-purpose or
// mismatched ones get 403, and only then does the assignment port run.
type fakeRunnerNextProvider struct {
	t           *testing.T
	mu          sync.Mutex
	runs        map[string]*fakeRunnerRun
	credentials map[string]fakeRunnerCredential
	assign      func(machine fakeRunnerRun, completedRunID string) (string, error)
}

func newFakeRunnerNextProvider(t *testing.T) *fakeRunnerNextProvider {
	return &fakeRunnerNextProvider{t: t, runs: map[string]*fakeRunnerRun{}, credentials: map[string]fakeRunnerCredential{}}
}

func fakeRunnerRandomHex(t *testing.T, size int) string {
	t.Helper()
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// open records a new in-progress run and returns it with its ingest credential.
func (p *fakeRunnerNextProvider) open(workspaceID string) (fakeRunnerRun, string) {
	p.t.Helper()
	run := &fakeRunnerRun{ID: fakeRunnerRandomHex(p.t, 32), WorkspaceID: workspaceID, Status: fakeRunnerStatusInProgress}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs[run.ID] = run
	return *run, p.mintLocked(run.ID, false)
}

func (p *fakeRunnerNextProvider) complete(runID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs[runID].Status = fakeRunnerStatusCompleted
}

// mintRunnerCredential mints a fresh host-purpose alias for the machine run.
func (p *fakeRunnerNextProvider) mintRunnerCredential(runID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mintLocked(runID, true)
}

// mintCredentialAlias mints a fresh ingest-purpose alias for a job run.
func (p *fakeRunnerNextProvider) mintCredentialAlias(runID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mintLocked(runID, false)
}

func (p *fakeRunnerNextProvider) mintLocked(runID string, host bool) string {
	token := "dcr1_" + fakeRunnerRandomHex(p.t, 32)
	p.credentials[token] = fakeRunnerCredential{runID: runID, host: host}
	return token
}

// authenticate resolves any live credential, whatever its purpose, to its run.
func (p *fakeRunnerNextProvider) authenticate(token string) (fakeRunnerRun, fakeRunnerCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	credential, ok := p.credentials[token]
	if !ok {
		return fakeRunnerRun{}, fakeRunnerCredential{}, errors.New("unknown run credential")
	}
	return *p.runs[credential.runID], credential, nil
}

func fakeRunnerRunID(id *string) bool {
	if id == nil || len(*id) != 64 {
		return false
	}
	_, err := hex.DecodeString(*id)
	return err == nil && strings.ToLower(*id) == *id
}

func (p *fakeRunnerNextProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.RequestURI() != fakeRunnerNextPath {
		p.t.Errorf("runner-next called an unexpected route: %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		p.t.Errorf("runner-next request content type = %q", r.Header.Get("Content-Type"))
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeFakeRunnerError(w, http.StatusUnauthorized, "runner reuse requires a run credential")
		return
	}
	machine, credential, err := p.authenticate(token)
	if err != nil {
		writeFakeRunnerError(w, http.StatusUnauthorized, "run credential missing, invalid or expired")
		return
	}
	if !credential.host {
		writeFakeRunnerError(w, http.StatusForbidden, "runner reuse requires a host credential")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, fakeRunnerNextMaxBody+1))
	if err != nil || len(raw) > fakeRunnerNextMaxBody {
		writeFakeRunnerError(w, http.StatusBadRequest, "runner reuse request exceeds its body limit")
		return
	}
	var body deliveryapiclient.RunnerNextRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeFakeRunnerError(w, http.StatusBadRequest, "invalid runner reuse request")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || body.WorkspaceId == nil || *body.WorkspaceId == "" ||
		*body.WorkspaceId != strings.TrimSpace(*body.WorkspaceId) ||
		!fakeRunnerRunID(body.MachineRunId) || !fakeRunnerRunID(body.CompletedRunId) {
		writeFakeRunnerError(w, http.StatusBadRequest, "invalid runner reuse request")
		return
	}
	if *body.WorkspaceId != machine.WorkspaceID || *body.MachineRunId != machine.ID {
		writeFakeRunnerError(w, http.StatusForbidden, "machine or workspace does not match the run credential")
		return
	}
	env, err := p.assign(machine, *body.CompletedRunId)
	if err != nil || len(env) > fakeRunnerNextMaxEnv {
		writeFakeRunnerError(w, http.StatusBadGateway, "failed to assign runner job")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(deliveryapiclient.RunnerNextResponse{Env: &env}); err != nil {
		p.t.Errorf("encode runner-next response: %v", err)
	}
}

func writeFakeRunnerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Bind the generated client to a fake of delivery-api's runner-next route that
// keeps its credential and request checks. The assignment port is the only
// scripted part: the fake authenticates the completed machine, validates every
// JSON field, and serializes the fresh response as the generated client's type.
func TestRunnerNextAgainstProviderFakeRetainsCredentialAliases(t *testing.T) {
	runnerNextProviderProof(t, func(args []string, input []byte) (int, string, string) {
		var output bytes.Buffer
		code := RunnerNext(args, bytes.NewReader(input), &output)
		return code, output.String(), ""
	})
}

// Both the in-process contract test and the built CLI subprocess proof use the
// same provider fake, run records and purpose-scoped credential checks. Only
// the assignment port is scripted; saga admission has its own API integration
// proof in delivery-api.
func runnerNextProviderProof(t *testing.T, invoke func([]string, []byte) (int, string, string)) {
	t.Helper()
	provider := newFakeRunnerNextProvider(t)
	origin, credential := provider.open("ws-a")
	hostCredential := provider.mintRunnerCredential(origin.ID)
	bootstrapRetryCredential := provider.mintRunnerCredential(origin.ID)
	if hostCredential == credential || bootstrapRetryCredential == hostCredential {
		t.Fatal("bootstrap did not issue distinct host-purpose aliases")
	}
	target, _ := provider.open("ws-a")
	provider.complete(origin.ID)
	var calls atomic.Int32
	var idle atomic.Bool
	var delivered atomic.Value
	provider.assign = func(machine fakeRunnerRun, completed string) (string, error) {
		calls.Add(1)
		if machine.ID != origin.ID || machine.WorkspaceID != "ws-a" || machine.Status != fakeRunnerStatusCompleted || completed != origin.ID {
			t.Errorf("provider did not bind original completed machine: %+v, completed=%s", machine, completed)
		}
		if idle.Load() {
			return "", nil
		}
		alias := provider.mintCredentialAlias(target.ID)
		env := "CI_RUN_ID=" + target.ID + "\nPUTNAMI_WORKSPACE_ID=ws-a\nCI_INGEST_CREDENTIAL=" + alias + "\nLITERAL=$(never-evaluate)\n"
		delivered.Store(env)
		return env, nil
	}
	host := httptest.NewServer(provider)
	t.Cleanup(host.Close)
	header := filepath.Join(t.TempDir(), "root-header")
	writeHeader := func(token string) {
		t.Helper()
		if err := os.WriteFile(header, []byte("Authorization: Bearer "+token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHeader(hostCredential)
	workspaceID, machineRunID, completedRunID := origin.WorkspaceID, origin.ID, origin.ID
	body, err := json.Marshal(deliveryapiclient.RunnerNextRequest{WorkspaceId: &workspaceID, MachineRunId: &machineRunID, CompletedRunId: &completedRunID})
	if err != nil {
		t.Fatal(err)
	}
	poll := func(want int) string {
		t.Helper()
		code, stdout, stderr := invoke([]string{host.URL + fakeRunnerNextPath, "2", header}, body)
		if code != want || stderr != "" {
			t.Fatalf("provider exit=%d want=%d; stdout bytes=%d stderr bytes=%d", code, want, len(stdout), len(stderr))
		}
		if strings.Contains(stdout, hostCredential) || strings.Contains(stdout, bootstrapRetryCredential) || strings.Contains(stdout, credential) {
			t.Fatal("machine or original job credential appeared on stdout")
		}
		if want != 0 && stdout != "" {
			t.Fatalf("idle/denied invocation wrote %d bytes to stdout", len(stdout))
		}
		if want == 0 && stdout != delivered.Load() {
			t.Fatal("helper stdout differs from the provider's exact env file")
		}
		return stdout
	}
	var aliases []string
	for _, hostAlias := range []string{hostCredential, bootstrapRetryCredential} {
		writeHeader(hostAlias)
		env := poll(0)
		if strings.Contains(env, hostCredential) || strings.Contains(env, bootstrapRetryCredential) {
			t.Fatal("host-purpose credential escaped into a successor environment")
		}
		if !strings.Contains(env, "CI_RUN_ID="+target.ID+"\n") || !strings.Contains(env, "LITERAL=$(never-evaluate)\n") {
			t.Fatal("provider response was altered")
		}
		for line := range strings.SplitSeq(env, "\n") {
			if value, ok := strings.CutPrefix(line, "CI_INGEST_CREDENTIAL="); ok {
				aliases = append(aliases, value)
			}
		}
	}
	if len(aliases) != 2 || aliases[0] == aliases[1] || calls.Load() != 2 {
		t.Fatal("provider replay did not deliver fresh aliases")
	}
	for _, alias := range aliases {
		if authenticated, _, err := provider.authenticate(alias); err != nil || authenticated.ID != target.ID {
			t.Fatal("a previously delivered credential stopped authenticating")
		}
	}
	idle.Store(true)
	writeHeader(hostCredential) // the original alias remains valid after a bootstrap retry
	if env := poll(runnerNextEmpty); env != "" {
		t.Fatal("no-job response wrote output")
	}
	writeHeader(credential) // the original job's ordinary ingest token cannot request later jobs
	if env := poll(runnerNextDenied); env != "" || calls.Load() != 3 {
		t.Fatal("original job ingest authority reached assignment")
	}
	writeHeader(aliases[1]) // a valid successor credential cannot impersonate the original machine
	if env := poll(runnerNextDenied); env != "" || calls.Load() != 3 {
		t.Fatal("foreign machine authority reached assignment")
	}
	writeHeader("dcr1_invalid")
	if env := poll(runnerNextDenied); env != "" || calls.Load() != 3 {
		t.Fatal("invalid credential reached assignment")
	}
}
