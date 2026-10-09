package treecmd

// Synthetic consistency fixtures, not evidence of a real gate or agent pilot.
//
// Each fixture is a real Git repository with a real CI policy and real
// evidence files. Only the two producers are synthetic: the tree fingerprint is
// the injected Verifier.Fingerprint, and the native plan is the injected
// Verifier.Plan, or, for the cases that exercise the default plan, a fake
// ./putnamiw script that prints .context/plan.json or
// .context/plan-fix-false.json.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

var (
	fxTree      = strings.Repeat("a", 64)
	fxOtherTree = strings.Repeat("b", 64)
	// fxCommands is the command list the fixture CI policy declares.
	fxCommands = []string{"lint", "test", "build", "validate", "validate-workspace"}
)

const (
	fxStart = "2026-09-23T10:00:00Z"
	fxEnd   = "2026-09-23T10:00:01Z"
)

// fxPutnamiw answers the native plan only: it records its arguments, prints
// .context/plan-fix-false.json when it receives --fix=false and that file
// exists, .context/plan.json otherwise, and exits with the code in
// .context/plan-exit, 0 when that file is absent.
const fxPutnamiw = `#!/bin/sh
printf '%s\n' "$@" > .context/plan-args
plan=.context/plan.json
for arg in "$@"; do
  if [ "$arg" = --fix=false ] && [ -f .context/plan-fix-false.json ]; then plan=.context/plan-fix-false.json; fi
done
cat "$plan"
code=0
if [ -f .context/plan-exit ]; then code=$(cat .context/plan-exit); fi
exit "$code"
`

type (
	fxObj = map[string]any
	fxArr = []any
)

// fxFingerprint changes the injected fingerprint on its second call, the call
// the verifier makes after it read every piece of evidence.
type fxFingerprint struct {
	mutateTree     bool
	mutateEvidence string
}

type verifyFixture struct {
	t       *testing.T
	root    string
	head    string
	record  fxObj
	session fxObj
	report  fxObj
	qualify fxObj
	plan    fxObj
	// fixFalsePlan is the native plan with --fix=false; nil when it is plan.
	fixFalsePlan fxObj
	// planCommands, planFlags and planBaseline are the request the native plan
	// expects.
	planCommands []string
	planFlags    []string
	planBaseline string
	// cliPlan leaves Verifier.Plan nil, so the fake ./putnamiw answers.
	cliPlan bool

	mu          sync.Mutex
	calls       int
	fingerprint fxFingerprint
}

type verifyResult struct {
	err   error
	out   string
	value fxObj
}

func requireVerifyGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	requireVerifyGit(t)
	f := &verifyFixture{t: t, root: t.TempDir()}
	f.git("init", "-q", "-b", "main")
	f.git("config", "user.name", "Verification Fixture")
	f.git("config", "user.email", "fixture@example.invalid")
	f.git("config", "commit.gpgSign", "false")
	// A global hooksPath must not run inside the fixture.
	f.git("config", "core.hooksPath", filepath.Join(f.root, ".git", "no-hooks"))
	f.write(".gitignore", ".context/\n")
	f.write("app.txt", "original\n")
	f.write("putnami.ci.json", fxObj{"version": 3, "commands": fxStrings(fxCommands), "flags": fxArr{"--enforce-coverage"}})
	f.write("putnamiw", fxPutnamiw)
	if err := os.Chmod(filepath.Join(f.root, "putnamiw"), 0o755); err != nil {
		t.Fatalf("chmod putnamiw: %v", err)
	}
	f.git("add", ".")
	f.git("commit", "-q", "-m", "Synthetic initial tree")
	f.head = f.git("rev-parse", "HEAD")
	f.write("app.txt", "changed\n")

	initial := f.invoke("--snapshot", "--base", f.head)
	if initial.err != nil {
		t.Fatalf("snapshot: %v: %s", initial.err, initial.out)
	}
	if _, ok := initial.value["verdict"]; ok {
		t.Fatalf("snapshot carries a verdict: %s", initial.out)
	}
	f.record = initial.value
	f.record["objective"] = f.note("objective", "Exercise the declared behavior.")
	f.record["scope"] = f.note("scope", "Application scope; app.txt.")
	f.record["implementers"] = fxArr{"implementation-session"}
	context := f.note("context", "Contract version one.")
	proposal := f.note("proposal", "Keep application ownership.")
	consultation := fxObj{
		"scope": "/application", "owner": "consulted-owner-session",
		"context": fxArr{context}, "proposal": proposal,
		"design": fxObj{"position": "accepted", "note": f.note("design", "Design accepted.")},
		"realization": fxObj{"position": "accepted", "fingerprint": fxTree,
			"note": f.note("realization", "Realization satisfies contract.")},
		"objections": fxArr{},
	}
	for _, phase := range []string{"design", "realization"} {
		opinion := consultation[phase].(fxObj)
		opinion["proposalSha256"] = proposal["sha256"]
		opinion["contextSha256"] = fxArr{context["sha256"]}
	}
	f.record["scopes"] = fxArr{consultation}
	f.record["review"] = fxObj{"author": "review-session", "fingerprint": fxTree,
		"report": f.note("review", "Independent synthetic review fixture."),
		"coverage": fxArr{fxObj{"path": "app.txt", "status": "reviewed",
			"reason": "Checked behavior and application ownership."}},
		"findings": fxArr{}}
	f.record["acceptance"] = fxArr{fxObj{"requirement": "Required behavior", "status": "passed",
		"evidence": f.note("acceptance", "Synthetic acceptance fixture.")}}

	counts := func() fxObj {
		return fxObj{"total": 5, "succeeded": 5, "failed": 0, "skipped": 0, "canceled": 0}
	}
	reuse := func() fxObj { return fxObj{"localCache": 0, "remoteCache": 0, "coalesced": 0} }
	run := func() fxObj {
		return fxObj{"outcome": "success", "exitCode": 0, "counts": counts(), "reuse": reuse(), "durationMs": 1000}
	}
	tasks := fxArr{}
	for _, command := range fxCommands {
		tasks = append(tasks, f.task(command))
	}
	f.session = fxObj{"protocolVersion": 2, "sessionId": "synthetic-gate", "startTime": fxStart, "endTime": fxEnd,
		"commands":  fxStrings(fxCommands),
		"selection": fxObj{"mode": "impacted", "scoped": true, "projects": fxArr{"/application"}},
		"git":       fxObj{"baseline": f.head},
		"tree":      fxObj{"fingerprint": fxTree, "headSHA": f.head, "dirty": true},
		"run":       run(), "tasks": tasks}
	f.plan = fxObj{"protocolVersion": 2, "status": "success", "exitCode": 0,
		"plan": fxObj{"dryRun": true, "tasks": fxArr{}}}
	f.planFromSession()
	f.planCommands = slices.Clone(fxCommands)
	f.planFlags = []string{"--enforce-coverage"}
	f.planBaseline = f.head
	f.writePlan()
	f.report = fxObj{"protocolVersion": 2, "sessionId": "synthetic-gate", "startTime": fxStart, "endTime": fxEnd,
		"origin": "cli", "enforceCoverage": true, "git": fxObj{"sha": f.head, "dirty": true}, "run": run(),
		"commands": fxArr{}, "jobs": fxArr{}, "elidedJobs": 0}
	for _, command := range fxCommands {
		commandCounts := counts()
		commandCounts["total"], commandCounts["succeeded"] = 1, 1
		fxAppend(f.report, "commands", fxObj{"command": command, "counts": commandCounts,
			"reuse": reuse(), "freshWallMs": 200, "errors": 0, "warnings": 0})
		fxAppend(f.report, "jobs", fxObj{"key": "/application:" + command, "project": "/application",
			"task": command, "command": command, "outcome": "success", "reuse": "none", "durationMs": 200})
	}
	phases := fxArr{}
	for _, name := range []string{"resolve-target", "readiness", "version-binding", "smoke", "teardown"} {
		phases = append(phases, fxObj{"name": name, "state": "passed", "durationMs": 1})
	}
	f.qualify = fxObj{"protocolVersion": 1, "project": "/application",
		"target":  fxObj{"kind": "local", "url": "http://127.0.0.1:12345", "compositionId": "fixture"},
		"binding": fxObj{"kind": "tree", "fingerprint": fxTree, "headSHA": f.head, "dirty": true},
		"contract": fxObj{"digest": "sha256:" + strings.Repeat("c", 64), "requests": 1, "derivedFrom": fxArr{
			fxObj{"kind": "http-routes", "path": "schema/http-routes.json", "digest": "sha256:" + strings.Repeat("d", 64)}}},
		"phases":   phases,
		"requests": fxArr{fxObj{"id": "GET /items", "status": 200, "durationMs": 1, "state": "passed"}},
		"state":    "passed", "startedAt": fxStart, "finishedAt": fxEnd,
		"cleanup": fxObj{"state": "clean", "leftovers": fxArr{}}}
	f.bindProducers()
	return f
}

func (f *verifyFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// write stores a string or bytes as is and any other value as compact JSON.
func (f *verifyFixture) write(relative string, value any) {
	f.t.Helper()
	var data []byte
	switch value := value.(type) {
	case string:
		data = []byte(value)
	case []byte:
		data = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			f.t.Fatalf("encode %s: %v", relative, err)
		}
		data = encoded
	}
	location := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(location), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", relative, err)
	}
	if err := os.WriteFile(location, data, 0o644); err != nil {
		f.t.Fatalf("write %s: %v", relative, err)
	}
}

func (f *verifyFixture) readJSON(relative string) fxObj {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err != nil {
		f.t.Fatalf("read %s: %v", relative, err)
	}
	var value fxObj
	if err := json.Unmarshal(data, &value); err != nil {
		f.t.Fatalf("decode %s: %v", relative, err)
	}
	return value
}

func (f *verifyFixture) remove(relative string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(relative))); err != nil {
		f.t.Fatalf("remove %s: %v", relative, err)
	}
}

func (f *verifyFixture) reference(relative string) fxObj {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(relative)))
	if err != nil {
		f.t.Fatalf("read %s: %v", relative, err)
	}
	sum := sha256.Sum256(data)
	return fxObj{"path": relative, "sha256": hex.EncodeToString(sum[:])}
}

func (f *verifyFixture) note(name, content string) fxObj {
	f.t.Helper()
	relative := ".context/" + name + ".md"
	f.write(relative, content)
	return f.reference(relative)
}

func (f *verifyFixture) task(command string) fxObj {
	return fxObj{"identity": fxObj{"key": "/application:" + command, "scope": "project",
		"project":  fxObj{"id": "/application", "name": "application"},
		"task":     fxObj{"name": command, "command": command, "kind": "command"},
		"provider": fxObj{"extension": "fixture"}},
		"status": "success", "exitCode": 0, "reuse": "none", "durationMs": 200}
}

// projectTask is task(command) for another project.
func (f *verifyFixture) projectTask(command, name string) fxObj {
	task := f.task(command)
	identity := task["identity"].(fxObj)
	identity["project"] = fxObj{"id": "/" + name, "name": name}
	identity["key"] = "/" + name + ":" + command
	return task
}

func (f *verifyFixture) bindProducers() {
	f.t.Helper()
	f.write(".context/session.json", f.session)
	f.write(".context/report.json", f.report)
	f.write(".context/qualify.json", f.qualify)
	f.record["gate"] = fxObj{"session": f.reference(".context/session.json"),
		"report": f.reference(".context/report.json")}
	f.record["qualification"] = fxObj{"required": fxArr{"/application"},
		"results": fxArr{f.reference(".context/qualify.json")}, "notApplicable": nil}
}

// planFromSession makes the native plan the identities of the session tasks.
func (f *verifyFixture) planFromSession() {
	tasks := fxArr{}
	for _, task := range fxArrAt(f.session, "tasks") {
		tasks = append(tasks, fxObj{"identity": fxClone(fxAt(task, "identity"))})
	}
	fxObjAt(f.plan, "plan")["tasks"] = tasks
}

func (f *verifyFixture) writePlan() {
	f.t.Helper()
	f.write(".context/plan.json", f.plan)
	if f.fixFalsePlan != nil {
		f.write(".context/plan-fix-false.json", f.fixFalsePlan)
	}
}

// checkOnlyLint models a lint pipeline that --fix selects: the gate ran
// /application:lint~check-only, the native plan holds lint~check and
// lint~format instead, and the native plan with --fix=false holds
// lint~check-only.
func (f *verifyFixture) checkOnlyLint() {
	f.t.Helper()
	lintTask := func(name string) fxObj {
		task := f.task("lint")
		identity := task["identity"].(fxObj)
		identity["key"] = "/application:" + name
		identity["task"].(fxObj)["name"] = name
		return task
	}
	tasks := fxArr{}
	for _, task := range fxArrAt(f.session, "tasks") {
		if fxAt(task, "identity", "task", "command") == "lint" {
			task = lintTask("lint~check-only")
		}
		tasks = append(tasks, task)
	}
	f.session["tasks"] = tasks
	f.planFromSession()
	f.fixFalsePlan = fxClone(f.plan).(fxObj)
	planned := fxArr{}
	for _, task := range fxArrAt(f.plan, "plan", "tasks") {
		if fxAt(task, "identity", "key") == "/application:lint~check-only" {
			for _, name := range []string{"lint~check", "lint~format"} {
				planned = append(planned, fxObj{"identity": lintTask(name)["identity"]})
			}
			continue
		}
		planned = append(planned, task)
	}
	fxObjAt(f.plan, "plan")["tasks"] = planned
	f.writePlan()
	f.bindProducers()
}

// refreshPolicies rebinds every recorded policy reference to its current bytes.
func (f *verifyFixture) refreshPolicies() {
	f.t.Helper()
	policies := fxArr{}
	for _, item := range fxArrAt(f.record, "policies") {
		policies = append(policies, f.reference(fxAt(item, "path").(string)))
	}
	f.record["policies"] = policies
}

// setPolicyFlags replaces the flags of the fixture CI policy and binds the
// dossier to the changed policy file.
func (f *verifyFixture) setPolicyFlags(flags ...string) {
	f.t.Helper()
	policy := f.readJSON("putnami.ci.json")
	policy["flags"] = fxStrings(flags)
	f.write("putnami.ci.json", policy)
	f.refreshPolicies()
	f.record["changedFiles"] = fxArr{"app.txt", "putnami.ci.json"}
	fxAppend(fxObjAt(f.record, "review"), "coverage", fxObj{"path": "putnami.ci.json", "status": "reviewed",
		"reason": "Synthetic CI policy flags."})
}

// useCLIPlan leaves the native plan to the default Plan, which runs the fake
// ./putnamiw. Windows never runs that script, so the case is skipped there.
func (f *verifyFixture) useCLIPlan(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the default plan runs the putnami on PATH on Windows, not the fixture script")
	}
	f.cliPlan = true
}

func (f *verifyFixture) fakeFingerprint(string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 2 && f.fingerprint.mutateEvidence != "" {
		location := filepath.Join(f.root, filepath.FromSlash(f.fingerprint.mutateEvidence))
		if err := os.WriteFile(location, []byte("changed while checking"), 0o644); err != nil {
			f.t.Errorf("mutate evidence: %v", err)
		}
	}
	if f.calls == 2 && f.fingerprint.mutateTree {
		return fxOtherTree, nil
	}
	return fxTree, nil
}

func (f *verifyFixture) fakePlan(_ string, commands, flags []string, baseSHA string) ([]string, error) {
	if got, want := slices.Sorted(slices.Values(commands)), slices.Sorted(slices.Values(f.planCommands)); !slices.Equal(got, want) {
		f.t.Errorf("plan commands = %v, want the set %v", commands, f.planCommands)
	}
	if !slices.Equal(flags, f.planFlags) {
		f.t.Errorf("plan flags = %q, want %q", flags, f.planFlags)
	}
	if baseSHA != f.planBaseline {
		f.t.Errorf("plan baseline = %q, want %q", baseSHA, f.planBaseline)
	}
	plan := f.plan
	if f.fixFalsePlan != nil && slices.Contains(flags, "--fix=false") {
		plan = f.fixFalsePlan
	}
	keys := []string{}
	for _, task := range fxArrAt(plan, "plan", "tasks") {
		keys = append(keys, fxAt(task, "identity", "key").(string))
	}
	return keys, nil
}

// checkCLIPlanRequest checks the arguments the fake ./putnamiw received: the
// required commands, then exactly --impacted --baseline <base SHA>, the plan
// flags, --plan and --output=json.
func (f *verifyFixture) checkCLIPlanRequest() {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, ".context", "plan-args"))
	if err != nil {
		f.t.Errorf("the workspace CLI never received a plan request: %v", err)
		return
	}
	args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := slices.Concat([]string{"--impacted", "--baseline", f.planBaseline}, f.planFlags, []string{"--plan", "--output=json"})
	if !slices.Equal(slices.Sorted(slices.Values(strings.Split(args[0], ","))), slices.Sorted(slices.Values(f.planCommands))) ||
		!slices.Equal(args[1:], want) {
		f.t.Errorf("plan request = %q, want %v %q", args, f.planCommands, want)
	}
}

func (f *verifyFixture) invoke(args ...string) verifyResult {
	f.t.Helper()
	var stdout bytes.Buffer
	verifier := Verifier{Dir: f.root, Fingerprint: f.fakeFingerprint, Plan: f.fakePlan, Stdout: &stdout}
	if f.cliPlan {
		verifier.Plan = nil
	}
	return decodeVerdict(f.t, verifier.Run(args), stdout.String())
}

// decodeVerdict checks the output contract: a success is one 2-space indented
// JSON document, a failure one line {"verdict":"not-verified","reason":...}
// with ErrNotVerified. Both end with a newline.
func decodeVerdict(t *testing.T, runErr error, out string) verifyResult {
	t.Helper()
	var value fxObj
	if err := json.Unmarshal([]byte(out), &value); err != nil {
		t.Fatalf("verifier produced no JSON document (Run returned %v): %q", runErr, out)
	}
	document, ok := strings.CutSuffix(out, "\n")
	if !ok {
		t.Errorf("verifier output does not end with a newline: %q", out)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(document)); err != nil {
		t.Fatalf("compact output: %v", err)
	}
	if runErr != nil {
		if !errors.Is(runErr, ErrNotVerified) {
			t.Fatalf("Run returned %v, want ErrNotVerified", runErr)
		}
		reason, _ := value["reason"].(string)
		if value["verdict"] != "not-verified" || reason == "" || len(value) != 2 {
			t.Errorf("failure document = %s, want only verdict not-verified and a reason", out)
		}
		if document != compact.String() {
			t.Errorf("not-verified verdict is not one line of JSON: %q", out)
		}
	} else {
		var indented bytes.Buffer
		if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
			t.Fatalf("indent output: %v", err)
		}
		if document != indented.String() {
			t.Errorf("success document is not 2-space indented JSON: %q", out)
		}
	}
	return verifyResult{err: runErr, out: out, value: value}
}

// expect checks a verdict: reason "" wants success with the given verdict,
// any other reason is a regular expression the not-verified reason matches.
func (f *verifyFixture) expect(result verifyResult, reason, verdict string) {
	f.t.Helper()
	if reason == "" {
		if result.err != nil {
			f.t.Fatalf("want %q, got %s", verdict, result.out)
		}
		if result.value["verdict"] != verdict {
			f.t.Fatalf("verdict = %v, want %q: %s", result.value["verdict"], verdict, result.out)
		}
		return
	}
	if result.err == nil {
		f.t.Fatalf("want not-verified matching %q, got %s", reason, result.out)
	}
	if got, _ := result.value["reason"].(string); !regexp.MustCompile(reason).MatchString(got) {
		f.t.Fatalf("reason = %q, want a match for %q", got, reason)
	}
}

func (f *verifyFixture) resetFingerprint(options fxFingerprint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = 0
	f.fingerprint = options
}

func (f *verifyFixture) verify(reason string) {
	f.t.Helper()
	f.verifyAgainst(reason, "", fxFingerprint{})
}

// verifyAgainst checks the dossier with --record, and with --base when base is
// not empty.
func (f *verifyFixture) verifyAgainst(reason, base string, options fxFingerprint) {
	f.t.Helper()
	f.resetFingerprint(options)
	f.write(".context/dossier.json", f.record)
	args := []string{"--record", ".context/dossier.json"}
	if base != "" {
		args = append(args, "--base", base)
	}
	result := f.invoke(args...)
	if f.cliPlan {
		f.checkCLIPlanRequest()
	}
	f.expect(result, reason, "Putnami verified")
	if reason == "" && result.value["stage"] != "ready" {
		f.t.Fatalf("stage = %v, want ready", result.value["stage"])
	}
}

// gate checks the fixture gate for reuse by the finalizer, without a dossier.
func (f *verifyFixture) gate(reason string, options fxFingerprint) {
	f.t.Helper()
	f.resetFingerprint(options)
	f.bindProducers()
	result := f.invoke("--gate", ".context/session.json", "--report", ".context/report.json", "--base", f.head)
	if f.cliPlan {
		f.checkCLIPlanRequest()
	}
	f.expect(result, reason, "gate reusable")
	if reason == "" && result.value["gateSession"] != "synthetic-gate" {
		f.t.Fatalf("gateSession = %v, want synthetic-gate", result.value["gateSession"])
	}
}

func fxStrings(values []string) fxArr {
	out := fxArr{}
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// fxAt walks string keys through objects and int indexes through arrays.
func fxAt(value any, path ...any) any {
	for _, step := range path {
		switch step := step.(type) {
		case string:
			value = value.(fxObj)[step]
		case int:
			value = value.(fxArr)[step]
		}
	}
	return value
}

func fxObjAt(value any, path ...any) fxObj { return fxAt(value, path...).(fxObj) }

func fxArrAt(value any, path ...any) fxArr { return fxAt(value, path...).(fxArr) }

func fxAppend(parent fxObj, key string, values ...any) {
	parent[key] = append(parent[key].(fxArr), values...)
}

func fxClone(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var clone any
	if err := json.Unmarshal(data, &clone); err != nil {
		panic(err)
	}
	return clone
}

// fxDropCommands removes commands from the session's commands and tasks.
func fxDropCommands(session fxObj, commands ...string) {
	kept := fxArr{}
	for _, command := range fxArrAt(session, "commands") {
		if !slices.Contains(commands, command.(string)) {
			kept = append(kept, command)
		}
	}
	session["commands"] = kept
	tasks := fxArr{}
	for _, task := range fxArrAt(session, "tasks") {
		if !slices.Contains(commands, fxAt(task, "identity", "task", "command").(string)) {
			tasks = append(tasks, task)
		}
	}
	session["tasks"] = tasks
}

func TestVerifierRecord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, f *verifyFixture)
		// base is the --base value, none when empty.
		base        string
		fingerprint fxFingerprint
		// reason is the expected not-verified reason, success when empty.
		reason string
	}{
		{name: "stale dossier", reason: "stale dossier tree", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "binding")["fingerprint"] = fxOtherTree
		}},
		{name: "stale gate", reason: "stale gate tree", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.session, "tree")["fingerprint"] = fxOtherTree
			f.bindProducers()
		}},
		{name: "stale review", reason: "stale review tree", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "review")["fingerprint"] = fxOtherTree
		}},
		{name: "stale qualification", reason: "stale qualification tree", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.qualify, "binding")["fingerprint"] = fxOtherTree
			f.bindProducers()
		}},
		{name: "stale realization opinion", reason: "stale realization opinion", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0, "realization")["fingerprint"] = fxOtherTree
		}},
		{name: "report belongs to gate", reason: "gate report/session mismatch", setup: func(_ *testing.T, f *verifyFixture) {
			f.report["sessionId"] = "different-session"
			f.bindProducers()
		}},
		{name: "coverage enforced", reason: "coverage enforcement not proved", setup: func(_ *testing.T, f *verifyFixture) {
			f.report["enforceCoverage"] = false
			f.bindProducers()
		}},
		{name: "project selection is not final gate", reason: "gate must cover impacted or all projects", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.session, "selection")["mode"] = "projects"
			f.bindProducers()
		}},
		{name: "impacted selection binds exact base", reason: "immutable base SHA", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.session, "git")["baseline"] = "origin/main"
			f.bindProducers()
		}},
		{name: "implicit workspace companion accepted", setup: func(_ *testing.T, f *verifyFixture) {
			kept := fxArr{}
			for _, command := range fxArrAt(f.session, "commands") {
				if command != "validate-workspace" {
					kept = append(kept, command)
				}
			}
			f.session["commands"] = kept
			f.bindProducers()
		}},
		{name: "missing workspace companion rejected", reason: "gate lacks required commands", setup: func(_ *testing.T, f *verifyFixture) {
			fxDropCommands(f.session, "validate-workspace")
			f.bindProducers()
		}},
		{name: "review covers every file", reason: "review coverage must account", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "review")["coverage"] = fxArr{}
		}},
		{name: "exclusion needs reason", reason: "missing review coverage or exclusion reason", setup: func(_ *testing.T, f *verifyFixture) {
			item := fxObjAt(f.record, "review", "coverage", 0)
			item["status"], item["reason"] = "excluded", ""
		}},
		{name: "finding requires resolution", reason: "unresolved finding", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "review")["findings"] = fxArr{fxObj{"id": "F1", "severity": "high", "location": "app.txt:1",
				"scenario": "Incorrect response for declared input", "state": "open"}}
		}},
		{name: "deferral requires named authority", reason: "deferral has no authority", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "review")["findings"] = fxArr{fxObj{"id": "F1", "severity": "medium", "location": "app.txt:1",
				"scenario": "Known limited behavior", "state": "deferred",
				"resolution": f.note("defer", "Requested deferral without decision.")}}
		}},
		{name: "unresolved consulted scope position blocks", reason: "unresolved scope position", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0, "design")["position"] = "alternative"
		}},
		{name: "reserved consulted scope position blocks", reason: "unresolved scope position", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0, "design")["position"] = "reserved"
		}},
		{name: "resolved consultation remains attributable", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["objections"] = fxArr{fxObj{
				"note":       f.note("reservation", "The initial proposal crossed a scope boundary."),
				"resolution": f.note("reservation-resolution", "The receiving scope accepted the revised proposal."),
				"authority":  "consulted-owner-session",
			}}
		}},
		{name: "no consulted scope is an explicit reviewed perimeter", setup: func(_ *testing.T, f *verifyFixture) {
			f.record["scopes"] = fxArr{}
			f.record["scope"] = f.note("scope", "Reviewed perimeter and workload inventory: no additional scope consultation applies.")
		}},
		{name: "consultation owner must be attributed", reason: "missing consulted owner attribution", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["owner"] = ""
		}},
		{name: "objection requires resolution", reason: "missing evidence reference", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["objections"] = fxArr{fxObj{"note": f.note("objection", "Ownership conflict."),
				"resolution": nil, "authority": "consulted-owner-session"}}
		}},
		{name: "same session cannot review itself", reason: "independent session", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "review")["author"] = "implementation-session"
		}},
		{name: "modified evidence rejected", reason: "evidence digest changed", setup: func(_ *testing.T, f *verifyFixture) {
			f.write(".context/review.md", "Changed after binding.")
		}},
		{name: "tree mutation during verification", reason: "tree changed during verification",
			fingerprint: fxFingerprint{mutateTree: true}},
		{name: "evidence mutation during verification", reason: "evidence changed during verification",
			fingerprint: fxFingerprint{mutateEvidence: ".context/review.md"}},
		{name: "required qualification cannot be omitted", reason: "missing, duplicate or out-of-scope qualification", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "qualification")["results"] = fxArr{}
		}},
		{name: "gate success cannot hide failed tasks", reason: "gate", setup: func(_ *testing.T, f *verifyFixture) {
			counts := fxObjAt(f.session, "run", "counts")
			counts["failed"], counts["succeeded"] = 1, 4
			f.bindProducers()
		}},
		{name: "report success cannot hide canceled tasks", reason: "gate", setup: func(_ *testing.T, f *verifyFixture) {
			counts := fxObjAt(f.report, "run", "counts")
			counts["canceled"], counts["succeeded"] = 1, 4
			f.bindProducers()
		}},
		{name: "qualification without requests is not proof", reason: "qualification", setup: func(_ *testing.T, f *verifyFixture) {
			f.qualify["requests"] = fxArr{}
			fxObjAt(f.qualify, "contract")["requests"] = 0
			f.bindProducers()
		}},
		{name: "qualification with failed request cannot pass", reason: "qualification", setup: func(_ *testing.T, f *verifyFixture) {
			request := fxObjAt(f.qualify, "requests", 0)
			request["state"], request["status"] = "failed", 500
			f.bindProducers()
		}},
		{name: "qualification without completed phases cannot pass", reason: "qualification", setup: func(_ *testing.T, f *verifyFixture) {
			f.qualify["phases"] = fxArr{}
			f.bindProducers()
		}},
		{name: "qualification request count matches", reason: "qualification", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.qualify, "contract")["requests"] = 2
			f.bindProducers()
		}},
		{name: "narrowed task selection rejected", reason: "gate", setup: func(_ *testing.T, f *verifyFixture) {
			tasks := fxArr{}
			for _, task := range fxArrAt(f.session, "tasks") {
				if fxAt(task, "identity", "task", "command") != "test" {
					tasks = append(tasks, task)
				}
			}
			f.session["tasks"] = tasks
			counts := fxObjAt(f.session, "run", "counts")
			counts["total"], counts["succeeded"] = 4, 4
			f.bindProducers()
		}},
		{name: "missing impacted project rejected", reason: "gate", setup: func(_ *testing.T, f *verifyFixture) {
			dependent := f.projectTask("test", "dependent")
			fxAppend(fxObjAt(f.plan, "plan"), "tasks", fxObj{"identity": dependent["identity"]})
			f.writePlan()
		}},
		{name: "superset gate accepted", setup: func(_ *testing.T, f *verifyFixture) {
			fxAppend(f.session, "tasks", f.projectTask("test", "additional"))
			fxAppend(fxObjAt(f.session, "selection"), "projects", "/additional")
			for _, counts := range []fxObj{fxObjAt(f.session, "run", "counts"), fxObjAt(f.report, "run", "counts")} {
				counts["total"], counts["succeeded"] = 6, 6
			}
			f.report["elidedJobs"] = 1
			counts := fxObjAt(f.report, "commands", 1, "counts")
			counts["total"], counts["succeeded"] = 2, 2
			f.bindProducers()
		}},
		{name: "failed native plan blocks", reason: "plan", setup: func(t *testing.T, f *verifyFixture) {
			f.useCLIPlan(t)
			f.plan["status"], f.plan["exitCode"] = "failed", 1
			f.writePlan()
		}},
		{name: "native plan process failure blocks", reason: "plan", setup: func(t *testing.T, f *verifyFixture) {
			f.useCLIPlan(t)
			f.write(".context/plan-exit", "1")
		}},
		{name: "native plan with invalid UTF-8 is rejected", reason: "encoded data", setup: func(t *testing.T, f *verifyFixture) {
			f.useCLIPlan(t)
			encoded, err := json.Marshal(f.plan)
			if err != nil {
				t.Fatalf("encode plan: %v", err)
			}
			data := slices.Concat(encoded[:len(encoded)-1], []byte(`,"ignored":"`), []byte{0xff}, []byte(`"}`))
			f.write(".context/plan.json", data)
		}},
		{name: "replaced proposal invalidates opinion", reason: "proposal", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["proposal"] = f.note("replacement-proposal", "New contract boundary.")
		}},
		{name: "replaced context invalidates opinion", reason: "context", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["context"] = fxArr{f.note("replacement-context", "Contract version two.")}
		}},
		{name: "expected integration base matches", reason: "base", base: "HEAD", setup: func(_ *testing.T, f *verifyFixture) {
			f.git("commit", "--allow-empty", "-q", "-m", "New integration baseline")
			fxObjAt(f.record, "binding")["headSHA"] = f.git("rev-parse", "HEAD")
		}},
		{name: "unsupported policy flag is rejected", reason: "flag", setup: func(_ *testing.T, f *verifyFixture) {
			policy := f.readJSON("putnami.ci.json")
			fxAppend(policy, "flags", "--some-new-selection")
			f.write("putnami.ci.json", policy)
			f.refreshPolicies()
			fxAppend(f.record, "changedFiles", "putnami.ci.json")
			fxAppend(fxObjAt(f.record, "review"), "coverage", fxObj{"path": "putnami.ci.json", "status": "reviewed",
				"reason": "Synthetic policy change for unsupported flag behavior."})
		}},
		{name: "version 1 dossier is rejected without migration", reason: "v2 scopes required", setup: func(_ *testing.T, f *verifyFixture) {
			f.record["version"] = 1
		}},
		{name: "mixed top-level domains field is rejected", reason: "legacy domains field", setup: func(_ *testing.T, f *verifyFixture) {
			f.record["domains"] = fxArr{}
		}},
		{name: "legacy domains-only dossier is rejected", reason: "legacy domains field", setup: func(_ *testing.T, f *verifyFixture) {
			f.record["domains"] = fxArr{fxObj{"domain": "/application", "guardian": "legacy-session"}}
			delete(f.record, "scopes")
		}},
		{name: "mixed consulted domain field is rejected", reason: "legacy domain or guardian field", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["domain"] = "/application"
		}},
		{name: "mixed guardian attribution is rejected", reason: "legacy domain or guardian field", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.record, "scopes", 0)["guardian"] = "legacy-session"
		}},
		{name: "boolean gate counts are rejected", reason: "invalid gate counts", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.session, "run", "counts")["succeeded"] = true
			f.bindProducers()
		}},
		{name: "a --fix=false policy is proved by a gate report with fix false", setup: func(_ *testing.T, f *verifyFixture) {
			f.setPolicyFlags("--enforce-coverage", "--fix=false")
			f.planFlags = []string{"--enforce-coverage", "--fix=false"}
			f.report["fix"] = false
			f.bindProducers()
		}},
		{name: "a --fix=false gate covers the native plan computed with --fix=false", setup: func(_ *testing.T, f *verifyFixture) {
			f.setPolicyFlags("--enforce-coverage", "--fix=false")
			f.planFlags = []string{"--enforce-coverage", "--fix=false"}
			f.report["fix"] = false
			f.checkOnlyLint()
		}},
		{name: "the default plan passes the policy's --fix=false to the workspace CLI", setup: func(t *testing.T, f *verifyFixture) {
			f.useCLIPlan(t)
			f.setPolicyFlags("--fix=false", "--enforce-coverage")
			f.planFlags = []string{"--fix=false", "--enforce-coverage"}
			f.report["fix"] = false
			f.checkOnlyLint()
		}},
		{name: "without --fix=false in the policy the native plan keeps the fixing lint tasks", reason: "narrower",
			setup: func(t *testing.T, f *verifyFixture) {
				f.useCLIPlan(t)
				f.report["fix"] = false
				f.checkOnlyLint()
			}},
		{name: "a --fix=false policy is not proved by a report without fix", reason: "^CI flag --fix=false not proved by the gate report$",
			setup: func(_ *testing.T, f *verifyFixture) {
				f.setPolicyFlags("--enforce-coverage", "--fix=false")
			}},
		{name: "a --fix=false policy is not proved by a gate that fixed", reason: "--fix=false not proved", setup: func(_ *testing.T, f *verifyFixture) {
			f.setPolicyFlags("--enforce-coverage", "--fix=false")
			f.report["fix"] = true
			f.bindProducers()
		}},
		{name: "a --fix=false policy is not proved by a string fix", reason: "--fix=false not proved", setup: func(_ *testing.T, f *verifyFixture) {
			f.setPolicyFlags("--enforce-coverage", "--fix=false")
			f.report["fix"] = "false"
			f.bindProducers()
		}},
		{name: "a --fix=true policy is rejected", reason: "^CI flags require evidence this verifier does not support$",
			setup: func(_ *testing.T, f *verifyFixture) {
				f.setPolicyFlags("--enforce-coverage", "--fix=true")
				f.report["fix"] = true
				f.bindProducers()
			}},
		{name: "a bare --fix policy is rejected", reason: "^CI flags require evidence this verifier does not support$",
			setup: func(_ *testing.T, f *verifyFixture) {
				f.setPolicyFlags("--enforce-coverage", "--fix")
				f.report["fix"] = true
				f.bindProducers()
			}},
		{name: "CI policy may omit optional flags", setup: func(_ *testing.T, f *verifyFixture) {
			f.planFlags = nil
			policy := f.readJSON("putnami.ci.json")
			delete(policy, "flags")
			f.write("putnami.ci.json", policy)
			f.refreshPolicies()
			f.record["changedFiles"] = fxArr{"app.txt", "putnami.ci.json"}
			fxAppend(fxObjAt(f.record, "review"), "coverage", fxObj{"path": "putnami.ci.json", "status": "reviewed",
				"reason": "Optional policy flags were absent."})
		}},
		{name: "nonempty malformed cleanup leftovers are rejected", reason: "qualification cleanup has leftovers", setup: func(_ *testing.T, f *verifyFixture) {
			fxObjAt(f.qualify, "cleanup")["leftovers"] = fxObj{"resource": "live"}
			f.bindProducers()
		}},
		{name: "the default plan asks the workspace CLI for the native impacted plan", setup: func(t *testing.T, f *verifyFixture) {
			f.useCLIPlan(t)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newVerifyFixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			f.verifyAgainst(tc.reason, tc.base, tc.fingerprint)
		})
	}
}

func TestVerifierRecordIsReadOnly(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	before := f.git("status", "--porcelain")
	f.verify("")
	if after := f.git("status", "--porcelain"); after != before {
		t.Fatalf("git status changed from %q to %q", before, after)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "app.txt"))
	if err != nil {
		t.Fatalf("read app.txt: %v", err)
	}
	if string(data) != "changed\n" {
		t.Fatalf("app.txt = %q, want the uncommitted change kept", data)
	}
}

func TestVerifierSnapshotNeverVerifiesPendingWork(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	result := f.invoke("--snapshot", "--base", f.head)
	if result.err != nil {
		t.Fatalf("snapshot: %v: %s", result.err, result.out)
	}
	if _, ok := result.value["verdict"]; ok {
		t.Fatalf("snapshot carries a verdict: %s", result.out)
	}
	if _, ok := result.value["domains"]; ok {
		t.Fatalf("snapshot carries a legacy domains field: %s", result.out)
	}
	if result.value["version"] != float64(2) {
		t.Fatalf("snapshot version = %v, want 2", result.value["version"])
	}
	if scopes, ok := result.value["scopes"].(fxArr); !ok || len(scopes) != 0 {
		t.Fatalf("snapshot scopes = %v, want an empty list", result.value["scopes"])
	}
	f.record = result.value
	f.verify("missing evidence reference")
}

// Every tracked decision registry is a policy binding: the root one binds every
// project, a project's own binds that project.
func TestVerifierSnapshotBindsEveryDecisionRegistry(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	f.write("decisions.json", "{}\n")
	f.write("tooling/app/decisions.json", "{}\n")
	f.git("add", "decisions.json", "tooling/app/decisions.json")
	snapshot := f.invoke("--snapshot", "--base", f.head)
	if snapshot.err != nil {
		t.Fatalf("snapshot: %v: %s", snapshot.err, snapshot.out)
	}
	paths := []string{}
	for _, policy := range fxArrAt(snapshot.value, "policies") {
		paths = append(paths, fxAt(policy, "path").(string))
	}
	for _, want := range []string{"decisions.json", "tooling/app/decisions.json"} {
		if !slices.Contains(paths, want) {
			t.Fatalf("policies = %q, want %s among them", paths, want)
		}
	}
}

func TestVerifierPreservesLeadingWhitespaceInChangedPaths(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	f.write(" leading.txt", "leading\n")
	snapshot := f.invoke("--snapshot", "--base", f.head)
	if snapshot.err != nil {
		t.Fatalf("snapshot: %v: %s", snapshot.err, snapshot.out)
	}
	changed := fxArrAt(snapshot.value, "changedFiles")
	if !slices.Equal(changed, fxArr{" leading.txt", "app.txt"}) {
		t.Fatalf("changedFiles = %q, want [\" leading.txt\" \"app.txt\"]", changed)
	}
	f.record["changedFiles"] = changed
	fxAppend(fxObjAt(f.record, "review"), "coverage", fxObj{"path": " leading.txt", "status": "reviewed",
		"reason": "Leading whitespace is part of the actual path."})
	f.verify("")
}

func TestVerifierRejectsDuplicateJSONMembers(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	f.write(".context/dossier.json", `{"version":2,"version":2}`)
	f.expect(f.invoke("--record", ".context/dossier.json"), "duplicate JSON member", "")
}

func TestVerifierReportNeedsGate(t *testing.T) {
	t.Parallel()
	f := newVerifyFixture(t)
	f.expect(f.invoke("--snapshot", "--report", ".context/report.json"), "--report goes with --gate", "")
}

func TestVerifierGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		setup       func(f *verifyFixture)
		fingerprint fxFingerprint
		// reason is the expected not-verified reason, success when empty.
		reason string
	}{
		// The native plan decides the selection, so a baseline named by ref is fine.
		{name: "finalizer reuses a current impacted gate without a dossier", setup: func(f *verifyFixture) {
			fxObjAt(f.session, "git")["baseline"] = "origin/main"
		}},
		{name: "reused gate must be on the current tree", reason: "stale gate tree", setup: func(f *verifyFixture) {
			fxObjAt(f.session, "tree")["fingerprint"] = fxOtherTree
		}},
		{name: "reused gate must be green", reason: "gate failed", setup: func(f *verifyFixture) {
			run := fxObjAt(f.session, "run")
			run["outcome"], run["exitCode"] = "failure", 1
		}},
		{name: "reused gate must enforce coverage", reason: "coverage enforcement not proved", setup: func(f *verifyFixture) {
			f.report["enforceCoverage"] = false
		}},
		{name: "project gate is never reused as the impacted gate", reason: "impacted or all projects", setup: func(f *verifyFixture) {
			fxObjAt(f.session, "selection")["mode"] = "projects"
		}},
		{name: "reused gate must cover the native impacted plan", reason: "narrower", setup: func(f *verifyFixture) {
			dependent := f.projectTask("test", "dependent")
			fxAppend(fxObjAt(f.plan, "plan"), "tasks", fxObj{"identity": dependent["identity"]})
			f.writePlan()
		}},
		{name: "tree mutation while checking a reused gate", reason: "tree changed during verification",
			fingerprint: fxFingerprint{mutateTree: true}},
		{name: "without a CI policy the finalizer commands are required", setup: func(f *verifyFixture) {
			f.remove("putnami.ci.json")
			fxDropCommands(f.session, "validate-workspace")
			f.planFromSession()
			f.planCommands = []string{"lint", "test", "build", "validate"}
			f.planFlags = nil
			f.writePlan()
		}},
		{name: "without a CI policy a gate missing a finalizer command is refused", reason: "gate lacks required commands", setup: func(f *verifyFixture) {
			f.remove("putnami.ci.json")
			fxDropCommands(f.session, "build", "validate-workspace")
		}},
		{name: "a narrow CI policy does not narrow the reused gate", reason: "gate lacks required commands", setup: func(f *verifyFixture) {
			f.write("putnami.ci.json", fxObj{"version": 3, "commands": fxArr{"test"}, "flags": fxArr{"--enforce-coverage"}})
			fxDropCommands(f.session, "lint", "build", "validate", "validate-workspace")
		}},
		{name: "finalizer reuses a gate whose report proves the policy's --fix=false", setup: func(f *verifyFixture) {
			f.setPolicyFlags("--enforce-coverage", "--continue-on-error", "--fix=false")
			f.planFlags = []string{"--enforce-coverage", "--fix=false"}
			f.report["fix"] = false
		}},
		{name: "finalizer reuses a --fix=false gate that ran check-only lint", setup: func(f *verifyFixture) {
			f.useCLIPlan(f.t)
			f.setPolicyFlags("--fix=false", "--continue-on-error", "--enforce-coverage")
			f.planFlags = []string{"--fix=false", "--enforce-coverage"}
			f.report["fix"] = false
			f.checkOnlyLint()
		}},
		{name: "finalizer refuses a check-only lint gate when the policy lacks --fix=false", reason: "narrower",
			setup: func(f *verifyFixture) {
				f.report["fix"] = false
				f.checkOnlyLint()
			}},
		{name: "finalizer refuses a gate whose report does not prove the policy's --fix=false",
			reason: "^CI flag --fix=false not proved by the gate report$", setup: func(f *verifyFixture) {
				f.setPolicyFlags("--enforce-coverage", "--fix=false")
			}},
		{name: "finalizer refuses a --fix=true policy", reason: "^CI flags require evidence this verifier does not support$",
			setup: func(f *verifyFixture) {
				f.setPolicyFlags("--enforce-coverage", "--fix=true")
				f.report["fix"] = true
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newVerifyFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			f.gate(tc.reason, tc.fingerprint)
		})
	}
}

func TestVerifierHelpWorksOutsideAGitCheckout(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			if err := (Verifier{Dir: t.TempDir(), Stdout: &stdout}).Run([]string{flag}); err != nil {
				t.Fatalf("Run(%s) = %v, want nil", flag, err)
			}
			if stdout.String() != VerifyUsage {
				t.Fatalf("help = %q, want %q", stdout.String(), VerifyUsage)
			}
		})
	}
}

func TestWorkspaceCLI(t *testing.T) {
	t.Parallel()
	executable := func(t *testing.T, root string) {
		t.Helper()
		location := filepath.Join(root, "putnamiw")
		if err := os.WriteFile(location, []byte(fxPutnamiw), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(location, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name     string
		goos     string
		putnamiw func(t *testing.T, root string)
		want     string
		// unixHost marks a case that needs Unix execute bits on the host.
		unixHost bool
	}{
		{
			// Windows cannot start the putnamiw shell script as a process.
			name: "Windows runs the putnami on PATH, never the putnamiw script",
			goos: "windows", putnamiw: executable, want: "putnami",
		},
		{
			name: "an executable putnamiw runs as ./putnamiw",
			goos: "linux", putnamiw: executable, want: "./putnamiw", unixHost: true,
		},
		{
			name: "a putnamiw without the execute bit falls back to putnami",
			goos: "linux", want: "putnami",
			putnamiw: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "putnamiw"), []byte(fxPutnamiw), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(root, "putnamiw"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a missing putnamiw falls back to putnami",
			goos: "linux", putnamiw: func(*testing.T, string) {}, want: "putnami",
		},
		{
			name: "a putnamiw directory falls back to putnami",
			goos: "linux", want: "putnami",
			putnamiw: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, "putnamiw"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.unixHost && runtime.GOOS == "windows" {
				t.Skip("Windows files carry no execute bit")
			}
			root := t.TempDir()
			tc.putnamiw(t, root)
			if got := workspaceCLI(root, tc.goos); got != tc.want {
				t.Fatalf("workspaceCLI(%q) = %q, want %q", tc.goos, got, tc.want)
			}
		})
	}
}
