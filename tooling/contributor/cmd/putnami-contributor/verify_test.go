package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/treefingerprint"
)

const (
	// verifyRequirement names the requirement the tests below prove, in the
	// feature languageFeature names.
	verifyRequirement = "change-verify-parity"

	// recordVerifyEnv names the core command a recording run takes the
	// goldens from, such as the repository's putnamiw. When it is set,
	// TestChangeVerifyMatchesTheCoreCommand runs `<command> tree verify
	// <arguments>` for every case and writes testdata/verify/<case>.golden
	// instead of comparing.
	recordVerifyEnv = "PUTNAMI_CONTRIBUTOR_RECORD_VERIFY"

	// restoredCase is the one case whose workflow proof restores a report,
	// the only verification that reads through the host channel.
	restoredCase = "workflow-restored"
)

// TestTheManifestDeclaresChangeVerify pins the launch: an interactive
// subcommand that runs with or without a workspace, every flag the core
// command takes, and an uncached task with no declared working directory, so
// putnami starts it in the workspace root, or in the caller's directory
// outside a workspace.
func TestTheManifestDeclaresChangeVerify(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, verifyRequirement, "change-verify-runs-with-and-without-a-workspace")
	manifest := readManifest(t)
	subcommand, declared := manifest.CommandGroups["change"].Subcommands["verify"]
	if !declared || subcommand.Command != "change-verify" || !subcommand.Interactive || !subcommand.RunsWithoutWorkspace() {
		t.Fatalf("change verify is %+v, want an interactive change-verify that runs without a workspace", subcommand)
	}
	flags := map[string]string{}
	for name, flag := range manifest.Commands["change-verify"].Flags {
		flags[name] = flag.Type
	}
	want := map[string]string{"record": "string", "workflow": "string", "snapshot": "boolean", "ref": "string",
		"gate": "string", "report": "string", "base": "string"}
	if !maps.Equal(flags, want) {
		t.Errorf("change-verify flags %v, want %v", flags, want)
	}
	task := manifest.Tasks["change-verify"]
	if task.Command != "{extensionRuntime}" || !slices.Equal(task.Args, []string{"verify"}) {
		t.Errorf("change-verify runs %s %v, want {extensionRuntime} verify", task.Command, task.Args)
	}
	if task.Cwd != "" {
		t.Errorf("change-verify declares cwd %q, which would replace the caller's directory outside a workspace", task.Cwd)
	}
	if task.Cache == nil || task.Cache.IsEnabled() {
		t.Error("change-verify is cacheable; a verdict reads the worktree, which no cache key covers")
	}
}

// verifyCase is one line of testdata/verify/cases.txt.
type verifyCase struct {
	name, dir string
	args      []string
}

func verifyCases(t *testing.T) []verifyCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "verify", "cases.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []verifyCase
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			t.Fatalf("case line %q has no name|directory|arguments", line)
		}
		cases = append(cases, verifyCase{name: fields[0], dir: fields[1], args: strings.Fields(fields[2])})
	}
	return cases
}

// golden reads the core command's answer to c.
func (c verifyCase) golden(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "verify", c.name+".golden"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// renderVerdict writes a result in the golden format: the exit code and
// stdout. The verdict is the whole answer: both commands write nothing on
// stderr, which the tests check apart.
func renderVerdict(r languageResult) string {
	return "exit " + strconv.Itoa(r.code) + "\n-- stdout --\n" + r.stdout
}

// fixtureCommands is the command list the fixture CI policy declares.
var fixtureCommands = []string{"lint", "test", "build", "validate", "validate-workspace"}

const (
	fixtureStart   = "2026-09-23T10:00:00Z"
	fixtureEnd     = "2026-09-23T10:00:01Z"
	fixtureFeature = "fixture/workflow"
	// fixtureDate dates every fixture commit, so the commits, and the tree
	// fingerprint that folds HEAD in, are the same on every machine.
	fixtureDate = "2026-09-23T09:00:00Z"
)

// fixturePutnamiw is the workspace CLI of the fixture repository. It answers
// the native plan only: it exits 3 when it receives the host channel, 4 when
// its arguments are not the request .context/plan-args holds, and prints
// .context/plan.json otherwise.
var fixturePutnamiw = `#!/bin/sh
if [ -n "${` + wire.SocketEnv + `-}" ] || [ -n "${` + wire.TokenEnv + `-}" ]; then
  echo "putnamiw: the host channel reached the workspace CLI" >&2
  exit 3
fi
if [ "$*" != "$(cat .context/plan-args)" ]; then
  echo "putnamiw: unexpected plan request: $*" >&2
  exit 4
fi
cat .context/plan.json
`

type (
	fxObj = map[string]any
	fxArr = []any
)

// verifyRepo is the fixture repository the parity cases run in. A base
// commit holds the policies, a feature with its spec and the fake putnamiw;
// a second commit adds src/main.txt; the worktree changes app.txt and adds
// new.txt. .context/, which Git ignores, holds the evidence of a synthetic
// build run bound to that tree: a dossier, a gate, a qualification and two
// workflows, one of which restores its verification report from the cache.
// Only the gate is synthetic; the fingerprint is the real one.
type verifyRepo struct {
	t                *testing.T
	home, root       string
	base, head, tree string
}

func newVerifyRepo(t *testing.T) *verifyRepo {
	t.Helper()
	home := t.TempDir()
	r := &verifyRepo{t: t, home: home, root: filepath.Join(home, "repo")}
	r.git("init", "-q", r.root)
	r.git("symbolic-ref", "HEAD", "refs/heads/main")
	r.write(".gitignore", ".context/\n")
	r.write("AGENTS.md", "# Fixture\n")
	r.write("decisions.json", "{\"decisions\":[]}\n")
	r.writeJSON("putnami.ci.json", fxObj{"version": 3, "commands": fixtureCommands, "flags": fxArr{"--enforce-coverage"}})
	r.write("app.txt", "original\n")
	r.write("putnamiw", fixturePutnamiw)
	if err := os.Chmod(filepath.Join(r.root, "putnamiw"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.writeJSON("feature/putnami.json", fxObj{"name": "feature"})
	r.writeJSON("feature/putnami.features.json", fxObj{"protocolVersion": 2, "namespace": "fixture", "features": fxArr{fxObj{
		"id": fixtureFeature, "type": "feature", "name": "Workflow", "outcome": "A reviewed workflow is ready",
		"owner": "fixture", "target": "coded", "requirements": fxArr{fxObj{
			"id": "acceptance", "stage": "coded", "evidenceKinds": fxArr{"attestation"},
			"verification": fxObj{"kind": "acceptance", "checks": fxArr{"first-check", "second-check"}},
		}},
	}}})
	r.writeJSON("feature/specs/workflow.json", fxObj{"protocolVersion": 1, "feature": fixtureFeature,
		"outcomes": fxArr{"A reviewed workflow is ready"}, "requirements": fxArr{fxObj{"id": "acceptance", "text": "Both checks pass."}}})
	r.commit("Fixture base")
	r.base = r.git("rev-parse", "HEAD")
	r.write("src/main.txt", "main\n")
	r.commit("Fixture change")
	r.head = r.git("rev-parse", "HEAD")
	r.write("app.txt", "changed\n")
	r.write("new.txt", "new\n")
	tree, err := treefingerprint.Compute(r.root)
	if err != nil {
		t.Fatalf("fingerprint the fixture: %v", err)
	}
	r.tree = tree.Fingerprint
	r.writeEvidence()
	return r
}

func (r *verifyRepo) git(args ...string) string {
	r.t.Helper()
	command := exec.Command("git", args...)
	command.Dir = r.home
	if len(args) > 0 && args[0] != "init" {
		command.Dir = r.root
	}
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=" + fixtureDate,
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_COMMITTER_DATE=" + fixtureDate}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func (r *verifyRepo) commit(message string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("-c", "commit.gpgSign=false", "commit", "-q", "-m", message)
}

func (r *verifyRepo) write(name, content string) {
	r.t.Helper()
	location := filepath.Join(r.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(location), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(location, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *verifyRepo) writeJSON(name string, value any) {
	r.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		r.t.Fatalf("encode %s: %v", name, err)
	}
	r.write(name, string(data))
}

func (r *verifyRepo) read(name string) []byte {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(name)))
	if err != nil {
		r.t.Fatal(err)
	}
	return data
}

// ref is the evidence reference to a file: its path and the SHA-256 of its
// bytes.
func (r *verifyRepo) ref(name string) fxObj {
	sum := sha256.Sum256(r.read(name))
	return fxObj{"path": name, "sha256": hex.EncodeToString(sum[:])}
}

func (r *verifyRepo) note(name, content string) fxObj {
	r.write(".context/"+name+".md", content)
	return r.ref(".context/" + name + ".md")
}

// writeEvidence writes the evidence of a synthetic build run of the current
// tree into .context/.
func (r *verifyRepo) writeEvidence() {
	counts := func() fxObj { return fxObj{"total": 5, "succeeded": 5, "failed": 0, "skipped": 0, "canceled": 0} }
	reuse := func() fxObj { return fxObj{"localCache": 0, "remoteCache": 0, "coalesced": 0} }
	run := func() fxObj {
		return fxObj{"outcome": "success", "exitCode": 0, "counts": counts(), "reuse": reuse(), "durationMs": 1000}
	}
	identity := func(command string) fxObj {
		return fxObj{"key": "/application:" + command, "scope": "project",
			"project":  fxObj{"id": "/application", "name": "application"},
			"task":     fxObj{"name": command, "command": command, "kind": "command"},
			"provider": fxObj{"extension": "fixture"}}
	}
	tasks := make(fxArr, 0, len(fixtureCommands))
	planned := make(fxArr, 0, len(fixtureCommands))
	commands := make(fxArr, 0, len(fixtureCommands))
	jobs := make(fxArr, 0, len(fixtureCommands))
	for _, command := range fixtureCommands {
		tasks = append(tasks, fxObj{"identity": identity(command), "status": "success", "exitCode": 0, "reuse": "none", "durationMs": 200})
		planned = append(planned, fxObj{"identity": identity(command)})
		commandCounts := counts()
		commandCounts["total"], commandCounts["succeeded"] = 1, 1
		commands = append(commands, fxObj{"command": command, "counts": commandCounts, "reuse": reuse(),
			"freshWallMs": 200, "errors": 0, "warnings": 0})
		jobs = append(jobs, fxObj{"key": "/application:" + command, "project": "/application",
			"task": command, "command": command, "outcome": "success", "reuse": "none", "durationMs": 200})
	}
	session := func(id string, tasks fxArr) fxObj {
		return fxObj{"protocolVersion": 2, "sessionId": id, "startTime": fixtureStart, "endTime": fixtureEnd,
			"commands":  fixtureCommands,
			"selection": fxObj{"mode": "impacted", "scoped": true, "projects": fxArr{"/application"}},
			"git":       fxObj{"baseline": r.base},
			"tree":      fxObj{"fingerprint": r.tree, "headSHA": r.head, "dirty": true},
			"run":       run(), "tasks": tasks}
	}
	r.writeJSON(".context/session.json", session("synthetic-gate", tasks))
	// The narrow gate misses /application:build, which the native plan holds.
	r.writeJSON(".context/session-narrow.json", session("synthetic-gate", slices.Delete(slices.Clone(tasks), 2, 3)))
	r.writeJSON(".context/report.json", fxObj{"protocolVersion": 2, "sessionId": "synthetic-gate",
		"startTime": fixtureStart, "endTime": fixtureEnd, "origin": "cli", "enforceCoverage": true,
		"git": fxObj{"sha": r.head, "dirty": true}, "run": run(), "commands": commands, "jobs": jobs, "elidedJobs": 0})
	r.writeJSON(".context/plan.json", fxObj{"protocolVersion": 2, "status": "success", "exitCode": 0,
		"plan": fxObj{"dryRun": true, "tasks": planned}})
	r.write(".context/plan-args", strings.Join(fixtureCommands, ",")+" --impacted --baseline "+r.base+
		" --enforce-coverage --plan --output=json\n")
	phaseNames := []string{"resolve-target", "readiness", "version-binding", "smoke", "teardown"}
	phases := make(fxArr, 0, len(phaseNames))
	for _, name := range phaseNames {
		phases = append(phases, fxObj{"name": name, "state": "passed", "durationMs": 1})
	}
	r.writeJSON(".context/qualify.json", fxObj{"protocolVersion": 1, "project": "/application",
		"target":  fxObj{"kind": "local", "url": "http://127.0.0.1:12345", "compositionId": "fixture"},
		"binding": fxObj{"kind": "tree", "fingerprint": r.tree, "headSHA": r.head, "dirty": true},
		"contract": fxObj{"digest": "sha256:" + strings.Repeat("c", 64), "requests": 1, "derivedFrom": fxArr{
			fxObj{"kind": "http-routes", "path": "schema/http-routes.json", "digest": "sha256:" + strings.Repeat("d", 64)}}},
		"phases":   phases,
		"requests": fxArr{fxObj{"id": "GET /items", "status": 200, "durationMs": 1, "state": "passed"}},
		"state":    "passed", "startedAt": fixtureStart, "finishedAt": fixtureEnd,
		"cleanup": fxObj{"state": "clean", "leftovers": fxArr{}}})

	changed := fxArr{"app.txt", "new.txt", "src/main.txt"}
	coverage := make(fxArr, 0, len(changed))
	for _, path := range changed {
		coverage = append(coverage, fxObj{"path": path, "status": "reviewed", "reason": "Checked the fixture change."})
	}
	context := r.note("context", "Contract version one.")
	proposal := r.note("proposal", "Keep application ownership.")
	dossier := fxObj{"version": 2,
		"binding":      fxObj{"fingerprint": r.tree, "headSHA": r.head, "baseSHA": r.base},
		"changedFiles": changed,
		"policies":     fxArr{r.ref("AGENTS.md"), r.ref("decisions.json"), r.ref("putnami.ci.json")},
		"objective":    r.note("objective", "Exercise the declared behavior."),
		"scope":        r.note("scope", "Application scope; app.txt."),
		"implementers": fxArr{"implementation-session"},
		"scopes": fxArr{fxObj{"scope": "/application", "owner": "consulted-owner-session",
			"context": fxArr{context}, "proposal": proposal,
			"design": fxObj{"position": "accepted", "note": r.note("design", "Design accepted."),
				"proposalSha256": proposal["sha256"], "contextSha256": fxArr{context["sha256"]}},
			"realization": fxObj{"position": "accepted", "fingerprint": r.tree,
				"note":           r.note("realization", "Realization satisfies contract."),
				"proposalSha256": proposal["sha256"], "contextSha256": fxArr{context["sha256"]}},
			"objections": fxArr{}}},
		"review": fxObj{"author": "review-session", "fingerprint": r.tree,
			"report":   r.note("review", "Independent synthetic review fixture."),
			"coverage": coverage, "findings": fxArr{}},
		"gate": fxObj{"session": r.ref(".context/session.json"), "report": r.ref(".context/report.json")},
		"qualification": fxObj{"required": fxArr{"/application"},
			"results": fxArr{r.ref(".context/qualify.json")}, "notApplicable": nil},
		"acceptance": fxArr{fxObj{"requirement": "Required behavior", "status": "passed",
			"evidence": r.note("acceptance", "Synthetic acceptance fixture.")}},
	}
	r.writeJSON(".context/dossier.json", dossier)
	stale := maps.Clone(dossier)
	stale["binding"] = fxObj{"fingerprint": strings.Repeat("0", 64), "headSHA": r.head, "baseSHA": r.base}
	r.writeJSON(".context/dossier-stale.json", stale)

	r.writeJSON(".context/verification-report.json", fxObj{"protocolVersion": 1, "observations": fxArr{
		fxObj{"feature": fixtureFeature, "requirement": "acceptance", "check": "first-check", "status": "passed",
			"provenance": fxObj{"path": "app_test.go", "symbol": "TestFirst"}},
		fxObj{"feature": fixtureFeature, "requirement": "acceptance", "check": "second-check", "status": "passed",
			"provenance": fxObj{"path": "app_test.go", "symbol": "TestSecond"}},
	}})
	reportDigest := "sha256:" + r.ref(".context/verification-report.json")["sha256"].(string)
	for _, id := range []string{"synthetic-gate", "restored"} {
		report := fxObj{"task": "/application:test", "project": "/application",
			"path": ".context/verification-report.json", "digest": reportDigest}
		if id == "restored" {
			report["restored"] = true
		}
		r.writeJSON(".context/sessions/"+id+"/session.json", session(id, tasks))
		r.writeJSON(".context/sessions/"+id+"/spec-verification.json", fxObj{"protocolVersion": 1, "sessionId": id,
			"generatedAt": fixtureEnd, "groups": fxArr{fxObj{"project": "/feature", "feature": fixtureFeature,
				"spec": "feature/specs/workflow.json", "mode": "enforce", "automaticEvaluation": true,
				"blocked": false, "requirements": fxArr{fxObj{"requirement": "acceptance", "state": "verified",
					"checks": fxArr{fxObj{"check": "first-check", "state": "satisfied"},
						fxObj{"check": "second-check", "state": "satisfied"}}}},
				"reports": fxArr{report},
				"counts": fxObj{"specRequirements": 1, "executable": 1, "verified": 1, "unmapped": 0,
					"unexecutable": 0, "missing": 0, "stale": 0, "contradicted": 0}}}})
	}
	mandate := r.note("workflow-mandate", "Ready for review of the current unit only.")
	boundary := r.note("workflow-boundary", "Current unit proves both acceptance checks.")
	stageReason := r.note("stage-nonapplicable", "This settled correction reuses earlier material.")
	candidate := r.note("candidate-stage", "Candidate reviewed.")
	stageNames := []string{"intent", "spec", "design", "prototype", "review", "adr", "candidate", "handoff"}
	stages := make(fxArr, 0, len(stageNames))
	for _, name := range stageNames {
		stage := fxObj{"unit": "first", "name": name, "state": "not-applicable",
			"entryCondition": "The bounded unit has an owner and exact inputs.", "inputs": fxArr{mandate},
			"expectedOutput": name + " decision", "exitCriteria": "The bounded unit is inspectable", "reason": stageReason}
		if name == "candidate" {
			stage["state"] = "passed"
			delete(stage, "reason")
			stage["receipt"] = candidate
		}
		stages = append(stages, stage)
	}
	workflow := func(proof string) fxObj {
		return fxObj{
			"version": 1, "outcome": "fixture-outcome", "mandate": mandate,
			"sources": fxArr{fxObj{"feature": fixtureFeature,
				"manifest": r.ref("feature/putnami.features.json"), "spec": r.ref("feature/specs/workflow.json")}},
			"requirements": fxArr{fxObj{"feature": fixtureFeature, "id": "acceptance", "disposition": "current", "unit": "first"}},
			"units": fxArr{fxObj{"id": "first", "outcome": "Both checks pass", "boundary": boundary, "exclusions": fxArr{},
				"projects":     fxArr{fxObj{"id": "/feature", "manifest": r.ref("feature/putnami.json")}},
				"requirements": fxArr{fixtureFeature + "#acceptance"}, "dependsOn": fxArr{},
				"requiredStage": "ready", "stage": "candidate", "nextResult": "Independent candidate review",
				"nextCycleTasks": fxArr{}, "decisions": fxArr{}}},
			"artifacts": fxArr{}, "experiments": fxArr{},
			"stages": stages,
			"handoff": fxObj{"mandate": mandate, "unit": "first", "scopes": fxArr{"/feature"},
				"receiver": "reviewer", "responsibility": "Inspect the candidate", "inputs": fxArr{mandate},
				"expectedOutput": "A review finding report", "acceptance": "Every current check is considered",
				"nextAction": "Inspect the candidate", "unresolvedFindings": fxArr{}},
			"proof": fxObj{"session": r.ref(".context/sessions/" + proof + "/session.json"),
				"specVerification": r.ref(".context/sessions/" + proof + "/spec-verification.json")},
		}
	}
	r.writeJSON(".context/workflow.json", workflow("synthetic-gate"))
	r.writeJSON(".context/"+restoredCase+".json", workflow("restored"))
	dossier["workflow"] = r.ref(".context/workflow.json")
	r.writeJSON(".context/dossier-workflow.json", dossier)
}

// dir is where case c runs: a directory of the fixture repository, or, for
// "outside", a directory outside any Git repository.
func (r *verifyRepo) dir(t *testing.T, c verifyCase) string {
	t.Helper()
	if c.dir != "outside" {
		return filepath.Join(r.root, filepath.FromSlash(c.dir))
	}
	outside := filepath.Join(r.home, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("git", "rev-parse", "--show-toplevel")
	probe.Dir = outside
	if probe.Run() == nil {
		t.Skip("the temporary directory is inside a Git repository")
	}
	return outside
}

// launch runs command in dir with the environment putnami gives a launch
// on this machine: PATH, a HOME of the fixture's own, and the C locale.
// extraEnv adds or overrides entries.
func (r *verifyRepo) launch(t *testing.T, dir string, command []string, extraEnv ...string) languageResult {
	t.Helper()
	process := exec.Command(command[0], command[1:]...)
	process.Dir = dir
	process.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "LC_ALL=C"}, extraEnv...)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	err := process.Run()
	result := languageResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch %s: %v", command[0], err)
	}
	return result
}

// launchVerify runs the executable as putnami launches change verify: the
// arguments follow `verify`, and the launch ends with --putnamiContext.
func (r *verifyRepo) launchVerify(t *testing.T, executable string, c verifyCase, extraEnv ...string) languageResult {
	t.Helper()
	command := slices.Concat([]string{executable, "verify"}, c.args, []string{"--putnamiContext", filepath.Join(r.home, "absent-context.json")})
	return r.launch(t, r.dir(t, c), command, extraEnv...)
}

// reportHost serves the host RPC channel on a Unix socket: it answers
// verification.report with answer, refuses any other request, and counts the
// connections and requests it receives.
type reportHost struct {
	socket, token string

	mu          sync.Mutex
	connections int
	requests    []wire.Request
}

func newReportHost(t *testing.T, answer func(wire.VerificationReportParams) ([]byte, *wire.Error)) *reportHost {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes: the temporary
	// directory of a test is longer on macOS.
	dir, err := os.MkdirTemp("/tmp", "pv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	host := &reportHost{socket: filepath.Join(dir, "host.sock"), token: "launch-token"}
	listener, err := net.Listen("unix", host.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			host.mu.Lock()
			host.connections++
			host.mu.Unlock()
			go host.serve(conn, answer)
		}
	}()
	return host
}

func (h *reportHost) serve(conn net.Conn, answer func(wire.VerificationReportParams) ([]byte, *wire.Error)) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		frame, err := wire.ReadFrame(reader, wire.MaxFrameBytes)
		if err != nil {
			return
		}
		var request wire.Request
		if err := json.Unmarshal(frame, &request); err != nil {
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, request)
		h.mu.Unlock()
		response := wire.Response{JSONRPC: wire.JSONRPCVersion, ID: request.ID}
		var params wire.VerificationReportParams
		if request.Token != h.token || request.Method != wire.MethodVerificationReport || json.Unmarshal(request.Params, &params) != nil {
			response.Error = &wire.Error{Code: wire.CodeUnauthorized, Message: "refused"}
		} else if report, refusal := answer(params); refusal != nil {
			response.Error = refusal
		} else {
			response.Result, _ = json.Marshal(wire.VerificationReportResult{Report: report})
		}
		reply, err := wire.EncodeFrame(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func (h *reportHost) env() []string {
	return []string{wire.SocketEnv + "=" + h.socket, wire.TokenEnv + "=" + h.token}
}

func (h *reportHost) seen() (int, []wire.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connections, slices.Clone(h.requests)
}

// refuseEveryReport answers as the host does when its task cache holds no
// report: the fixture's gate never ran, so no cache corroborates it.
func refuseEveryReport(wire.VerificationReportParams) ([]byte, *wire.Error) {
	return nil, &wire.Error{Code: wire.CodeNotVerified, Message: "no cached report"}
}

// TestChangeVerifyMatchesTheCoreCommand runs the built executable on the
// fixture repository for every case of testdata/verify/cases.txt and
// compares its exit code and stdout with testdata/verify/<case>.golden. The
// goldens are the core command's answers: a run with recordVerifyEnv set
// wrote them from `putnami tree verify` on the same fixture, which the test
// builds the same way on every machine. The test never reads the core's
// files, so a change to the core cannot change what it compares against.
// No normalization applies.
//
// Each launch has a host channel whose cache corroborates no report, as the
// core's cache does for the fixture. The fake putnamiw fails a gate when the
// channel reaches it.
func TestChangeVerifyMatchesTheCoreCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, verifyRequirement, "change-verify-prints-what-tree-verify-prints")
	core := os.Getenv(recordVerifyEnv)
	executable := ""
	if core == "" {
		executable = languageExecutable(t)
	}
	repo := newVerifyRepo(t)
	host := newReportHost(t, refuseEveryReport)
	modes := map[string]bool{}
	for _, c := range verifyCases(t) {
		if len(c.args) > 0 {
			modes[c.args[0]] = true
		}
		t.Run(c.name, func(t *testing.T) {
			if core != "" {
				// The core needs its own home to start: putnamiw finds the CLI
				// it built there.
				recorded := repo.launch(t, repo.dir(t, c), slices.Concat([]string{core, "tree", "verify"}, c.args),
					"PUTNAMI_HOME="+putnamiHome(t), "PUTNAMI_NO_RELAUNCH=1")
				if err := os.WriteFile(filepath.Join("testdata", "verify", c.name+".golden"), []byte(renderVerdict(recorded)), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			got := repo.launchVerify(t, executable, c, host.env()...)
			if rendered, want := renderVerdict(got), c.golden(t); rendered != want {
				t.Errorf("the executable differs from the core command's golden\n--- got\n%s\n--- want\n%s", rendered, want)
			}
			if got.stderr != "" {
				t.Errorf("stderr %q, want nothing", got.stderr)
			}
		})
	}
	for _, mode := range []string{"--record", "--workflow", "--snapshot", "--ref", "--gate"} {
		if !modes[mode] {
			t.Errorf("no parity case runs the %s mode", mode)
		}
	}
	if core != "" {
		return
	}
	// Only the restored workflow reads through the channel; the cache's
	// refusal ends it at the first read.
	if connections, requests := host.seen(); connections != 1 || len(requests) != 1 {
		t.Errorf("%d connections and %d requests, want 1 and 1", connections, len(requests))
	}
}

// putnamiHome is the Putnami home of the test process, which a recording run
// gives the core command.
func putnamiHome(t *testing.T) string {
	t.Helper()
	if home := os.Getenv("PUTNAMI_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".putnami")
}

// TestARestoredReportIsReadThroughTheHostChannel pins the one call the
// verifier makes into putnami: verification.report, with the reference the
// proof records, on one connection for the read and the stability recheck.
// With the cache's bytes, the restored workflow is valid.
func TestARestoredReportIsReadThroughTheHostChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, verifyRequirement, "a-restored-report-is-read-through-the-host-channel")
	executable := languageExecutable(t)
	repo := newVerifyRepo(t)
	report := repo.read(".context/verification-report.json")
	host := newReportHost(t, func(wire.VerificationReportParams) ([]byte, *wire.Error) { return report, nil })
	got := repo.launchVerify(t, executable, verifyCase{name: restoredCase, dir: ".",
		args: []string{"--workflow", ".context/" + restoredCase + ".json"}}, host.env()...)
	var verdict struct {
		Verdict              string `json:"verdict"`
		VerifiedRequirements int    `json:"verifiedRequirements"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &verdict); err != nil || got.code != 0 || got.stderr != "" ||
		verdict.Verdict != "workflow valid" || verdict.VerifiedRequirements != 1 {
		t.Fatalf("exit %d, stdout %s, stderr %q; want a valid workflow with its requirement verified", got.code, got.stdout, got.stderr)
	}
	sum := sha256.Sum256(report)
	want := wire.VerificationReportParams{Project: "/application", Task: "/application:test",
		Path: ".context/verification-report.json", Digest: "sha256:" + hex.EncodeToString(sum[:])}
	connections, requests := host.seen()
	if connections != 1 || len(requests) != 2 {
		t.Fatalf("%d connections and %d requests, want 1 and 2", connections, len(requests))
	}
	for _, request := range requests {
		var params wire.VerificationReportParams
		if request.Method != wire.MethodVerificationReport || json.Unmarshal(request.Params, &params) != nil || params != want {
			t.Errorf("request %s %s, want %s %+v", request.Method, request.Params, wire.MethodVerificationReport, want)
		}
	}
}

// TestWithoutAChannelOnlyARestoredReportIsNotVerified runs every case with
// no channel, as a hosted run and a process putnami did not launch have none.
// Every case still answers the core's golden except the restored workflow,
// whose not-verified reason names the missing channel.
func TestWithoutAChannelOnlyARestoredReportIsNotVerified(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, verifyRequirement, "without-a-channel-only-a-restored-report-is-not-verified")
	executable := languageExecutable(t)
	repo := newVerifyRepo(t)
	for _, c := range verifyCases(t) {
		t.Run(c.name, func(t *testing.T) {
			got := repo.launchVerify(t, executable, c)
			if c.name != restoredCase {
				if rendered, want := renderVerdict(got), c.golden(t); rendered != want {
					t.Errorf("without a channel the executable differs from the core command's golden\n--- got\n%s\n--- want\n%s", rendered, want)
				}
				return
			}
			var verdict struct{ Verdict, Reason string }
			if err := json.Unmarshal([]byte(got.stdout), &verdict); err != nil || got.code != 1 || got.stderr != "" ||
				verdict.Verdict != "not-verified" || strings.Count(got.stdout, "\n") != 1 {
				t.Fatalf("exit %d, stdout %q, stderr %q; want one not-verified line", got.code, got.stdout, got.stderr)
			}
			for _, want := range []string{"restored report has no host channel to corroborate it: /application:test",
				"putnami change verify", wire.SocketEnv + " and " + wire.TokenEnv + " not set"} {
				if !strings.Contains(verdict.Reason, want) {
					t.Errorf("reason %q does not say %q", verdict.Reason, want)
				}
			}
		})
	}
}

// TestNoProcessReceivesTheHostChannel runs the cases that start Git and the
// workspace CLI with a channel and a git on PATH that fails when it receives
// the channel. The fake putnamiw fails the same way. Every case answers the
// core's golden.
func TestNoProcessReceivesTheHostChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, languageFeature, verifyRequirement, "no-process-receives-the-host-channel")
	executable := languageExecutable(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	repo := newVerifyRepo(t)
	shims := filepath.Join(repo.home, "shims")
	shim := "#!/bin/sh\nif [ -n \"${" + wire.SocketEnv + "-}\" ] || [ -n \"${" + wire.TokenEnv + "-}\" ]; then\n" +
		"  echo 'git: the host channel reached git' >&2\n  exit 97\nfi\nexec '" + realGit + "' \"$@\"\n"
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shims, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	host := newReportHost(t, refuseEveryReport)
	env := append(host.env(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	ran := 0
	for _, c := range verifyCases(t) {
		if c.dir == "outside" {
			continue
		}
		ran++
		got := repo.launchVerify(t, executable, c, env...)
		if rendered, want := renderVerdict(got), c.golden(t); rendered != want || got.stderr != "" {
			t.Errorf("%s with a channel and the git shim answered\n%s\nstderr %q\nwant\n%s", c.name, rendered, got.stderr, want)
		}
	}
	if ran == 0 {
		t.Fatal("no case ran")
	}
}
