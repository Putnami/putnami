package deliverycli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// newRebuiltPackageReceiver serves receiver behind the package headers
// headers names, set on every answer in place of the receiver's own.
func newRebuiltPackageReceiver(t *testing.T, receiver *packageReceiver, headers map[string]string) string {
	t.Helper()
	receiver.packages = false
	receiver.arrived = make(chan string, 64)
	receiver.gates = map[string]chan struct{}{}
	receiver.stored = map[string][]byte{}
	receiver.bundles = map[string][]string{}
	if receiver.route == "" {
		receiver.route = "/ingest/sessions"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		receiver.ServeHTTP(w, req)
	}))
	t.Cleanup(func() {
		receiver.releaseAll()
		srv.Close()
	})
	return srv.URL
}

// A receiver whose latest decisive answer carries X-Putnami-Session-Packages:
// 1 and X-Putnami-Session-Packages-Rebuilt: 1 stores the task packages that
// name rebuilt records (CLI ADR 0137): every acknowledgement of an engine that
// named framing version 5 offers packages and packagesRebuilt. An engine of
// version 4, which refuses packagesRebuilt, gets packages alone, as does an
// engine of version 5 whose receiver advertises packages only. The rebuilt
// header alone, or beside packages of another version, offers nothing.
func TestAcksOfferRebuiltPackagesOnlyToAnEngineAndReceiverThatReadThem(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "acks-offer-packages-only-to-a-package-engine-and-receiver")
	stubReporterDenyInspection(t, func() error { return nil })
	both := map[string]string{"X-Putnami-Session-Packages": "1", "X-Putnami-Session-Packages-Rebuilt": "1"}
	for _, tc := range []struct {
		name    string
		framing string
		headers map[string]string
		offered bool
		rebuilt bool
	}{
		{"engine 5 and both headers", "5", both, true, true},
		{"engine 6 and both headers", "6", both, true, true},
		{"engine 4 and both headers", "4", both, true, false},
		{"engine 5 and packages only", "5", map[string]string{"X-Putnami-Session-Packages": "1"}, true, false},
		{"engine 5 and rebuilt only", "5", map[string]string{"X-Putnami-Session-Packages-Rebuilt": "1"}, false, false},
		{"engine 5, packages 2 and rebuilt 1", "5", map[string]string{"X-Putnami-Session-Packages": "2", "X-Putnami-Session-Packages-Rebuilt": "1"}, false, false},
		{"engine 5, packages 1 and rebuilt 2", "5", map[string]string{"X-Putnami-Session-Packages": "1", "X-Putnami-Session-Packages-Rebuilt": "2"}, true, false},
		{"engine 3 and both headers", "3", both, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newRebuiltPackageReceiver(t, &packageReceiver{token: packageToken, contract: "2"}, tc.headers)
			env := map[string]string{
				SessionReporterIngestURLEnv:            url + "/ingest",
				protocolcli.SessionReporterTokenEnv:    packageToken,
				protocolcli.SessionReportingFramingEnv: tc.framing,
			}
			attempt := frame(t, packageSession, "plan.json", 0, 0, "{}", false)
			var out strings.Builder
			if err := serveSessionReporter(env, nil, strings.NewReader(attempt+attempt), &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("acks %q, want 2", lines)
			}
			for i, line := range lines {
				if !tc.rebuilt && strings.Contains(line, "packagesRebuilt") {
					t.Fatalf("ack %d = %s: an engine or a receiver that reads no rebuilt records got the member", i, line)
				}
				ack, err := protocolcli.ParseSessionReportingAck([]byte(line))
				if err != nil {
					t.Fatalf("ack %s: %v", line, err)
				}
				if !ack.OK || ack.Packages != tc.offered || ack.PackagesRebuilt != tc.rebuilt {
					t.Fatalf("ack %d = %+v, want packages %v and packagesRebuilt %v", i, ack, tc.offered, tc.rebuilt)
				}
			}
		})
	}

	for value, want := range map[string]bool{"5": true, "\t5\n": true, "6": true, "4": false, "3": false, "": false, "five": false} {
		if got := rebuiltPackagesRequested(map[string]string{protocolcli.SessionReportingFramingEnv: value}); got != want {
			t.Errorf("rebuiltPackagesRequested(%q) = %v, want %v", value, got, want)
		}
	}
	for _, tc := range []struct {
		packages, rebuilt string
		want              bool
	}{
		{"1", "1", true},
		{"1", "\t1\n", true},
		{"1", "", false},
		{"1", "2", false},
		{"", "1", false},
		{"2", "1", false},
	} {
		header := http.Header{}
		header.Set("X-Putnami-Session-Packages", tc.packages)
		header.Set("X-Putnami-Session-Packages-Rebuilt", tc.rebuilt)
		if got := advertisesRebuiltPackages(header); got != tc.want {
			t.Errorf("advertisesRebuiltPackages(packages %q, rebuilt %q) = %v, want %v", tc.packages, tc.rebuilt, got, tc.want)
		}
	}
	var c frameCap
	header := http.Header{}
	header.Set("X-Putnami-Session-Packages", "1")
	header.Set("X-Putnami-Session-Packages-Rebuilt", "1")
	c.observe("https://receiver", http.StatusOK, header)
	if !c.offersPackages("https://receiver") || !c.offersRebuiltPackages("https://receiver") {
		t.Fatalf("an answer of both headers = %+v, want packages and rebuilt records offered", c)
	}
	c.observe("https://receiver", http.StatusServiceUnavailable, http.Header{})
	if !c.offersRebuiltPackages("https://receiver") {
		t.Fatal("a gateway answer withdrew the offer of the latest decisive answer")
	}
	c.observe("https://receiver", http.StatusOK, http.Header{"X-Putnami-Session-Packages": {"1"}})
	if !c.offersPackages("https://receiver") || c.offersRebuiltPackages("https://receiver") {
		t.Fatal("a decisive answer without the rebuilt header kept the rebuilt offer")
	}
	c.observe("https://receiver", 0, nil)
	if c.offersPackages("https://receiver") || c.offersRebuiltPackages("https://receiver") {
		t.Fatal("a request without an answer kept the offer")
	}
}
