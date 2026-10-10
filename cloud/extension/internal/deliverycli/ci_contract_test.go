package deliverycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
)

// writeTestExtension writes an extension manifest that declares the named
// commands into dir.
func writeTestExtension(t *testing.T, dir, name string, commands ...string) {
	t.Helper()
	declared := map[string]any{}
	tasks := map[string]any{}
	for _, command := range commands {
		task := strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-") + "-" + command
		declared[command] = map[string]any{"description": command, "run": []any{map[string]any{"task": task}}}
		tasks[task] = map[string]any{"kind": "command", "command": "true", "description": command}
	}
	manifest := map[string]any{"cliContract": 4, "name": name, "version": "0.1.0", "commands": declared, "tasks": tasks}
	writeContractJSON(t, filepath.Join(dir, "putnami.extension.json"), manifest)
}

func writeContractJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// contractWorkspace is a workspace that pins @acme/tools by name, installed
// under .putnami/bin/extensions, which declares lint, test and build.
func contractWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeContractJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
		"name": "acme", "extensions": map[string]any{"@acme/tools": "0.1.0"},
	})
	writeTestExtension(t, filepath.Join(root, ".putnami", "bin", "extensions", "acme-tools"), "@acme/tools", "lint", "test", "build")
	return root
}

func runCIContract(t *testing.T, root string, params map[string]any, args ...string) ([]string, error) {
	t.Helper()
	var stdout []string
	err := CI(params, args, root, map[string]string{}, clicore.IO{
		Stdout: func(line string) { stdout = append(stdout, line) },
		Stderr: func(string) {},
	})
	return stdout, err
}

func TestCIInitSeedsTheDeclaredJobs(t *testing.T) {
	root := contractWorkspace(t)
	stdout, err := runCIContract(t, root, map[string]any{}, "init")
	if err != nil {
		t.Fatalf("ci init: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ciproto.Filename))
	if err != nil {
		t.Fatal(err)
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		t.Fatalf("written document does not parse: %v", err)
	}
	names := make([]string, 0, len(document.Commands))
	for _, command := range document.Commands {
		names = append(names, command.Name)
	}
	if strings.Join(names, ",") != "lint,test,build" || document.Distribution != nil || len(document.Envs) != 0 {
		t.Fatalf("document = %+v, want lint,test,build and no provider section", document)
	}
	if text := strings.Join(stdout, "\n"); !strings.Contains(text, "Created") || !strings.Contains(text, "Digest: sha256:") {
		t.Fatalf("stdout = %q", text)
	}
	if _, err := runCIContract(t, root, map[string]any{}, "init"); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second init error = %v, want a usage error naming --force", err)
	}
	if _, err := runCIContract(t, root, map[string]any{"force": true}, "init"); err != nil {
		t.Fatalf("ci init --force: %v", err)
	}
	if _, err := runCIContract(t, root, map[string]any{}, "init", "extra"); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "takes no positional argument") {
		t.Fatalf("ci init extra error = %v", err)
	}
}

func TestCIInitNeedsAVerificationJob(t *testing.T) {
	root := t.TempDir()
	writeContractJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "acme"})
	if _, err := runCIContract(t, root, map[string]any{}, "init"); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "write the commands by hand") {
		t.Fatalf("ci init without jobs error = %v", err)
	}
}

func TestCIValidateResolvesJobsAndProviderSections(t *testing.T) {
	root := contractWorkspace(t)
	if _, err := runCIContract(t, root, map[string]any{}, "validate"); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "ci init") {
		t.Fatalf("validate without a document error = %v, want a usage error naming ci init", err)
	}
	path := filepath.Join(root, ciproto.Filename)
	writeContractJSON(t, path, map[string]any{"version": 3, "commands": []string{"lint", "deploy"}})
	stdout, err := runCIContract(t, root, map[string]any{}, "validate")
	if clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(strings.Join(stdout, "\n"), "is invalid") {
		t.Fatalf("unknown job: error = %v, stdout = %q", err, stdout)
	}

	writeContractJSON(t, path, map[string]any{
		"version": 3, "commands": []string{"lint", "test"},
		"distribution": map[string]any{"namespace": "acme", "channels": map[string]any{"stable": map[string]any{}}},
	})
	var envelope struct {
		Data CIContractReport `json:"data"`
	}
	stdout, err = runCIContract(t, root, map[string]any{"output": "json"}, "validate")
	if clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("distribution without a provider error = %v", err)
	}
	if err := json.Unmarshal([]byte(strings.Join(stdout, "\n")), &envelope); err != nil {
		t.Fatalf("report %q: %v", stdout, err)
	}
	if report := envelope.Data; report.Valid || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "ci.provider_required" {
		t.Fatalf("report = %+v, want one provider_required finding", report)
	}

	project := filepath.Join(root, "tools", "release")
	writeContractJSON(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "release"})
	writeTestExtension(t, project, "@acme/release", distribution.ProviderCommandName)
	stdout, err = runCIContract(t, root, map[string]any{}, "validate")
	if err != nil || !strings.Contains(strings.Join(stdout, "\n"), "is valid") {
		t.Fatalf("validate with a provider: %v, stdout = %q", err, stdout)
	}
}

func TestCIFormatRewritesOrChecks(t *testing.T) {
	root := contractWorkspace(t)
	path := filepath.Join(root, ciproto.Filename)
	if err := os.WriteFile(path, []byte(`{"commands":["lint"],"version":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCIContract(t, root, map[string]any{"check": true}, "fmt"); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "canonical form") {
		t.Fatalf("fmt --check error = %v, want a usage error", err)
	}
	stdout, err := runCIContract(t, root, map[string]any{}, "fmt")
	if err != nil || !strings.Contains(strings.Join(stdout, "\n"), "Formatted") {
		t.Fatalf("fmt: %v, stdout = %q", err, stdout)
	}
	stdout, err = runCIContract(t, root, map[string]any{"check": true}, "fmt")
	if err != nil || !strings.Contains(strings.Join(stdout, "\n"), "canonical form") {
		t.Fatalf("fmt --check after fmt: %v, stdout = %q", err, stdout)
	}
	if err := os.WriteFile(path, []byte(`{"version":3,"commands":["missing"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCIContract(t, root, map[string]any{}, "fmt"); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("fmt of an invalid document error = %v, want a usage error", err)
	}
}

func TestCIExplainNamesTheRuleAndWhatItPublishes(t *testing.T) {
	root := contractWorkspace(t)
	project := filepath.Join(root, "tools", "release")
	writeContractJSON(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "release"})
	writeTestExtension(t, project, "@acme/release", distribution.ProviderCommandName)
	writeContractJSON(t, filepath.Join(root, ciproto.Filename), map[string]any{
		"version":      3,
		"commands":     []string{"lint", "test"},
		"distribution": map[string]any{"namespace": "acme", "channels": map[string]any{"canary": map[string]any{}}},
		"envs":         map[string]any{"staging": map[string]any{"channel": "canary"}},
		"rules": []any{
			map[string]any{"pullRequests": true, "publish": false},
			map[string]any{"branches": "main", "publish": []string{"canary"}},
		},
	})
	stdout, err := runCIContract(t, root, map[string]any{"event": "push", "branch": "main"}, "explain")
	if err != nil {
		t.Fatalf("ci explain: %v", err)
	}
	text := strings.Join(stdout, "\n")
	for _, want := range []string{
		"Assumption: event=push branch=main",
		"Commands: putnami lint,test --impacted",
		"First match: rules[1]",
		"channel=canary followed by staging",
		"Channel lifetime: none declared",
		"Impact baseline: none declared",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("explain =\n%s\nwant %q", text, want)
		}
	}
	stdout, err = runCIContract(t, root, map[string]any{"event": "pull_request", "branch": "feature", "pr": "7"}, "explain")
	if err != nil || !strings.Contains(strings.Join(stdout, "\n"), "pr=7") {
		t.Fatalf("explain a pull request: %v, stdout = %q", err, stdout)
	}
}

func TestParseCIExplainInputRefusesFactsTheEventIgnores(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{}, "needs --event"},
		{map[string]any{"event": "push", "branch": "main", "pr": "x"}, "positive integer"},
		{map[string]any{"event": "push", "branch": "main", "pr": "3"}, "only with --event pull_request"},
		{map[string]any{"event": "tag"}, "needs --git-tag"},
		{map[string]any{"event": "tag", "git-tag": "v1", "branch": "main"}, "not evaluated for a tag"},
		{map[string]any{"event": "push", "git-tag": "v1"}, "only with --event tag"},
		{map[string]any{"event": "push"}, "needs --branch"},
	} {
		if _, err := parseCIExplainInput(tc.params); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("parseCIExplainInput(%v) error = %v, want %q", tc.params, err, tc.want)
		}
	}
	input, err := parseCIExplainInput(map[string]any{"event": "tag", "git-tag": "v1.2.0"})
	if err != nil || input.Tag != "v1.2.0" {
		t.Fatalf("tag input = %+v, %v", input, err)
	}
}

func TestLoadCIGraphFollowsTheFrameworkDiscovery(t *testing.T) {
	root := t.TempDir()
	writeContractJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"extensions": []string{"@acme/missing"}})
	if _, err := loadCIGraph(root); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "putnami install") {
		t.Fatalf("missing extension error = %v, want a usage error naming putnami install", err)
	}

	// A pinned extension that is not installed falls back to the workspace
	// project of the same name.
	writeContractJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"extensions": []string{"@acme/missing", "./local"}})
	project := filepath.Join(root, "tools", "missing")
	writeContractJSON(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "missing"})
	writeTestExtension(t, project, "@acme/missing", "lint")
	writeTestExtension(t, filepath.Join(root, "local"), "@acme/local", "test", distribution.ProviderCommandName)
	graph, err := loadCIGraph(root)
	if err != nil {
		t.Fatalf("loadCIGraph: %v", err)
	}
	if graph.known["lint"] == "" || graph.known["test"] == "" || graph.provider != "@acme/local" {
		t.Fatalf("graph = %+v, want lint and test, served by @acme/local", graph)
	}

	writeTestExtension(t, project, "@acme/missing", "lint", distribution.ProviderCommandName)
	if _, err := loadCIGraph(root); clicore.ExitCode(err) != clicore.ExitUsage || !strings.Contains(err.Error(), "only one may serve it") {
		t.Fatalf("two providers error = %v", err)
	}

	writeContractJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"extensions": "nope"})
	if _, err := workspaceExtensionRefs(root); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("invalid extensions error = %v", err)
	}
	if _, err := workspaceExtensionRefs(t.TempDir()); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("missing workspace manifest error = %v", err)
	}
	if got := encodeExtensionName("@putnami/cloud"); got != "putnami-cloud" {
		t.Fatalf("encodeExtensionName = %q", got)
	}
}

func TestCIExplainDescriptions(t *testing.T) {
	for retain, want := range map[string]string{
		"":                      "kept until someone removes it",
		ciproto.RetainWhileOpen: "retract the channel when the pull request closes",
		"1d":                    "1 day after its last move",
		"7d":                    "7 days after its last move",
	} {
		if got := describeCIChannelLifetime(retain); !strings.Contains(got, want) {
			t.Fatalf("describeCIChannelLifetime(%q) = %q, want %q", retain, got, want)
		}
	}
	if got := describeCIPublishBaseline("stable"); !strings.HasPrefix(got, "stable; read and never advanced") {
		t.Fatalf("describeCIPublishBaseline = %q", got)
	}
	if got := describeCIAssumption(ciproto.ExplainInput{Event: ciproto.EventTag, Tag: "v1"}); got != "event=tag tag=v1" {
		t.Fatalf("describeCIAssumption = %q", got)
	}
	if ciExplanationLines(nil) != nil {
		t.Fatal("no explanation must render no line")
	}
}
