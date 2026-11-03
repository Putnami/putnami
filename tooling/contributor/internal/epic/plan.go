// Package epic serves putnami epic plan and putnami epic status.
//
// An epic plan, .putnami/<epic>/epic.json, names the issues of one outcome,
// their dependencies, the slices of each change plan and the envelope the
// owner approves. epic plan checks it, sizes each issue and orders the issues
// in waves. epic status reads, for each issue, its task, its proposal and its
// sessions through the collaboration contracts, and names the issues that may
// launch now.
package epic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	job "go.putnami.dev/protocol/job"

	"go.putnami.dev/tooling/contributor/internal/estimate"
)

// PlanCommand is one run of `putnami epic plan`.
type PlanCommand struct {
	// Root is the workspace root, the directory putnami launches the command
	// in.
	Root string
	// ContextFile is the job context putnami passes with --putnamiContext.
	// Empty means none: the command then writes text, and resolves
	// --plan-file against Root.
	ContextFile string
	Stdout      io.Writer
	Stderr      io.Writer
}

// Run sizes and orders the epic plan args name, and returns the exit code: 2
// for a usage error, 1 for a plan or a policy it refuses, 0 for a result.
func (c PlanCommand) Run(args []string) int {
	loaded, code := load("epic plan", c.Root, c.ContextFile, args, c.Stderr)
	if code != 0 {
		return code
	}
	if loaded.structured {
		return writeJSON("epic plan", c.Stdout, c.Stderr, loaded.result)
	}
	writePlanText(c.Stdout, loaded.result)
	return 0
}

// loaded is an epic plan read, checked and sized.
type loaded struct {
	plan       estimate.EpicPlan
	result     estimate.EpicResult
	file       string
	structured bool
}

// load reads the job context, the epic plan args name and the workspace's
// estimate table. It prints its error under the command's name and returns
// the exit code to stop with, or 0.
func load(name, root, contextFile string, args []string, stderr io.Writer) (loaded, int) {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return loaded{}, 2
	}
	var launch *job.Context
	if contextFile != "" {
		if launch, err = job.ParseFile(contextFile); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return loaded{}, 1
		}
	}
	shown, file := opts.plan, opts.plan
	if file == "" {
		shown = filepath.Join(".putnami", opts.epic, "epic.json")
		file = filepath.Join(root, shown)
	} else if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
		if launch != nil && launch.InvocationDir != "" {
			file = filepath.Join(launch.InvocationDir, opts.plan)
		}
	}
	data, err := estimate.ReadBounded(file)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", name, shown, err)
		return loaded{}, 1
	}
	plan, err := estimate.ParseEpicPlan(data)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", name, shown, err)
		return loaded{}, 1
	}
	if opts.epic != "" && plan.Epic != opts.epic {
		fmt.Fprintf(stderr, "%s: %s: the plan is for epic %q, not %q\n", name, shown, plan.Epic, opts.epic)
		return loaded{}, 1
	}
	table, err := estimate.ReadTable(root)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return loaded{}, 1
	}
	return loaded{
		plan:       plan,
		result:     estimate.EstimateEpic(plan, table, shown),
		file:       file,
		structured: launch != nil && estimate.StructuredOutput(launch.Params.String("output")),
	}, 0
}

// options are the flags after `epic plan` or `epic status`.
type options struct {
	epic string
	plan string
}

// parseOptions reads `--epic <id>` and `--plan-file <file>`, each in its two
// forms. One of them is required. An epic id is a token the plan format
// accepts, with an optional leading #.
func parseOptions(args []string) (options, error) {
	var opts options
	seen := map[string]bool{}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		if name != "--epic" && name != "--plan-file" {
			return opts, fmt.Errorf("unexpected argument %q; use --epic <id> or --plan-file <file>", bounded(raw))
		}
		if seen[name] {
			return opts, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if !hasValue {
			if len(args) == 0 {
				return opts, fmt.Errorf("%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		if value == "" || strings.HasPrefix(value, "--") {
			return opts, fmt.Errorf("%s needs a nonempty value", name)
		}
		if name == "--plan-file" {
			opts.plan = value
			continue
		}
		id := strings.TrimPrefix(value, "#")
		if !estimate.ValidID(id) {
			return opts, errors.New("--epic must be an epic id: letters, digits, '.', '_' and '-'")
		}
		opts.epic = id
	}
	if opts.epic == "" && opts.plan == "" {
		return opts, errors.New("give --epic <id> to read .putnami/<id>/epic.json, or --plan-file <file>")
	}
	return opts, nil
}

func bounded(text string) string {
	if len(text) > 64 {
		return text[:64] + "..."
	}
	return text
}

func writeJSON(name string, stdout, stderr io.Writer, value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// writePlanText writes the epic for a reader: the epic and its envelope, then
// each wave and its issues.
func writePlanText(w io.Writer, result estimate.EpicResult) {
	fmt.Fprintf(w, "Epic %s: %s\n", result.Plan, result.Title)
	fmt.Fprintf(w, "  Estimate: %s, %s tokens, %s minutes\n", result.Size, result.Tokens.TokenText(), result.Minutes.MinuteText())
	fmt.Fprintf(w, "  Envelope: %s tokens, %d minutes, %d sessions at once\n",
		estimate.Millions(result.Envelope.Tokens), result.Envelope.ActiveMinutes, result.Envelope.Sessions)
	if result.Envelope.Tokens < result.Tokens.Low || result.Envelope.ActiveMinutes < result.Minutes.Low {
		fmt.Fprintln(w, "  The envelope is below the low bound of the estimate.")
	}
	byKey := map[string]estimate.IssueEstimate{}
	for _, issue := range result.Issues {
		byKey[issue.Key] = issue
	}
	for i, wave := range result.Waves {
		fmt.Fprintf(w, "Wave %d\n", i+1)
		for _, key := range wave {
			issue := byKey[key]
			fmt.Fprintf(w, "  %s%s: %s, %s tokens, %s minutes%s\n", issue.Key, taskText(issue.Task), issue.Size,
				issue.Tokens.TokenText(), issue.Minutes.MinuteText(), dependsText(issue.DependsOn))
		}
	}
}

func taskText(task *estimate.TaskRef) string {
	if task == nil {
		return " (no task yet)"
	}
	return " (task " + task.ID + ")"
}

func dependsText(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return ", after " + strings.Join(keys, ", ")
}
