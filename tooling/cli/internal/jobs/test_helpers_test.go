package jobs

import (
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
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// unboundedJobTimeoutMs is the wall-clock budget a fixture job carries when
// the budget is not what the test proves: none.
//
// A test that forks /bin/sh to exit in milliseconds does not need a budget to
// terminate, and a budget it does not need can still fire: it is wall clock,
// so a loaded host can spend the whole allowance on the fork alone (one
// measurement showed 5.003 s of wall for 578 µs of CPU). The timed-out job then reports
// status "failed" and "job timed out" — byte-identical to the regression the
// assertions guard — so machine load decided the verdict, and a later
// measurement found two gates lost to exactly that on an unmodified tree. Raising the number
// only moves the load at which the verdict flips, so the budget is removed.
//
// A negative timeout is the manifest's own "unbounded" sentinel: resolved by
// ScheduledJob.EffectiveTimeoutMs, RunJob installs no deadline, and
// applyTaskDeadline strips the deadline variable from the child environment.
// The Go test binary's -timeout remains the backstop, and it fails the package
// with a stack dump no assertion can misread. Tests whose subject IS the
// budget (deadline propagation, coalescing fallback, the runtime handshake
// racing a 1 ms timeout) keep a literal and are listed in
// TestFixtureJobsCarryNoIncidentalWallClockBudget.
const unboundedJobTimeoutMs = -1

// answerWaitBudget bounds a wait for an answer that MUST arrive — a goroutine
// entering a hook, a follower unblocking on cancellation — when the regression
// the test guards is exactly that the answer never comes. t.Context is no use
// there: it is canceled only after the test body returns, which the blocked
// select prevents. Such a wait stays bounded so a regression fails with a
// message naming what was waited for instead of a package-wide stack dump,
// but the bound is a hang detector, never a latency budget: 1–2 s waits lost
// gates to scheduling delay alone, so it sits far above any host load.
const answerWaitBudget = 60 * time.Second

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	writeTestFile(t, path, contents)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// burnCPUShell is the shell fragment every rusage-asserting fixture runs
// first: ~60ms of pure arithmetic. getrusage accounts CPU in clock ticks, so a
// stand-in that only printfs finishes inside a single tick and the kernel
// honestly reports zero CPU — which is exactly what happened on CI, where two
// 2-3ms executions came back with 0 CPU and a populated MaxRSS, failing the
// ledger assertions on a claim about tick granularity rather than about the
// ledger. The loop size clears any tick size a runner is likely to use.
// Sleeping instead would accrue wall time with no CPU time and make the same
// assertions pass for the wrong reason. One calibrated burn, shared by every
// site: the previous four copies had drifted to 20k vs 200k iterations.
func burnCPUShell() string {
	return "i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done\n"
}

// burnCPUDuration is the "burn" step's share of the same calibration: CPU for
// several ticks of any tick size a runner is likely to use.
const burnCPUDuration = 60 * time.Millisecond

// TestFixtureJobsCarryNoIncidentalWallClockBudget pins the two halves of
// this rule together, the way TestCapabilityProbesKeepHostLoadOutOfTheSecurityVerdict
// pins a related one: the sentinel really is unbounded on every path that applies a
// budget, and no fixture job in this package carries a literal budget unless
// its test is ABOUT that budget. The second half reads this package's own
// test sources with go/ast (the idiom internal/cli's ratchets use), because a
// budget is reintroduced one "TimeoutMs: 5000" at a time and nothing else
// would notice until a loaded gate did.
//
// The exception list is a census, not an allowlist: every name must still
// carry a literal budget, so a test that stops needing one leaves the list.
func TestFixtureJobsCarryNoIncidentalWallClockBudget(t *testing.T) {
	t.Parallel()

	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/test", TimeoutMs: unboundedJobTimeoutMs},
	}
	if got := job.EffectiveTimeoutMs(); got >= 0 {
		t.Fatalf("EffectiveTimeoutMs = %d for the unbounded sentinel, want < 0 so no wall clock decides a fixture verdict", got)
	}
	prefix := extensionproto.TaskDeadlineMsEnv + "="
	for _, entry := range applyTaskDeadline([]string{"PATH=/bin", prefix + "5000"}, job) {
		if strings.HasPrefix(entry, prefix) {
			t.Fatalf("an unbounded fixture job exported %q to its child; the sentinel must strip every deadline", entry)
		}
	}

	// Tests whose subject is the budget itself: deadline propagation, a
	// coalesced waiter outliving or falling back on a job timeout, the finalizer
	// cleanup context derived from it, and the runtime handshake racing a 1 ms
	// timeout. A timed-out result there is the behavior under test, not a
	// verdict host load can forge.
	budgetIsTheSubject := map[string]bool{
		"TestRuntimeSynchronizationPrecedesTaskTimeoutAndInvocation":     true,
		"TestScheduler_CoalescedWaiterReclaimsAbandonedLease":            true,
		"TestScheduler_CoalescedWaiterFallsBackAfterJobTimeout":          true,
		"TestScheduler_CoalescingHonorsBreakEvenFloor":                   true,
		"TestFinalizerCleanupContext_IsIndependentOnlyAfterCancellation": true,
		"TestBatchLeaderDeadlineDoesNotMutateMembers":                    true,
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob package test files: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
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
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				kv, ok := node.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "TimeoutMs" {
					return true
				}
				lit, ok := kv.Value.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return true // an identifier, a call, or a negative literal: no budget
				}
				budget, err := strconv.ParseInt(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
				if err != nil || budget <= 0 {
					return true
				}
				if budgetIsTheSubject[fn.Name.Name] {
					seen[fn.Name.Name] = true
					return true
				}
				t.Errorf("%s: %s stamps a fixture job with TimeoutMs %d; use unboundedJobTimeoutMs, or list the test here if the budget is what it proves",
					fset.Position(kv.Pos()), fn.Name.Name, budget)
				return true
			})
		}
	}
	for name := range budgetIsTheSubject {
		if !seen[name] {
			t.Errorf("%s no longer stamps a literal budget; remove it from the exception census", name)
		}
	}
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
		case "print-env": // print-env TEXT: print with TEXT expanded (expandFixture).
			fmt.Println(expandFixture(ops[1], args))
		case "print-if": // print-if TEXT CONDITION...: print-env when every condition holds.
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
		case "log": // log MESSAGE: prints an info log event carrying MESSAGE.
			printFixtureLog(ops[1])
		case "flood": // flood N: prints N log events, "ordinary-0" onwards.
			count, _ := strconv.Atoi(ops[1])
			for i := range count {
				printFixtureLog("ordinary-" + strconv.Itoa(i))
			}
		case "stderr": // stderr TEXT: writes TEXT and a newline to stderr.
			fmt.Fprintln(os.Stderr, ops[1])
		case "cat-to-stderr": // cat-to-stderr PATH: copies a file to stderr.
			data, err := os.ReadFile(ops[1])
			if err != nil {
				return fail(step, err)
			}
			_, _ = os.Stderr.Write(data)
		case "write", "append": // write|append TEXT PATH
			flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			if ops[0] == "append" {
				flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
			}
			file, err := os.OpenFile(ops[2], flags, 0o644)
			if err != nil {
				return fail(step, err)
			}
			_, err = file.WriteString(ops[1])
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fail(step, err)
			}
		case "append-file": // append-file SOURCE PATH: appends SOURCE when it exists.
			data, err := os.ReadFile(ops[1])
			if err != nil {
				continue
			}
			file, err := os.OpenFile(ops[2], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				return fail(step, err)
			}
			_, _ = file.Write(data)
			_ = file.Close()
		case "remove": // remove PATH
			if err := os.Remove(ops[1]); err != nil && !os.IsNotExist(err) {
				return fail(step, err)
			}
		case "write-env": // write-env NAME PATH: writes the variable's value.
			if err := os.WriteFile(ops[2], []byte(os.Getenv(ops[1])), 0o644); err != nil {
				return fail(step, err)
			}
		case "write-pid": // write-pid PATH
			if err := os.WriteFile(ops[1], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				return fail(step, err)
			}
		case "read-job-credential": // read-job-credential PATH: writes jobCredentialReport.
			if err := os.WriteFile(ops[1], []byte(jobCredentialReport(args)), 0o644); err != nil {
				return fail(step, err)
			}
		case "kill-self": // kill-self: ends the task at once, as SIGKILL does.
			process, err := os.FindProcess(os.Getpid())
			if err == nil {
				err = process.Kill()
			}
			if err != nil {
				return fail(step, err)
			}
			time.Sleep(time.Minute)
		case "copy-flag-file": // copy-flag-file FLAG PATH: copies the file named after FLAG.
			for i := 0; i+1 < len(args); i++ {
				if args[i] != ops[1] {
					continue
				}
				data, err := os.ReadFile(args[i+1])
				if err == nil {
					err = os.WriteFile(ops[2], data, 0o644)
				}
				if err != nil {
					return fail(step, err)
				}
			}
		case "provision": // provision: the invocation fixture's provider.
			if err := provisionFixture(args); err != nil {
				return fail(step, err)
			}
		case "consume": // consume: the invocation fixture's consumer; exits 3 without the credential.
			data, err := os.ReadFile(args[0])
			if err != nil || !strings.Contains(string(data), "DATABASE_URL") {
				return 3
			}
			if err := os.WriteFile(args[1], []byte("ok"), 0o644); err != nil {
				return fail(step, err)
			}
		case "observe-invocations": // observe-invocations: lists the invocations directory into $4.
			entries, err := os.ReadDir(filepath.Dir(filepath.Dir(args[1])))
			if err != nil {
				return fail(step, err)
			}
			var names strings.Builder
			for _, entry := range entries {
				names.WriteString(entry.Name() + "\n")
			}
			if err := os.WriteFile(args[3], []byte(names.String()), 0o644); err != nil {
				return fail(step, err)
			}
		case "verdict": // verdict CONDITION...: prints a success result when every condition holds, else a failed one.
			status := "success"
			if !fixtureConditionsHold(ops[1:], args) {
				status = "failed"
			}
			fmt.Printf(`{"v":2,"type":"result","data":{"status":%q}}`+"\n", status)
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
		case "fail-once": // fail-once MARKER TEXT STATUS: without MARKER, creates it, prints TEXT and exits with STATUS.
			if _, err := os.Stat(ops[1]); err == nil {
				continue
			}
			if err := os.WriteFile(ops[1], nil, 0o644); err != nil {
				return fail(step, err)
			}
			fmt.Println(ops[2])
			status, _ := strconv.Atoi(ops[3])
			return status
		case "burn": // burn: spends burnCPUDuration of CPU (see burnCPUShell).
			for start := time.Now(); time.Since(start) < burnCPUDuration; {
			}
		case "sleep": // sleep SECONDS
			seconds, _ := strconv.ParseFloat(ops[1], 64)
			time.Sleep(time.Duration(seconds * float64(time.Second)))
		case "exit": // exit STATUS
			status, _ := strconv.Atoi(ops[1])
			return status
		default:
			return fail(step, errors.New("unknown operation"))
		}
	}
	return 0
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
// started with args: "unset:NAME" (NAME is absent from the environment),
// "equal:NAME=VALUE" (NAME's value, empty when absent, is VALUE),
// "same:NAME=OTHER" (the two variables' values are equal, each empty when
// absent), "nonempty:PATH" (PATH is a file with content) or "arg:N=VALUE"
// (argument N is VALUE). A leading "!" negates a condition.
func fixtureConditionsHold(conditions []string, args []string) bool {
	for _, condition := range conditions {
		negated := strings.HasPrefix(condition, "!")
		kind, operand, _ := strings.Cut(strings.TrimPrefix(condition, "!"), ":")
		name, value, _ := strings.Cut(operand, "=")
		var holds bool
		switch kind {
		case "unset":
			_, set := os.LookupEnv(name)
			holds = !set
		case "equal":
			holds = os.Getenv(name) == value
		case "same":
			holds = os.Getenv(name) == os.Getenv(value)
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

// printFixtureLog prints one v2 info log event carrying message.
func printFixtureLog(message string) {
	line, _ := json.Marshal(struct {
		V       int    `json:"v"`
		Type    string `json:"type"`
		Level   string `json:"level"`
		Message string `json:"message"`
	}{2, "log", "info", message})
	fmt.Println(string(line))
}

// provisionFixture writes the credential to $1, creating its directory, and
// stamps the invocation id, the directory that owns the artifact root $2, onto
// the external resource $3.
func provisionFixture(args []string) error {
	if err := os.MkdirAll(filepath.Dir(args[0]), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(args[0], []byte("DATABASE_URL="+fixtureSecret+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(args[2], []byte(filepath.Base(filepath.Dir(args[1]))+"\n"), 0o644)
}
