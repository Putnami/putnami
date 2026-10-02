package credentialcustody

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// custodyGateTimeout bounds each wait of an upload probe run: the probe's
// wait for the upload, and the registry's wait for the probe's report. A
// passing run waits on events only; the bound turns a missed event into a
// failure instead of a hang.
const custodyGateTimeout = 2 * time.Minute

// uploadGateProbe is the finding the "upload-probe" role records for its
// wait: checked when the registry held the upload as the probe started.
const uploadGateProbe = "upload-in-progress"

// uploadGate orders an upload probe run across processes without a sleep.
// The npm registry holds the first request that carries the publish
// credential, which the engine sends only after open, until the probe has
// reported; the probe starts probing only once the registry holds that
// request. The probe therefore probes while the engine holds the credential
// for an upload in progress.
type uploadGate struct {
	server    *httptest.Server
	uploading chan struct{}
	reported  chan struct{}
	done      chan struct{}

	start, report, end sync.Once
	// held records that the registry answered the held request only after
	// the probe reported.
	held atomic.Bool
}

func newUploadGate(t *testing.T) *uploadGate {
	t.Helper()
	gate := &uploadGate{uploading: make(chan struct{}), reported: make(chan struct{}), done: make(chan struct{})}
	gate.server = httptest.NewServer(http.HandlerFunc(gate.serve))
	t.Cleanup(gate.server.Close)
	t.Cleanup(gate.finish)
	return gate
}

// finish ends every pending wait, so a server that closes after a failed run
// does not wait for the timeout.
func (gate *uploadGate) finish() {
	gate.end.Do(func() { close(gate.done) })
}

// wait blocks until ready is closed and reports whether it was: false after
// custodyGateTimeout or once the test ended.
func (gate *uploadGate) wait(ready <-chan struct{}) bool {
	timer := time.NewTimer(custodyGateTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
	case <-gate.done:
	}
	return false
}

// serve answers the probe: GET /uploading once the registry holds the
// upload, POST /reported once the probe wrote its report.
func (gate *uploadGate) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/uploading":
		if !gate.wait(gate.uploading) {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "/reported":
		gate.report.Do(func() { close(gate.reported) })
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// hold passes every registry request to next, and holds the first one that
// carries the publish credential until the probe reported. It answers that
// request 503 when the probe never reports.
func (gate *uploadGate) hold(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := false
		if r.Header.Get("Authorization") == "Bearer "+custodyBearer {
			gate.start.Do(func() {
				first = true
				close(gate.uploading)
			})
		}
		if first {
			if !gate.wait(gate.reported) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			gate.held.Store(true)
		}
		next.ServeHTTP(w, r)
	})
}

// newHeldNPMRegistry is a custodyNPMRegistry whose requests pass gate.hold
// first.
func newHeldNPMRegistry(t *testing.T, gate *uploadGate) *custodyNPMRegistry {
	t.Helper()
	reg := &custodyNPMRegistry{tarballs: map[string]custodyTarball{}}
	reg.server = httptest.NewServer(gate.hold(http.HandlerFunc(reg.serve)))
	t.Cleanup(reg.server.Close)
	t.Cleanup(gate.finish)
	return reg
}

// runUploadProbeRole is a hostile publish step beside the publication job:
// repository code the upload does not wait for. It waits until the registry
// holds the engine's upload, probes for the credential as every hostile role
// does, records whether the upload was in progress, and then lets the upload
// finish. It exits 0 when it could report, so the run completes.
func runUploadProbeRole() int {
	gate := os.Getenv(custodyGateEnv)
	client := &http.Client{Timeout: custodyGateTimeout + time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}
	waited := finding{Role: "upload-probe", Probe: uploadGateProbe}
	response, err := client.Get(gate + "/uploading")
	if err != nil {
		waited.Detail = err.Error()
	} else {
		_ = response.Body.Close()
		waited.Checked, waited.Detail = response.StatusCode == http.StatusOK, response.Status
	}
	code := runHostileRole("upload-probe")
	if err := writeFindings(os.Getenv(custodyReportEnv), []finding{waited}); err != nil {
		fmt.Fprintf(os.Stderr, "custody upload-probe: write report: %v\n", err)
		code = 1
	}
	response, err = client.Post(gate+"/reported", "text/plain", strings.NewReader(""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "custody upload-probe: report to the gate: %v\n", err)
		return 1
	}
	_ = response.Body.Close()
	return code
}

// A probe that runs while the engine uploads finds no credential. The probe
// is a publish step beside the publication job: the upload does not wait for
// it, and it starts probing only once the registry holds the upload, which the
// engine sends with the publish credential after open. The registry answers
// that upload only after the probe reported, so the probe searched its
// environment, the home, workspace and temporary directories, its
// descriptors, and, on Linux, the engine's /proc and a ptrace attach, while
// the engine held the credential.
func TestAProbeDuringAnUploadFindsNoCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/provider-publication", "uploads-run-in-the-engine", "a-probe-during-an-upload-finds-no-credential")
	clitest.RequireShell(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gate := newUploadGate(t)
	npm := newHeldNPMRegistry(t, gate)
	fx := writePublicationFixture(t, self, npm.server.URL, gate.server.URL)
	// The probe waits for the upload in a worker slot of its own.
	code, output := runEngine(t, self, fx.wsRoot, t.TempDir(), false,
		custodyArgsEnv+"=publish\n--all\n--channel\npr-0\n--providers\npublish\n--max-parallel\n4")
	if code != 0 {
		t.Fatalf("publish exit=%d, want 0: %s\n%s", code, failureLines(output), output)
	}

	members, refused := npm.stored()
	if len(members) != 1 || !strings.HasPrefix(members[0], publicationCoordinate+"@") || refused != 0 {
		t.Errorf("the registry stores %v and refused %d requests; want the one member, every request with the publish credential", members, refused)
	}
	if !gate.held.Load() {
		t.Error("the registry answered the upload before the probe reported")
	}
	findings := readFindings(t, fx.probeReport)
	if waited := findings[uploadGateProbe]; !waited.Checked {
		t.Fatalf("the probe did not start while the upload was in progress (detail %q)", waited.Detail)
	}
	assertNoProbeFoundTheBearer(t, "upload-probe", findings)
}
