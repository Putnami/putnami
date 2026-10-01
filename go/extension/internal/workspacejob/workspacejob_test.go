package workspacejob_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

func TestMain(m *testing.M) {
	jobtest.ServeFake()
	os.Exit(m.Run())
}

func TestEnvKeepsOrderAndAppliesOverridesToOneCommand(t *testing.T) {
	env := workspacejob.NewEnv([]string{"A=1", "B=2", "=ignored", "novalue", "A=3", "C=x=y"})
	if got := env.Get("A"); got != "3" {
		t.Fatalf("A = %q, want the later entry 3", got)
	}
	if got := env.Get("C"); got != "x=y" {
		t.Fatalf("C = %q, want the value after the first =", got)
	}
	if got := env.Get("MISSING"); got != "" {
		t.Fatalf("MISSING = %q, want empty", got)
	}
	want := []string{"A=3", "B=2", "C=x=y"}
	if got := env.Environ(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Environ() = %v, want %v", got, want)
	}

	overridden := env.Environ("B=override", "D=new", "broken")
	if want := []string{"A=3", "B=override", "C=x=y", "D=new"}; !reflect.DeepEqual(overridden, want) {
		t.Fatalf("Environ(overrides) = %v, want %v", overridden, want)
	}
	if got := env.Get("B"); got != "2" {
		t.Fatalf("an override leaked into the exported environment: B = %q", got)
	}

	clone := env.Clone()
	clone.Set("A", "changed")
	if env.Get("A") != "3" {
		t.Fatal("a clone shares its values with the original")
	}
}

func TestEnvPrependPathKeepsTheExistingSpelling(t *testing.T) {
	sep := string(filepath.ListSeparator)
	env := workspacejob.NewEnv(nil)
	env.PrependPath("/first")
	if got := env.Get("PATH"); got != "/first" {
		t.Fatalf("PATH = %q, want /first", got)
	}
	env.PrependPath("/second")
	if got := env.Get("PATH"); got != "/second"+sep+"/first" {
		t.Fatalf("PATH = %q, want /second before /first", got)
	}

	spelled := workspacejob.NewEnv([]string{"Path=/system"})
	spelled.PrependPath("/tool")
	entries := spelled.Environ()
	if runtime.GOOS == "windows" {
		// Windows has one variable whatever its spelling, and the prepend keeps
		// the spelling the process was started with.
		if want := []string{"Path=/tool" + sep + "/system"}; !reflect.DeepEqual(entries, want) {
			t.Fatalf("Environ() = %v, want %v", entries, want)
		}
		return
	}
	if want := []string{"Path=/system", "PATH=/tool"}; !reflect.DeepEqual(entries, want) {
		t.Fatalf("Environ() = %v, want %v", entries, want)
	}
}

func TestCommaValues(t *testing.T) {
	for _, tc := range []struct{ list, value, appended, removed string }{
		{"", "a/*", "a/*", ""},
		{"b/*", "a/*", "b/*,a/*", "b/*"},
		{"a/*,b/*,a/*", "a/*", "a/*,b/*,a/*,a/*", "b/*"},
		{",b/*,,", "a/*", ",b/*,,,a/*", "b/*"},
	} {
		if got := workspacejob.AppendCommaValue(tc.list, tc.value); got != tc.appended {
			t.Errorf("AppendCommaValue(%q, %q) = %q, want %q", tc.list, tc.value, got, tc.appended)
		}
		if got := workspacejob.RemoveCommaValue(tc.list, tc.value); got != tc.removed {
			t.Errorf("RemoveCommaValue(%q, %q) = %q, want %q", tc.list, tc.value, got, tc.removed)
		}
	}
}

func TestLineFilters(t *testing.T) {
	if got := workspacejob.MapLines("", strings.ToUpper); got != "" {
		t.Fatalf("MapLines(empty) = %q", got)
	}
	if got := workspacejob.MapLines("go1.26\ngo1.25\n\n", func(line string) string {
		return strings.TrimPrefix(line, "go")
	}); got != "1.26\n1.25" {
		t.Fatalf("MapLines = %q", got)
	}
	for _, tc := range []struct {
		line string
		n    int
		want string
	}{
		{"go version go1.26.1 darwin/arm64", 3, "go1.26.1"},
		{"  go   1.22  ", 2, "1.22"},
		{"go", 2, ""},
		{"go", 0, ""},
	} {
		if got := workspacejob.Field(tc.line, tc.n); got != tc.want {
			t.Errorf("Field(%q, %d) = %q, want %q", tc.line, tc.n, got, tc.want)
		}
	}
}

// The trap is the port of the script's `trap '...; exit 130' INT` and
// `exit 143` TERM: a signal cancels the command in flight, then the armed
// action (a rollback or a restore) runs on the job goroutine, then the job
// exits with 128 plus the signal number.
func TestTrapRunsTheArmedActionThenExitsWithTheShellStatus(t *testing.T) {
	for _, tc := range []struct {
		signal os.Signal
		code   int
	}{
		{syscall.SIGTERM, 143},
		{os.Interrupt, 130},
	} {
		t.Run(tc.signal.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var order []string
			trap := workspacejob.NewTrap(cancel, func(code int) { order = append(order, "exit "+strconv.Itoa(code)) })
			trap.Arm(func() { order = append(order, "first action") })
			trap.Arm(func() { order = append(order, "rollback") })

			trap.Check()
			if len(order) != 0 {
				t.Fatalf("a trap without a signal acted: %v", order)
			}

			trap.Receive(tc.signal)
			if ctx.Err() == nil {
				t.Fatal("the signal did not cancel the job context")
			}
			trap.Receive(syscall.SIGHUP) // a second signal changes nothing
			trap.Check()
			if want := []string{"rollback", "exit " + strconv.Itoa(tc.code)}; !reflect.DeepEqual(order, want) {
				t.Fatalf("trap sequence = %v, want %v", order, want)
			}
			// The exit above ends the process in production. Here it returns,
			// and the action it ran is spent.
			trap.Disarm()
			if got := strings.Count(strings.Join(order, "\n"), "rollback"); got != 1 {
				t.Fatalf("the armed action ran %d times: %v", got, order)
			}
		})
	}
}

func TestTrapReceivedBeforeDisarmStillEndsTheJob(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	var order []string
	trap := workspacejob.NewTrap(cancel, func(code int) { order = append(order, "exit "+strconv.Itoa(code)) })
	trap.Arm(func() { order = append(order, "restore") })
	trap.Receive(syscall.SIGTERM)
	trap.Disarm()
	if want := []string{"restore", "exit 143"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("trap sequence = %v, want %v", order, want)
	}

	var nilTrap *workspacejob.Trap
	nilTrap.Arm(func() {})
	nilTrap.Disarm()
	nilTrap.Check()
}

// A signal that arrives just before Disarm, while the signal goroutine may not
// have run yet, is not lost: Disarm still runs the armed action and ends the
// job with the signal's status.
func TestTrapSignalArrivingAsItIsDisarmedStillEndsTheJob(t *testing.T) {
	for range 500 {
		_, cancel := context.WithCancel(context.Background())
		var order []string
		trap := workspacejob.NewTrap(cancel, func(code int) { order = append(order, "exit "+strconv.Itoa(code)) })
		trap.Arm(func() { order = append(order, "restore") })
		trap.Deliver(syscall.SIGTERM)
		trap.Disarm()
		cancel()
		if want := []string{"restore", "exit 143"}; !reflect.DeepEqual(order, want) {
			t.Fatalf("trap sequence = %v, want %v", order, want)
		}
	}
}

func TestExitCodes(t *testing.T) {
	if got := workspacejob.ExitCodeFor(syscall.SIGTERM); got != 143 {
		t.Fatalf("SIGTERM exit = %d", got)
	}
	if got := workspacejob.ExitCodeFor(os.Interrupt); got != 130 {
		t.Fatalf("SIGINT exit = %d", got)
	}
}

func TestGoVersions(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"go1.22", "1.22.0"},
		{"1.26.1", "1.26.1"},
		{"go 1.22 ", "1.22.0"},
		{"1.22rc1", "1.22rc1"},
		{"", ""},
	} {
		if got := workspacejob.NormalizeGoVersion(tc.in); got != tc.want {
			t.Errorf("NormalizeGoVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		have, want string
		ok         bool
	}{
		{"1.26.1", "1.22", true},
		{"1.22.0", "1.22.0", true},
		{"1.21.9", "1.22", false},
		{"1.22.10", "1.22.9", true},
		{"2.0.0", "1.99.99", true},
		{"1.22rc1", "1.22", false},
		{"1.22.0", "", false},
		{"1.x.0", "1.0.0", false},
		{"1.22.99999999999999999999", "1.22.0", false},
	} {
		if got := workspacejob.GoVersionSatisfies(tc.have, tc.want); got != tc.ok {
			t.Errorf("GoVersionSatisfies(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestRequestedGoVersionReadsGoWorkThenTheProjectGoMod(t *testing.T) {
	ws := t.TempDir()
	project := filepath.Join(ws, "app")
	rec := &jobtest.Recorder{}
	j := workspacejob.New(context.Background(), rec, nil, ws, project)
	if got := j.RequestedGoVersion(); got != "" {
		t.Fatalf("no go.work and no go.mod requested %q", got)
	}

	jobtest.WriteFile(t, project, "go.mod", "module example.com/app\n\ngo 1.23\n")
	if got := j.RequestedGoVersion(); got != "1.23.0" {
		t.Fatalf("go directive = %q, want 1.23.0", got)
	}
	jobtest.WriteFile(t, project, "go.mod", "module example.com/app\n\ngo 1.23\n\ntoolchain go1.24.2\n")
	if got := j.RequestedGoVersion(); got != "1.24.2" {
		t.Fatalf("toolchain directive = %q, want 1.24.2", got)
	}
	jobtest.WriteFile(t, ws, "go.work", "go 1.25.1\n\nuse ./app\n")
	if got := j.RequestedGoVersion(); got != "1.25.1" {
		t.Fatalf("go.work directive = %q, want 1.25.1", got)
	}
	if got := workspacejob.GoDirective(filepath.Join(ws, "missing")); got != "" {
		t.Fatalf("GoDirective(missing) = %q", got)
	}
}

func TestNewTakesTheRootsFromTheProjectedVariables(t *testing.T) {
	j := workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{
		"PUTNAMI_WORKSPACE_ROOT=/env/ws", "PUTNAMI_PROJECT_ROOT=/env/root",
	}, "/ctx/ws", "/ctx/project")
	if j.WorkspaceRoot != "/env/ws" || j.ProjectPath != "/env/root" {
		t.Fatalf("roots = %q, %q", j.WorkspaceRoot, j.ProjectPath)
	}
	j = workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{
		"PUTNAMI_PROJECT_PATH=/env/path", "PUTNAMI_PROJECT_ROOT=/env/root",
	}, "/ctx/ws", "/ctx/project")
	if j.WorkspaceRoot != "/ctx/ws" || j.ProjectPath != "/env/path" {
		t.Fatalf("roots = %q, %q", j.WorkspaceRoot, j.ProjectPath)
	}
	if got := j.UserAgent(); got != "putnami-cli/dev" {
		t.Fatalf("UserAgent() = %q", got)
	}
	j.Env.Set("PUTNAMI_CLI_USER_AGENT", "putnami-cli/1.2.3")
	if got := j.UserAgent(); got != "putnami-cli/1.2.3" {
		t.Fatalf("UserAgent() = %q", got)
	}
}

func TestGoCacheRootIsMachineGlobalAndOverrideable(t *testing.T) {
	home := t.TempDir()
	j := workspacejob.New(context.Background(), &jobtest.Recorder{},
		[]string{"HOME=" + home, "USERPROFILE=" + home, "PUTNAMI_GO_CACHE_DIR=", "PUTNAMI_HOME="}, t.TempDir(), "")
	if got, want := j.GoCacheRoot(), filepath.Join(home, ".putnami", "cache", "go"); got != want {
		t.Fatalf("GoCacheRoot() = %q, want %q", got, want)
	}
	override := filepath.Join(t.TempDir(), "override")
	j.Env.Set("PUTNAMI_GO_CACHE_DIR", override)
	if got := j.GoCacheRoot(); got != override {
		t.Fatalf("GoCacheRoot() = %q, want the override %q", got, override)
	}

	ws := t.TempDir()
	bare := workspacejob.New(context.Background(), &jobtest.Recorder{}, nil, ws, "")
	if got, want := bare.GoCacheRoot(), filepath.Join(ws, ".putnami", "extensions", "@putnami-go", "cache"); got != want {
		t.Fatalf("GoCacheRoot() without any root = %q, want the workspace fallback %q", got, want)
	}
}

func TestSetupGoEnvLeavesNetrcToTheUser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"implicit", nil, ""},
		{"putnami home", []string{"PUTNAMI_HOME=" + t.TempDir()}, ""},
		{"explicit", []string{"NETRC=/caller/owned.netrc"}, "/caller/owned.netrc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append([]string{"HOME=" + t.TempDir(), "PUTNAMI_GO_CACHE_DIR=" + t.TempDir()}, tc.extra...)
			j := workspacejob.New(context.Background(), &jobtest.Recorder{}, env, t.TempDir(), "")
			j.SetupGoEnv("")
			// Unset means Go reads $HOME/.netrc, where the per-user registry
			// credential is written.
			if got := j.Env.Get("NETRC"); got != tc.want {
				t.Fatalf("NETRC = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetupGoEnvUsesTheDeclaredModuleOrigin(t *testing.T) {
	cache := t.TempDir()
	j := workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{
		"PUTNAMI_GO_CACHE_DIR=" + cache, "GOPROXY=", "GONOPROXY=", "GONOSUMDB=corp.example/*",
	}, t.TempDir(), "")
	j.SetupGoEnv("vanity.example.test")
	for key, want := range map[string]string{
		"GOPROXY":      "https://proxy.golang.org,direct",
		"GONOPROXY":    "vanity.example.test/*",
		"GONOSUMDB":    "corp.example/*,vanity.example.test/*",
		"GONOSUMCHECK": "vanity.example.test/*",
		"GOTOOLCHAIN":  "local",
		"GOCACHE":      filepath.Join(cache, "build"),
		"GOMODCACHE":   filepath.Join(cache, "mod"),
	} {
		if got := j.Env.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, dir := range []string{"build", "mod"} {
		if info, err := os.Stat(filepath.Join(cache, dir)); err != nil || !info.IsDir() {
			t.Errorf("cache directory %s was not created: %v", dir, err)
		}
	}

	// A caller-named proxy stays, and the origin leaves GONOPROXY: it exists
	// only beside the default proxy the job supplies.
	named := workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{
		"PUTNAMI_GO_CACHE_DIR=" + t.TempDir(), "GOPROXY=https://mirror.example.test",
		"GONOPROXY=vanity.example.test/*,corp.example/*",
		"GO_REGISTRY_URL=https://user@vanity.example.test/go/",
	}, t.TempDir(), "")
	named.SetupGoEnv("")
	if got := named.Env.Get("GOPROXY"); got != "https://mirror.example.test" {
		t.Errorf("GOPROXY = %q, want the caller's", got)
	}
	if got := named.Env.Get("GONOPROXY"); got != "corp.example/*" {
		t.Errorf("GONOPROXY = %q, want the origin removed", got)
	}
}

func TestOriginHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://go.example.test/modules/": "go.example.test",
		"http://user:secret@host:8080/x":   "host:8080",
		"bare.example.test":                "bare.example.test",
		"bare.example.test/path":           "bare.example.test",
		"":                                 "",
	} {
		if got := workspacejob.OriginHost(in); got != want {
			t.Errorf("OriginHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookPathSkipsRelativeEntriesAndNonExecutables(t *testing.T) {
	dir := t.TempDir()
	name := pkgmeta.ExecutableName(runtime.GOOS, "tool")
	notExecutable := t.TempDir()
	jobtest.WriteFile(t, notExecutable, name, "data")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(notExecutable, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	executable := jobtest.WriteFile(t, dir, name, "binary")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(t.TempDir(), name), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)
	path := strings.Join([]string{"", "relative", filepath.Join(t.TempDir(), name)}, sep)
	if runtime.GOOS != "windows" {
		path += sep + notExecutable
	}
	path += sep + dir
	j := workspacejob.New(context.Background(), &jobtest.Recorder{}, []string{"PATH=" + path}, t.TempDir(), "")
	if got := j.LookPath("tool"); got != executable {
		t.Fatalf("LookPath(tool) = %q, want %q", got, executable)
	}
	if got := j.LookPath("absent"); got != "" {
		t.Fatalf("LookPath(absent) = %q", got)
	}
	if !workspacejob.IsExecutable(executable) || workspacejob.IsExecutable(dir) {
		t.Fatal("IsExecutable does not tell a program from a directory")
	}
}

func TestParamFollowsJQAlternativeSemantics(t *testing.T) {
	params := jobtest.Params(t, `{
		"s": "text", "n": 3, "f": false, "z": null, "t": true,
		"o": {"inner": {"leaf": "value"}, "list": [1, "a"]},
		"notObject": "x"
	}`)
	for _, tc := range []struct {
		key  string
		path []string
		want string
		ok   bool
	}{
		{"s", nil, "text", true},
		{"n", nil, "3", true},
		{"t", nil, "true", true},
		{"f", nil, "", false},
		{"z", nil, "", false},
		{"missing", nil, "", false},
		{"o", []string{"inner", "leaf"}, "value", true},
		{"o", []string{"inner", "missing"}, "", false},
		{"z", []string{"inner"}, "", false},
		{"notObject", []string{"inner"}, "", false},
		{"o", []string{"list"}, "[\n  1,\n  \"a\"\n]", true},
		{"o", []string{"inner"}, "{\n  \"leaf\": \"value\"\n}", true},
	} {
		_, ok := workspacejob.Param(params, tc.key, tc.path...)
		if ok != tc.ok {
			t.Errorf("Param(%s %v) ok = %v, want %v", tc.key, tc.path, ok, tc.ok)
		}
		if got := workspacejob.ParamText(params, tc.key, tc.path...); got != tc.want {
			t.Errorf("ParamText(%s %v) = %q, want %q", tc.key, tc.path, got, tc.want)
		}
	}
}

func TestJSONHelpers(t *testing.T) {
	for raw, want := range map[string]string{
		`"a\"b"`:      `a"b`,
		` 12.5 `:      `12.5`,
		`{"a": [1 ]}`: "{\n  \"a\": [\n    1\n  ]\n}",
		`null`:        `null`,
		`{broken`:     `{broken`,
	} {
		if got := workspacejob.JQRawText(json.RawMessage(raw)); got != want {
			t.Errorf("JQRawText(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := workspacejob.JSONText(json.RawMessage(`{ "a" : 1 }`)); got != `{"a":1}` {
		t.Errorf("JSONText compacts objects: %q", got)
	}
	if _, ok := workspacejob.JSONString(json.RawMessage(`3`)); ok {
		t.Error("JSONString accepted a number")
	}
	if _, ok := workspacejob.JSONString(json.RawMessage(`"broken`)); ok {
		t.Error("JSONString accepted a malformed string")
	}
	for _, tc := range []struct {
		raw   string
		count int
		ok    bool
	}{
		{``, 0, true},
		{`false`, 0, true},
		{`null`, 0, true},
		{`[1, 2]`, 2, true},
		{`{"a": 1, "b": {}}`, 2, true},
		{`"text"`, 0, false},
		{`3`, 0, false},
		{`[1,`, 0, false},
		{`{"a":`, 0, false},
	} {
		values, ok := workspacejob.IterateOrEmpty(json.RawMessage(tc.raw))
		if ok != tc.ok || len(values) != tc.count {
			t.Errorf("IterateOrEmpty(%s) = %d values, %v; want %d, %v", tc.raw, len(values), ok, tc.count, tc.ok)
		}
	}
}

func TestUsesStandaloneMetadataAnswersTheScriptPredicate(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   bool
	}{
		{`{"publish":["go"]}`, true},
		{`{"publish":["npm","go"]}`, true},
		{`{"publish":["npm"]}`, false},
		{`{"publish":[1,{"go":true}]}`, false},
		{`{"publish":"go"}`, true},
		// jq's index finds a substring, so any publish string holding "go" counts.
		{`{"publish":"cargo"}`, true},
		{`{"publish":"npm"}`, false},
		{`{"publish":3}`, false},
		{`{"publish":3,"options":{"@putnami/go":{"standalone":true}}}`, false},
		{`{"publish":{"go":true}}`, false},
		{`{"publish":null,"options":{"@putnami/go":{"standalone":true}}}`, true},
		{`{"publish":false,"options":{"@putnami/go":{"standalone":true}}}`, true},
		{`{"options":{"@putnami/go":{"standalone":"true"}}}`, false},
		{`{"options":{"@putnami/go":{"standalone":false}}}`, false},
		{`{"options":{"@putnami/go":true}}`, false},
		{`{"options":[]}`, false},
		{`{}`, false},
		{`not json`, false},
	} {
		dir := t.TempDir()
		jobtest.WriteFile(t, dir, "putnami.json", tc.config)
		if got := workspacejob.UsesStandaloneMetadata(filepath.Join(dir, "go.mod")); got != tc.want {
			t.Errorf("UsesStandaloneMetadata(%s) = %v, want %v", tc.config, got, tc.want)
		}
	}
	if workspacejob.UsesStandaloneMetadata(filepath.Join(t.TempDir(), "go.mod")) {
		t.Error("a member without putnami.json is standalone")
	}
}

func TestGoProjectsFollowTheDeclarationOrder(t *testing.T) {
	ws := t.TempDir()
	for _, project := range []string{"b", "a", "3", "null", "nested/c"} {
		jobtest.WriteFile(t, ws, project+"/go.mod", "module example.com/"+project+"\n")
	}
	jobtest.WriteFile(t, ws, "docs/package.json", "{}")
	jobtest.WriteFile(t, ws, "putnami.workspace.json",
		`{"projects":["b","docs","a",3,null,{"x":1},"nested/c","b"]}`)
	want := []string{"b", "a", "3", "null", "nested/c", "b"}
	if got := workspacejob.GoProjects(ws); !reflect.DeepEqual(got, want) {
		t.Fatalf("GoProjects() = %v, want %v", got, want)
	}

	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"projects":{"one":"a","two":"b"}}`)
	if got := workspacejob.GoProjects(ws); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("GoProjects(object) = %v", got)
	}
	for _, config := range []string{`{"projects":"a"}`, `{"projects":null}`, `broken`} {
		jobtest.WriteFile(t, ws, "putnami.workspace.json", config)
		if got := workspacejob.GoProjects(ws); len(got) != 0 {
			t.Errorf("GoProjects(%s) = %v, want none", config, got)
		}
	}

	legacy := t.TempDir()
	jobtest.WriteFile(t, legacy, "svc/go.mod", "module example.com/svc\n")
	jobtest.WriteFile(t, legacy, ".putnamirc.json", `{"projects":["svc"]}`)
	if got := workspacejob.WorkspaceConfigPath(legacy); got != filepath.Join(legacy, ".putnamirc.json") {
		t.Fatalf("WorkspaceConfigPath(legacy) = %q", got)
	}
	if got := workspacejob.GoProjects(legacy); !reflect.DeepEqual(got, []string{"svc"}) {
		t.Fatalf("GoProjects(legacy) = %v", got)
	}
}

// The members are the workspace membership the CLI resolved from includes.
// Only those with a go.mod are Go members; the workspace root and a path
// outside the workspace are never members, whatever the context says.
func TestGoMembersKeepTheGoProjectsOfTheMembership(t *testing.T) {
	parent := t.TempDir()
	ws := filepath.Join(parent, "ws")
	for _, project := range []string{"svc/b", "a", "a2"} {
		jobtest.WriteFile(t, ws, project+"/go.mod", "module example.com/x\n")
	}
	jobtest.WriteFile(t, ws, "go.mod", "module example.com/root\n")
	jobtest.WriteFile(t, parent, "escaped/go.mod", "module example.com/escaped\n")
	jobtest.WriteFile(t, ws, "web/package.json", "{}")

	// "svc\\b" is svc/b on Windows and a name without a go.mod elsewhere.
	members := []string{"svc/b", "web", "a", "./a", "svc\\b", ".", "", "../escaped", filepath.Join(parent, "escaped"), "a2", "missing"}
	want := []string{"a", "a2", "svc/b"}
	if got := workspacejob.GoMembers(ws, members); !reflect.DeepEqual(got, want) {
		t.Fatalf("GoMembers() = %v, want %v", got, want)
	}
	if got := workspacejob.GoMembers(ws, nil); len(got) != 0 {
		t.Fatalf("GoMembers(nil) = %v", got)
	}
}

// The Go jobs act on one Go membership. The legacy "projects" member wins
// when it names a Go project, as declared; otherwise the Go members of the
// resolved membership, members reached through includes among them, are the
// Go projects.
func TestWorkspaceGoProjectsPrefersALegacyProjectsMemberThenTheMembership(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "included-members-are-go-projects", "a-legacy-projects-member-wins-over-the-membership")
	ws := t.TempDir()
	for _, project := range []string{"api", "legacy", "svc/lib"} {
		jobtest.WriteFile(t, ws, project+"/go.mod", "module example.com/x\n")
	}
	refs := []pctx.ProjectRef{{ID: "/svc/lib", Path: "svc/lib"}, {ID: "/api", Path: "api"}, {ID: "/web", Path: "web"}}
	members := workspacejob.MemberPaths(refs)
	if want := []string{"svc/lib", "api", "web"}; !reflect.DeepEqual(members, want) {
		t.Fatalf("MemberPaths() = %v, want %v", members, want)
	}

	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"includes":["api","svc"]}`)
	projects, legacy := workspacejob.WorkspaceGoProjects(ws, members)
	if want := []string{"api", "svc/lib"}; !reflect.DeepEqual(projects, want) || legacy {
		t.Fatalf("WorkspaceGoProjects(includes) = %v, %v; want %v from the membership", projects, legacy, want)
	}

	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"projects":["legacy","web","legacy"],"includes":["api"]}`)
	projects, legacy = workspacejob.WorkspaceGoProjects(ws, members)
	if want := []string{"legacy", "legacy"}; !reflect.DeepEqual(projects, want) || !legacy {
		t.Fatalf("WorkspaceGoProjects(projects) = %v, %v; want %v from the legacy member", projects, legacy, want)
	}

	// A legacy member that names no Go project leaves the membership in charge.
	jobtest.WriteFile(t, ws, "putnami.workspace.json", `{"projects":["web"]}`)
	projects, legacy = workspacejob.WorkspaceGoProjects(ws, members)
	if want := []string{"api", "svc/lib"}; !reflect.DeepEqual(projects, want) || legacy {
		t.Fatalf("WorkspaceGoProjects(projects without Go) = %v, %v; want %v", projects, legacy, want)
	}
}

// A value that leads with "-" would reach go as a FLAG rather than as a
// module, so it is refused on the boundary between `go list` output and
// `go mod download` arguments.
func TestValidModuleCoordinate(t *testing.T) {
	for coordinate, valid := range map[string]bool{
		"example.com/lib@v1.0.0":  true,
		"":                        false,
		"-insecure@v1.0.0":        false,
		"example.com/lib v1.0.0":  false,
		"example.com/lib\tv1.0.0": false,
		"example.com/lib":         false,
	} {
		if got := workspacejob.ValidModuleCoordinate(coordinate); got != valid {
			t.Errorf("ValidModuleCoordinate(%q) = %v, want %v", coordinate, got, valid)
		}
	}
}

// A rollback puts a file's mode back with its bytes: a file the job replaced
// through a rename, or removed, gets the mode it had, not the mode a new file
// would get.
func TestSnapshotRestoresTheRecordedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows records no permission but read-only")
	}
	dir := t.TempDir()
	replaced := jobtest.WriteFile(t, dir, "go.work", "before\n")
	deleted := jobtest.WriteFile(t, dir, "go.mod", "module example.com/m\n")
	sameBytes := jobtest.WriteFile(t, dir, "go.sum", "sum\n")
	for path, mode := range map[string]os.FileMode{replaced: 0o664, deleted: 0o640, sameBytes: 0o600} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	var snapshot workspacejob.Snapshot
	for _, path := range []string{replaced, deleted, sameBytes} {
		if err := snapshot.Add(path); err != nil {
			t.Fatalf("Add(%s): %v", path, err)
		}
	}

	staged := jobtest.WriteFile(t, dir, "go.work.tmp", "after\n")
	if err := os.Chmod(staged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, replaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sameBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := snapshot.Restore(nil, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for path, want := range map[string]os.FileMode{replaced: 0o664, deleted: 0o640, sameBytes: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want the recorded %o", filepath.Base(path), got, want)
		}
	}
	if got := jobtest.ReadFile(t, replaced); got != "before\n" {
		t.Fatalf("go.work = %q, want its recorded content", got)
	}
}

// A file Restore cannot put back is named in the error it returns, and every
// other file is still put back.
func TestSnapshotRestoreReportsWhatItCannotPutBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory does not refuse writes on Windows")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	lost := jobtest.WriteFile(t, locked, "go.mod", "module example.com/m\n")
	restored := jobtest.WriteFile(t, dir, "go.work", "before\n")
	var snapshot workspacejob.Snapshot
	for _, path := range []string{lost, restored} {
		if err := snapshot.Add(path); err != nil {
			t.Fatalf("Add(%s): %v", path, err)
		}
	}
	if err := os.Remove(lost); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	jobtest.WriteFile(t, dir, "go.work", "after\n")

	err := snapshot.Restore(nil, nil)
	if err == nil || !strings.Contains(err.Error(), lost) {
		t.Fatalf("Restore = %v, want an error naming %s", err, lost)
	}
	if got := jobtest.ReadFile(t, restored); got != "before\n" {
		t.Fatalf("go.work = %q, want its recorded content despite the other failure", got)
	}
}

func TestSnapshotRestoresExactlyTheRecordedState(t *testing.T) {
	dir := t.TempDir()
	existing := jobtest.WriteFile(t, dir, "go.work", "before\n")
	unchanged := jobtest.WriteFile(t, dir, "go.mod", "same\n")
	created := filepath.Join(dir, "go.work.sum")
	directory := filepath.Join(dir, "subdir")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}

	var snapshot workspacejob.Snapshot
	for _, path := range []string{existing, unchanged, created, directory} {
		if err := snapshot.Add(path); err != nil {
			t.Fatalf("Add(%s): %v", path, err)
		}
	}
	if got := snapshot.Paths(); !reflect.DeepEqual(got, []string{existing, unchanged, created, directory}) {
		t.Fatalf("Paths() = %v", got)
	}

	jobtest.WriteFile(t, dir, "go.work", "after\n")
	jobtest.WriteFile(t, dir, "go.work.sum", "new\n")
	var changed, removed []string
	if err := snapshot.Restore(
		func(path string) { changed = append(changed, path) },
		func(path string) { removed = append(removed, path) }); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := jobtest.ReadFile(t, existing); got != "before\n" {
		t.Fatalf("go.work = %q, want its recorded content", got)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("a file that did not exist was kept: %v", err)
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatalf("the directory was touched: %v", err)
	}
	if !reflect.DeepEqual(changed, []string{existing}) || !reflect.DeepEqual(removed, []string{created}) {
		t.Fatalf("changed = %v, removed = %v", changed, removed)
	}

	// Restore forgets the state: a second one does nothing.
	jobtest.WriteFile(t, dir, "go.work", "later\n")
	if err := snapshot.Restore(nil, nil); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	if got := jobtest.ReadFile(t, existing); got != "later\n" {
		t.Fatalf("a second Restore rewrote go.work: %q", got)
	}
	if !workspacejob.FileExists(existing) || workspacejob.FileExists(directory) {
		t.Fatal("FileExists does not tell a file from a directory")
	}
}
