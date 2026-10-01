// Package jobtest is the fixture kit of the lifecycle job tests: an event
// recorder that stands in for the JSONL emitter, a Go module proxy laid out on
// disk, a sandboxed Go environment, and the test binary itself linked under
// the names of the programs a job may start.
//
// It is imported by tests only.
package jobtest

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// Event is one event a job emitted.
type Event struct {
	// Kind is "log", "diagnostic", "metric", "phase-start" or "phase-end".
	Kind string
	// Level is the log level or the diagnostic severity.
	Level string
	// Name is the phase or the metric name.
	Name string
	// Status is the phase-end status.
	Status  string
	Message string
	File    string
	Value   any
	Unit    string
}

// String renders the event on one transcript line.
func (e Event) String() string {
	switch e.Kind {
	case "log":
		return "log " + e.Level + ": " + e.Message
	case "diagnostic":
		line := "diagnostic " + e.Level + ": " + e.Message
		if e.File != "" {
			line += " [" + e.File + "]"
		}
		return line
	case "metric":
		return fmt.Sprintf("metric %s=%v %s", e.Name, e.Value, e.Unit)
	case "phase-start":
		return "phase " + e.Name + " start"
	default:
		return "phase " + e.Name + " " + e.Status
	}
}

// Recorder records the events of a job. It satisfies workspacejob.Emitter.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

var _ workspacejob.Emitter = (*Recorder)(nil)

func (r *Recorder) add(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

// Log records a log event.
func (r *Recorder) Log(level, message string) {
	r.add(Event{Kind: "log", Level: level, Message: message})
}

// PhaseStart records the start of a phase.
func (r *Recorder) PhaseStart(name string) { r.add(Event{Kind: "phase-start", Name: name}) }

// PhaseEnd records the end of a phase.
func (r *Recorder) PhaseEnd(name, status string) {
	r.add(Event{Kind: "phase-end", Name: name, Status: status})
}

// Diagnostic records a diagnostic.
func (r *Recorder) Diagnostic(severity, message, file string, _ int) {
	r.add(Event{Kind: "diagnostic", Level: severity, Message: message, File: file})
}

// Metric records a metric.
func (r *Recorder) Metric(name string, value any, unit string) {
	r.add(Event{Kind: "metric", Name: name, Value: value, Unit: unit})
}

// Events returns the recorded events in emission order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Transcript is every recorded event, one per line.
func (r *Recorder) Transcript() string {
	var out strings.Builder
	for _, event := range r.Events() {
		out.WriteString(event.String())
		out.WriteByte('\n')
	}
	return out.String()
}

// Contains reports whether a log or diagnostic message contains text.
func (r *Recorder) Contains(text string) bool {
	for _, event := range r.Events() {
		if (event.Kind == "log" || event.Kind == "diagnostic") && strings.Contains(event.Message, text) {
			return true
		}
	}
	return false
}

// MetricValue returns the value of the last metric named name.
func (r *Recorder) MetricValue(name string) (any, bool) {
	events := r.Events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "metric" && events[i].Name == name {
			return events[i].Value, true
		}
	}
	return nil, false
}

// PhaseStatus returns the status the last end of phase name reported, or "".
func (r *Recorder) PhaseStatus(name string) string {
	events := r.Events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "phase-end" && events[i].Name == name {
			return events[i].Status
		}
	}
	return ""
}

// Printer writes each event on its own line as it is emitted, for a job that
// runs in a child process and may exit before a recorder could be read.
type Printer struct{ W io.Writer }

var _ workspacejob.Emitter = Printer{}

func (p Printer) print(event Event) { _, _ = fmt.Fprintln(p.W, event.String()) }

// Log prints a log event.
func (p Printer) Log(level, message string) {
	p.print(Event{Kind: "log", Level: level, Message: message})
}

// PhaseStart prints the start of a phase.
func (p Printer) PhaseStart(name string) { p.print(Event{Kind: "phase-start", Name: name}) }

// PhaseEnd prints the end of a phase.
func (p Printer) PhaseEnd(name, status string) {
	p.print(Event{Kind: "phase-end", Name: name, Status: status})
}

// Diagnostic prints a diagnostic.
func (p Printer) Diagnostic(severity, message, file string, _ int) {
	p.print(Event{Kind: "diagnostic", Level: severity, Message: message, File: file})
}

// Metric prints a metric.
func (p Printer) Metric(name string, value any, unit string) {
	p.print(Event{Kind: "metric", Name: name, Value: value, Unit: unit})
}

// NewJob returns a job that records its events in rec, exports environ to its
// commands, and acts on workspace. The output of the commands the job lets
// inherit its streams goes to stdout and stderr, which the caller reads: in
// production the job's standard output is the JSONL stream, so a command that
// writes there corrupts it.
func NewJob(t *testing.T, rec *Recorder, environ []string, workspace string) (j *workspacejob.Job, stdout, stderr *bytes.Buffer) {
	t.Helper()
	j = workspacejob.New(context.Background(), rec, environ, workspace, "")
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	j.Stdout, j.Stderr = stdout, stderr
	t.Cleanup(func() { j.Trap().Disarm() })
	return j, stdout, stderr
}

// RequireGo skips the test when no go command is on PATH.
func RequireGo(t *testing.T) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go not available: %v", err)
	}
	return goBinary
}

// RealTempDir is t.TempDir() with its symbolic links resolved. macOS hands out
// a /var path that links to /private/var, and go.work use entries are compared
// against the real path.
func RealTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// hostVariables are the variables of the test process a go command needs to
// run at all; everything else a job sees is set by the test.
var hostVariables = []string{
	"PATH", "GOROOT", "TMPDIR",
	"SYSTEMROOT", "SYSTEMDRIVE", "TEMP", "TMP",
	"USERPROFILE", "LOCALAPPDATA", "APPDATA", "PROGRAMDATA", "ComSpec", "PATHEXT",
}

// Env returns a sandboxed environment for a job: the host variables go needs,
// a private HOME and PUTNAMI_HOME, the Go cache root, and module settings that
// route every fetch to proxyURL and nowhere else. extra entries are appended
// and win.
//
// GONOPROXY, GOPRIVATE and GONOSUMDB are set empty and GOENV=off so a
// developer's private-module configuration, even one persisted with
// `go env -w`, cannot send a fetch around the fixture proxy.
func Env(t *testing.T, proxyURL, cacheRoot string, extra ...string) []string {
	t.Helper()
	var env []string
	for _, key := range hostVariables {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	home := t.TempDir()
	disableGoTelemetry(t, home)
	env = append(env,
		"HOME="+home,
		"PUTNAMI_HOME="+t.TempDir(),
		"PUTNAMI_GO_CACHE_DIR="+cacheRoot,
		"GOPROXY="+proxyURL,
		"GOSUMDB=off",
		"GONOPROXY=",
		"GOPRIVATE=",
		"GONOSUMDB=",
		"GOENV=off",
		"GOFLAGS=",
		"GOWORK=",
		"GOTOOLCHAIN=local",
		"GOPATH="+filepath.Join(home, "go"),
	)
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+home)
	}
	return append(env, extra...)
}

// disableGoTelemetry turns Go telemetry off for a fake home. Left on, the go
// command writes its counters under the config directory of that home and may
// start a detached upload process that keeps writing there after the command
// returned, which races the removal of the test directory.
func disableGoTelemetry(t *testing.T, home string) {
	t.Helper()
	var config string
	switch runtime.GOOS {
	case "windows":
		// The config directory is the host's %AppData%, outside the test tree.
		return
	case "darwin", "ios":
		config = filepath.Join(home, "Library", "Application Support")
	default:
		// Env passes no XDG_CONFIG_HOME, so the go command falls back to this.
		config = filepath.Join(home, ".config")
	}
	WriteFile(t, config, "go/telemetry/mode", "off\n")
}

// CacheRoot returns an isolated Go cache root whose read-only module cache
// entries are made writable again before the test directory is removed.
func CacheRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ChmodCleanup(t, root)
	return root
}

// ChmodCleanup restores the write bit over root before the test directory is
// removed: go writes module cache entries read-only on purpose.
func ChmodCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, info.Mode()|0o200)
			}
			return nil
		})
	})
}

// WriteFile writes content at the slash-separated rel under root, creating
// the parent directories.
func WriteFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ReadFile returns the content of p.
func ReadFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ReadOptional returns the content of p and whether it exists.
func ReadOptional(t *testing.T, p string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err == nil {
		return string(data), true
	}
	if os.IsNotExist(err) {
		return "", false
	}
	t.Fatal(err)
	return "", false
}

// Params decodes the params object of a job context.
func Params(t *testing.T, document string) map[string]json.RawMessage {
	t.Helper()
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &params); err != nil {
		t.Fatalf("params %s: %v", document, err)
	}
	return params
}

// FileURL is the file:// URL of a local directory, as GOPROXY accepts it.
func FileURL(dir string) string {
	slashed := filepath.ToSlash(dir)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return "file://" + slashed
}

// ProxyModule is one module version a fixture proxy serves.
type ProxyModule struct {
	Path    string
	Version string
	GoMod   string
	Source  string
}

// SimpleProxyModules returns one trivial module per path at version.
func SimpleProxyModules(version string, modulePaths ...string) []ProxyModule {
	modules := make([]ProxyModule, 0, len(modulePaths))
	for _, modulePath := range modulePaths {
		packageName := strings.ReplaceAll(path.Base(modulePath), "-", "_")
		modules = append(modules, ProxyModule{
			Path:    modulePath,
			Version: version,
			GoMod:   "module " + modulePath + "\n\ngo 1.22\n",
			Source:  "package " + packageName + "\n",
		})
	}
	return modules
}

// WriteModuleProxy lays out enough of the Go module proxy protocol for go get,
// go mod tidy and go mod download to fetch modules from a file:// URL, and
// returns the directory.
func WriteModuleProxy(t *testing.T, modules ...ProxyModule) string {
	t.Helper()
	dir := t.TempDir()
	WriteModuleProxyAt(t, dir, modules...)
	return dir
}

// WriteModuleProxyAt adds modules to the proxy tree at dir.
func WriteModuleProxyAt(t *testing.T, dir string, modules ...ProxyModule) {
	t.Helper()
	for _, module := range modules {
		versionDir := filepath.Join(dir, filepath.FromSlash(module.Path), "@v")
		if err := os.MkdirAll(versionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := `{"Version":"` + module.Version + `","Time":"2026-07-23T00:00:00Z"}`
		if err := os.WriteFile(filepath.Join(versionDir, module.Version+".info"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versionDir, module.Version+".mod"), []byte(module.GoMod), 0o644); err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		root := module.Path + "@" + module.Version
		for _, entry := range []struct{ name, content string }{
			{path.Join(root, "go.mod"), module.GoMod},
			{path.Join(root, "module.go"), module.Source},
		} {
			file, err := writer.Create(entry.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(file, entry.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versionDir, module.Version+".zip"), archive.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// WriteLatestInfo adds the publication-ordered answer a proxy serves for the
// "latest" selector: GET <module>/@latest.
func WriteLatestInfo(t *testing.T, dir, modulePath, version string) {
	t.Helper()
	body := `{"Version":"` + version + `","Time":"2026-06-11T00:00:00Z"}`
	WriteFile(t, dir, modulePath+"/@latest", body)
}

// WriteChannelInfo adds the version-query answer a proxy serves for a
// non-semver selector: GET <module>/@v/<channel>.info.
func WriteChannelInfo(t *testing.T, dir, modulePath, channel, version string) {
	t.Helper()
	body := `{"Version":"` + version + `","Time":"2026-09-02T17:30:00Z"}`
	WriteFile(t, dir, modulePath+"/@v/"+channel+".info", body)
}

// WriteVersionList adds the version list a proxy serves at
// GET <module>/@v/list, the answer Go resolves a "latest" query through.
func WriteVersionList(t *testing.T, dir, modulePath string, versions ...string) {
	t.Helper()
	WriteFile(t, dir, modulePath+"/@v/list", strings.Join(versions, "\n")+"\n")
}

// The linked test binary reads these variables to know what to be.
const (
	// fakeMarker must be set for a linked binary to act as a fake at all, so
	// a test binary that happens to share a name keeps running its tests.
	fakeMarker = "JOBTEST_FAKE"
	// trapLog names the file a trap appends its invocation to.
	trapLog = "JOBTEST_TRAP_LOG"
	// versionPrefix + an upper-cased tool name with "-" as "_" is the version
	// a fake tool reports.
	versionPrefix = "JOBTEST_VERSION_"
	// realGo is the go command the fake go runs for every other request.
	realGo = "JOBTEST_REAL_GO"
	// goBlock is an argument sequence the fake go stops at: it writes the file
	// goReady names, then waits to be killed.
	goBlock = "JOBTEST_GO_BLOCK"
	goReady = "JOBTEST_GO_READY"
	// goRecord is the directory the fake go writes a GoRecord of each of its
	// invocations to, before it runs the real go command.
	goRecord = "JOBTEST_GO_RECORD"
)

// GoRecord is what the fake go saw when a job ran it: its arguments, its
// environment, and the file NETRC named at that moment.
type GoRecord struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	// Netrc is the file NETRC named, or nil when NETRC was unset or empty.
	Netrc *NetrcRecord `json:"netrc,omitempty"`
}

// NetrcRecord is the state of a NETRC file while a go command ran.
type NetrcRecord struct {
	Path string `json:"path"`
	// Mode and DirMode are the permission bits of the file and of its
	// directory.
	Mode    os.FileMode `json:"mode"`
	DirMode os.FileMode `json:"dirMode"`
	Content string      `json:"content"`
	// Err is why the file could not be read, or "".
	Err string `json:"err,omitempty"`
}

// Value returns the value of key in the recorded environment, and whether the
// environment set it.
func (r GoRecord) Value(key string) (string, bool) {
	value, ok := "", false
	for _, entry := range r.Env {
		if name, v, found := strings.Cut(entry, "="); found && name == key {
			value, ok = v, true
		}
	}
	return value, ok
}

// recordGo writes the GoRecord of this invocation into dir.
func recordGo(dir string, args []string) {
	record := GoRecord{Args: args, Env: os.Environ()}
	if netrc := os.Getenv("NETRC"); netrc != "" {
		state := &NetrcRecord{Path: netrc}
		if info, err := os.Stat(netrc); err == nil {
			state.Mode = info.Mode().Perm()
		}
		if info, err := os.Stat(filepath.Dir(netrc)); err == nil {
			state.DirMode = info.Mode().Perm()
		}
		if content, err := os.ReadFile(netrc); err == nil { //nolint:gosec // G703: the fake reads the file the job under test named
			state.Content = string(content)
		} else {
			state.Err = err.Error()
		}
		record.Netrc = state
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	name := fmt.Sprintf("%020d-%d.json", time.Now().UnixNano(), os.Getpid())
	_ = os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

// TrapExitCode is the status a trap exits with.
const TrapExitCode = 97

// ShellPrograms are the programs no lifecycle job may start: the shells and
// the tools the former scripts ran.
var ShellPrograms = []string{
	"sh", "bash", "dash", "zsh", "ksh", "cmd", "powershell", "pwsh",
	"curl", "wget", "tar", "unzip", "gzip", "jq", "sed", "awk", "grep", "find", "cp", "mv", "rm", "ln",
}

// ServeFake runs the fake the test binary was linked as and exits, or returns
// when the binary runs as itself. A TestMain calls it first.
func ServeFake() {
	if os.Getenv(fakeMarker) != "1" {
		return
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	if release, err := os.ReadFile(os.Args[0] + ".goversion"); err == nil {
		serveGoRelease(strings.TrimSpace(string(release)), args)
	}
	if version, err := os.ReadFile(os.Args[0] + ".version"); err == nil {
		if want, err := os.ReadFile(os.Args[0] + ".args"); err == nil && string(want) != strings.Join(args, " ") {
			fmt.Fprintln(os.Stderr, "unsupported arguments: "+strings.Join(args, " ")) //nolint:gosec // G705: a test fake reports its argv to the test
			os.Exit(3)
		}
		fmt.Printf("%s has version %s built with go\n", name, strings.TrimSpace(string(version)))
		os.Exit(0)
	}
	switch {
	case name == "go":
		serveGo(args)
	case os.Getenv(versionPrefix+envName(name)) != "":
		fmt.Printf("%s has version %s built with go\n", name, os.Getenv(versionPrefix+envName(name)))
		os.Exit(0)
	default:
		for _, program := range ShellPrograms {
			if name == program {
				if logPath := os.Getenv(trapLog); logPath != "" {
					if file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
						_, _ = fmt.Fprintln(file, strings.Join(append([]string{name}, args...), " ")) //nolint:gosec // G705: the trap logs its argv to a file the test reads
						_ = file.Close()
					}
				}
				os.Exit(TrapExitCode)
			}
		}
	}
}

func envName(tool string) string {
	return strings.ToUpper(strings.ReplaceAll(tool, "-", "_"))
}

// serveGo stops at the blocking request and runs the real go command for every
// other one, with the same streams and exit status.
func serveGo(args []string) {
	if dir := os.Getenv(goRecord); dir != "" {
		recordGo(dir, args)
	}
	if block := os.Getenv(goBlock); block != "" && strings.Contains(strings.Join(args, " "), block) {
		if ready := os.Getenv(goReady); ready != "" {
			_ = os.WriteFile(ready, []byte("ready\n"), 0o644)
		}
		// A sleep rather than an empty select: with no other goroutine, the
		// runtime aborts an empty select as a deadlock.
		for {
			time.Sleep(time.Hour)
		}
	}
	cmd := exec.Command(os.Getenv(realGo), args...) //nolint:gosec // G702: the test names the real go and the fake forwards its own argv
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		exit := &exec.ExitError{}
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
	os.Exit(0)
}

// serveGoRelease answers the version queries of a go command that runs Go
// release, and fails on any other request.
func serveGoRelease(release string, args []string) {
	switch strings.Join(args, " ") {
	case "env GOVERSION":
		fmt.Println("go" + release)
	case "version":
		fmt.Println("go version go" + release + " " + runtime.GOOS + "/" + runtime.GOARCH)
	default:
		fmt.Fprintln(os.Stderr, "unsupported arguments: "+strings.Join(args, " ")) //nolint:gosec // G705: a test fake reports its argv to the test
		os.Exit(3)
	}
	os.Exit(0)
}

// Fakes is a directory of programs that are the test binary under other
// names, and the environment entries that tell them what to do.
type Fakes struct {
	// Dir is the directory to put first on PATH.
	Dir string
	// TrapLog is the file every trap invocation is appended to.
	TrapLog string
	env     []string
	// records is the directory RecordingGo writes to, or "".
	records string
}

// NewFakes links the test binary into a fresh directory as every shell
// program, each a trap that records its invocation and fails.
func NewFakes(t *testing.T) *Fakes {
	t.Helper()
	f := &Fakes{Dir: t.TempDir()}
	f.TrapLog = filepath.Join(t.TempDir(), "traps.log")
	f.env = []string{fakeMarker + "=1", trapLog + "=" + f.TrapLog}
	for _, program := range ShellPrograms {
		f.link(t, program)
	}
	return f
}

// Tool adds a fake tool that reports version for any arguments.
func (f *Fakes) Tool(t *testing.T, name, version string) string {
	t.Helper()
	f.env = append(f.env, versionPrefix+envName(name)+"="+version)
	return f.link(t, name)
}

// ToolAt links a fake tool at path that reports version. When wantArgs is
// given, it answers only those arguments and fails on any others.
func (f *Fakes) ToolAt(t *testing.T, path, version string, wantArgs ...string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	linkBinary(t, path)
	if err := os.WriteFile(path+".version", []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(wantArgs) > 0 {
		if err := os.WriteFile(path+".args", []byte(strings.Join(wantArgs, " ")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Go adds a fake go command that runs the real one, except for a request whose
// arguments contain block: it writes the ready file, then waits to be killed.
func (f *Fakes) Go(t *testing.T, real, block, ready string) string {
	t.Helper()
	f.env = append(f.env, realGo+"="+real, goBlock+"="+block, goReady+"="+ready)
	return f.link(t, "go")
}

// RecordingGo adds a fake go command that runs the real one and records every
// invocation, which GoRecords returns.
func (f *Fakes) RecordingGo(t *testing.T, real string) string {
	t.Helper()
	f.records = t.TempDir()
	f.env = append(f.env, realGo+"="+real, goRecord+"="+f.records)
	return f.link(t, "go")
}

// GoRecords returns the invocations of the RecordingGo fake so far, in the
// order they started.
func (f *Fakes) GoRecords(t *testing.T) []GoRecord {
	t.Helper()
	if f.records == "" {
		t.Fatal("GoRecords without RecordingGo")
	}
	entries, err := os.ReadDir(f.records)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	records := make([]GoRecord, 0, len(names))
	for _, name := range names {
		var record GoRecord
		if err := json.Unmarshal([]byte(ReadFile(t, filepath.Join(f.records, name))), &record); err != nil {
			t.Fatalf("decode go record %s: %v", name, err)
		}
		records = append(records, record)
	}
	return records
}

// GoRelease links a fake go command at path that reports Go release to
// `go env GOVERSION` and `go version`, and fails on any other request.
func (f *Fakes) GoRelease(t *testing.T, path, release string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	linkBinary(t, path)
	if err := os.WriteFile(path+".goversion", []byte(release+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Environ returns the entries the fakes read.
func (f *Fakes) Environ() []string { return append([]string(nil), f.env...) }

// Invocations returns the trap invocations recorded so far, sorted.
func (f *Fakes) Invocations(t *testing.T) []string {
	t.Helper()
	content, ok := ReadOptional(t, f.TrapLog)
	if !ok || strings.TrimSpace(content) == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	sort.Strings(lines)
	return lines
}

// link places the test binary at Dir under name.
func (f *Fakes) link(t *testing.T, name string) string {
	t.Helper()
	return linkBinary(t, filepath.Join(f.Dir, pkgmeta.ExecutableName(runtime.GOOS, name)))
}

// linkBinary places the test binary at target: a hard link, a symbolic link
// where the file system refuses one, and a copy as the last resort. Windows
// always gets a copy: a hard link to the running test binary cannot be deleted
// while the binary runs, so the test's TempDir cleanup would fail, and a
// standard user cannot create a symbolic link.
func linkBinary(t *testing.T, target string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Link(self, target); err == nil {
			return target
		}
		if err := os.Symlink(self, target); err == nil {
			return target
		}
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}
