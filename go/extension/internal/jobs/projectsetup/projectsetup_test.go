package projectsetup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/modfile"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/registrycred"
)

func TestMain(m *testing.M) {
	jobtest.ServeFake()
	os.Exit(m.Run())
}

// projectPath is the directory of the created project the tests set up.
const projectPath = "apps/api"

// frameworkGoMod is the go.mod a template renders: each framework module at
// the placeholder version.
const frameworkGoMod = "module example.com/api\n\ngo 1.22\n\nrequire (\n" +
	"\tgo.putnami.dev/app v0.0.0\n\tgo.putnami.dev/sdk v0.0.0\n)\n"

// frameworkMain imports both framework modules, so go mod tidy keeps them.
const frameworkMain = "package main\n\nimport (\n\t_ \"go.putnami.dev/app\"\n\t_ \"go.putnami.dev/sdk\"\n)\n\nfunc main() {}\n"

// setupRun is one project-setup run under test.
type setupRun struct {
	rec    *jobtest.Recorder
	status string
	// refreshed are the hosts the job asked to refresh a credential for.
	refreshed []string
}

// runSetup runs the job in-process on ws with the job parameters params.
// proxy is the GOPROXY the job reads; extra environment entries win. PATH holds
// the shell traps, then the directory of the real go command, unless extra
// sets it. client, when set, is the job's HTTP client.
func runSetup(t *testing.T, ws, params, proxy string, client *http.Client, extra ...string) *setupRun {
	t.Helper()
	return runSetupRefreshing(t, ws, params, proxy, client, "debug", "refreshed the credential for ", extra...)
}

// runSetupRefreshing is runSetup with the outcome the credential refresh
// reports: the level, and the message, which ends with the host.
func runSetupRefreshing(t *testing.T, ws, params, proxy string, client *http.Client, level, message string, extra ...string) *setupRun {
	t.Helper()
	fakes := jobtest.NewFakes(t)
	path := fakes.Dir
	if goBinary, err := exec.LookPath("go"); err == nil {
		path += string(filepath.ListSeparator) + filepath.Dir(goBinary)
	}
	overrides := append(append(fakes.Environ(), "PATH="+path), extra...)
	env := jobtest.Env(t, proxy, jobtest.CacheRoot(t), overrides...)
	rec := &jobtest.Recorder{}
	j, stdout, _ := jobtest.NewJob(t, rec, env, ws)

	run := &setupRun{rec: rec}
	var mu sync.Mutex
	refresh := func(_ context.Context, workspaceRoot, host string) registrycred.Outcome {
		mu.Lock()
		defer mu.Unlock()
		if workspaceRoot != ws {
			t.Errorf("refresh got workspace %s, want %s", workspaceRoot, ws)
		}
		run.refreshed = append(run.refreshed, host)
		return registrycred.Outcome{Level: level, Message: message + host}
	}
	if client == nil {
		client = newClient()
	}
	s := &setup{Job: j, refresh: refresh, client: client}
	run.status = s.run(jobtest.Params(t, params), filepath.Join(ws, "context.json"))
	if stdout.Len() > 0 {
		t.Errorf("a command wrote to the job event stream:\n%s", stdout)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("project-setup started shell programs: %v", invocations)
	}
	return run
}

// errorDiagnostics are the messages of the error diagnostics rec holds.
func errorDiagnostics(rec *jobtest.Recorder) []string {
	var messages []string
	for _, event := range rec.Events() {
		if event.Kind == "diagnostic" && event.Level == "error" {
			messages = append(messages, event.Message)
		}
	}
	return messages
}

func (r *setupRun) requireStatus(t *testing.T, want string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("project-setup = %s, want %s:\n%s", r.status, want, r.rec.Transcript())
	}
}

func (r *setupRun) requireTranscript(t *testing.T, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !r.rec.Contains(want) {
			t.Errorf("the transcript does not say %q:\n%s", want, r.rec.Transcript())
		}
	}
}

// frameworkProxy is a file:// module proxy that serves both framework modules
// at version, as latest and as the candidate channel.
func frameworkProxy(t *testing.T, version string) string {
	t.Helper()
	dir := jobtest.WriteModuleProxy(t, jobtest.SimpleProxyModules(version, "go.putnami.dev/app", "go.putnami.dev/sdk")...)
	for _, module := range []string{"go.putnami.dev/app", "go.putnami.dev/sdk"} {
		jobtest.WriteLatestInfo(t, dir, module, version)
		jobtest.WriteChannelInfo(t, dir, module, "candidate", version)
	}
	return jobtest.FileURL(dir)
}

// requirements are the module versions the go.mod at path requires.
func requirements(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := modfile.Parse(path, []byte(jobtest.ReadFile(t, path)), nil)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]string{}
	for _, require := range file.Require {
		required[require.Mod.Path] = require.Mod.Version
	}
	return required
}

// project-setup adds the created project to go.work, creating it when the
// workspace has none, gives go.work and the project's go.mod the workspace's
// go directive, pins each framework module the template requires at v0.0.0 to
// the version the channel names, and tidies the module.
func TestProjectSetupPinsTheFrameworkAndAddsTheProject(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-sets-up-a-go-project",
		"setup-adds-the-project-pins-the-framework-and-tidies")
	jobtest.RequireGo(t)
	tests := []struct {
		name   string
		goWork string
		params string
		// wantGo is the go directive go.work and go.mod end with.
		wantGo string
	}{
		{"a new workspace on latest", "", `{"path":"apps/api"}`, "1.22"},
		{"an existing workspace on a channel", "go 1.23\n", `{"path":"apps/api","putnami-channel":"candidate"}`, "1.23"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
			jobtest.WriteFile(t, ws, projectPath+"/main.go", frameworkMain)
			if tt.goWork != "" {
				jobtest.WriteFile(t, ws, "go.work", tt.goWork)
			}

			run := runSetup(t, ws, tt.params, frameworkProxy(t, "v0.4.2"), nil)
			run.requireStatus(t, statusOK)
			run.requireTranscript(t, "Added apps/api to go.work")

			goMod := filepath.Join(ws, projectPath, "go.mod")
			required := requirements(t, goMod)
			for _, module := range []string{"go.putnami.dev/app", "go.putnami.dev/sdk"} {
				if required[module] != "v0.4.2" {
					t.Errorf("go.mod requires %s %s, want v0.4.2:\n%s", module, required[module], jobtest.ReadFile(t, goMod))
				}
			}
			if sum := jobtest.ReadFile(t, filepath.Join(ws, projectPath, "go.sum")); !strings.Contains(sum, "go.putnami.dev/app v0.4.2") {
				t.Errorf("go.sum does not hold the pinned framework:\n%s", sum)
			}
			work, err := modfile.ParseWork("go.work", []byte(jobtest.ReadFile(t, filepath.Join(ws, "go.work"))), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(work.Use) != 1 || work.Use[0].Path != "./apps/api" || work.Go == nil || work.Go.Version != tt.wantGo {
				t.Errorf("go.work uses %v at go %v, want ./apps/api at go %s", work.Use, work.Go, tt.wantGo)
			}
			if got := workspacejob.GoDirective(goMod); got != tt.wantGo {
				t.Errorf("go.mod states go %s, want %s", got, tt.wantGo)
			}
		})
	}
}

// When nothing serves the version the channel names, the setup fails naming
// the module, the channel and each source it asked, and it edits neither
// go.work nor go.mod.
func TestProjectSetupWritesNothingWhenNoSourceAnswers(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"setup-writes-nothing-when-no-source-answers")
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
	jobtest.WriteFile(t, ws, projectPath+"/main.go", frameworkMain)
	empty := jobtest.FileURL(jobtest.WriteModuleProxy(t))

	run := runSetup(t, ws, `{"path":"apps/api","putnami-channel":"candidate"}`, empty, nil)
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "resolve the version of go.putnami.dev/app on channel candidate", empty, "Set GOPROXY")
	if got := jobtest.ReadFile(t, goMod); got != frameworkGoMod {
		t.Errorf("go.mod changed:\n%s", got)
	}
	if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, "go.work")); ok {
		t.Error("the failed setup wrote go.work")
	}
}

// A tidy that fails fails the setup, quoting the end of go's output; the
// project stays in go.work with its framework pinned, so a fix and a second
// create resume from there.
func TestProjectSetupFailsWhenGoModTidyFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-fails-when-go-mod-tidy-fails",
		"setup-quotes-go-when-go-mod-tidy-fails")
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
	jobtest.WriteFile(t, ws, projectPath+"/main.go",
		"package main\n\nimport (\n\t_ \"go.putnami.dev/app\"\n\t_ \"example.com/missing/pkg\"\n)\n\nfunc main() {}\n")

	run := runSetup(t, ws, `{"path":"apps/api"}`, frameworkProxy(t, "v0.4.2"), nil)
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "go mod tidy failed in apps/api", "example.com/missing")
	if required := requirements(t, goMod); required["go.putnami.dev/app"] != "v0.4.2" {
		t.Errorf("go.mod requires go.putnami.dev/app %s, want the pin v0.4.2 kept", required["go.putnami.dev/app"])
	}
	if work := jobtest.ReadFile(t, filepath.Join(ws, "go.work")); !strings.Contains(work, "./apps/api") {
		t.Errorf("go.work does not keep the project:\n%s", work)
	}
}

// On a host without a go command and with no Go pinned, the setup writes the
// go.work the workspace lock pins Go from and fails naming putnami install,
// which pins and installs that Go. It claims no install of its own.
func TestProjectSetupWithoutGoWritesGoWorkAndNamesPutnamiInstall(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-runs-the-pinned-go",
		"setup-writes-go-work-and-names-putnami-install-without-go")
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)

	run := runSetup(t, ws, `{"path":"apps/api"}`, "off", nil, "PATH="+t.TempDir())
	run.requireStatus(t, statusFailed)
	run.requireTranscript(t, "Initialized go.work",
		"Cannot set apps/api up: no go command is available; run `putnami install`")
	if run.rec.Contains("Installing Go") {
		t.Errorf("the setup claims to install a Go it does not install:\n%s", run.rec.Transcript())
	}
	if errors := errorDiagnostics(run.rec); len(errors) != 1 {
		t.Errorf("a missing Go emits %d error diagnostics, want the one that names putnami install: %q", len(errors), errors)
	}
	if got, want := jobtest.ReadFile(t, filepath.Join(ws, "go.work")), "go 1.22\n\nuse ./apps/api\n"; got != want {
		t.Errorf("go.work = %q, want %q", got, want)
	}
}

// The prepare stage writes a missing go.work with the bytes `go work init`,
// `go work use` and `go work edit -go` leave, keeps an existing one, and runs
// no go command, even where one is on PATH.
func TestProjectSetupPrepareStageWritesGoWorkAndRunsNoGo(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-runs-the-pinned-go",
		"the-prepare-stage-writes-go-work-and-runs-no-go")
	tests := []struct {
		name, goWork, want string
	}{
		{"a new workspace", "", "go 1.22\n\nuse ./apps/api\n"},
		{"an existing go.work", "go 1.23\n\nuse ./apps/web\n", "go 1.23\n\nuse ./apps/web\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := jobtest.RealTempDir(t)
			goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
			if tt.goWork != "" {
				jobtest.WriteFile(t, ws, "go.work", tt.goWork)
			}
			fakes := jobtest.NewFakes(t)
			fakes.RecordingGo(t, filepath.Join(t.TempDir(), "go"))

			run := runSetup(t, ws, `{"path":"apps/api","stage":"prepare"}`, "off", nil,
				append(fakes.Environ(), "PATH="+fakes.Dir)...)
			run.requireStatus(t, statusOK)
			if records := fakes.GoRecords(t); len(records) != 0 {
				t.Errorf("the prepare stage ran go %d times: %+v", len(records), records)
			}
			if got := jobtest.ReadFile(t, filepath.Join(ws, "go.work")); got != tt.want {
				t.Errorf("go.work = %q, want %q", got, tt.want)
			}
			if got := jobtest.ReadFile(t, goMod); got != frameworkGoMod {
				t.Errorf("the prepare stage edited go.mod:\n%s", got)
			}
		})
	}
}

// The setup asks the lookup for the channel it receives, as given: stable is
// the go version query @v/stable.info, not @latest.
func TestProjectSetupAsksTheChannelItReceives(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-resolves-the-go-framework-version",
		"the-channel-param-is-asked-as-given")
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
	jobtest.WriteFile(t, ws, projectPath+"/main.go", frameworkMain)
	modules := append(jobtest.SimpleProxyModules("v0.4.2", "go.putnami.dev/app", "go.putnami.dev/sdk"),
		jobtest.SimpleProxyModules("v0.3.0", "go.putnami.dev/app", "go.putnami.dev/sdk")...)
	dir := jobtest.WriteModuleProxy(t, modules...)
	for _, module := range []string{"go.putnami.dev/app", "go.putnami.dev/sdk"} {
		jobtest.WriteLatestInfo(t, dir, module, "v0.4.2")
		jobtest.WriteChannelInfo(t, dir, module, "stable", "v0.3.0")
	}

	run := runSetup(t, ws, `{"path":"apps/api","putnami-channel":"stable"}`, jobtest.FileURL(dir), nil)
	run.requireStatus(t, statusOK)
	required := requirements(t, goMod)
	for _, module := range []string{"go.putnami.dev/app", "go.putnami.dev/sdk"} {
		if required[module] != "v0.3.0" {
			t.Errorf("go.mod requires %s %s, want v0.3.0 from @v/stable.info:\n%s", module, required[module], jobtest.ReadFile(t, goMod))
		}
	}
}

// A framework module the go command reads from its origin only gets its netrc
// credential refreshed first, once per host, and the lookup reads the origin:
// the settings come from the job's go command.
func TestProjectSetupRefreshesThePrivateOriginCredentialOnce(t *testing.T) {
	t.Parallel()
	jobtest.RequireGo(t)
	ws := jobtest.RealTempDir(t)
	goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)

	var mu sync.Mutex
	var asked []string
	origin := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		body, status := "", http.StatusNotFound
		switch r.URL.String() {
		case "https://go.putnami.dev/app?go-get=1":
			body, status = `<meta name="go-import" content="go.putnami.dev/app mod https://modules.example/go">`, http.StatusOK
		case "https://modules.example/go/go.putnami.dev/app/@latest":
			body, status = latestAnswer("v0.4.2"), http.StatusOK
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}

	run := runSetup(t, ws, `{"path":"apps/api"}`, "off", origin, "GONOPROXY=go.putnami.dev")
	run.requireStatus(t, statusFailed)
	if len(run.refreshed) != 1 || run.refreshed[0] != "go.putnami.dev" {
		t.Fatalf("refreshed %v, want go.putnami.dev once", run.refreshed)
	}
	run.requireTranscript(t, "refreshed the credential for go.putnami.dev",
		"Resolved go.putnami.dev/app v0.4.2 from direct (https://modules.example/go)",
		"resolve the version of go.putnami.dev/sdk", "netrc")
	want := []string{
		"https://go.putnami.dev/app?go-get=1",
		"https://modules.example/go/go.putnami.dev/app/@latest",
		"https://go.putnami.dev/sdk?go-get=1",
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the lookup asked %v, want %v", asked, want)
	}
	if got := jobtest.ReadFile(t, goMod); got != frameworkGoMod {
		t.Errorf("go.mod changed:\n%s", got)
	}
}

// originAnswering is an HTTP client for a module origin whose go-import page
// answers status. A status of 200 names a module proxy at modules.example,
// which serves v0.4.2 for every module.
func originAnswering(status int) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch {
		case status != http.StatusOK:
		case strings.HasSuffix(r.URL.String(), "?go-get=1"):
			body = `<meta name="go-import" content="go.putnami.dev mod https://modules.example/go">`
		default:
			body = latestAnswer("v0.4.2")
		}
		code := status
		if strings.HasPrefix(r.URL.String(), "https://modules.example/") {
			code = http.StatusOK
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
}

// A missing credential matters only when the origin refuses a request. The
// refresh outcome at the warn level is held, and logged with the failure of a
// lookup the origin answered with 401 or 403; any other outcome is logged at
// once.
func TestProjectSetupLogsTheHeldWarningWhenTheOriginRefuses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-holds-a-missing-credential-warning",
		"a-refused-lookup-logs-the-held-warning")
	jobtest.RequireGo(t)
	// The held warnings live in the process, and a refusal logs every one held
	// for its host: the message tells this test's apart.
	const message = "held and refused: refreshed the credential for "
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		ws := jobtest.RealTempDir(t)
		goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)

		run := runSetupRefreshing(t, ws, `{"path":"apps/api"}`, "off", originAnswering(status), "warn", message, "GONOPROXY=go.putnami.dev")
		run.requireStatus(t, statusFailed)
		run.requireTranscript(t, "the registry refused a request (HTTP 401 or 403); "+message+"go.putnami.dev",
			fmt.Sprintf("HTTP %d", status))
		warnings := 0
		for _, event := range run.rec.Events() {
			if event.Kind == "log" && event.Level == "warn" && strings.Contains(event.Message, message) {
				warnings++
			}
		}
		if warnings != 1 {
			t.Errorf("status %d: %d warnings, want the held one logged once:\n%s", status, warnings, run.rec.Transcript())
		}
		if got := jobtest.ReadFile(t, goMod); got != frameworkGoMod {
			t.Errorf("go.mod changed:\n%s", got)
		}
	}
}

// A lookup the origin answers, or fails for another reason than a refusal,
// logs no held warning; an outcome of another level is logged at once.
func TestProjectSetupLogsNoHeldWarningWithoutARefusal(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-holds-a-missing-credential-warning",
		"a-successful-or-unrefused-lookup-logs-no-warning")
	jobtest.RequireGo(t)
	// The held warning of a warn outcome stays in the process, for the
	// refusal of a later run that is not this test's: this test makes none.
	const message = "held and kept: refreshed the credential for "
	for _, tt := range []struct {
		name   string
		status int
		level  string
	}{
		{"an answered lookup", http.StatusOK, "warn"},
		{"a server error", http.StatusInternalServerError, "warn"},
		{"a module nothing serves", http.StatusNotFound, "warn"},
		{"an outcome that is not a warning", http.StatusNotFound, "info"},
	} {
		ws := jobtest.RealTempDir(t)
		jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)

		run := runSetupRefreshing(t, ws, `{"path":"apps/api"}`, "off", originAnswering(tt.status), tt.level, message,
			"GONOPROXY=go.putnami.dev")
		for _, event := range run.rec.Events() {
			if event.Kind == "log" && event.Level == "warn" {
				t.Errorf("%s: a warning is logged:\n%s", tt.name, run.rec.Transcript())
			}
		}
		if logged := run.rec.Contains(message + "go.putnami.dev"); logged != (tt.level != "warn") {
			t.Errorf("%s: the refresh outcome is logged = %v, want %v:\n%s", tt.name, logged, tt.level != "warn", run.rec.Transcript())
		}
	}
}

// The setup refuses what it cannot do: a project path outside the workspace,
// and a hosted run, whose dependencies are offline.
func TestProjectSetupRefusesWhatItCannotSetUp(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-sets-up-a-go-project",
		"setup-refuses-a-path-outside-the-workspace-and-a-hosted-run")
	tests := []struct {
		name   string
		params string
		extra  []string
		want   string
	}{
		{"no path", `{}`, nil, "needs the directory of the created project"},
		{"a path outside the workspace", `{"path":"../elsewhere"}`, nil, "needs the directory of the created project"},
		{"an absolute path", `{"path":"/srv/api"}`, nil, "needs the directory of the created project"},
		{"offline dependencies", `{"path":"apps/api"}`, []string{"PUTNAMI_OFFLINE_DEPENDENCIES=1"}, "does not run when dependencies are offline"},
		{"an unknown stage", `{"path":"apps/api","stage":"later"}`, nil, `params.stage to be prepare or absent, not "later"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := jobtest.RealTempDir(t)
			goMod := jobtest.WriteFile(t, ws, projectPath+"/go.mod", frameworkGoMod)
			run := runSetup(t, ws, tt.params, "off", nil, tt.extra...)
			run.requireStatus(t, statusFailed)
			run.requireTranscript(t, tt.want)
			if got := jobtest.ReadFile(t, goMod); got != frameworkGoMod {
				t.Errorf("go.mod changed:\n%s", got)
			}
			if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, "go.work")); ok {
				t.Error("the refused setup wrote go.work")
			}
		})
	}
}

// A project without go.mod, such as one a template of another language
// renders, has nothing to set up.
func TestProjectSetupWithoutGoModSetsNothingUp(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "go/project-setup", "create-sets-up-a-go-project",
		"setup-leaves-a-project-without-go-mod-alone")
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "apps/web/package.json", "{}\n")
	run := runSetup(t, ws, `{"path":"apps/web"}`, "off", nil)
	run.requireStatus(t, statusOK)
	run.requireTranscript(t, "apps/web has no go.mod: nothing to set up")
	if _, ok := jobtest.ReadOptional(t, filepath.Join(ws, "go.work")); ok {
		t.Error("the setup wrote go.work for a project without go.mod")
	}
}
