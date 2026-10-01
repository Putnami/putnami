package lifecycle

import (
	"fmt"
	"os"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ProjectsTag manages project tags.
// Usage: projects tag <project> [tags] [--remove] [--set]
func ProjectsTag(wsRoot string, cfg *wsproto.Config, args []string) error {
	if len(args) < 1 {
		return cmderr.Usagef("project name required: projects tag <project> [tags] [--remove] [--set]")
	}

	projectName := args[0]
	var tagsArg string
	var remove, set bool

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--remove":
			remove = true
		case "--set":
			set = true
		default:
			if !strings.HasPrefix(args[i], "-") && tagsArg == "" {
				tagsArg = args[i]
			}
		}
	}

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}

	proj := ws.ProjectByName(projectName)
	if proj == nil {
		return cmderr.NotFoundf("project not found: %s", projectName)
	}

	// List mode: no tags argument
	if tagsArg == "" {
		if len(proj.Tags) == 0 {
			iox.Fprintln(os.Stdout, "  (no tags)")
		} else {
			iox.Fprintf(os.Stdout, "  %s\n", strings.Join(proj.Tags, ", "))
		}
		return nil
	}

	// Parse input tags
	inputTags := parseTags(tagsArg)

	// Compute new tags
	var newTags []string
	switch {
	case set:
		newTags = inputTags
	case remove:
		removeSet := make(map[string]bool, len(inputTags))
		for _, t := range inputTags {
			removeSet[t] = true
		}
		for _, t := range proj.Tags {
			if !removeSet[t] {
				newTags = append(newTags, t)
			}
		}
	default: // add
		seen := make(map[string]bool)
		for _, t := range proj.Tags {
			seen[t] = true
			newTags = append(newTags, t)
		}
		for _, t := range inputTags {
			if !seen[t] {
				newTags = append(newTags, t)
			}
		}
	}
	sort.Strings(newTags)

	// Clearing writes an explicit EMPTY ARRAY, it does not delete the key.
	//
	// "Cleared" and "never authored" have to be two different states on disk,
	// because a project's tags do not only come from putnami.json: a provider
	// reports the tags its own manifest declares (a package.json `putnami.tags`
	// block, say) and MergeProbeResults unions them into the view. Deleting the
	// key left putnami.json silent, the view still carried the provider's tags,
	// and resolveProjectIdentity's fallthrough put them straight back — so
	// `projects tag --remove` on the last tag printed "tags cleared" and changed
	// nothing observable. An empty array is the authored statement "this project
	// has no tags", and probe_view.go distinguishes it from absence by nil-ness.
	//
	// UpdateProjectConfigField keeps its nil-means-delete contract for every
	// other caller; this one passes an empty slice on purpose.
	tagValue := any(newTags)
	if len(newTags) == 0 {
		tagValue = []string{}
	}
	if err := workspace.UpdateProjectConfigField(wsRoot, proj, "tags", tagValue); err != nil {
		return err
	}

	if len(newTags) == 0 {
		iox.Fprintf(os.Stdout, "  %s: tags cleared\n", projectName)
	} else {
		iox.Fprintf(os.Stdout, "  %s: %s\n", projectName, strings.Join(newTags, ", "))
	}
	return nil
}

// parseTags splits a comma-separated string into trimmed, non-empty tags.
func parseTags(s string) []string {
	parts := strings.Split(s, ",")
	var tags []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			tags = append(tags, p)
		}
	}
	return tags
}
