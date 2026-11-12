package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// These tests pin how a declared generated site-content section reaches the
// plan: build runs cloud-site-content-generate for every site project, the
// step is cached on putnami.json and on the generator inputs the project
// lists, and validate and package plan build first so the section exists when
// they read it.

const siteContentGenerateTaskName = "cloud-site-content-generate"

func readCloudManifestCommands(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commands map[string]json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Commands
}

// TestSiteContentGenerateTaskContract pins the generate task. Its field set is
// closed: a new field such as env or an effect changes what a hit replays.
func TestSiteContentGenerateTaskContract(t *testing.T) {
	raw, ok := readCloudManifestTasks(t)[siteContentGenerateTaskName]
	if !ok {
		t.Fatalf("manifest has no %s task", siteContentGenerateTaskName)
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
	if want := []string{"args", "cache", "command", "cwd", "declares", "description", "inputs", "kind", "timeoutMs", "writes"}; !slices.Equal(keys, want) {
		t.Fatalf("%s fields = %v, want %v", siteContentGenerateTaskName, keys, want)
	}

	var task struct {
		Command string                       `json:"command"`
		Args    []string                     `json:"args"`
		CWD     string                       `json:"cwd"`
		Cache   map[string]bool              `json:"cache"`
		Writes  []string                     `json:"writes"`
		Inputs  map[string]manifestInputPort `json:"inputs"`
		// Declares is decoded without the free-text description.
		Declares struct {
			Outputs map[string]struct {
				Kind          string `json:"kind"`
				Root          string `json:"root"`
				Path          string `json:"path"`
				OptionalEmpty bool   `json:"optionalEmpty"`
				Emptied       bool   `json:"emptied"`
			} `json:"outputs"`
			Effects []string `json:"effects"`
		} `json:"declares"`
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.Command != "{extensionRuntime}" || task.CWD != "{projectRoot}" || !slices.Equal(task.Args, []string{"generate-site-content"}) {
		t.Fatalf("command = %q cwd = %q args = %v", task.Command, task.CWD, task.Args)
	}
	// Cached and deterministic: the section is a function of the key.
	if want := map[string]bool{"enabled": true, "deterministic": true}; !reflect.DeepEqual(task.Cache, want) {
		t.Fatalf("cache = %v, want %v", task.Cache, want)
	}
	// The commands live in putnami.json. The generator's own inputs reach the
	// key through options["@putnami/cloud:build"].filePatterns, which the
	// engine adds to every key of the extension's build command.
	if want := map[string]manifestInputPort{"manifest": {From: "project", Files: []string{"putnami.jso[n]"}}}; !reflect.DeepEqual(task.Inputs, want) {
		t.Fatalf("inputs = %v, want %v", task.Inputs, want)
	}
	if !slices.Equal(task.Writes, []string{"gen"}) {
		t.Fatalf("writes = %v, want [gen]", task.Writes)
	}
	output, declared := task.Declares.Outputs["site-content"]
	if len(task.Declares.Outputs) != 1 || !declared || output.Kind != "directory" || output.Root != "project" ||
		output.Path != ".gen/site-content" || !output.OptionalEmpty || !output.Emptied {
		t.Fatalf("declared outputs = %+v, want the emptied optional directory .gen/site-content", task.Declares.Outputs)
	}
	// An effect would turn the cache off; the extension sends no request.
	if len(task.Declares.Effects) != 0 {
		t.Fatalf("effects = %v, want none", task.Declares.Effects)
	}

	tasks := readCloudManifestTasks(t)
	for _, name := range []string{"cloud-package-site-content", "cloud-publish-site-content-project"} {
		var reader struct {
			Reads []string `json:"reads"`
		}
		if err := json.Unmarshal(tasks[name], &reader); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(reader.Reads, []string{"gen"}) {
			t.Fatalf("%s reads = %v, want [gen]: it reads .gen/site-content", name, reader.Reads)
		}
	}
}

type commandStepContract struct {
	ID         string `json:"id"`
	If         string `json:"if"`
	Task       string `json:"task"`
	Activation *struct {
		Files []string `json:"files"`
	} `json:"activation"`
}

type commandContract struct {
	Activation           string            `json:"activation"`
	ActivationFiles      []string          `json:"activationFiles"`
	Traits               map[string]any    `json:"traits"`
	SessionPrerequisites []json.RawMessage `json:"sessionPrerequisites"`
	Run                  []commandStepContract
}

// TestSiteContentGenerateRunsBeforeTheStepsThatReadIt pins the build step and
// the prerequisites. A site project names no extension, so build activates
// for every project and each step keeps its own condition. The build step
// reads params.site-content, which injectPublishChannels sets exactly as it
// does for validate and package. Each prerequisite is its own entry: an
// unresolved operand makes a whole `||` expression false.
func TestSiteContentGenerateRunsBeforeTheStepsThatReadIt(t *testing.T) {
	commands := readCloudManifestCommands(t)
	decode := func(name string) commandContract {
		t.Helper()
		var command commandContract
		if err := json.Unmarshal(commands[name], &command); err != nil {
			t.Fatal(err)
		}
		return command
	}

	build := decode("build")
	if build.Activation != "workspace" || !slices.Contains(build.ActivationFiles, "putnami.json") ||
		build.Traits["injectPublishChannels"] != true || build.Traits["kind"] != "files" {
		t.Fatalf("build = activation %q, files %v, traits %v", build.Activation, build.ActivationFiles, build.Traits)
	}
	steps := map[string]commandStepContract{}
	for _, step := range build.Run {
		steps[step.ID] = step
	}
	if step := steps["cloud-image-layers"]; step.Activation == nil || !slices.Equal(step.Activation.Files, []string{"image-layers.json"}) {
		t.Fatalf("cloud-image-layers step = %+v, want it gated on image-layers.json", step)
	}
	if step := steps["cloud-site-content-generate"]; step.If != "params.site-content" || step.Task != siteContentGenerateTaskName || step.Activation != nil {
		t.Fatalf("cloud-site-content-generate step = %+v", step)
	}
	if len(steps) != 2 {
		t.Fatalf("build steps = %v, want cloud-image-layers and cloud-site-content-generate", steps)
	}

	want := json.RawMessage(`{"command":"build","if":"params.site-content"}`)
	for _, name := range []string{"validate", "package"} {
		found := false
		for _, prerequisite := range decode(name).SessionPrerequisites {
			var got, expected any
			if err := json.Unmarshal(prerequisite, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want, &expected); err != nil {
				t.Fatal(err)
			}
			found = found || reflect.DeepEqual(got, expected)
		}
		if !found {
			t.Fatalf("%s session prerequisites = %s, want %s", name, decode(name).SessionPrerequisites, want)
		}
	}
}
