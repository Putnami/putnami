package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
)

// These tests pin the cache key of cloud-image-layers-check. The task keys on
// image-layers.json, on the produced layers and their records, and on the
// workspace files that derive a layer's version. The deliverycli test
// TestImageLayersCheckReadsOnlyKeyedFiles proves the check reads no other
// file.

const (
	imageLayersTaskName      = "cloud-image-layers"
	imageLayersCheckTaskName = "cloud-image-layers-check"
)

// TestImageLayersCheckCacheContract pins the declared contract. The field set
// is closed so a new field such as writes, outputs or env cannot change what a
// hit replays without a reviewed test change.
func TestImageLayersCheckCacheContract(t *testing.T) {
	raw, ok := readCloudManifestTasks(t)[imageLayersCheckTaskName]
	if !ok {
		t.Fatalf("manifest has no %s task", imageLayersCheckTaskName)
	}
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
		t.Fatalf("%s fields = %v, want %v", imageLayersCheckTaskName, keys, want)
	}

	var task validateConfigTaskContract
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.Cache != (taskCacheSetting{Declared: true, Object: true, Enabled: true, NoOutput: true}) {
		t.Fatalf("cache = %+v, want {enabled: true, noOutput: true}", task.Cache)
	}
	var declares map[string]json.RawMessage
	if err := json.Unmarshal(task.Declares, &declares); err != nil || declares == nil || len(declares) != 0 {
		t.Fatalf("declares = %s, want an empty object", task.Declares)
	}
	// Reading the gen resource keeps a task that rewrites the project's .gen
	// tree in the same plan from running beside the check.
	if !slices.Equal(task.Reads, []string{"gen"}) {
		t.Fatalf("reads = %v, want [gen]", task.Reads)
	}
	// The --project argument names the project the key hashes. A task argument
	// overrides a parameter of the same name, and --project wins over every
	// other way to select the project, so no parameter needs a key.
	if task.CWD != "{projectRoot}" || !slices.Equal(task.Args, []string{"image-layers", "--check", "--project", "{projectRoot}"}) {
		t.Fatalf("cwd = %q args = %v", task.CWD, task.Args)
	}
	want := map[string]manifestInputPort{
		"manifest": {From: "project", Files: []string{"image-layers.json"}},
		// A layer is <name>.tar or bin/<name>, and its record is <name>.json. A
		// glob keeps every byte of a record: the record of a layer named putnami
		// is putnami.json, which a literal pattern hashes as a project config.
		"layers": {From: "project", Files: []string{".gen/layers/*.json", ".gen/layers/*.tar", ".gen/layers/bin/*"}},
		// The files a layer's version derives from.
		"pins": {From: "workspace", Files: []string{
			"go.work", "package.json", "putnami.lock.json",
			".putnami/bin/extensions/putnami-go/tools/versions.json",
		}},
	}
	if !reflect.DeepEqual(task.Inputs, want) {
		t.Fatalf("input ports = %v, want %v", task.Inputs, want)
	}
}

// TestImageLayersProducerDeclaresTheLayersTheCheckKeys pins the producer side:
// it stays uncached, and it declares the directory that holds every produced
// file the check keys, so no other task of a plan may write there.
func TestImageLayersProducerDeclaresTheLayersTheCheckKeys(t *testing.T) {
	tasks := readCloudManifestTasks(t)
	var producer struct {
		Cache    taskCacheSetting `json:"cache"`
		Declares struct {
			Outputs map[string]map[string]any `json:"outputs"`
			Effects []string                  `json:"effects"`
		} `json:"declares"`
	}
	if err := json.Unmarshal(tasks[imageLayersTaskName], &producer); err != nil {
		t.Fatal(err)
	}
	if producer.Cache != (taskCacheSetting{Declared: true}) {
		t.Fatalf("cache = %+v, want an explicit false", producer.Cache)
	}
	if !slices.Equal(producer.Declares.Effects, []string{"network"}) {
		t.Fatalf("effects = %v, want [network]", producer.Declares.Effects)
	}
	layers := producer.Declares.Outputs["layers"]
	if len(producer.Declares.Outputs) != 1 || layers["kind"] != "directory" || layers["root"] != "project" || layers["path"] != ".gen/layers" {
		t.Fatalf("outputs = %v, want the project directory .gen/layers alone", producer.Declares.Outputs)
	}

	var check validateConfigTaskContract
	if err := json.Unmarshal(tasks[imageLayersCheckTaskName], &check); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range check.Inputs["layers"].Files {
		if !strings.HasPrefix(pattern, ".gen/layers/") {
			t.Errorf("the check keys %s, which the producer does not own", pattern)
		}
	}
}

// TestRunImageLayersCheckKeepsToItsOwnProject runs the check as the task does:
// with the task's arguments, and with parameters that name another project.
// The check must verify the project of its --project argument, because that is
// the project its key hashes.
func TestRunImageLayersCheckKeepsToItsOwnProject(t *testing.T) {
	root := t.TempDir()
	const manifest = `{"layers":[{"name":"workspace","producer":"dirs","path":"/workspace"}]}`
	produced, unproduced := filepath.Join(root, "images", "produced"), filepath.Join(root, "images", "unproduced")
	for dir, name := range map[string]string{produced: "images/produced", unproduced: "images/unproduced"} {
		writeProjectManifest(t, dir, name)
		writeFixtureFile(t, filepath.Join(dir, "image-layers.json"), manifest)
	}

	run := func(t *testing.T, params map[string]any, args ...string) (int, string) {
		t.Helper()
		contextFile := filepath.Join(t.TempDir(), "context.json")
		writeJSONFile(t, contextFile, map[string]any{"workspaceRoot": root, "params": params})
		var output []string
		code := RunMain(append(args, "--putnamiContext", contextFile), IO{
			Env:    hometest.Env(t.TempDir(), nil),
			Stdout: func(line string) { output = append(output, line) },
			Stderr: func(line string) { output = append(output, line) },
		})
		return code, strings.Join(output, "\n")
	}
	if code, output := run(t, map[string]any{}, "image-layers", "--project", produced); code != 0 {
		t.Fatalf("produce the fixture layers: exit=%d output=%s", code, output)
	}
	if _, err := os.Stat(filepath.Join(unproduced, ".gen")); !os.IsNotExist(err) {
		t.Fatalf("the producer wrote outside its project: %v", err)
	}

	taskArgs := readImageLayersCheckArgs(t)
	checkArgs := func(project string) []string {
		args := slices.Clone(taskArgs)
		for i, arg := range args {
			args[i] = strings.ReplaceAll(arg, "{projectRoot}", project)
		}
		return args
	}
	for _, name := range []string{"project", "context", "app", "appName"} {
		t.Run(name, func(t *testing.T) {
			other := func(dir, project string) map[string]any {
				if name == "app" || name == "appName" {
					return map[string]any{name: project}
				}
				return map[string]any{name: dir}
			}
			t.Chdir(produced)
			code, output := run(t, other(unproduced, "images/unproduced"), checkArgs(produced)...)
			if code != 0 || !strings.Contains(output, filepath.Join(produced, ".gen", "layers")) {
				t.Fatalf("a parameter moved the check off its project: exit=%d output=%s", code, output)
			}
			t.Chdir(unproduced)
			code, output = run(t, other(produced, "images/produced"), checkArgs(unproduced)...)
			if code == 0 || !strings.Contains(output, "no layer has been produced") {
				t.Fatalf("a parameter moved the check onto another project's layers: exit=%d output=%s", code, output)
			}
		})
	}
}

func readImageLayersCheckArgs(t *testing.T) []string {
	t.Helper()
	var task validateConfigTaskContract
	if err := json.Unmarshal(readCloudManifestTasks(t)[imageLayersCheckTaskName], &task); err != nil {
		t.Fatal(err)
	}
	return task.Args
}
