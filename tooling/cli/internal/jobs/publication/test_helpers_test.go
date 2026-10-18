package publication

// The release-set and publication tests keep their own copies of the scheduler
// package's test helpers: a test package cannot import another one's. Each
// copy holds only what these tests use.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
)

func TestMain(m *testing.M) {
	// A fixture task (fixtureTask) is this binary running the steps it was
	// handed, in place of a shell script.
	if script := os.Getenv(fixtureScriptEnv); script != "" {
		os.Exit(runFixtureScript(script, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// TestExecutableFixturesAreWrittenUnderTheForkLock keeps every executable
// fixture on writeExecutable: an os.WriteFile or os.OpenFile with an
// executable mode elsewhere reopens the "text file busy" race that helper
// closes. A mode the guard cannot read is refused too, except in a helper
// listed here that sends every executable mode to writeExecutable itself.
// The guard reads the tests of every package under internal/jobs, this one
// included, and each package's own copy of writeExecutable must take the lock.
func TestExecutableFixturesAreWrittenUnderTheForkLock(t *testing.T) {
	t.Parallel()

	routesExecutablesToTheHelper := map[string]bool{
		"mustWriteRuntimeFile": true,
	}

	own, err := filepath.Glob("*_test.go")
	if err != nil || len(own) == 0 {
		t.Fatalf("glob package test files: %v (%d files)", err, len(own))
	}
	// This package sits beside the others under internal/jobs, so one glob of
	// the parent reads its own tests and theirs.
	files, err := filepath.Glob(filepath.Join("..", "*", "*_test.go"))
	if err != nil || len(files) <= len(own) {
		t.Fatalf("glob internal/jobs test files: %v (%d files, %d of them this package's)", err, len(files), len(own))
	}
	fset := token.NewFileSet()
	helperTakesTheLock := false
	routesToTheHelper := map[string]bool{}
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "writeExecutable" {
				takesTheLock := false
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok && isSelector(call.Fun, "syscall", "ForkLock", "RLock") {
						takesTheLock = true
					}
					return true
				})
				if !takesTheLock {
					t.Errorf("%s: writeExecutable does not call syscall.ForkLock.RLock; a fork in a parallel test can inherit its descriptor", fset.Position(fn.Pos()))
				}
				helperTakesTheLock = helperTakesTheLock || takesTheLock
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "writeExecutable" && routesExecutablesToTheHelper[fn.Name.Name] {
					routesToTheHelper[fn.Name.Name] = true
				}
				if len(call.Args) != 3 {
					return true
				}
				if !isSelector(call.Fun, "os", "WriteFile") && !isSelector(call.Fun, "os", "OpenFile") {
					return true
				}
				modeArg := call.Args[2]
				if conversion, ok := modeArg.(*ast.CallExpr); ok && len(conversion.Args) == 1 {
					modeArg = conversion.Args[0] // os.FileMode(0o755)
				}
				lit, ok := modeArg.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					if !routesExecutablesToTheHelper[fn.Name.Name] {
						t.Errorf("%s: %s writes a file with a mode the guard cannot read; pass a literal, or use writeExecutable for an executable", fset.Position(call.Pos()), fn.Name.Name)
					}
					return true
				}
				mode, err := strconv.ParseUint(lit.Value, 0, 32)
				if err == nil && mode&0o111 != 0 {
					t.Errorf("%s: %s writes an executable outside writeExecutable; use writeExecutable", fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	if !helperTakesTheLock {
		t.Error("writeExecutable no longer calls syscall.ForkLock.RLock; a fork in a parallel test can inherit its descriptor again")
	}
	for name := range routesExecutablesToTheHelper {
		if !routesToTheHelper[name] {
			t.Errorf("%s is exempt from this guard but no longer calls writeExecutable; route its executables there or drop the exemption", name)
		}
	}
}

// isSelector reports whether expr is the selector chain names spells, such as
// os.WriteFile or syscall.ForkLock.RLock.
func isSelector(expr ast.Expr, names ...string) bool {
	for i := len(names) - 1; i > 0; i-- {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != names[i] {
			return false
		}
		expr = sel.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == names[0]
}

// fixtureScriptEnv hands a re-executed test binary the steps it runs as a
// fixture task. It is the portable stand-in for a #! shell script, which
// Windows cannot run: TestMain runs the steps and exits.
const fixtureScriptEnv = "PUTNAMI_JOBS_FIXTURE_SCRIPT"

// fixtureScript is what a fixture task does, one step per entry: an operation
// and its operands. An operand "$1" to "$9" names the task's argument. See
// runFixtureScript for the operations.
type fixtureScript [][]string

// then returns s followed by more.
func (s fixtureScript) then(more fixtureScript) fixtureScript {
	return append(append(fixtureScript(nil), s...), more...)
}

// fixtureTask makes def run this test binary as a task that does s.
func fixtureTask(t *testing.T, def *extension.JobDefinition, s fixtureScript) {
	t.Helper()
	command, env := fixtureCommand(t, s)
	def.Command = command
	merged := map[string]string{}
	for key, value := range def.Env {
		merged[key] = value
	}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		merged[key] = value
	}
	def.Env = merged
}

// fixtureCommand returns this test binary and the environment entries that
// make it do s.
func fixtureCommand(t *testing.T, s fixtureScript) (string, []string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return executable, []string{
		// Base64, because the runner expands {name} templates in task
		// environment values and a step may carry JSON.
		fixtureScriptEnv + "=" + base64.StdEncoding.EncodeToString(encoded),
		// A race-instrumented binary otherwise sleeps a second before it exits.
		"GORACE=atexit_sleep_ms=0",
	}
}

// runFixtureScript runs the steps encoded in encoded with args as the task's
// arguments, and returns the task's exit status.
func runFixtureScript(encoded string, args []string) int {
	data, err := base64.StdEncoding.DecodeString(encoded)
	var steps fixtureScript
	if err == nil {
		err = json.Unmarshal(data, &steps)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture script: %v\n", err)
		return 125
	}
	operand := func(value string) string {
		if len(value) == 2 && value[0] == '$' && value[1] >= '1' && value[1] <= '9' {
			if index := int(value[1] - '1'); index < len(args) {
				return args[index]
			}
			return ""
		}
		return value
	}
	fail := func(step []string, err error) int {
		fmt.Fprintf(os.Stderr, "fixture script %q: %v\n", step, err)
		return 125
	}
	for _, step := range steps {
		ops := make([]string, len(step))
		for i, value := range step {
			ops[i] = operand(value)
		}
		switch ops[0] {
		case "print": // print TEXT: writes TEXT and a newline to stdout.
			fmt.Println(ops[1])
		case "print-if": // print-if TEXT CONDITION...: print TEXT expanded (expandFixture) when every condition holds.
			if fixtureConditionsHold(ops[2:], args) {
				fmt.Println(expandFixture(ops[1], args))
			}
		case "cat": // cat PATH: copies a file to stdout.
			data, err := os.ReadFile(ops[1])
			if err != nil {
				return fail(step, err)
			}
			_, _ = os.Stdout.Write(data)
		case "append-env": // append-env TEXT PATH: append, both operands expanded (expandFixture).
			file, err := os.OpenFile(expandFixture(ops[2], args), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				return fail(step, err)
			}
			_, err = file.WriteString(expandFixture(ops[1], args))
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fail(step, err)
			}
		case "write": // write TEXT PATH
			if err := os.WriteFile(ops[2], []byte(ops[1]), 0o644); err != nil {
				return fail(step, err)
			}
		case "copy-tree": // copy-tree SOURCE TARGET: copies the tree SOURCE into TARGET, both expanded (expandFixture).
			if err := copyFixtureTree(expandFixture(ops[1], args), expandFixture(ops[2], args)); err != nil {
				return fail(step, err)
			}
		case "write-environ": // write-environ PATH: writes the environment, one variable a line.
			if err := os.WriteFile(ops[1], []byte(strings.Join(os.Environ(), "\n")), 0o644); err != nil {
				return fail(step, err)
			}
		case "write-mode": // write-mode NAME PATH: writes the permission bits of the path the variable names.
			info, err := os.Stat(os.Getenv(ops[1]))
			if err == nil {
				err = os.WriteFile(ops[2], []byte(fmt.Sprintf("%#o", info.Mode().Perm())), 0o644)
			}
			if err != nil {
				return fail(step, err)
			}
		case "exit-unless": // exit-unless STATUS CONDITION...: exits with STATUS unless every condition holds.
			if !fixtureConditionsHold(ops[2:], args) {
				status, _ := strconv.Atoi(ops[1])
				return status
			}
		case "exit-if": // exit-if STATUS CONDITION...: exits with STATUS when every condition holds.
			if fixtureConditionsHold(ops[2:], args) {
				status, _ := strconv.Atoi(ops[1])
				return status
			}
		default:
			return fail(step, errors.New("unknown operation"))
		}
	}
	return 0
}

// copyFixtureTree copies the directories and regular files under source into
// target, which may exist.
func copyFixtureTree(source, target string) error {
	if source == "" || target == "" {
		return errors.New("copy-tree needs a source and a target")
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	})
}

// expandFixture expands text as a shell does: $NAME and ${NAME} from the
// environment, $PWD to the working directory, $1 to $9 to the task's
// arguments, and ${NAME+x} to "x" when NAME is set.
func expandFixture(text string, args []string) string {
	return os.Expand(text, func(name string) string {
		if set, ok := strings.CutSuffix(name, "+x"); ok {
			if _, present := os.LookupEnv(set); present {
				return "x"
			}
			return ""
		}
		if index, err := strconv.Atoi(name); err == nil {
			if index >= 1 && index <= len(args) {
				return args[index-1]
			}
			return ""
		}
		if name == "PWD" {
			dir, _ := os.Getwd()
			return dir
		}
		return os.Getenv(name)
	})
}

// fixtureConditionsHold reports whether every condition holds for a task
// started with args: "nonempty:PATH" (PATH is a file with content) or
// "arg:N=VALUE" (argument N is VALUE). A leading "!" negates a condition.
func fixtureConditionsHold(conditions []string, args []string) bool {
	for _, condition := range conditions {
		negated := strings.HasPrefix(condition, "!")
		kind, operand, _ := strings.Cut(strings.TrimPrefix(condition, "!"), ":")
		name, value, _ := strings.Cut(operand, "=")
		var holds bool
		switch kind {
		case "nonempty":
			info, err := os.Stat(operand)
			holds = err == nil && info.Size() > 0
		case "arg":
			index, _ := strconv.Atoi(name)
			holds = index >= 1 && index <= len(args) && args[index-1] == value
		}
		if holds == negated {
			return false
		}
	}
	return true
}

// capturedCapabilityContext captures token and after as the trusted process
// does, and returns the context that carries them.
func capturedCapabilityContext(t *testing.T, token, after string) context.Context {
	t.Helper()
	t.Setenv(extensionproto.CloudTokenEnv, token)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)
	return scheduler.CaptureProcessCapabilities(context.Background())
}

// mockRenderer is a thread-safe test renderer that records job events.
type mockRenderer struct {
	mu        sync.Mutex
	jobEvents map[string][]jobmodel.RawJobEvent
}

func (*mockRenderer) Start([]*jobmodel.ScheduledJob) {}

func (*mockRenderer) JobStart(*jobmodel.ScheduledJob) {}

func (m *mockRenderer) JobEvent(job *jobmodel.ScheduledJob, event jobmodel.RawJobEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobEvents == nil {
		m.jobEvents = map[string][]jobmodel.RawJobEvent{}
	}
	m.jobEvents[job.Key()] = append(m.jobEvents[job.Key()], event)
}

// eventsOfType returns the recorded job:event records of a given type for a job
// key, in emission order.
func (m *mockRenderer) eventsOfType(jobKey, eventType string) []jobmodel.RawJobEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []jobmodel.RawJobEvent
	for _, ev := range m.jobEvents[jobKey] {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func (*mockRenderer) JobComplete(*jobmodel.ScheduledJob, *jobmodel.JobResult) {}

func (*mockRenderer) Finish(map[string]*jobmodel.JobResult, jobmodel.SessionOutcome) {}

func envValues(env []string, key string) []string {
	prefix := key + "="
	var values []string
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			values = append(values, strings.TrimPrefix(entry, prefix))
		}
	}
	return values
}

// findJobsRepoRoot locates the workspace root so a test can read the REAL
// manifests. It skips rather than fails when the tree is unavailable, so a
// packaged-source test run stays green.
func findJobsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find the Putnami repo root from %s", start)
		}
		dir = parent
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeProjectFile(t *testing.T, root, projPath, rel, content string) {
	t.Helper()
	full := filepath.Join(root, projPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
