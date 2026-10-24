package workspace

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"

	extproto "go.putnami.dev/protocol/extension"
)

// ManifestBaseline reads a workspace-relative file as the diff's base records
// it, and reports false when the base records no such file or it cannot be
// read.
type ManifestBaseline func(path string) ([]byte, bool)

// WithManifestBaseline returns an index that answers TasksOfManifestChange
// against baseline. It leaves the receiver unchanged, so one index can serve
// two diffs at once. An index this package did not build, a nil index and a
// nil baseline come back as they are: such an index confines no manifest
// change, and the extension stays whole.
func WithManifestBaseline(index TaskImpactIndex, baseline ManifestBaseline) TaskImpactIndex {
	idx, ok := index.(*taskIndex)
	if !ok || idx == nil || baseline == nil {
		return index
	}
	bound := *idx
	bound.baseline = baseline
	return &bound
}

// TasksOfManifestChange answers a change to an in-workspace extension
// project's own putnami.extension.json (TaskImpactIndex).
//
// It compares the manifest the diff's base records with the one in the
// working tree, which is the one the run loaded. Every member outside the
// `tasks` map and outside each command's `run` list is compared as a JSON
// value, and any difference there leaves the change unconfined: hooks, the
// runtime, a command's activation, traits or flags, the vocabulary and the
// version all reach every task.
//
// Inside, a task moved when its contract digest (extproto.TaskContractDigest)
// differs or it exists on one side only, so a description edit moves nothing.
// A step moved when its JSON value differs, when it exists on one side only,
// or when it names a task that moved. A step order that changes moves every
// step of its command. The answer is the moved steps, the steps a dependent's
// `^` reference reaches from them, and the steps that depend on those inside a
// project, qualified as TasksOfExtension qualifies them. A moved step the
// working tree no longer declares is in the answer; it names no job.
//
// The change is unconfined when the base records no manifest, when either
// side does not parse, when the path is not the project's own manifest, or
// when a pinned build replaces it.
func (idx *taskIndex) TasksOfManifestChange(extensionProjectID, path string) ([]string, bool) {
	project := idx.ws.ProjectByID(extensionProjectID)
	if project == nil || idx.baseline == nil || !idx.ownManifests[extensionProjectID] {
		return nil, false
	}
	manifest := cleanWorkspacePath(path)
	if manifest != cleanWorkspacePath(filepath.Join(project.Path, extproto.ManifestFilename)) {
		return nil, false
	}
	before, recorded := idx.baseline(manifest)
	if !recorded {
		return nil, false
	}
	after, err := os.ReadFile(filepath.Join(idx.ws.Root, filepath.FromSlash(manifest)))
	if err != nil {
		return nil, false
	}
	moved, confined := changedDeclarations(before, after)
	if !confined {
		return nil, false
	}
	return idx.qualifiedMoves(extensionProjectID, moved), true
}

// qualifiedMoves widens moved steps of one extension to the steps they reach
// and qualifies each with the extension project: the moved steps themselves,
// and the reached ones the extension declares.
func (idx *taskIndex) qualifiedMoves(extensionProjectID string, moved []string) []string {
	if len(moved) == 0 {
		return nil
	}
	prefix := extensionProjectID + extensionScopeSeparator
	declared := make(map[string]bool, len(idx.extensionProjects[extensionProjectID]))
	for _, scope := range idx.extensionProjects[extensionProjectID] {
		declared[scope] = true
	}
	answer := make(map[string]bool, len(moved))
	for _, scope := range moved {
		answer[prefix+scope] = true
	}
	reached := append(append([]string(nil), moved...), idx.TasksReachedFrom(moved)...)
	for _, scope := range idx.closureOf(reached) {
		if declared[prefix+scope] {
			answer[prefix+scope] = true
		}
	}
	return sortedSet(answer)
}

// changedDeclarations compares two manifests and returns the task scopes,
// `<command>~<step>`, whose declaration moved. It reports false when anything
// outside task and step declarations moved, or when either side is not a JSON
// object a manifest parses from.
func changedDeclarations(before, after []byte) ([]string, bool) {
	oldShape, oldRead := readManifestShape(before)
	newShape, newRead := readManifestShape(after)
	if !oldRead || !newRead || !oldShape.sameOutsideDeclarations(newShape) {
		return nil, false
	}
	var oldManifest, newManifest extproto.Manifest
	if json.Unmarshal(before, &oldManifest) != nil || json.Unmarshal(after, &newManifest) != nil {
		return nil, false
	}
	movedTasks := movedTaskContracts(oldManifest.Tasks, newManifest.Tasks)
	moved := make(map[string]bool)
	// Both sides name the same commands: their members other than `run`
	// compared equal above.
	for command := range newShape.commands {
		movedSteps(command, oldShape.runs[command], newShape.runs[command], movedTasks, moved)
	}
	return sortedSet(moved), true
}

// manifestShape is a manifest document read as canonical JSON values: every
// top-level member but `tasks` and `commands`, every member of each command
// but `run`, and each command's `run` list as written. Two values compare
// equal when they decode to the same JSON value, whatever their layout; a
// number compares by its text.
type manifestShape struct {
	outside         map[string]string
	commandsPresent bool
	commands        map[string]map[string]string
	runs            map[string]json.RawMessage
}

// readManifestShape reads a manifest document. It reports false when the
// document is not a JSON object, or its `commands` member is not an object of
// objects.
func readManifestShape(data []byte) (manifestShape, bool) {
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || document == nil {
		return manifestShape{}, false
	}
	shape := manifestShape{
		outside:  make(map[string]string, len(document)),
		commands: make(map[string]map[string]string),
		runs:     make(map[string]json.RawMessage),
	}
	for key, value := range document {
		if key == "tasks" || key == "commands" {
			continue
		}
		canonical, ok := canonicalJSON(value)
		if !ok {
			return manifestShape{}, false
		}
		shape.outside[key] = canonical
	}
	raw, present := document["commands"]
	if !present {
		return shape, true
	}
	shape.commandsPresent = true
	var commands map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &commands) != nil {
		return manifestShape{}, false
	}
	for name, members := range commands {
		kept := make(map[string]string, len(members))
		for key, value := range members {
			if key == "run" {
				shape.runs[name] = value
				continue
			}
			canonical, ok := canonicalJSON(value)
			if !ok {
				return manifestShape{}, false
			}
			kept[key] = canonical
		}
		shape.commands[name] = kept
	}
	return shape, true
}

// sameOutsideDeclarations reports whether two manifests agree on everything a
// change must leave alone to stay confined to task and step declarations.
func (shape manifestShape) sameOutsideDeclarations(other manifestShape) bool {
	return shape.commandsPresent == other.commandsPresent &&
		maps.Equal(shape.outside, other.outside) &&
		maps.EqualFunc(shape.commands, other.commands, maps.Equal)
}

// canonicalJSON is one JSON value re-encoded with sorted object keys and no
// layout. Numbers keep their text.
func canonicalJSON(raw json.RawMessage) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// movedTaskContracts names the tasks whose contract digest differs between two
// task maps, a task on one side only included.
func movedTaskContracts(before, after map[string]extproto.TaskDefinition) map[string]bool {
	moved := make(map[string]bool)
	for name, task := range before {
		if next, kept := after[name]; !kept || extproto.TaskContractDigest(task) != extproto.TaskContractDigest(next) {
			moved[name] = true
		}
	}
	for name := range after {
		if _, existed := before[name]; !existed {
			moved[name] = true
		}
	}
	return moved
}

// declaredStep is one step of a command's `run` list: its canonical JSON value
// and the task it names, "" when it names none.
type declaredStep struct {
	value string
	task  string
}

// declaredSteps is one command's `run` list: the step ids in order, and each
// step by id.
type declaredSteps struct {
	order []string
	byID  map[string]declaredStep
}

// readSteps reads a command's `run` list; an absent or null list declares no
// step. It reports false when the list is not a list of objects with distinct
// string ids.
func readSteps(run json.RawMessage) (declaredSteps, bool) {
	steps := declaredSteps{byID: make(map[string]declaredStep)}
	if len(run) == 0 || string(bytes.TrimSpace(run)) == "null" {
		return steps, true
	}
	var list []json.RawMessage
	if json.Unmarshal(run, &list) != nil {
		return steps, false
	}
	for _, entry := range list {
		var members map[string]json.RawMessage
		if json.Unmarshal(entry, &members) != nil || members == nil {
			return steps, false
		}
		id, ok := jsonString(members["id"])
		if !ok {
			return steps, false
		}
		if _, repeated := steps.byID[id]; repeated {
			return steps, false
		}
		value, ok := canonicalJSON(entry)
		if !ok {
			return steps, false
		}
		task, _ := jsonString(members["task"])
		steps.order = append(steps.order, id)
		steps.byID[id] = declaredStep{value: value, task: task}
	}
	return steps, true
}

// jsonString reads a JSON string. It reports false for an absent value, null
// and any other kind of value.
func jsonString(raw json.RawMessage) (string, bool) {
	var text *string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil || text == nil {
		return "", false
	}
	return *text, true
}

// movedSteps adds to moved the task scopes of one command whose step
// declaration moved between the `run` lists before and after.
//
// A command whose steps cannot be read on either side moves whole: its bare
// scope and every step id either side names. So does a command whose step
// order changed, because the order of a pipeline is part of its declaration.
// A command that gains its first step or loses its last moves its bare scope
// too, which is the scope a command with no pipeline runs under.
func movedSteps(command string, before, after json.RawMessage, movedTasks map[string]bool, moved map[string]bool) {
	oldSteps, oldRead := readSteps(before)
	newSteps, newRead := readSteps(after)
	every := func() {
		moved[command] = true
		for _, id := range append(append([]string(nil), oldSteps.order...), newSteps.order...) {
			moved[taskScope(command, id)] = true
		}
	}
	if !oldRead || !newRead || !slices.Equal(commonOrder(oldSteps, newSteps), commonOrder(newSteps, oldSteps)) {
		every()
		return
	}
	if (len(oldSteps.order) == 0) != (len(newSteps.order) == 0) {
		moved[command] = true
	}
	for _, id := range append(append([]string(nil), oldSteps.order...), newSteps.order...) {
		oldStep, inOld := oldSteps.byID[id]
		newStep, inNew := newSteps.byID[id]
		if !inOld || !inNew || oldStep != newStep || movedTasks[oldStep.task] || movedTasks[newStep.task] {
			moved[taskScope(command, id)] = true
		}
	}
}

// commonOrder is steps' order restricted to the ids other declares too.
func commonOrder(steps, other declaredSteps) []string {
	common := make([]string, 0, len(steps.order))
	for _, id := range steps.order {
		if _, shared := other.byID[id]; shared {
			common = append(common, id)
		}
	}
	return common
}
