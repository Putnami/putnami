package workspace

import "slices"

// ProjectCommandDependencies answers, by command, every entry a project of the
// workspace names under commands.<command>.dependsOn of its putnami.json,
// sorted and each once (CLI ADR 0097). The runtime preparation that runs
// before planning reads it, so a command reaches the extensions a project's
// prerequisite runs. Nil when no project names one.
func ProjectCommandDependencies(ws *Workspace) map[string][]string {
	if ws == nil {
		return nil
	}
	var out map[string][]string
	for _, project := range ws.Projects {
		if project == nil || project.Config == nil {
			continue
		}
		for command, declared := range project.Config.Commands {
			for _, dependency := range declared.DependsOn {
				if out == nil {
					out = make(map[string][]string)
				}
				if !slices.Contains(out[command], dependency) {
					out[command] = append(out[command], dependency)
				}
			}
		}
	}
	for command := range out {
		slices.Sort(out[command])
	}
	return out
}
