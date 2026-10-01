package credentialcustody

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// workspaceRootEnv is the variable the CLI sets to the workspace root for every
// job (jobs.BuildEnvVars) and every workspace hook (cli_hooks): the root a
// probe walks. The CLI owns the literal; there is no exported constant.
const workspaceRootEnv = "PUTNAMI_WORKSPACE_ROOT"

// The three secrets a hosted run holds. They are compile-time constants of this
// binary, shared by the engine, the hostile roles and the outer test, so a
// hostile role knows what to hunt for WITHOUT being handed it in an
// environment, an argument or a file — which would defeat the test. The engine
// receives the bearer only on a descriptor and the tokens only in its own
// environment; the constants are the oracle, never a channel.
const (
	custodyBearer = "hosted-run-credential-9f2c4e-do-not-leak"
	custodyCache  = "hosted-cache-token-4a1b8c"
	custodyCloud  = "hosted-cloud-token-7d3e2f"
)

// Bounds for the file probes. A hosted build writes no credential to disk, and
// every probed root is a small isolated temporary directory, so these bounds
// are never reached in a passing run; they keep a probe that met a large tree
// (a misconfigured root) from running unbounded.
const (
	maxProbeFiles    = 8192
	maxProbeBytes    = 64 << 20
	maxProbeFileSize = 8 << 20
)

// secretNames are the labels scanBytes returns, in a stable order.
var secretNames = []string{"bearer", "cache", "cloud"}

// finding is one probe's result, written as one JSON line to the role's report.
// Checked records that the probe actually ran: a probe that finds nothing only
// because it never executed must fail the test, so the assertions require every
// expected probe to be present AND checked.
type finding struct {
	Role    string            `json:"role"`
	Probe   string            `json:"probe"`
	Checked bool              `json:"checked"`
	Found   []string          `json:"found,omitempty"`
	Detail  string            `json:"detail,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// scanBytes returns which of the three secrets appear in data.
func scanBytes(data []byte) []string {
	var found []string
	if bytes.Contains(data, []byte(custodyBearer)) {
		found = append(found, "bearer")
	}
	if bytes.Contains(data, []byte(custodyCache)) {
		found = append(found, "cache")
	}
	if bytes.Contains(data, []byte(custodyCloud)) {
		found = append(found, "cloud")
	}
	return found
}

// searchProbes are the probe names that must find no secret on a hosted run,
// for the current OS. envValuesProbe is asserted separately (it records values,
// not a search). The Linux-only probes are named by platformSearchProbes.
func searchProbes() []string {
	probes := []string{"env-self", "files-home", "files-workspace", "files-tmpdir", "files-systemtmp"}
	return append(probes, platformSearchProbes()...)
}

const envValuesProbe = "env-values"

// runHostileRole is one hostile process — the build's before-hook or its build
// task. It probes for the credential everywhere a repository process can reach
// and appends every finding to its report. It always exits 0: the engine must
// see the hook and task succeed so the build completes and both reports exist.
func runHostileRole(role string) int {
	report := os.Getenv(custodyReportEnv)
	wsRoot := os.Getenv(workspaceRootEnv)

	findings := []finding{
		probeEnvSelf(role),
		probeEnvValues(role),
		probeFiles(role, "home", os.Getenv("HOME")),
		probeFiles(role, "workspace", wsRoot),
		probeFiles(role, "tmpdir", os.Getenv("TMPDIR")),
		probeFiles(role, "systemtmp", os.TempDir()),
	}
	findings = append(findings, platformProbes(role)...)

	if err := writeFindings(report, findings); err != nil {
		// The report is the only channel the test reads; a failure to write it
		// makes the missing-probe assertion fail with a clear message.
		fmt.Fprintf(os.Stderr, "custody %s: write report: %v\n", role, err)
		return 1
	}
	return 0
}

// probeEnvSelf searches this process's whole environment for the secrets.
func probeEnvSelf(role string) finding {
	joined := strings.Join(os.Environ(), "\n")
	return finding{Role: role, Probe: "env-self", Checked: true, Found: scanBytes([]byte(joined))}
}

// probeEnvValues records the values a repository process reads for the two
// framework tokens and the offline signal, so the flag-off scenario can assert
// they pass through and no offline signal is added.
func probeEnvValues(role string) finding {
	return finding{Role: role, Probe: envValuesProbe, Checked: true, Env: map[string]string{
		"cache":   os.Getenv(runcredential.CacheTokenEnv),
		"cloud":   os.Getenv(runcredential.CloudTokenEnv),
		"offline": os.Getenv(extensionproto.OfflineDependenciesEnv),
	}}
}

// probeFiles walks root, reads each file within the bounds, and records which
// secrets it found. An empty root is recorded as checked with no root, so a
// missing directory is not silently a pass.
func probeFiles(role, name, root string) finding {
	f := finding{Role: role, Probe: "files-" + name, Checked: true}
	if root == "" {
		f.Detail = "no-root"
		return f
	}
	foundSet := map[string]bool{}
	files, read := 0, 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if files >= maxProbeFiles || read >= maxProbeBytes {
			return fs.SkipAll
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, e := d.Info()
		if e != nil || info.Size() > maxProbeFileSize {
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return nil
		}
		files++
		read += len(data)
		for _, s := range scanBytes(data) {
			foundSet[s] = true
		}
		return nil
	})
	for _, s := range secretNames {
		if foundSet[s] {
			f.Found = append(f.Found, s)
		}
	}
	f.Detail = fmt.Sprintf("files=%d bytes=%d", files, read)
	return f
}

// writeFindings appends one JSON object per finding to path.
func writeFindings(path string, findings []finding) error {
	if path == "" {
		return fmt.Errorf("no report path")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	encoder := json.NewEncoder(file)
	for _, f := range findings {
		if err := encoder.Encode(f); err != nil {
			return err
		}
	}
	return nil
}

// readFindings reads a role's report into a map keyed by probe name.
func readFindings(t *testing.T, path string) map[string]finding {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	out := map[string]finding{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var f finding
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("decode finding %q: %v", line, err)
		}
		out[f.Probe] = f
	}
	return out
}

// hostileFixture is the workspace the engine runs `build` in. Its one project's
// build has a before-hook and a build task, both this test binary re-executed
// in a hostile role that writes to the returned report path.
type hostileFixture struct {
	wsRoot     string
	hookReport string
	taskReport string
}

// writeHostileFixture builds the fixture. self is the absolute path of this
// test binary, which the hook and the task both run. reportsDir holds the two
// reports and is a sibling of every probed root, so a probe never reads a
// report file (which, in the flag-off scenario, records token values). The
// extension is installed in the artifact store under home, the one runEngine
// names, and linked from the workspace as a registry pin is: a hosted run runs
// no other extension.
func writeHostileFixture(t *testing.T, self, reportsDir, home string) hostileFixture {
	t.Helper()
	wsRoot := t.TempDir()
	fx := hostileFixture{
		wsRoot:     wsRoot,
		hookReport: filepath.Join(reportsDir, "hook.jsonl"),
		taskReport: filepath.Join(reportsDir, "task.jsonl"),
	}
	extRoot := filepath.Join(artifactDir(home), "extensions", "hostile@0.1.0")

	// The before-hook runs through `sh -c`: it sets the hostile role and report
	// on the command it execs, overriding whatever the engine's environment
	// carried, and `exec` replaces the shell so the probe's parent is the engine
	// that holds the credential.
	hookCmd := fmt.Sprintf("%s=hook %s=%s exec %s",
		custodyRoleEnv, custodyReportEnv, shellQuote(fx.hookReport), shellQuote(self))
	workspace := fmt.Sprintf(`{
  "name": "hostile-ws",
  "includes": ["app"],
  "extensions": { "@fixture/hostile": "0.1.0" },
  "hooks": { "commands": { "build": { "before": [%s] } } }
}`, jsonString(hookCmd))
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), workspace)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/hostile"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "hostile.project"), "")

	// The build task is this binary in the "task" role. Its manifest env sets
	// the role and report; the engine's job environment appends it after the
	// inherited one, so the child reads "task". cache:false keeps the task off
	// the object cache so it runs every time.
	manifest := fmt.Sprintf(`{
  "name": "@fixture/hostile",
  "version": "0.1.0",
  "cliContract": %d,
  "commands": {
    "build": {
      "description": "Build.",
      "activationFiles": ["hostile.project"],
      "run": [{ "id": "probe", "task": "probe-task" }]
    }
  },
  "tasks": {
    "probe-task": {
      "kind": "command",
      "command": %s,
      "cache": false,
      "timeoutMs": 30000,
      "env": { %s: "task", %s: %s }
    }
  }
}`,
		protocolcli.CurrentContract,
		jsonString(self),
		jsonString(custodyRoleEnv), jsonString(custodyReportEnv), jsonString(fx.taskReport))
	clitest.WriteFile(t, filepath.Join(extRoot, "putnami.extension.json"), manifest)
	link := layout.StableDir(wsRoot, layout.Extensions, "@fixture/hostile")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(extRoot, link); err != nil {
		t.Fatal(err)
	}
	return fx
}

// runEngine re-executes this binary as the engine, which runs `build` in the
// fixture workspace. On a hosted run it hands the bearer on descriptor 3 and
// sets the two tokens in the engine's own environment. HOME, TMPDIR and the
// stores are isolated per run so probes have small, credential-free roots.
// extraEnv is added last, so it wins. The engine's exit status is returned
// with its captured output.
func runEngine(t *testing.T, self, wsRoot, cleanHome string, hosted bool, extraEnv ...string) (int, string) {
	t.Helper()
	cleanTmp := t.TempDir()

	cmd := exec.Command(self)
	cmd.Env = append(engineEnv(wsRoot, cleanHome, cleanTmp, hosted), extraEnv...)
	if hosted {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		go func() {
			_, _ = w.WriteString(custodyBearer + "\n")
			_ = w.Close()
		}()
		cmd.ExtraFiles = []*os.File{r} // becomes descriptor 3 in the child
	}

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run engine: %v\n%s", err, out.String())
		}
	}
	if strings.Contains(out.String(), custodyBearer) {
		t.Errorf("the bearer reached the engine output:\n%s", out.String())
	}
	return code, out.String()
}

// artifactDir is the artifact store of a run whose home is home.
func artifactDir(home string) string {
	return filepath.Join(home, "artifacts")
}

// engineEnv is the engine child's environment: the outer environment with every
// secret and role variable removed, then the run's own values added last so
// they win. On a hosted run the two tokens live ONLY here, so /proc/<engine>/environ
// holds them (procguard must then deny a repository process that read).
func engineEnv(wsRoot, home, tmp string, hosted bool) []string {
	strip := []string{
		runcredential.CacheTokenEnv, runcredential.CloudTokenEnv,
		extensionproto.OfflineDependenciesEnv,
		custodyRoleEnv, custodyReportEnv, custodyWorkspaceEnv, custodyCredentialFDEnv,
		custodyCacheEnv, custodyTargetEnv, custodyArgsEnv,
		"HOME", "TMPDIR",
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(strip, key) {
			env = append(env, entry)
		}
	}
	env = append(env,
		custodyRoleEnv+"=engine",
		custodyWorkspaceEnv+"="+wsRoot,
		"HOME="+home,
		"TMPDIR="+tmp,
		"PUTNAMI_STORE_DIR="+filepath.Join(home, "store"),
		"PUTNAMI_ARTIFACT_DIR="+artifactDir(home),
		"PUTNAMI_NO_AUTO_INSTALL=1",
		"PUTNAMI_NO_RELAUNCH=1",
		runcredential.CacheTokenEnv+"="+custodyCache,
		runcredential.CloudTokenEnv+"="+custodyCloud,
	)
	if hosted {
		env = append(env, custodyCredentialFDEnv+"=3")
	}
	return env
}

// TestHostileHookAndTaskFindNothingOnAHostedRun runs a hosted `build` whose
// before-hook and build task hunt for the run credential and the framework
// tokens in their environment, the user's home, the workspace, the temporary
// directory, and, on Linux, the /proc environ, memory and descriptors of the
// engine and a ptrace attach. Every probe must run and find nothing.
func TestHostileHookAndTaskFindNothingOnAHostedRun(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "hostile-hook-and-test-find-nothing")
	clitest.RequireShell(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reportsDir := t.TempDir()
	home := t.TempDir()
	fx := writeHostileFixture(t, self, reportsDir, home)

	code, output := runEngine(t, self, fx.wsRoot, home, true)
	if code != 0 {
		t.Fatalf("hosted build exit=%d, want 0 (hook and task must run to completion)\n%s", code, output)
	}

	// Root defeats PR_SET_DUMPABLE by design — CAP_SYS_PTRACE reads any process
	// of its user — so the ADR's threat model is a same-user NON-root process.
	// Under root the /proc and ptrace probes may read the engine, exactly as
	// procguard_linux_test.go exempts them; the environment and file probes must
	// still find nothing, because the credential is genuinely nowhere on disk or
	// in a child's environment.
	rootExempt := runtime.GOOS == "linux" && os.Geteuid() == 0
	platform := map[string]bool{}
	for _, probe := range platformSearchProbes() {
		platform[probe] = true
	}
	for role, report := range map[string]string{"hook": fx.hookReport, "task": fx.taskReport} {
		findings := readFindings(t, report)
		for _, probe := range searchProbes() {
			f, ok := findings[probe]
			switch {
			case !ok:
				t.Errorf("%s: probe %q did not run", role, probe)
			case !f.Checked:
				t.Errorf("%s: probe %q recorded no check (detail %q)", role, probe, f.Detail)
			case len(f.Found) > 0:
				if rootExempt && platform[probe] {
					t.Logf("%s: probe %q read the engine as root (found %v); root is outside the same-user threat model", role, probe, f.Found)
					continue
				}
				t.Errorf("%s: probe %q found %v (detail %q) — a repository process reached the credential", role, probe, f.Found, f.Detail)
			}
		}
		// The engine dropped both tokens and set the offline signal, so a
		// hosted job and hook read neither token and offline "1".
		ev, ok := findings[envValuesProbe]
		if !ok || !ev.Checked {
			t.Errorf("%s: %q did not run", role, envValuesProbe)
			continue
		}
		if ev.Env["cache"] != "" || ev.Env["cloud"] != "" {
			t.Errorf("%s: a hosted process read tokens cache=%q cloud=%q, want both empty", role, ev.Env["cache"], ev.Env["cloud"])
		}
		if ev.Env["offline"] != "1" {
			t.Errorf("%s: offline signal = %q, want \"1\" on a hosted run", role, ev.Env["offline"])
		}
	}
}

// TestFlagOffLeavesJobAndHookEnvUnchanged runs the same workspace as a local
// build, without --credential-fd. The job and hook environment is the one it
// always had: both framework tokens pass through and no offline signal is
// added. This is the end-to-end counterpart of TestJobEnvironmentOfALocalRun.
func TestFlagOffLeavesJobAndHookEnvUnchanged(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "flag-off-changes-nothing", "flag-off-job-env-unchanged")
	clitest.RequireShell(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reportsDir := t.TempDir()
	home := t.TempDir()
	fx := writeHostileFixture(t, self, reportsDir, home)

	code, output := runEngine(t, self, fx.wsRoot, home, false)
	if code != 0 {
		t.Fatalf("local build exit=%d, want 0\n%s", code, output)
	}

	for role, report := range map[string]string{"hook": fx.hookReport, "task": fx.taskReport} {
		findings := readFindings(t, report)
		ev, ok := findings[envValuesProbe]
		if !ok || !ev.Checked {
			t.Fatalf("%s: %q did not run", role, envValuesProbe)
		}
		if ev.Env["cache"] != custodyCache {
			t.Errorf("%s: PUTNAMI_CACHE_TOKEN = %q, want the inherited %q", role, ev.Env["cache"], custodyCache)
		}
		if ev.Env["cloud"] != custodyCloud {
			t.Errorf("%s: PUTNAMI_CLOUD_TOKEN = %q, want the inherited %q", role, ev.Env["cloud"], custodyCloud)
		}
		if ev.Env["offline"] != "" {
			t.Errorf("%s: a local run added the offline signal (%q); the flag off changes nothing", role, ev.Env["offline"])
		}
	}
}

// shellQuote wraps s in single quotes for `sh -c`, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jsonString renders s as a JSON string literal for the fixture manifests.
func jsonString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}
