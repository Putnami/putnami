package jobs

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A run computes every cache key before any task starts, and each task
// computes its key again when it starts. A key that reads a file inside a
// literal declared output of another task reads bytes that task can write in
// between, so the two values differ. When the reader's key folds the writer's
// key, those bytes are already fixed by the folded key and reading them only
// moves the key (issue #270). This guard reads every extension manifest of the
// repository and fails on each key pattern that names such an output and
// selects a file inside it, unless the allowlist says why the key must read
// those bytes. A pattern names an output
// when its leading segments spell, literally, the output's path for a
// directory output, or the directory that holds a file output.
//
// The guard reads a key the way the key code reads it:
//   - keyFilePatterns assembles the project-rooted patterns: the task's
//     `from: project` and `from: closure` input files when it declares inputs,
//     otherwise its cache.key files and closureFiles. A closure pattern is read
//     in every member of the closure, so it is project-rooted too.
//   - The CLI's own collector, store.CollectKeyFiles, decides what an ordinary
//     pattern and its "!" exclusions select, on a project tree that holds one
//     witness file for each place the pattern could reach inside each output
//     it names.
//   - A "git:" pattern selects the Git candidates its path grammar matches,
//     through the same wsproto calls the Git collector makes. Candidate
//     membership depends on the repository's index and ignore rules, which no
//     manifest fixes, so the guard counts every output path as a candidate.
//
// It does not read these forms, which a manifest alone does not resolve:
//   - a "go-embed:" selector, which resolves from Go sources and their embed
//     directives;
//   - a project's options filePatterns and the filePatterns flag, which no
//     manifest declares;
//   - the part of a "../" pattern that reaches another project's tree;
//   - a pattern that reaches an output only through a wildcard from outside
//     it ("**/*.json", "git:**"), and an empty pattern set, which reads every
//     non-hidden project file: whether the writer puts a file of that name
//     there is not in the manifest, which names no file of a directory output.
//
// An output counts when it is durable, has a literal path and resolves under
// the project root. A pathFrom output is unknown until its task runs, and an
// invocation-scoped, command-output or workspace-rooted output is not in the
// project tree. A task's own outputs are not a conflict for it, and a path an
// output cedes (excludes) is not inside that output.

// keyOutputConflict is one cache key pattern that selects a file inside a
// literal declared output of another task.
type keyOutputConflict struct {
	reader  string // "<extension> <task>" whose key reads the file
	pattern string // the reader's key pattern that selects it
	writer  string // "<extension> <task>" that declares the output
	output  string // the output's project-relative path
}

// keyOutputAllowlist holds each conflict the current manifests carry on
// purpose, with the reason: the run that precomputes the key never writes the
// bytes, or the reader's key folds no key that fixes them, so it must read
// them and the plan marks it movable (planKeyMovement). An entry that no
// longer matches a conflict fails the guard, so the list only shrinks.
var keyOutputAllowlist = map[keyOutputConflict]string{
	{"@putnami/clientgen clientgen-go", "schema/openapi.json", "@putnami/typescript build-generate", "schema/openapi.json"}:                 "build-generate polices this committed contract with drift fail: a run that succeeds rewrites it unchanged, and a run that changes it fails build-generate.",
	{"@putnami/clientgen clientgen-ts", "schema/openapi.json", "@putnami/typescript build-generate", "schema/openapi.json"}:                 "build-generate polices this committed contract with drift fail: a run that succeeds rewrites it unchanged, and a run that changes it fails build-generate.",
	{"@putnami/cloud cloud-validate-config-project", ".gen/conf/.env.prod.yaml", "@putnami/typescript build-generate", ".gen"}:              "No TypeScript task writes .gen/conf: the merged env files come from the Go config-merge, to which the Go build-generate cedes .gen/conf.",
	{"@putnami/cloud cloud-validate-config-project", ".gen/conf/env.prod.yaml", "@putnami/typescript build-generate", ".gen"}:               "No TypeScript task writes .gen/conf: the merged env files come from the Go config-merge, to which the Go build-generate cedes .gen/conf.",
	{"@putnami/cloud cloud-validate-config-project", "schema/config.json", "@putnami/go config-extract-exec", "schema/config.json"}:         "Only the config-extract command runs config-extract-exec, and no other command runs it or depends on it.",
	{"@putnami/cloud cloud-validate-config-project", "schema/config.json", "@putnami/typescript config-extract-exec", "schema/config.json"}: "Only the config-extract command runs config-extract-exec, and no other command runs it or depends on it.",
	{"@putnami/typescript config-extract-exec", "schema/config.json", "@putnami/go config-extract-exec", "schema/config.json"}:              "The TypeScript task reads its own output; the Go task declares the same path for a Go project, and only the config-extract command runs either.",
	{"@putnami/cloud cloud-image-layers-check", ".gen/layers/*.json", "@putnami/cloud cloud-image-layers", ".gen/layers"}:                   "cloud-image-layers caches nothing, so no key fixes the layers it writes: the check validates those bytes and its key must read them.",
	{"@putnami/cloud cloud-image-layers-check", ".gen/layers/*.tar", "@putnami/cloud cloud-image-layers", ".gen/layers"}:                    "cloud-image-layers caches nothing, so no key fixes the layers it writes: the check validates those bytes and its key must read them.",
	{"@putnami/cloud cloud-image-layers-check", ".gen/layers/bin/*", "@putnami/cloud cloud-image-layers", ".gen/layers"}:                    "cloud-image-layers caches nothing, so no key fixes the layers it writes: the check validates those bytes and its key must read them.",
	{"@putnami/cloud cloud-validate-config-project", ".gen/config-schema.json", "@putnami/go build-generate", ".gen"}:                       "The cloud validate command does not depend on build, so this key folds no build-generate key: it must read the config-extract fallback it validates.",
}

func TestNoCacheKeyReadsAnotherTasksLiteralOutput(t *testing.T) {
	tasks := repositoryTasks(t)
	outputs := literalProjectOutputs(t, tasks)
	if len(outputs) == 0 {
		t.Fatal("no task declares a literal project output; the guard would be vacuously green")
	}

	found := make(map[keyOutputConflict]bool)
	keyed := 0
	for _, task := range tasks {
		if !task.keyed {
			continue
		}
		keyed++
		for _, conflict := range keyOutputConflicts(t, task.id, task.patterns, outputs) {
			found[conflict] = true
		}
	}
	if keyed == 0 {
		t.Fatal("no task has a cache key; the guard would be vacuously green")
	}

	for _, conflict := range sortedKeyOutputConflicts(found) {
		if reason := keyOutputAllowlist[conflict]; reason == "" {
			t.Errorf("%s: key pattern %q reads inside output %s, which %s can write during the run; exclude the output from the key, or allowlist {%q, %q, %q, %q} with the reason the key must read those bytes",
				conflict.reader, conflict.pattern, conflict.output, conflict.writer,
				conflict.reader, conflict.pattern, conflict.writer, conflict.output)
		}
	}
	listed := make(map[keyOutputConflict]bool, len(keyOutputAllowlist))
	for conflict := range keyOutputAllowlist {
		listed[conflict] = true
	}
	for _, conflict := range sortedKeyOutputConflicts(listed) {
		if !found[conflict] {
			t.Errorf("stale entry: %s no longer reads %s of %s through %q; delete it", conflict.reader, conflict.output, conflict.writer, conflict.pattern)
		}
	}
}

// TestKeyOutputGuardReadsKeysTheWayTheKeyCodeDoes pins the guard's own reading
// on small declarations, so a green guard means what it says.
func TestKeyOutputGuardReadsKeysTheWayTheKeyCodeDoes(t *testing.T) {
	describeSchema := literalOutput{writer: "go build-describe", path: ".gen/schema", directory: true}
	generate := literalOutput{writer: "go build-generate", path: ".gen", directory: true, excludes: []string{".gen/schema"}}
	configSchema := literalOutput{writer: "go config-extract-exec", path: "schema/config.json"}
	dist := literalOutput{writer: "ts build", path: "dist", directory: true}

	for _, tc := range []struct {
		name     string
		reader   string
		patterns []string
		output   literalOutput
		want     string
	}{
		{"a literal file inside the output", "clientgen", []string{"go.mod", ".gen/schema/openapi.json"}, describeSchema, ".gen/schema/openapi.json"},
		{"a literal tree glob inside the output", "check", []string{".gen/schema/*.json"}, describeSchema, ".gen/schema/*.json"},
		{"a glob beside a literal file output", "validate", []string{"schema/*.json"}, configSchema, "schema/*.json"},
		{"a glob that selects no file of the named output", "validate", []string{"schema/*.yaml"}, configSchema, ""},
		{"a recursive glob reaches the output only through a wildcard", "lint", []string{"**/*.go"}, describeSchema, ""},
		{"a wildcard segment reaches the output only through a wildcard", "lint", []string{".gen/*/openapi.json"}, describeSchema, ""},
		{"an exclusion removes the output", "compile", []string{".gen/schema/x.go", "!**/.gen/**"}, describeSchema, ""},
		{"a pattern beside the output", "clientgen", []string{"schema/openapi.json"}, describeSchema, ""},
		{"a ceded subpath is not inside the output", "clientgen", []string{".gen/schema/openapi.json"}, generate, ""},
		{"the rest of a ceding output still counts", "lint", []string{".gen/**/*.ts"}, generate, ".gen/**/*.ts"},
		{"a task's own output is no conflict", "go build-describe", []string{".gen/schema/openapi.json"}, describeSchema, ""},
		{"no pattern is not read", "package", nil, configSchema, ""},
		{"a pattern rooted in dist reads it", "lint", []string{"dist/**"}, dist, "dist/**"},
		{"a git pattern selects by its path grammar", "verify", []string{"git:.gen/schema/**"}, describeSchema, "git:.gen/schema/**"},
		{"an exclusion applies to a git pattern", "verify", []string{"git:.gen/schema/**", "!.gen/**"}, describeSchema, ""},
		{"a whole-repository git pattern reaches the output only through a wildcard", "verify", []string{"git:**"}, describeSchema, ""},
		{"a go-embed selector is not read", "compile", []string{"go-embed:build"}, describeSchema, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, conflict := range keyOutputConflicts(t, tc.reader, tc.patterns, []literalOutput{tc.output}) {
				got = append(got, conflict.pattern)
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("conflicts = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("conflicts = %v, want exactly %q", got, tc.want)
			}
		})
	}
}

// guardTask is one task of a repository manifest, with the project-rooted
// patterns its cache key reads.
type guardTask struct {
	id       string
	keyed    bool
	patterns []string
	outputs  map[string]extension.DeclaredOutput
}

// literalOutput is a durable declared output at a literal project-relative
// path.
type literalOutput struct {
	writer    string
	path      string
	directory bool
	excludes  []string
}

// contains reports whether the slash path rel lies inside the output: at or
// under its path and under none of the subpaths it cedes.
func (o literalOutput) contains(rel string) bool {
	if !pathAtOrUnder(rel, o.path) {
		return false
	}
	for _, ceded := range o.excludes {
		if pathAtOrUnder(rel, ceded) {
			return false
		}
	}
	return true
}

func pathAtOrUnder(rel, root string) bool {
	return rel == root || strings.HasPrefix(rel, root+"/")
}

// repositoryTasks loads every putnami.extension.json of the repository through
// the CLI's extension loader and returns its tasks in a stable order.
func repositoryTasks(t *testing.T) []guardTask {
	t.Helper()
	root := findJobsRepoRoot(t)
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "dist" || name == "out" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "putnami.extension.json" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository for extension manifests: %v", err)
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		t.Fatal("found no extension manifest; the guard would be vacuously green")
	}

	var tasks []guardTask
	for _, dir := range dirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		ext := extension.LoadExtensionFromDir(dir, "/"+filepath.ToSlash(rel))
		if ext == nil {
			t.Fatalf("load extension manifest %s", filepath.ToSlash(rel))
		}
		ext.LocalSource = false
		ext.RelPath = filepath.ToSlash(rel)
		names := make([]string, 0, len(ext.Tasks))
		for name := range ext.Tasks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			definition := ext.Tasks[name]
			task := guardTask{
				id:       ext.Name + " " + name,
				keyed:    taskCacheReachable(ext, name, definition),
				patterns: taskKeyFilePatterns(ext, name, definition),
			}
			if definition.Declares != nil {
				task.outputs = definition.Declares.Outputs
			}
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// taskCacheReachable reports whether any step can run the task with its cache
// on, so that the run computes its key.
func taskCacheReachable(ext *extension.ExtensionDescription, name string, definition extension.TaskDefinition) bool {
	if definition.Cache.IsEnabled() {
		return true
	}
	for _, job := range ext.Jobs {
		for _, step := range job.PipelineSteps {
			if step.Task == name && step.Cache != nil && step.Cache.Enabled != nil && *step.Cache.Enabled {
				return true
			}
		}
	}
	return false
}

// taskKeyFilePatterns returns the project-rooted patterns the task's key reads.
// It derives the step's cache policy the way pipeline expansion does — declared
// inputs win over cache.key — and lets keyFilePatterns assemble the list, so the
// guard follows the key code when either changes.
func taskKeyFilePatterns(ext *extension.ExtensionDescription, name string, definition extension.TaskDefinition) []string {
	policy := definition.Cache
	var filePatterns []string
	if len(definition.Inputs) > 0 {
		key := extension.DeriveTaskCacheKey(definition.Inputs)
		policy = &extension.TaskCachePolicy{Key: &key}
		filePatterns = key.Files
	} else if definition.Cache != nil && definition.Cache.Key != nil {
		filePatterns = definition.Cache.Key.Files
	}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/guard", Name: "guard", Path: "guard"},
		Extension: ext,
		Step:      &extension.PipelineStep{Task: name},
		JobDef: &extension.JobDefinition{
			Name: name, ExtensionName: ext.Name, Cache: true,
			FilePatterns: filePatterns, TaskCachePolicy: policy,
		},
	}
	files, _ := keyFilePatterns(nil, job, nil)
	return files
}

// literalProjectOutputs returns every durable output at a literal path under
// the project root that a task of the repository declares.
func literalProjectOutputs(t *testing.T, tasks []guardTask) []literalOutput {
	t.Helper()
	var outputs []literalOutput
	for _, task := range tasks {
		ids := make([]string, 0, len(task.outputs))
		for id := range task.outputs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			declared := task.outputs[id]
			if declared.Path == "" || declared.PathFrom != "" || declared.IsInvocationScoped() ||
				declared.EffectiveRoot() != extension.OutputRootProject {
				continue
			}
			normalized, err := extension.NormalizeOutputPath(declared.Path)
			if err != nil {
				t.Fatalf("%s output %s: %v", task.id, id, err)
			}
			output := literalOutput{writer: task.id, path: normalized, directory: declared.Kind == extension.OutputKindDirectory}
			for _, ceded := range declared.Excludes {
				cededPath, err := extension.NormalizeOutputPath(ceded)
				if err != nil {
					t.Fatalf("%s output %s cedes %s: %v", task.id, id, ceded, err)
				}
				output.excludes = append(output.excludes, cededPath)
			}
			outputs = append(outputs, output)
		}
	}
	return outputs
}

// keyOutputConflicts returns each of one reader's key patterns that names an
// output another task declares and selects a file inside it.
func keyOutputConflicts(t *testing.T, reader string, patterns []string, outputs []literalOutput) []keyOutputConflict {
	t.Helper()
	_, excludes := wsproto.SplitFilePatterns(patterns)
	var exclusions, readable []string
	for _, pattern := range patterns {
		switch {
		case strings.HasPrefix(pattern, "!"):
			exclusions = append(exclusions, pattern)
		case strings.HasPrefix(pattern, "go-embed:"):
			// Resolved from Go sources and embed directives; not read.
		default:
			readable = append(readable, pattern)
		}
	}

	found := make(map[keyOutputConflict]bool)
	for _, pattern := range readable {
		var named []literalOutput
		for _, output := range outputs {
			if output.writer != reader && patternNamesOutput(pattern, output) {
				named = append(named, output)
			}
		}
		if len(named) == 0 {
			continue
		}
		witnesses := outputWitnesses(t, reader, named, pattern)
		var selected []string
		if _, git := wsproto.GitFilePattern(pattern); git {
			// The Git collector's own test: a candidate the include matches and
			// no exclusion matches.
			for _, witness := range witnesses {
				if wsproto.MatchesAnyFilePattern(witness, []string{pattern}) && !wsproto.MatchesAnyFilePattern(witness, excludes) {
					selected = append(selected, witness)
				}
			}
		} else {
			selected = collectWitnesses(t, reader, append([]string{pattern}, exclusions...), witnesses)
		}
		for _, witness := range selected {
			for _, output := range named {
				if output.contains(witness) {
					found[keyOutputConflict{reader: reader, pattern: pattern, writer: output.writer, output: output.path}] = true
				}
			}
		}
	}
	return sortedKeyOutputConflicts(found)
}

// patternNamesOutput reports whether the pattern's leading segments spell,
// literally, the output's path for a directory output, or the directory that
// holds a file output.
func patternNamesOutput(pattern string, output literalOutput) bool {
	inner, _ := wsproto.GitFilePattern(pattern)
	segments := strings.Split(strings.TrimPrefix(filepath.ToSlash(inner), "./"), "/")
	named := strings.Split(output.path, "/")
	if !output.directory {
		named = named[:len(named)-1]
	}
	if len(segments) <= len(named) {
		return false
	}
	for i, segment := range named {
		if segments[i] != segment {
			return false
		}
	}
	return true
}

// outputWitnesses returns one witness path for every place the pattern could
// select a file inside an output: the file output itself, and under a
// directory output one plain file and each tail of the pattern.
func outputWitnesses(t *testing.T, reader string, outputs []literalOutput, pattern string) []string {
	t.Helper()
	inner, _ := wsproto.GitFilePattern(pattern)
	segments := strings.Split(strings.TrimPrefix(filepath.ToSlash(inner), "./"), "/")
	if whole, ok := witnessPath(segments); !ok || (whole != "" && !wsproto.MatchFilePattern(whole, pattern)) {
		t.Fatalf("%s: the guard builds no witness file for key pattern %q", reader, pattern)
	}
	witnesses := make(map[string]bool)
	for _, output := range outputs {
		if !output.directory {
			witnesses[output.path] = true
			continue
		}
		witnesses[output.path+"/guard"] = true
		for i := range segments {
			if tail, _ := witnessPath(segments[i:]); tail != "" {
				witnesses[output.path+"/"+tail] = true
			}
		}
	}
	paths := make([]string, 0, len(witnesses))
	for path := range witnesses {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// collectWitnesses writes the witness files into project trees and returns
// the ones the CLI's key collector selects with patterns.
func collectWitnesses(t *testing.T, reader string, patterns, witnesses []string) []string {
	t.Helper()
	var selected []string
	for _, bucket := range witnessBuckets(witnesses) {
		dir := t.TempDir()
		for _, rel := range bucket {
			writeTestFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "")
		}
		files, err := store.CollectKeyFiles(dir, patterns)
		if err != nil {
			t.Fatalf("%s: collect key files with %q: %v", reader, patterns, err)
		}
		for _, file := range files {
			rel, err := filepath.Rel(dir, file)
			if err != nil {
				t.Fatal(err)
			}
			selected = append(selected, filepath.ToSlash(rel))
		}
	}
	return selected
}

// witnessPath returns a path the pattern segments match, with "**" matching no
// segment, or false when it cannot build one.
func witnessPath(segments []string) (string, bool) {
	var parts []string
	for _, segment := range segments {
		if segment == "**" {
			continue
		}
		var part strings.Builder
		for i := 0; i < len(segment); i++ {
			switch c := segment[i]; c {
			case '*':
				part.WriteString("guard")
			case '?':
				part.WriteByte('g')
			case '\\':
				if i+1 < len(segment) {
					i++
					part.WriteByte(segment[i])
				}
			case '[':
				end := strings.IndexByte(segment[i+1:], ']')
				if end <= 0 {
					return "", false
				}
				class := segment[i+1 : i+1+end]
				if class[0] == '^' || class[0] == '!' || class[0] == '\\' {
					return "", false
				}
				part.WriteByte(class[0])
				i += end + 1
			default:
				part.WriteByte(c)
			}
		}
		parts = append(parts, part.String())
	}
	return strings.Join(parts, "/"), true
}

// witnessBuckets splits the witness paths into trees that can each hold all of
// their paths: no path of a bucket is a directory of another.
func witnessBuckets(paths []string) [][]string {
	var buckets [][]string
	for _, path := range paths {
		placed := false
		for i, bucket := range buckets {
			clash := false
			for _, other := range bucket {
				if strings.HasPrefix(path, other+"/") || strings.HasPrefix(other, path+"/") {
					clash = true
					break
				}
			}
			if !clash {
				buckets[i] = append(bucket, path)
				placed = true
				break
			}
		}
		if !placed {
			buckets = append(buckets, []string{path})
		}
	}
	return buckets
}

func sortedKeyOutputConflicts(set map[keyOutputConflict]bool) []keyOutputConflict {
	conflicts := make([]keyOutputConflict, 0, len(set))
	for conflict := range set {
		conflicts = append(conflicts, conflict)
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.reader != b.reader {
			return a.reader < b.reader
		}
		if a.pattern != b.pattern {
			return a.pattern < b.pattern
		}
		if a.writer != b.writer {
			return a.writer < b.writer
		}
		return a.output < b.output
	})
	return conflicts
}
