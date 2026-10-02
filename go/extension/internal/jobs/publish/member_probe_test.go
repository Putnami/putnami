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

// The Go module dry run answers from what the registry said, under the reuse
// rule of the real publish, names the member, the registry and the version in
// every verdict, and asks with one GET. Every 404 is a response the production
// registry sent.
func TestGoModuleDryRunProbesTheRegistry(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "dry-run-registry-probe", "go-module-probe-is-read-only")
	cases := []struct {
		name      string
		recording string
		// served is what the registry holds for the version, when it holds it.
		// "staged" stands for the exact bytes of the staged zip.
		served    string
		planned   bool
		anonymous bool
		// unstaged removes the module an earlier package staged.
		unstaged bool

		wantState  string
		wantLocal  bool
		wantReason string
	}{
		{name: "a version the registry does not serve is absent", recording: "module-version-not-found.404.http", wantState: extproto.MemberProbeAbsent, wantLocal: true},
		{name: "a module the registry does not know is absent", recording: "module-not-found.404.http", wantState: extproto.MemberProbeAbsent, wantLocal: true},
		{
			name: "an anonymous 404 is absent and says it was anonymous", recording: "module-version-not-found.404.http",
			anonymous: true, wantState: extproto.MemberProbeAbsent, wantLocal: true,
		},
		{
			name: "outside a release set the same zip is a conflict", served: "staged",
			wantState: extproto.MemberProbeConflict, wantLocal: true, wantReason: "fails when the registry has publicly released it",
		},
		{
			name: "outside a release set another zip is a conflict", served: "another module zip",
			wantState: extproto.MemberProbeConflict, wantLocal: true, wantReason: "fails its verification of the served zip",
		},
		{
			name: "under a plan the staged zip is identical", served: "staged", planned: true,
			wantState: extproto.MemberProbeIdentical, wantLocal: true, wantReason: "compared with the module zip the last `package` staged",
		},
		{
			name: "under a plan the staged zip read without a credential is identical", served: "staged", planned: true,
			anonymous: true, wantState: extproto.MemberProbeIdentical, wantLocal: true,
			wantReason: "compared with the module zip the last `package` staged",
		},
		{
			name: "under a plan another zip is a conflict", served: "another module zip", planned: true,
			wantState: extproto.MemberProbeConflict, wantLocal: true, wantReason: "it keeps those bytes when the version is sent again",
		},
		{
			name: "under a plan without a staged zip a served version is a conflict that names the remedy", served: "staged",
			planned: true, unstaged: true,
			wantState: extproto.MemberProbeConflict, wantReason: "run package for this project without --dry-run",
		},
		{
			name: "under a plan without a staged zip an absent version is absent", recording: "module-version-not-found.404.http",
			planned: true, unstaged: true, wantState: extproto.MemberProbeAbsent,
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
			if tc.planned {
				ctx.Params = managedGoBootstrapParams(t)
			}
			if tc.unstaged {
				if err := os.Remove(filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", "mod", "package", "go", "module.json")); err != nil {
					t.Fatal(err)
				}
			}
			wantLocal := ""
			if tc.wantLocal {
				wantLocal = stagedDigest
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

// The zip a dry run compares is the one an earlier package staged. While it is
// the zip the registry serves, the version is reused and the verdict names the
// staged zip, because it may predate the source; once package stages other
// bytes, the same served version is a conflict.
func TestGoModuleDryRunComparesTheStagedZip(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "dry-run-registry-probe", "identical-names-the-staged-zip")
	ctx, zipBytes, _, stagedDigest := managedGoPublishTestContext(t)
	ctx.Params = managedGoBootstrapParams(t)
	server := recorded.NewServer(t, servedZip(zipBytes))
	declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)

	probe := oneGoProbe(t, ctx)
	if probe.State != extproto.MemberProbeIdentical || probe.ArtifactDigest != stagedDigest ||
		probe.Reason != memberprobe.StagedReason("module zip") {
		t.Fatalf("probe = %+v, want identical at %s, naming the staged module zip", probe, stagedDigest)
	}

	restaged := []byte("go module zip bytes a later package staged")
	if err := os.WriteFile(filepath.Join(ctx.WorkspaceRoot, "module.zip"), restaged, 0o644); err != nil {
		t.Fatal(err)
	}
	restagedDigest, err := sha256OfFile(filepath.Join(ctx.WorkspaceRoot, "module.zip"))
	if err != nil {
		t.Fatal(err)
	}
	probe = oneGoProbe(t, ctx)
	if probe.State != extproto.MemberProbeConflict || probe.ArtifactDigest != restagedDigest ||
		probe.RegistryDigest != stagedDigest || probe.Reason != reasonGoOtherDigest {
		t.Fatalf("probe = %+v, want a conflict of the restaged zip %s against the served %s", probe, restagedDigest, stagedDigest)
	}
	for _, request := range server.Requests() {
		if request.Method != http.MethodGet || request.URL.Path != probeZipPath {
			t.Fatalf("the dry run sent %s %s, want only the GET of the module zip", request.Method, request.URL.Path)
		}
	}
	if requests := server.Requests(); len(requests) != 2 {
		t.Fatalf("the two dry runs sent %d registry requests, want one each", len(requests))
	}
}

// A staged zip that cannot be read is no artifact to compare: the probe is
// unverified with the read error, and the registry is asked nothing.
func TestGoModuleDryRunReportsAnUnreadableStagedZip(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "dry-run-registry-probe", "unreadable-staged-zip-is-unverified")
	for name, planned := range map[string]bool{"outside a release set": false, "under a plan": true} {
		t.Run(name, func(t *testing.T) {
			ctx, _, _, _ := managedGoPublishTestContext(t)
			if planned {
				ctx.Params = managedGoBootstrapParams(t)
			}
			if err := os.Remove(filepath.Join(ctx.WorkspaceRoot, "module.zip")); err != nil {
				t.Fatal(err)
			}
			server := recorded.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("a probe without a readable staged zip asked the registry")
			}))
			declareGoOrigin(t, ctx.WorkspaceRoot, server.URL)

			probe := oneGoProbe(t, ctx)

			if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "the staged module zip cannot be read") ||
				!strings.Contains(probe.Reason, "module.zip") {
				t.Fatalf("probe = %+v, want unverified with the read error", probe)
			}
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

	probe := askGoRegistry(subject, server.URL, probeTestToken, true)

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

	t.Run("a served version is a conflict", func(t *testing.T) {
		workspace := t.TempDir()
		writeGoChannelRecord(t, workspace)
		server := recorded.NewServer(t, servedZip([]byte("the module the registry serves")))
		declareGoOrigin(t, workspace, server.URL)

		probe := oneGoProbe(t, &pctx.Context{WorkspaceRoot: workspace, Project: pctx.Project{Name: probeModulePath, Path: "mod"}})

		if probe.State != extproto.MemberProbeConflict || probe.ArtifactDigest != "" || !strings.Contains(probe.Reason, "outside a release set") {
			t.Fatalf("probe = %+v, want a conflict without a local digest", probe)
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
