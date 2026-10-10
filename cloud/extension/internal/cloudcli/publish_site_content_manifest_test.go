package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

func TestOrdinaryValidateSiteContentPlanHasOnlySourceChecks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	command := manifest["commands"].(map[string]any)["validate"].(map[string]any)
	if !reflect.DeepEqual(command["traits"], map[string]any{"injectPublishChannels": true}) {
		t.Fatalf("site selection must use project publication declarations: %v", command)
	}
	for _, field := range []string{"dependsOn", "sessionPrerequisites", "alsoRuns"} {
		if value, found := command[field]; found {
			t.Fatalf("validate must not schedule prerequisites: %s=%v", field, value)
		}
	}
	run := command["run"].([]any)
	if len(run) != 2 {
		t.Fatalf("unexpected validate steps: %v", run)
	}
	step := run[1].(map[string]any)
	if !reflect.DeepEqual(step, map[string]any{"id": "cloud-site-content", "if": "params.site-content", "task": "cloud-validate-site-content"}) {
		t.Fatalf("unrelated projects must not schedule site validation: %v", step)
	}
	task := manifest["tasks"].(map[string]any)["cloud-validate-site-content"].(map[string]any)
	if task["cache"] != false || task["cwd"] != "{projectRoot}" || !reflect.DeepEqual(task["args"], []any{"validate-site-content"}) {
		t.Fatalf("source validation must run fresh in the selected project: %v", task)
	}
	if !reflect.DeepEqual(task["inputs"], map[string]any{"declaration": map[string]any{"from": "project", "files": []any{"putnami.json"}}}) {
		t.Fatalf("uncached directory checks must not hash payload contents or require generated inputs: %v", task["inputs"])
	}
	for _, field := range []string{"writes", "outputs", "declares"} {
		if value, found := task[field]; found {
			t.Fatalf("source validation must not produce artifacts or effects: %s=%v", field, value)
		}
	}
}

func TestRunValidateSiteContentRejectsThenAcceptsCorrectedSources(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "docs", "example.test")
	if err := os.MkdirAll(filepath.Join(project, "guide"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(project, "putnami.json"), map[string]any{"name": "docs/example.test", "publish": []string{"site-content"}})
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": root, "params": map[string]any{"app": "docs/example.test", "json": true}})
	var output []string
	ioctx := IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(line string) { output = append(output, line) },
		Artifact: func(string, string, string, string, map[string]any) error {
			t.Fatal("source validation emitted an artifact")
			return nil
		},
	}
	args := []string{"validate-site-content", "--putnamiContext", contextFile}
	if code := RunMain(args, ioctx); code == 0 || !strings.Contains(strings.Join(output, "\n"), "has no files") {
		t.Fatalf("empty declared section: exit=%d output=%v", code, output)
	}
	page := filepath.Join(project, "guide", "index.md")
	if err := os.WriteFile(page, []byte("# Guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output = nil
	if code := RunMain(args, ioctx); code != 0 || !strings.Contains(strings.Join(output, "\n"), "validated") {
		t.Fatalf("corrected section: exit=%d output=%v", code, output)
	}
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 2 {
		t.Fatalf("validation mutated project: %v, %v", entries, err)
	}
	contents, err := os.ReadFile(page)
	if err != nil || string(contents) != "# Guide\n" {
		t.Fatalf("validation mutated source: %q, %v", contents, err)
	}
}

// TestExtensionManifestWiresSiteContentWithoutPublishDoc guards the
// @putnami/cloud manifest wiring for doc (site-content) bundles. The direct,
// workspace-once `publish-doc` publisher moved to the operator CLI, so the
// public manifest declares neither its verb, its command
// nor its task. A docs project's sections publish through its own package and
// publish steps as release-set members.
func TestExtensionManifestWiresSiteContentWithoutPublishDoc(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	subcommands := manifest["commandGroups"].(map[string]any)["cloud"].(map[string]any)["subcommands"].(map[string]any)
	if _, ok := subcommands["publish-doc"]; ok {
		t.Fatal("cloud publish-doc is still a public subcommand")
	}
	commands := manifest["commands"].(map[string]any)
	if _, ok := commands["cloud-publish-doc"]; ok {
		t.Fatal("manifest still declares the cloud-publish-doc command")
	}
	if _, ok := manifest["tasks"].(map[string]any)["cloud-publish-doc"]; ok {
		t.Fatal("manifest still declares the cloud-publish-doc task")
	}

	// `putnami publish` no longer runs the direct publisher. Doc bundles are
	// release-set members of their site project, so the native CI broker
	// authorizes them like any other planned member.
	publish := commands["publish"].(map[string]any)
	for _, raw := range publish["dependsOn"].([]any) {
		if raw == "!cloud-publish-doc" {
			t.Fatalf("publish dependsOn = %v, must not carry the direct doc barrier", publish["dependsOn"])
		}
	}
	if prerequisites, present := publish["sessionPrerequisites"]; present {
		t.Fatalf("publish sessionPrerequisites = %v, must not run the direct doc publisher", prerequisites)
	}

	// The site project's package step anchors the members' selection
	// fingerprint and its publish step writes them; both step ids are the
	// routes the workspace probe declares.
	requireStep := func(command, id, task, condition string) {
		t.Helper()
		for _, raw := range commands[command].(map[string]any)["run"].([]any) {
			step := raw.(map[string]any)
			if step["id"] != id {
				continue
			}
			if step["task"] != task || step["if"] != condition {
				t.Fatalf("%s step %s = %v, want task %s if %q", command, id, step, task, condition)
			}
			return
		}
		t.Fatalf("%s run has no %s step", command, id)
	}
	requireStep("package", cloudSiteContentPackageStep, "cloud-package-site-content", "params.site-content")
	// Without a package job the planner cannot compute the members' selection
	// fingerprint, so the package command must activate for a site project:
	// its section landing page is the activation file.
	activated := false
	for _, pattern := range commands["package"].(map[string]any)["activationFiles"].([]any) {
		if pattern == "*/index.md" {
			activated = true
		}
	}
	if !activated {
		t.Fatalf("package activationFiles = %v, want */index.md so a docs site project plans its package step", commands["package"].(map[string]any)["activationFiles"])
	}
	requireStep("publish", cloudSiteContentPublishStep, "cloud-publish-site-content-project",
		"params.site-content && !params.skip-publish-doc && !params.skipPublishDoc")

	tasks := manifest["tasks"].(map[string]any)
	for name, verb := range map[string]string{
		"cloud-package-site-content":         "package-site-content",
		"cloud-publish-site-content-project": "publish-site-content",
	} {
		task, ok := tasks[name].(map[string]any)
		if !ok {
			t.Fatalf("manifest missing %s task", name)
		}
		args := task["args"].([]any)
		if len(args) != 2 || args[0] != verb || args[1] != "--if-present" || task["cwd"] != "{projectRoot}" {
			t.Fatalf("%s = %v, want %s --if-present in the project root", name, task, verb)
		}
	}
}

// TestRunPublishDocNamesTheOperatorCommand proves the public CLI refuses
// publish-doc with a usage error that names its new home, before reading the
// docs tree.
func TestRunPublishDocNamesTheOperatorCommand(t *testing.T) {
	workspaceRoot := t.TempDir()
	contextFile := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, contextFile, map[string]any{
		"workspaceRoot": workspaceRoot,
		"params":        map[string]any{"json": true, "dry-run": true},
	})

	var output []string
	code := RunMain([]string{"publish-doc", "--putnamiContext", contextFile}, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { output = append(output, line) },
		Stderr: func(string) {},
	})
	if code != ExitUsage {
		t.Fatalf("code = %d, want %d; output = %v", code, ExitUsage, output)
	}
	assertContains(t, strings.Join(output, "\n"), "putnami operator publish-doc")
}
