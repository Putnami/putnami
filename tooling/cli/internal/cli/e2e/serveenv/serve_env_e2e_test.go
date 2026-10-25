// Package serveenv drives a supervised serve run through the real CLI, from
// the argument vector and the stdin document to the task processes: the
// terminal adapter reads `--serve-env -`, the engine names the serve-workload
// step, the scheduler hands that step alone the document's variables and
// announces its process group, and the jsonl renderer streams the session on
// stdout and the serve log on stderr. These tests swap the process streams,
// stdin included, and the working directory, so they run in their own test
// binary.
package serveenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

const (
	secretName  = "SUPERVISOR_SECRET"
	secretValue = "supervisor-secret-value-7f3a"
	readyLine   = `{"v":2,"type":"ready","target":"server","name":"api","endpoints":[{"scheme":"http","host":"127.0.0.1","port":43120}],"durationMs":420}`
)

// serveFixture writes a workspace whose one project, example, has a serve
// command of two steps, as a language extension's serve is: a finite generate
// step and the serve step after it, which declares FIXTURE_MODE in its own env.
// Each step writes the supervisor variable it sees and copies its job context
// into the project; the serve step then logs a line and claims readiness. A
// lint command runs no workload.
func serveFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"serve-env-fixture","includes":["extension","example"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "putnami.json"), `{"name":"example","extensions":["@fixture/serve"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "serve.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/serve"}`)
	step := func(name, then string) string {
		return `printf '%s' "${` + secretName + `-unset}" > "$PUTNAMI_PROJECT_ROOT/` + name + `.env"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    cp "$2" "$PUTNAMI_PROJECT_ROOT/` + name + `.context.json"
  fi
  shift
done
` + then
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "generate.sh"), step("generate", ""))
	clitest.WriteFile(t, filepath.Join(root, "extension", "serve.sh"), step("serve",
		`printf '%s\n' '{"v":2,"type":"log","level":"info","message":"fixture api is up"}'
printf '%s\n' '`+readyLine+`'
`))
	clitest.WriteFile(t, filepath.Join(root, "extension", "lint.sh"), "exit 0\n")
	disabled := false
	task := func(script string, env map[string]string) extensionproto.TaskDefinition {
		return extensionproto.TaskDefinition{
			Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/" + script},
			Env: env, Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: -1,
		}
	}
	manifest := extensionproto.Manifest{
		Name: "@fixture/serve", Version: "1.0.0", CLIContract: protocolcli.CommandKindContract,
		Commands: map[string]extensionproto.CommandDefinition{
			"serve": {
				ActivationFiles: []string{"serve.project"},
				Traits:          &extensionproto.CommandTraits{Workload: extensionproto.WorkloadServe},
				Flags: map[string]extensionproto.FlagDefinition{
					"port":  {Type: "number"},
					"watch": {Type: "boolean", Default: true},
				},
				Run: []extensionproto.PipelineStep{
					{ID: "generate", Task: "serve-generate"},
					{ID: "serve", Task: "serve-run", DependsOn: []string{"generate"}},
				},
			},
			"lint": {
				ActivationFiles: []string{"serve.project"},
				Run:             []extensionproto.PipelineStep{{ID: "lint", Task: "lint-run"}},
			},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"serve-generate": task("generate.sh", nil),
			"serve-run":      task("serve.sh", map[string]string{"FIXTURE_MODE": "serve"}),
			"lint-run":       task("lint.sh", nil),
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	return root
}

// runWithStdin runs one invocation of the real CLI in root with document on
// its stdin, and returns the exit code and both streams apart.
func runWithStdin(t *testing.T, root, document string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DO_NOT_TRACK", "1")
	input := filepath.Join(t.TempDir(), "stdin.json")
	clitest.WriteFile(t, input, document)
	stdin, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	original := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = original }()

	var code int
	stdout, stderr := clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(root)
		code = app.Run(t.Context(), args)
	})
	return code, stdout, stderr
}

func readProjectFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "example", name))
	if err != nil {
		t.Fatalf("the step did not write %s: %v", name, err)
	}
	return string(data)
}

// sessionFiles returns every file the run recorded under .putnami, joined.
func sessionFiles(t *testing.T, root string) string {
	t.Helper()
	var all strings.Builder
	_ = filepath.WalkDir(filepath.Join(root, ".putnami"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			all.Write(data)
		}
		return nil
	})
	return all.String()
}

var serveArgs = []string{"serve", "--projects", "example", "--port", "0", "--no-watch", "--serve-env", "-", "--output=jsonl"}

// TestSupervisedServeRunReachesOnlyTheServeStep drives a supervised serve run
// end to end: the serve step alone sees the supervisor's variable, which no
// job context, stream or session record holds; the stream names the serve
// step's process group before its ready claim; and stderr carries the
// workload's output as `putnami serve` renders it in text mode.
func TestSupervisedServeRunReachesOnlyTheServeStep(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/supervised-serve-run", "serve-env-reaches-only-serve-steps", "a-supervised-serve-run-hands-the-env-to-the-serve-step-alone")
	root := serveFixture(t)
	code, stdout, stderr := runWithStdin(t, root, `{"`+secretName+`":"`+secretValue+`","SERVE_ORIGIN":"http://127.0.0.1:43120"}`, serveArgs...)
	if code != 0 {
		t.Fatalf("putnami %s exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(serveArgs, " "), code, stdout, stderr)
	}

	if got := readProjectFile(t, root, "serve.env"); got != secretValue {
		t.Errorf("the serve step saw %s=%q, want the supervisor's value", secretName, got)
	}
	if got := readProjectFile(t, root, "generate.env"); got != "unset" {
		t.Errorf("the finite generate step saw %s=%q, want it unset", secretName, got)
	}
	for name, text := range map[string]string{
		"the serve job context":    readProjectFile(t, root, "serve.context.json"),
		"the generate job context": readProjectFile(t, root, "generate.context.json"),
		"stdout":                   stdout,
		"stderr":                   stderr,
		"the session records":      sessionFiles(t, root),
	} {
		if strings.Contains(text, secretValue) || strings.Contains(text, secretName) {
			t.Errorf("%s holds the supervisor's variable", name)
		}
	}

	var process, ready = -1, -1
	var group float64
	var leaderStart string
	for index, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("stdout line %d is not a session stream record: %v\n%s", index, err, line)
		}
		if record.Record != "task:event" || record.Identity == nil || !strings.HasSuffix(record.Identity.Key, ":serve~serve") {
			continue
		}
		switch record.Event["type"] {
		case "process":
			if process < 0 {
				process = index
			}
			group, _ = record.Event["group"].(float64)
			leaderStart, _ = record.Event["leaderStart"].(string)
		case "ready":
			ready = index
		}
	}
	if process < 0 || ready < 0 || process > ready {
		t.Fatalf("the serve step's process event (record %d) does not precede its ready claim (record %d):\n%s", process, ready, stdout)
	}
	if group <= 0 || leaderStart == "" {
		t.Errorf("process event group = %v, leaderStart = %q; want the spawned group and its leader's start time", group, leaderStart)
	}
	if !strings.Contains(stderr, "fixture api is up") {
		t.Errorf("stderr does not carry the serve log:\n%s", stderr)
	}
	if strings.Contains(stderr, `"record":`) {
		t.Errorf("stderr carries session stream records:\n%s", stderr)
	}
}

// TestSupervisedServeRunRefusals pins each usage refusal of `--serve-env -`
// through the real CLI: it exits with the usage code before any step runs,
// and its message names the variable, never a value.
func TestSupervisedServeRunRefusals(t *testing.T) {
	clitest.RequireShell(t)
	for _, tc := range []struct {
		name, document, want string
		args                 []string
	}{
		{"a command without a serve step", `{"` + secretName + `":"` + secretValue + `"}`, "needs a serve-workload step",
			[]string{"lint", "--projects", "example", "--serve-env", "-", "--output=jsonl"}},
		{"a manifest env conflict", `{"FIXTURE_MODE":"` + secretValue + `"}`, "FIXTURE_MODE", serveArgs},
		{"a document that is not an object", `["` + secretValue + `"]`, "not one JSON object", serveArgs},
		{"a reserved variable", `{"PUTNAMI_PARENT_SESSION":"` + secretValue + `"}`, "PUTNAMI_ namespace", serveArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "a manifest env conflict" {
				spectest.Proves(t, "cli/supervised-serve-run", "a-manifest-env-conflict-is-refused", "the-cli-refuses-the-run-before-any-step-starts")
			}
			root := serveFixture(t)
			code, stdout, stderr := runWithStdin(t, root, tc.document, tc.args...)
			if code != protocolcli.ExitUsage {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, protocolcli.ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not name %q:\n%s", tc.want, stderr)
			}
			if strings.Contains(stdout+stderr, secretValue) {
				t.Errorf("the refusal quotes a value:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
			for _, name := range []string{"serve.env", "generate.env"} {
				if _, err := os.Stat(filepath.Join(root, "example", name)); err == nil {
					t.Errorf("a step ran before the refusal: %s exists", name)
				}
			}
		})
	}
}
