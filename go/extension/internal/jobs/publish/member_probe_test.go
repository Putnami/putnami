package publish

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/sdk/extension/registrycred"
)

const (
	probeModulePath = "go.putnami.dev/mod"
	probeVersion    = "v1.2.3-canary.1"
	probeZipPath    = "/go.putnami.dev/mod/@v/v1.2.3-canary.1.zip"
	probeTestToken  = "go-probe-test-token"
)

func recordedGoResponse(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("testdata", "recorded", "go-registry", name))
}

// servedZip answers as the Go registry does for a version it serves: the zip
// bytes. The success is built here because the recorded one is a module zip.
func servedZip(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/zip")
		_, _ = w.Write(body)
	})
}

// withoutGoCredential makes every credential source of the publisher answer
// with nothing, so the probe is sent anonymously.
func withoutGoCredential(t *testing.T) {
	t.Helper()
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "")
	original := registrycred.ResolveToken
	registrycred.ResolveToken = func(string) (string, string) { return "", "sign in first" }
	t.Cleanup(func() { registrycred.ResolveToken = original })
}

// goDryRun runs the publisher's dry run and returns the member probes it
// emitted, read through the strict reader. The task must succeed whatever the
// registry answered, and a dry run is never publication evidence.
func goDryRun(t *testing.T, ctx *pctx.Context) []*extproto.MemberProbe {
	t.Helper()
	if ctx.Params == nil {
		ctx.Params = pctx.Params{}
	}
	ctx.Params["dry-run"] = json.RawMessage(`true`)
	var status string
	var err error
	output := capturePublishOutput(t, func() {
		status, _, err = goModule(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("dry run = (%q, %v), want OK whatever the registry answered: %s", status, err, output)
	}
	var probes []*extproto.MemberProbe
	for _, event := range parsePublishEvents(t, output) {
		switch event["kind"] {
		case "published", extproto.PublishedMemberEventKind:
			t.Fatalf("dry run emitted publication evidence: %+v", event)
		case extproto.MemberProbeEventKind:
			probe, err := memberprobe.Decode(event)
			if err != nil {
				t.Fatalf("member-probe event %+v does not pass the strict reader: %v", event, err)
			}
			probes = append(probes, probe)
		}
	}
	return probes
}

func oneGoProbe(t *testing.T, ctx *pctx.Context) *extproto.MemberProbe {
	t.Helper()
	probes := goDryRun(t, ctx)
	if len(probes) != 1 {
		t.Fatalf("dry run emitted %d member probes, want exactly one", len(probes))
	}
	return probes[0]
}

// assertOneZipRead fails unless the registry saw exactly one request, the GET
// of the module zip, carrying wantAuthorization.
func assertOneZipRead(t *testing.T, server *recorded.Server, wantAuthorization string) {
	t.Helper()
	requests := server.Requests()
	if len(requests) != 1 {
		t.Fatalf("the dry run sent %d registry requests, want exactly one", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("the dry run sent %s %s, want GET: a probe never writes", request.Method, request.URL.Path)
	}
	if request.URL.Path != probeZipPath {
		t.Fatalf("the dry run asked %s, want %s", request.URL.Path, probeZipPath)
	}
	if got := request.Header.Get("Authorization"); got != wantAuthorization {
		t.Fatalf("Authorization = %q, want %q: a probe sends a credential only when one resolved", got, wantAuthorization)
	}
}

// The Go module dry run answers the four states from what the registry said,
// names the member, the registry and the version in every verdict, and asks
// with one GET. Every 404 is a response the production registry sent.
func TestGoModuleDryRunProbesTheRegistry(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "dry-run-registry-probe", "go-module-probe-is-read-only")
	cases := []struct {
		name      string
		recording string
		// served is what the registry holds for the version, when it holds it.
		// "staged" stands for the exact bytes the dry run staged.
		served    string
		planned   bool
		anonymous bool

		wantState  string
		wantReason string
	}{
		{name: "a version the registry does not serve is absent", recording: "module-version-not-found.404.http", wantState: extproto.MemberProbeAbsent},
		{name: "a module the registry does not know is absent", recording: "module-not-found.404.http", wantState: extproto.MemberProbeAbsent},
		{
			name: "an anonymous 404 is absent and says it was anonymous", recording: "module-version-not-found.404.http",
			anonymous: true, wantState: extproto.MemberProbeAbsent,
		},
		{name: "the same zip is identical", served: "staged", wantState: extproto.MemberProbeIdentical},
		{
			name: "the same zip read without a credential is identical", served: "staged",
			anonymous: true, wantState: extproto.MemberProbeIdentical,
		},
		{
			name: "another zip is a conflict", served: "another module zip",
			wantState: extproto.MemberProbeConflict, wantReason: "another digest",
		},
		{
			name: "a planned dry run stages no zip, so a served version is a conflict", served: "staged", planned: true,
			wantState: extproto.MemberProbeConflict, wantReason: "built no artifact to compare",
		},
		{
			name: "a planned dry run reports an absent version", recording: "module-version-not-found.404.http", planned: true,
			wantState: extproto.MemberProbeAbsent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, zipBytes, _, stagedDigest := managedGoPublishTestContext(t)
			token := "test-token"
			if tc.anonymous {
				withoutGoCredential(t)
				token = ""
			}
			var server *recorded.Server
			switch {
			case tc.recording != "":
				server = recorded.NewServer(t, nil, recordedGoResponse(t, tc.recording))
			case tc.served == "staged":
				server = recorded.NewServer(t, servedZip(zipBytes))
			default:
				server = recorded.NewServer(t, servedZip([]byte(tc.served)))
			}
			declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)
			wantLocal := stagedDigest
			if tc.planned {
				ctx.Params = managedGoBootstrapParams(t)
				wantLocal = ""
			}

			probe := oneGoProbe(t, ctx)

			if probe.State != tc.wantState {
				t.Fatalf("state = %q (%s), want %q", probe.State, probe.Reason, tc.wantState)
			}
			if probe.Ecosystem != "go" || probe.Coordinate != probeModulePath || probe.Version != probeVersion || probe.Registry != server.URL {
				t.Fatalf("probe = %+v, want the member, the registry and the version", probe)
			}
			if probe.ArtifactDigest != wantLocal || probe.Anonymous != tc.anonymous {
				t.Fatalf("probe = %+v, want local digest %q and anonymous %t", probe, wantLocal, tc.anonymous)
			}
			if !strings.Contains(probe.Reason, tc.wantReason) || (tc.wantReason == "" && probe.Reason != "") {
				t.Fatalf("reason = %q, want %q", probe.Reason, tc.wantReason)
			}
			wantAuthorization := ""
			if token != "" {
				wantAuthorization = "Bearer " + token
			}
			assertOneZipRead(t, server, wantAuthorization)
		})
	}
}

// A dry run with no network access must not pass in silence: a registry that
// refuses the connection is an unverified verdict whose reason says so.
func TestGoModuleDryRunReportsAnUnreachableRegistry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, _, _, _ := managedGoPublishTestContext(t)
	declareGoOrigin(t, ctx.WorkspaceRoot, registry)

	probe := oneGoProbe(t, ctx)

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "the registry could not be reached") {
		t.Fatalf("probe = %+v, want unverified with a reason that names the unreachable registry", probe)
	}
	if probe.Coordinate != probeModulePath || probe.Version != probeVersion || probe.Registry != registry {
		t.Fatalf("probe = %+v, want the member, the registry and the version", probe)
	}
	if strings.Contains(probe.Reason, probeZipPath) || strings.Contains(probe.Reason, "test-token") {
		t.Fatalf("reason %q repeats the request URL or the credential", probe.Reason)
	}
}

// A registry answer that states neither a served version nor an absent one is
// unverified, and the reason carries neither the body's echo of the credential
// nor an unbounded body.
func TestGoRegistryAnswerOutsideTheProtocolIsUnverified(t *testing.T) {
	server := recorded.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "upstream said %s %s", r.Header.Get("Authorization"), strings.Repeat("x", 4096))
	}))
	subject := memberprobe.Subject{Ecosystem: "go", Coordinate: probeModulePath, Version: probeVersion, Registry: server.URL}

	probe := askGoRegistry(subject, server.URL, probeTestToken)

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "502 Bad Gateway") {
		t.Fatalf("probe = %+v, want unverified naming the status", probe)
	}
	if strings.Contains(probe.Reason, probeTestToken) || len(probe.Reason) > 400 {
		t.Fatalf("reason %q carries the credential or is unbounded", probe.Reason)
	}
	if diagnostics := extproto.ValidateMemberProbe(&probe); len(diagnostics) != 0 {
		t.Fatalf("probe %+v is not a valid member-probe: %v", probe, diagnostics)
	}
}

// writeGoChannelRecord stages what a dry-run package leaves behind: the go
// channel record, naming the module and the probed version, and no module.
func writeGoChannelRecord(t *testing.T, workspace string) {
	t.Helper()
	dir := filepath.Join(workspace, ".putnami", "out", "mod", "package", "go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"version":%q,"artifact":%q,"channels":["go"]}`, probeVersion, probeModulePath)
	if err := os.WriteFile(filepath.Join(dir, "channel.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A dry-run package stages no module, only the go channel record. A dry run
// without a release-set plan takes the module and the version from that record,
// so it reaches the registry instead of failing on the module it cannot find. A
// module staged by an earlier run at another version is not compared.
func TestUnplannedGoDryRunProbesWithoutAStagedModule(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "dry-run-registry-probe", "unplanned-dry-run-probes-without-a-staged-module")
	t.Setenv("PUTNAMI_REGISTRY_TOKEN", "test-token")

	t.Run("no staged module", func(t *testing.T) {
		workspace := t.TempDir()
		writeGoChannelRecord(t, workspace)
		server := recorded.NewServer(t, nil, recordedGoResponse(t, "module-version-not-found.404.http"))
		declareGoOrigin(t, workspace, server.URL)

		probe := oneGoProbe(t, &pctx.Context{WorkspaceRoot: workspace, Project: pctx.Project{Name: probeModulePath, Path: "mod"}})

		if probe.State != extproto.MemberProbeAbsent || probe.Coordinate != probeModulePath || probe.Version != probeVersion || probe.ArtifactDigest != "" {
			t.Fatalf("probe = %+v, want absent for the recorded module and version, with no local digest", probe)
		}
		assertOneZipRead(t, server, "Bearer test-token")
	})

	t.Run("a served version cannot be compared", func(t *testing.T) {
		workspace := t.TempDir()
		writeGoChannelRecord(t, workspace)
		server := recorded.NewServer(t, servedZip([]byte("the module the registry serves")))
		declareGoOrigin(t, workspace, server.URL)

		probe := oneGoProbe(t, &pctx.Context{WorkspaceRoot: workspace, Project: pctx.Project{Name: probeModulePath, Path: "mod"}})

		if probe.State != extproto.MemberProbeConflict || !strings.Contains(probe.Reason, "built no artifact to compare") {
			t.Fatalf("probe = %+v, want a conflict the dry run could not compare", probe)
		}
		assertOneZipRead(t, server, "Bearer test-token")
	})

	t.Run("a module staged at another version is not compared", func(t *testing.T) {
		ctx, zipBytes, _, _ := managedGoPublishTestContext(t)
		writeGoPublishMetadata(t, ctx.WorkspaceRoot, "v1.2.3-canary.0",
			filepath.Join(ctx.WorkspaceRoot, "module.zip"), filepath.Join(ctx.WorkspaceRoot, "version.mod"))
		writeGoChannelRecord(t, ctx.WorkspaceRoot)
		server := recorded.NewServer(t, servedZip(zipBytes))
		declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)

		probe := oneGoProbe(t, ctx)

		if probe.Version != probeVersion || probe.State != extproto.MemberProbeConflict || probe.ArtifactDigest != "" {
			t.Fatalf("probe = %+v, want the recorded version and no digest from the stale zip", probe)
		}
		assertOneZipRead(t, server, "Bearer test-token")
	})

	t.Run("a real publish still needs the staged module", func(t *testing.T) {
		workspace := t.TempDir()
		writeGoChannelRecord(t, workspace)
		server := recorded.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a publish with no staged module reached the registry")
		}))
		declareGoOrigin(t, workspace, server.URL)

		status, _, err := goModule(&pctx.Context{WorkspaceRoot: workspace, Project: pctx.Project{Name: probeModulePath, Path: "mod"}}, jsonl.New(), nil)

		if status != "FAILED" || err == nil {
			t.Fatalf("publish = (%q, %v), want FAILED: only a dry run reads the channel record in place of the module", status, err)
		}
	})
}
