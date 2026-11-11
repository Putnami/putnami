package stacks

// StepsOverride writes the steps a graph sync gives a task of one project,
// under options[Extension].tasks.<task> of its putnami.json, where they
// override the profile's steps: such as moon run on the moon task a command
// maps to, or rushx on the script an import keeps on Rush (ADR 0017).
type StepsOverride struct {
	// Extension is the stack extension whose option layer holds the steps.
	Extension string
	// Tasks are the tasks whose steps the sync owns.
	Tasks []string
	// Dir, when set, is where the steps the sync writes run; the sync writes
	// it beside them, and removes it with them.
	Dir string
	// Synced reports whether a task's steps are absent or the sync's own:
	// the sync never writes over a project's own steps.
	Synced func(steps any) bool
}

// Apply writes, in a putnami.json document, the steps of each owned task
// that steps names, removes the sync's steps of every other owned task, and
// removes each object left empty, so the project's own members, such as its
// reports, stay.
func (o StepsOverride) Apply(doc map[string]any, steps map[string][][]string) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[o.Extension].(map[string]any)
	tasks, _ := layer["tasks"].(map[string]any)
	if len(steps) == 0 && tasks == nil {
		return
	}
	if options == nil {
		options = map[string]any{}
	}
	if layer == nil {
		layer = map[string]any{}
	}
	if tasks == nil {
		tasks = map[string]any{}
	}
	for _, name := range o.Tasks {
		task, _ := tasks[name].(map[string]any)
		if !o.Synced(task["steps"]) {
			continue
		}
		if s, ok := steps[name]; ok {
			if task == nil {
				task = map[string]any{}
			}
			list := make([]any, len(s))
			for i, step := range s {
				args := make([]any, len(step))
				for j, a := range step {
					args[j] = a
				}
				list[i] = args
			}
			task["steps"] = list
			if o.Dir != "" {
				task["dir"] = o.Dir
			}
			tasks[name] = task
			continue
		}
		if task == nil {
			continue
		}
		if _, had := task["steps"]; had && o.Dir != "" && task["dir"] == o.Dir {
			delete(task, "dir")
		}
		delete(task, "steps")
		if len(task) == 0 {
			delete(tasks, name)
		}
	}
	setObject(layer, "tasks", tasks)
	setObject(options, o.Extension, layer)
	setObject(doc, "options", options)
}

// setObject sets the object at key, or removes it when it is empty.
func setObject(doc map[string]any, key string, value map[string]any) {
	if len(value) == 0 {
		delete(doc, key)
		return
	}
	doc[key] = value
}
