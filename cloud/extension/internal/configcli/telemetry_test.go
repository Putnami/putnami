package configcli

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const telemetryPath = "/v1/workspaces/ws-acme/config/telemetry"

// telemetryServicePattern is the service name config-api accepts, a
// workload's canonical name, and telemetryInvalid the reason it gives for any
// value it refuses.
var telemetryServicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(?:/[a-z0-9][a-z0-9._-]*)*$`)

const telemetryInvalid = "trace budget and floor are 0 to 1000000, trace ratio 0 to 1, trace parent follow or decide, log level debug, info, warn or error, and a service a workload's canonical name"

// telemetryClock fixes the clock `set --for` counts from for one test.
func telemetryClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := telemetryNow
	telemetryNow = func() time.Time { return now }
	t.Cleanup(func() { telemetryNow = previous })
}

// telemetryFake serves config-api's telemetry settings route over the rows it
// keeps, so a write shows in the next read, as config-api resolves them. It
// refuses what config-api refuses, with config-api's legacy error body.
type telemetryFake struct {
	t       *testing.T
	rows    map[string]map[string]any // service ("" for the workspace) → fields
	writes  []map[string]any
	refuse  int
	message string
}

func newTelemetryFake(t *testing.T) *telemetryFake {
	return &telemetryFake{t: t, rows: map[string]map[string]any{}}
}

func (f *telemetryFake) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	switch {
	case req.URL.Path == "/.well-known/openid-configuration":
		return jsonResponse(http.StatusOK, map[string]any{
			"issuer": testBaseURL, "token_endpoint": testBaseURL + "/token",
			"userinfo_endpoint": testBaseURL + "/userinfo",
		}), nil
	case req.URL.Path == "/token":
		return jsonResponse(http.StatusOK, tokenResponse(map[string]any{"scope_ref": map[string]any{"workspace_id": "ws-acme"}})), nil
	case req.URL.Path != telemetryPath:
		f.t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		return nil, nil
	}
	if f.refuse != 0 {
		return jsonResponse(f.refuse, map[string]any{"code": "forbidden", "message": f.message}), nil
	}
	if req.Header.Get("Authorization") == "" {
		f.t.Errorf("%s %s without the caller's bearer", req.Method, req.URL.Path)
	}
	if req.Method == http.MethodPut {
		var write map[string]any
		if err := json.Unmarshal(body, &write); err != nil {
			f.t.Fatalf("decode write: %v", err)
		}
		f.writes = append(f.writes, write)
		service := clicore.StringValue(write["service"])
		if service != "" && !telemetryServicePattern.MatchString(service) {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": telemetryInvalid}), nil
		}
		fields := map[string]any{}
		for _, key := range []string{"traceBudget", "traceRatio", "traceFloorPerMinute", "traceParent", "logLevel"} {
			if value, ok := write[key]; ok && value != nil {
				fields[key] = value
			}
		}
		// A write that sets no setting removes the row, whatever its expiry.
		if len(fields) == 0 {
			delete(f.rows, service)
		} else {
			if expires, ok := write["expiresAt"]; ok && expires != nil {
				fields["expiresAt"] = expires
			}
			f.rows[service] = fields
		}
	}
	return jsonResponse(http.StatusOK, f.view()), nil
}

func (f *telemetryFake) view() map[string]any {
	workspace := f.rows[""]
	view := map[string]any{"workspaceId": "ws-acme", "workspace": map[string]any{}, "services": []any{}}
	if workspace != nil {
		view["workspace"] = workspace
		view["workspaceUpdatedAt"] = "2026-05-19T09:00:00Z"
	}
	var services []any
	for _, name := range []string{"surfaces/workloads/app", "my-app"} {
		override, ok := f.rows[name]
		if !ok {
			continue
		}
		effective := map[string]any{}
		for k, v := range workspace {
			effective[k] = v
		}
		for k, v := range override {
			effective[k] = v
		}
		services = append(services, map[string]any{
			"service": name, "override": override, "effective": effective, "updatedAt": "2026-05-19T09:00:00Z",
		})
	}
	if services != nil {
		view["services"] = services
	}
	return view
}

func telemetryIO(t *testing.T, fake *telemetryFake) (clicore.IO, *[]string) {
	t.Helper()
	home := t.TempDir()
	workspaceRoot := t.TempDir()
	writeTestAuth(t, home)
	writeLinkFile(t, workspaceRoot)
	ioctx, stdout, _ := commonIO(t, home, &http.Client{Transport: fake})
	ioctx.Env["PUTNAMI_WORKSPACE_ROOT"] = workspaceRoot
	return ioctx, stdout
}

func runTelemetry(ioctx clicore.IO, args ...string) error {
	return runDomain(Telemetry, ioctx, args)
}

func TestTelemetryShow_PrintsWorkspaceAndServices(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "show-prints-the-rows", "telemetry-show-prints-workspace-and-services")
	fake := newTelemetryFake(t)
	fake.rows[""] = map[string]any{"traceRatio": 0.5}
	fake.rows["my-app"] = map[string]any{"traceBudget": 1000, "traceParent": "decide", "logLevel": "warn", "expiresAt": "2026-05-19T10:00:00Z"}
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "show"); err != nil {
		t.Fatalf("show: %v", err)
	}
	text := strings.Join(*stdout, "\n")
	assertContains(t, text, "Telemetry settings of workspace ws-acme. An unset field keeps the service's own setting.")
	assertContains(t, text, "workspace: ratio 0.5 (updated 2026-05-19T09:00:00Z)")
	assertContains(t, text, "my-app: budget 1000, parent decide, log level warn until 2026-05-19T10:00:00Z (updated 2026-05-19T09:00:00Z)")
	assertContains(t, text, "  in force: budget 1000, ratio 0.5, parent decide, log level warn until 2026-05-19T10:00:00Z")
	if len(fake.writes) != 0 {
		t.Fatalf("show wrote %v", fake.writes)
	}
}

func TestTelemetryShow_AppPrintsOneService(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "show-prints-the-rows", "telemetry-show-app-prints-one-service")
	fake := newTelemetryFake(t)
	fake.rows["my-app"] = map[string]any{"traceRatio": 1}
	fake.rows["surfaces/workloads/app"] = map[string]any{"traceRatio": 0.2}
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "show", "--app", "surfaces/workloads/app"); err != nil {
		t.Fatalf("show: %v", err)
	}
	text := strings.Join(*stdout, "\n")
	assertContains(t, text, "workspace: nothing set")
	assertContains(t, text, "surfaces/workloads/app: ratio 0.2")
	if strings.Contains(text, "my-app") {
		t.Fatalf("show --app printed another service:\n%s", text)
	}

	*stdout = nil
	if err := runTelemetry(ioctx, "show", "--app", "billing"); err != nil {
		t.Fatalf("show: %v", err)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "billing: no row; it takes the workspace row")
}

func TestTelemetryShow_JSONPrintsTheView(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "show-prints-the-rows", "telemetry-show-json-prints-the-view")
	fake := newTelemetryFake(t)
	fake.rows[""] = map[string]any{"traceFloorPerMinute": 5}
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "show", "--output=json"); err != nil {
		t.Fatalf("show: %v", err)
	}
	var view TelemetryView
	decodeJSON(t, strings.Join(*stdout, "\n"), &view)
	if view.WorkspaceID != "ws-acme" || view.Workspace.TraceFloorPerMinute == nil || *view.Workspace.TraceFloorPerMinute != 5 ||
		view.Workspace.TraceRatio != nil || len(view.Services) != 0 {
		t.Fatalf("view = %+v", view)
	}
}

func TestTelemetrySet_MergesFlagsIntoTheRow(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "set-merges-into-the-row", "telemetry-set-merges-flags-into-the-row")
	fake := newTelemetryFake(t)
	fake.rows["my-app"] = map[string]any{"traceFloorPerMinute": 2, "logLevel": "warn"}
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "set", "--app", "my-app", "--ratio", "1", "--budget", "500", "--parent", "DECIDE", "--log-level", "ERROR"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %v, want one", fake.writes)
	}
	write := fake.writes[0]
	if write["service"] != "my-app" || write["traceRatio"] != 1.0 || write["traceBudget"] != 500.0 ||
		write["traceFloorPerMinute"] != 2.0 || write["traceParent"] != "decide" || write["logLevel"] != "error" ||
		write["expiresAt"] != nil {
		t.Fatalf("write = %v, want the flags merged into the stored row, with no expiry", write)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "my-app: budget 500, ratio 1, floor 2/min, parent decide, log level error")

	// Without --app, set writes the workspace row and sends no service.
	if err := runTelemetry(ioctx, "set", "--floor", "0"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if write := fake.writes[1]; write["service"] != nil || write["traceFloorPerMinute"] != 0.0 || len(write) != 1 {
		t.Fatalf("workspace write = %v", write)
	}
}

func TestTelemetrySet_RefusesValuesOutOfRange(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "set-merges-into-the-row", "telemetry-set-refuses-values-out-of-range")
	for _, args := range [][]string{
		{"--ratio", "1.5"},
		{"--ratio", "-0.1"},
		{"--ratio", "half"},
		{"--budget", "-1"},
		{"--budget", "1.5"},
		{"--budget", "1000001"},
		{"--floor", "x"},
		{"--log-level", "fatal"},
		{"--parent", "always"},
		{"--parent", ""},
		{"--for", "0s"},
		{"--for", "-1h"},
		{"--for", "soon"},
		{"--for", "60"},
		{"--ratio", "1", "--for", "1d"},
		{"--ratio"},
	} {
		fake := newTelemetryFake(t)
		ioctx, _ := telemetryIO(t, fake)
		err := runTelemetry(ioctx, append([]string{"set"}, args...)...)
		if err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("set %v = %v, want a usage error", args, err)
		}
		if len(fake.writes) != 0 {
			t.Fatalf("set %v wrote %v", args, fake.writes)
		}
	}
}

func TestTelemetrySet_NeedsAFlag(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "set-merges-into-the-row", "telemetry-set-needs-a-flag")
	fake := newTelemetryFake(t)
	ioctx, _ := telemetryIO(t, fake)
	err := runTelemetry(ioctx, "set", "--app", "my-app")
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "at least one of") {
		t.Fatalf("set = %v, want a usage error naming the flags", err)
	}
}

func TestTelemetryReset_RemovesTheRow(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "reset-removes-the-row", "telemetry-reset-removes-the-row")
	fake := newTelemetryFake(t)
	fake.rows[""] = map[string]any{"traceRatio": 0.5}
	fake.rows["my-app"] = map[string]any{"traceRatio": 1}
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "reset", "--app", "my-app"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if write := fake.writes[0]; write["service"] != "my-app" || len(write) != 1 {
		t.Fatalf("write = %v, want the service alone", write)
	}
	if _, ok := fake.rows["my-app"]; ok {
		t.Fatal("reset kept the service row")
	}
	assertContains(t, strings.Join(*stdout, "\n"), "my-app: no row; it takes the workspace row")

	if err := runTelemetry(ioctx, "reset"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(fake.rows) != 0 {
		t.Fatalf("rows = %v after resetting the workspace row", fake.rows)
	}
}

func TestTelemetryApp_NamesTheServiceRow(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "app-names-the-service", "telemetry-app-names-the-service-row")
	fake := newTelemetryFake(t)
	ioctx, stdout := telemetryIO(t, fake)
	workspaceRoot := ioctx.Env["PUTNAMI_WORKSPACE_ROOT"]
	writeProjectFile(t, workspaceRoot, "surfaces/workloads/app")

	// The tail of a local project resolves to its canonical name, folders
	// included.
	if err := runTelemetry(ioctx, "set", "--app", "app", "--ratio", "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if service := fake.writes[0]["service"]; service != "surfaces/workloads/app" {
		t.Fatalf("service = %v, want the app's canonical name", service)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "surfaces/workloads/app: ratio 1")
	// Without --app, the command never takes the cwd project.
	if err := runTelemetry(ioctx, "set", "--ratio", "1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if service, ok := fake.writes[1]["service"]; ok {
		t.Fatalf("service = %v, want the workspace row", service)
	}
	// A refusal carries config-api's reason.
	err := runTelemetry(ioctx, "reset", "--app", "Billing")
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "cloud telemetry reset: "+telemetryInvalid) {
		t.Fatalf("reset = %v, want config-api's reason", err)
	}
}

func TestTelemetryFor_SetsTheExpiry(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "for-ends-the-row", "telemetry-for-sets-the-expiry")
	telemetryClock(t, time.Date(2026, 11, 21, 18, 30, 15, 500, time.FixedZone("CET", 3600)))
	fake := newTelemetryFake(t)
	ioctx, stdout := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "set", "--app", "my-app", "--ratio", "1", "--parent", "decide", "--for", "1h"); err != nil {
		t.Fatalf("set: %v", err)
	}
	write := fake.writes[0]
	if write["traceRatio"] != 1.0 || write["traceParent"] != "decide" || write["expiresAt"] != "2026-11-21T18:30:15Z" {
		t.Fatalf("write = %v, want the settings and an expiry an hour after the command, in UTC", write)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "my-app: ratio 1, parent decide until 2026-11-21T18:30:15Z")
}

func TestTelemetrySet_KeepsTheExpiry(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "for-ends-the-row", "telemetry-set-keeps-the-expiry")
	fake := newTelemetryFake(t)
	fake.rows["my-app"] = map[string]any{"traceRatio": 1, "expiresAt": "2026-11-21T19:00:00Z"}
	ioctx, _ := telemetryIO(t, fake)

	// A change to a temporary row never makes it permanent.
	if err := runTelemetry(ioctx, "set", "--app", "my-app", "--log-level", "warn"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if write := fake.writes[0]; write["traceRatio"] != 1.0 || write["logLevel"] != "warn" || write["expiresAt"] != "2026-11-21T19:00:00Z" {
		t.Fatalf("write = %v, want the row's expiry kept", write)
	}
}

func TestTelemetryFor_AloneMovesTheExpiry(t *testing.T) {
	spectest.Proves(t, "cloud/telemetry-settings", "for-ends-the-row", "telemetry-for-alone-moves-the-expiry")
	telemetryClock(t, time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC))
	fake := newTelemetryFake(t)
	fake.rows[""] = map[string]any{"traceRatio": 0.5, "expiresAt": "2026-11-21T18:10:00Z"}
	ioctx, _ := telemetryIO(t, fake)

	if err := runTelemetry(ioctx, "set", "--for", "2h"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if write := fake.writes[0]; write["traceRatio"] != 0.5 || write["expiresAt"] != "2026-11-21T20:00:00Z" || len(write) != 2 {
		t.Fatalf("write = %v, want the row kept and its expiry moved", write)
	}

	// A row that sets nothing has nothing to end: the command reads it and
	// writes nothing.
	err := runTelemetry(ioctx, "set", "--app", "my-app", "--for", "1h")
	if err == nil || clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "the row of my-app sets nothing to end") {
		t.Fatalf("set = %v, want a usage error", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %v, want none for the empty row", fake.writes[1:])
	}
}

func TestTelemetry_RefusalsNameTheVerbAndTheScope(t *testing.T) {
	fake := newTelemetryFake(t)
	fake.refuse, fake.message = http.StatusForbidden, "insufficient workspace permissions"
	ioctx, _ := telemetryIO(t, fake)
	err := runTelemetry(ioctx, "set", "--ratio", "1")
	if err == nil || clicore.ExitCode(err) != clicore.ExitAuth ||
		!strings.Contains(err.Error(), "cloud telemetry set: insufficient workspace permissions") ||
		!strings.Contains(err.Error(), "platform.workspace.manage") {
		t.Fatalf("set = %v, want an auth refusal naming the scope", err)
	}
	err = runTelemetry(ioctx, "show")
	if err == nil || !strings.Contains(err.Error(), "platform.workspace.read") {
		t.Fatalf("show = %v, want the read scope", err)
	}
}

func TestTelemetry_HelpAndUnknownVerbs(t *testing.T) {
	fake := newTelemetryFake(t)
	ioctx, stdout := telemetryIO(t, fake)
	if err := runTelemetry(ioctx); err != nil {
		t.Fatalf("help: %v", err)
	}
	assertContains(t, strings.Join(*stdout, "\n"), "putnami cloud telemetry set [--app <app>]")
	if err := runTelemetry(ioctx, "raise"); err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("raise = %v, want a usage error", err)
	}
	if err := runTelemetry(ioctx, "show", "my-app"); err == nil || !strings.Contains(err.Error(), "--app") {
		t.Fatalf("show my-app = %v, want a usage error naming --app", err)
	}
}
