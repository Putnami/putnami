package cloudcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	configcli "go.putnami.dev/cloud/extension/internal/configcli"
)

// These tests pin the cache key of cloud-validate-config-project, the
// only cached task in this manifest. A cache hit replays the verdict of an
// earlier run, so the key must name every file the check reads. The framework
// keys the task from its declared input ports, the project name and this
// extension's runtime digest, which hashes the extension's go.mod replace
// closure. Each test below fails when the check starts to read a file its
// ports do not select. The one exception is .gen/version.json: the CLI writes
// a canonical stamp for every planned project before it computes any key.

const validateConfigTaskName = "cloud-validate-config-project"

// versionStampRel is the CLI-written build stamp the check reads but does not key.
const versionStampRel = "apps/my-app/.gen/version.json"

// taskCacheSetting decodes a task "cache" value. The task contract accepts a
// bool or an {"enabled", "noOutput"} object.
type taskCacheSetting struct {
	Declared bool
	Object   bool
	Enabled  bool
	NoOutput bool
}

func (s *taskCacheSetting) UnmarshalJSON(data []byte) error {
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		*s = taskCacheSetting{Declared: true, Enabled: enabled}
		return nil
	}
	var object struct {
		Enabled  bool `json:"enabled"`
		NoOutput bool `json:"noOutput"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		return fmt.Errorf("cache must be a bool or {enabled, noOutput}: %w", err)
	}
	*s = taskCacheSetting{Declared: true, Object: true, Enabled: object.Enabled, NoOutput: object.NoOutput}
	return nil
}

type manifestInputPort struct {
	From  string   `json:"from"`
	Files []string `json:"files"`
}

type validateConfigTaskContract struct {
	Args     []string                     `json:"args"`
	CWD      string                       `json:"cwd"`
	Cache    taskCacheSetting             `json:"cache"`
	Declares json.RawMessage              `json:"declares"`
	Reads    []string                     `json:"reads"`
	Inputs   map[string]manifestInputPort `json:"inputs"`
}

func readCloudManifestTasks(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Tasks
}

func readValidateConfigTask(t *testing.T) validateConfigTaskContract {
	t.Helper()
	raw, ok := readCloudManifestTasks(t)[validateConfigTaskName]
	if !ok {
		t.Fatalf("manifest has no %s task", validateConfigTaskName)
	}
	var task validateConfigTaskContract
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

// TestValidateConfigProjectCacheContract pins the declared contract. The key
// set is closed so a new field such as writes, outputs or env cannot change
// what a hit replays without a reviewed test change.
func TestValidateConfigProjectCacheContract(t *testing.T) {
	raw := readCloudManifestTasks(t)[validateConfigTaskName]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if want := []string{"args", "cache", "command", "cwd", "declares", "description", "inputs", "kind", "reads", "timeoutMs"}; !slices.Equal(keys, want) {
		t.Fatalf("%s fields = %v, want %v", validateConfigTaskName, keys, want)
	}

	task := readValidateConfigTask(t)
	if task.Cache != (taskCacheSetting{Declared: true, Object: true, Enabled: true, NoOutput: true}) {
		t.Fatalf("cache = %+v, want {enabled: true, noOutput: true}", task.Cache)
	}
	// The framework caches a task only when it declares its effects. An empty
	// declaration states that the check has no network, process or registry
	// effect, and noOutput states that a hit has no files to restore.
	var declares map[string]json.RawMessage
	if err := json.Unmarshal(task.Declares, &declares); err != nil || declares == nil || len(declares) != 0 {
		t.Fatalf("declares = %s, want an empty object", task.Declares)
	}
	// The CLI computes every key before the session runs a task. Reading the
	// gen resource orders the check before this session's .gen writers, so it
	// reads the same .gen files its key hashed.
	if !slices.Equal(task.Reads, []string{"gen"}) {
		t.Fatalf("reads = %v, want [gen]", task.Reads)
	}
	if task.CWD != "{projectRoot}" || !slices.Equal(task.Args, []string{"validate-config-project"}) {
		t.Fatalf("cwd = %q args = %v", task.CWD, task.Args)
	}

	wantFrom := map[string]string{
		"manifest": "project", "schema": "project", "values": "project", "cloud-link": "workspace",
		// Every parameter the check reads. `putnami validate --app X` reaches each
		// project's step, so a key without it would store X's verdict under
		// every project's key.
		"app": "params", "appName": "params", "schema-from": "params", "schemaFrom": "params",
	}
	gotFrom := map[string]string{}
	for name, port := range task.Inputs {
		gotFrom[name] = port.From
	}
	if !reflect.DeepEqual(gotFrom, wantFrom) {
		t.Fatalf("input ports = %v, want %v", gotFrom, wantFrom)
	}

	// The hasher keeps every byte of a file that a glob selects. A file named
	// plainly is hashed as a view that drops any bare option block another
	// extension declares as its own, and options.publish is one of the blocks
	// where a project declares its Config namespace.
	manifest := task.Inputs["manifest"].Files
	if len(manifest) != 1 {
		t.Fatalf("manifest port = %v, want one pattern", manifest)
	}
	pattern := manifest[0]
	if last := pattern[strings.LastIndex(pattern, "/")+1:]; !strings.ContainsAny(last, "*?[") {
		t.Fatalf("manifest pattern %q must end in a glob so the hasher keeps options.publish", pattern)
	}
	for name, want := range map[string]bool{"putnami.json": true, "putnami.workspace.json": false, "putnami.jsonc": false} {
		if got, err := filepath.Match(pattern, name); err != nil || got != want {
			t.Fatalf("manifest pattern %q matches %s = %v (err %v), want %v", pattern, name, got, err, want)
		}
	}

	// The local override file never reaches the member, so a local edit must
	// not invalidate the key.
	for _, port := range []string{"values", "schema", "manifest"} {
		for _, file := range task.Inputs[port].Files {
			if strings.Contains(file, ".env.local") || strings.HasPrefix(file, "!") || strings.Contains(file, "**") {
				t.Fatalf("%s port pattern %q: project ports name exact files and never the local override", port, file)
			}
		}
	}
	link := task.Inputs["cloud-link"].Files
	for _, want := range []string{"putnami.workspace.json", "putnami.json", clicore.LinkFileRelative} {
		if !slices.Contains(link, want) {
			t.Fatalf("cloud-link port %v misses %s", link, want)
		}
	}
}

// TestValidateConfigProjectReadsOnlyKeyedFiles proves the key covers the read
// set. It builds a workspace, copies only the files the task's ports select
// plus the Git metadata and the CLI's version stamp into a second workspace, and requires the same outcome
// in both. Files the ports leave out, such as the .env.local.yaml overrides,
// carry values that would change the outcome if the check read them. Removing
// any selected file must change the outcome, so the fixture exercises every
// selected file.
func TestValidateConfigProjectReadsOnlyKeyedFiles(t *testing.T) {
	task := readValidateConfigTask(t)
	for _, variant := range []struct {
		name            string
		link            string
		generatedSchema bool
	}{
		{name: "link in putnami.workspace.json", link: "putnami.workspace.json"},
		{name: "link in the root putnami.json", link: "putnami.json"},
		{name: "cached link and generated schema", link: clicore.LinkFileRelative, generatedSchema: true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			full := validateConfigKeyFixture(t, variant.link, variant.generatedSchema)
			baseline := validateConfigOutcomeOf(t, full)
			if !strings.Contains(baseline, "Validated authored Config workspace-native/my-app-config@1.2.3 for prod.") {
				t.Fatalf("fixture does not validate:\n%s", baseline)
			}

			keyed := keyedValidateConfigFiles(t, task, full, "apps/my-app")
			pruned := t.TempDir()
			for _, rel := range keyed {
				copyFixtureFile(t, filepath.Join(full, rel), filepath.Join(pruned, rel))
			}
			// The CLI, not the checkout, writes the version stamp before any
			// key is computed, so it is present in every run.
			copyFixtureFile(t, filepath.Join(full, versionStampRel), filepath.Join(pruned, versionStampRel))
			if err := os.CopyFS(filepath.Join(pruned, ".git"), os.DirFS(filepath.Join(full, ".git"))); err != nil {
				t.Fatal(err)
			}
			if got := validateConfigOutcomeOf(t, pruned); got != baseline {
				t.Fatalf("the check reads a file its ports do not select (keyed: %v)\nfull tree:\n%s\nkeyed files only:\n%s", keyed, baseline, got)
			}

			for _, rel := range keyed {
				path := filepath.Join(pruned, rel)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if got := validateConfigOutcomeOf(t, pruned); got == baseline {
					t.Errorf("removing %s does not change the outcome, so the fixture does not exercise it", rel)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// keyedValidateConfigFiles expands the task's file ports the way the cache
// hasher does for patterns without "**": filepath.Glob relative to the project
// or the workspace root. It returns workspace-relative paths.
func keyedValidateConfigFiles(t *testing.T, task validateConfigTaskContract, workspaceRoot, projectPath string) []string {
	t.Helper()
	var files []string
	for _, port := range task.Inputs {
		base := workspaceRoot
		if port.From == "project" {
			base = filepath.Join(workspaceRoot, filepath.FromSlash(projectPath))
		}
		for _, pattern := range port.Files {
			matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range matches {
				if info, err := os.Stat(match); err != nil || info.IsDir() {
					continue
				}
				rel, err := filepath.Rel(workspaceRoot, match)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, filepath.ToSlash(rel))
			}
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// validateConfigOutcomeOf runs the task's command and the member preparation
// it performs. The result holds every observable: the error, the printed
// verdict, the canonical member bytes and the files the member came from.
func validateConfigOutcomeOf(t *testing.T, root string) string {
	t.Helper()
	var outcome strings.Builder
	var stdout []string
	// The task runs from {projectRoot}.
	t.Chdir(filepath.Join(root, "apps", "my-app"))
	err := validateConfigProject(map[string]any{"app": "my-app"}, nil, root, IO{Stdout: func(line string) { stdout = append(stdout, line) }})
	fmt.Fprintf(&outcome, "validate error: %v\nvalidate stdout: %s\n", err, strings.Join(stdout, "\n"))
	prepared, err := configcli.PrepareAuthoredConfig(map[string]any{"app": "my-app", "env": "prod"}, nil, root, IO{})
	fmt.Fprintf(&outcome, "prepare error: %v\n", err)
	if prepared != nil {
		fmt.Fprintf(&outcome, "schema: %s\nvalues: %s\nmember: %s\n", prepared.SchemaPath, strings.Join(prepared.ValuesPaths, ", "), prepared.Member.Bytes)
	}
	return strings.ReplaceAll(outcome.String(), root, "<root>")
}

// validateConfigKeyFixture writes a nested project in a Git workspace. Each
// production values file supplies one field, so every file the check reads
// changes its outcome. The decoys hold values the check must never read.
func validateConfigKeyFixture(t *testing.T, link string, generatedSchema bool) string {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "apps", "my-app")
	linkOptions := map[string]any{"@putnami/cloud": map[string]any{"workspace": map[string]any{
		"workspace_id": "ws-acme", "control_plane_url": "https://control.test",
	}}}
	switch link {
	case "putnami.workspace.json":
		writeFixtureJSON(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{"name": "acme", "options": linkOptions})
	case "putnami.json":
		writeFixtureJSON(t, filepath.Join(root, "putnami.json"), map[string]any{"name": "acme", "options": linkOptions})
	case clicore.LinkFileRelative:
		writeFixtureJSON(t, filepath.Join(root, clicore.LinkFileRelative), map[string]any{
			"version": 1, "workspace_id": "ws-acme", "environment": "prod", "control_plane_url": "https://control.test",
		})
	default:
		t.Fatalf("unknown link location %q", link)
	}
	writeFixtureJSON(t, filepath.Join(project, "putnami.json"), map[string]any{
		"name":    "my-app",
		"options": map[string]any{"publish": map[string]any{"namespace": "workspace-native"}},
	})
	schemaPath := filepath.Join(project, "schema", "config.json")
	if generatedSchema {
		schemaPath = filepath.Join(project, ".gen", "config-schema.json")
	}
	writeFixtureJSON(t, schemaPath, map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "server", "fields": []any{
			map[string]any{"name": "port", "type": "int", "required": true},
			map[string]any{"name": "host", "type": "string", "required": true},
			map[string]any{"name": "region", "type": "string", "required": true},
			map[string]any{"name": "zone", "type": "string", "required": true},
			map[string]any{"name": "tier", "type": "string", "required": true},
			map[string]any{"name": "provider", "type": "object"},
		}}},
	})
	writeFixtureFile(t, filepath.Join(project, "schema", "config-authored-fields.json"),
		`{"fields":[{"path":"server.provider.Issuer","type":"string"}]}`)
	for rel, content := range map[string]string{
		"conf/env.yaml":             "server:\n  port: 8080\n",
		"conf/.env.yaml":            "server:\n  host: api.example\n",
		"conf/env.prod.yaml":        "server:\n  region: eu\n",
		"conf/.env.prod.yaml":       "server:\n  zone: eu-a\n",
		".gen/conf/env.prod.yaml":   "server:\n  tier: gold\n",
		".gen/conf/.env.prod.yaml":  "server:\n  provider:\n    Issuer: https://issuer.example\n",
		"conf/.env.local.yaml":      "server:\n  port: 1\n  region: local-override\n",
		".gen/conf/.env.local.yaml": "server:\n  tier: local-override\n",
		"conf/env.dev.yaml":         "server:\n  zone: dev-override\n",
	} {
		writeFixtureFile(t, filepath.Join(project, filepath.FromSlash(rel)), content)
	}
	writeFixtureJSON(t, filepath.Join(project, ".gen", "version.json"), map[string]any{"version": "1.2.3"})
	isolateGit(t)
	gitCommand(t, root, "init", "-q")
	gitCommand(t, root, "config", "user.email", "test@example.com")
	gitCommand(t, root, "config", "user.name", "Test")
	gitCommand(t, root, "config", "commit.gpgsign", "false")
	gitCommand(t, root, "remote", "add", "origin", "git@github.com:acme/storefront.git")
	gitCommand(t, root, "add", "-A")
	gitCommand(t, root, "commit", "-qm", "fixture")
	return root
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, string(data))
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyFixtureFile(t *testing.T, from, to string) {
	t.Helper()
	source, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
}

// TestOnlyProjectConfigValidationIsCached keeps every other task uncached and
// requires each pipeline task to state why in its description.
func TestOnlyProjectConfigValidationIsCached(t *testing.T) {
	tasks := readCloudManifestTasks(t)
	for name, raw := range tasks {
		if name == validateConfigTaskName {
			continue
		}
		var task struct {
			Cache taskCacheSetting `json:"cache"`
		}
		if err := json.Unmarshal(raw, &task); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if task.Cache != (taskCacheSetting{Declared: true}) {
			t.Errorf("%s cache = %+v, want an explicit false", name, task.Cache)
		}
	}
	for _, name := range []string{
		"cloud-validate-site-content", "cloud-package-config-member", "cloud-package-site-content",
		"cloud-image-layers-check", "cloud-image-layers", "cloud-shell-test",
	} {
		var task struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(tasks[name], &task); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(task.Description, "Uncached") {
			t.Errorf("%s description does not say why it is uncached: %q", name, task.Description)
		}
	}
}

// TestValidateConfigProjectRefusesInputsOutsideItsKey covers the overrides
// that would point the cached check at files its key never hashed: a schema
// path and another project. Each one must fail rather than store a verdict.
func TestValidateConfigProjectRefusesInputsOutsideItsKey(t *testing.T) {
	root := validateConfigKeyFixture(t, "putnami.workspace.json", false)
	writeProjectManifest(t, filepath.Join(root, "apps", "other"), "other")
	projectDir := filepath.Join(root, "apps", "my-app")
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{name: "schema-from flag", params: map[string]any{"app": "my-app", "schema-from": "decoy.json"}, want: "does not accept --schema-from"},
		{name: "schemaFrom option", params: map[string]any{"app": "my-app", "schemaFrom": "decoy.json"}, want: "does not accept --schema-from"},
		{name: "another project", params: map[string]any{"app": "other"}, want: "checks only the project in its working directory"},
		{name: "another project by appName", params: map[string]any{"appName": "other"}, want: "checks only the project in its working directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(projectDir)
			var stdout []string
			err := validateConfigProject(tc.params, nil, root, IO{Stdout: func(line string) { stdout = append(stdout, line) }})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(stdout) != 0 {
				t.Fatalf("a refused check printed a verdict: %v", stdout)
			}
		})
	}
}

func writeProjectManifest(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "putnami.json"), map[string]any{"name": name})
}

// TestValidateConfigProjectAcceptsAnAliasedWorkingDirectory covers a task
// whose working directory reaches the project through a symlinked workspace
// root, as a Conductor branch alias does.
func TestValidateConfigProjectAcceptsAnAliasedWorkingDirectory(t *testing.T) {
	root := validateConfigKeyFixture(t, "putnami.workspace.json", false)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(alias, "apps", "my-app"))
	if err := validateConfigProject(map[string]any{"app": "my-app"}, nil, alias, IO{Stdout: func(string) {}}); err != nil {
		t.Fatalf("aliased working directory refused: %v", err)
	}
}

// TestValidateConfigProjectIgnoresSameNameCopies covers a copy of the project
// that a workspace walk reaches before the project itself (".copy" sorts
// before "apps"). The key never hashes the copy, so the outcome must not
// change: the check resolves its project from its working directory.
func TestValidateConfigProjectIgnoresSameNameCopies(t *testing.T) {
	root := validateConfigKeyFixture(t, "putnami.workspace.json", false)
	baseline := validateConfigOutcomeOf(t, root)
	copyDir := filepath.Join(root, ".copy", "apps", "my-app")
	writeProjectManifest(t, copyDir, "my-app")
	if err := os.MkdirAll(filepath.Join(copyDir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "schema", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := validateConfigOutcomeOf(t, root); got != baseline {
		t.Fatalf("a same-name copy changed the outcome\nwithout copy:\n%s\nwith copy:\n%s", baseline, got)
	}
}
