package estimate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	job "go.putnami.dev/protocol/job"
)

// Command is one run of `putnami change estimate`.
type Command struct {
	// Root is the workspace root, the directory putnami launches the
	// command in.
	Root string
	// ContextFile is the job context putnami passes with --putnamiContext.
	// Empty means none: the command then writes text, and resolves --plan-file
	// against Root.
	ContextFile string
	Stdout      io.Writer
	Stderr      io.Writer
}

// options are the flags after `change estimate`.
type options struct {
	issue string
	plan  string
}

// Run estimates the plan args name and returns the exit code: 2 for a usage
// error, 1 for a plan or a policy it refuses, 0 for an estimate.
func (c Command) Run(args []string) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change estimate: %v\n", err)
		return 2
	}
	var launch *job.Context
	if c.ContextFile != "" {
		if launch, err = job.ParseFile(c.ContextFile); err != nil {
			fmt.Fprintf(c.Stderr, "change estimate: %v\n", err)
			return 1
		}
	}
	shown, file := opts.plan, opts.plan
	if file == "" {
		shown = filepath.Join(".putnami", opts.issue, "plan.json")
		file = filepath.Join(c.Root, shown)
	} else if !filepath.IsAbs(file) {
		file = filepath.Join(c.Root, file)
		if launch != nil && launch.InvocationDir != "" {
			file = filepath.Join(launch.InvocationDir, opts.plan)
		}
	}
	result, err := c.estimate(opts, shown, file)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change estimate: %v\n", err)
		return 1
	}
	if launch != nil && protocolcli.OutputMode(launch.Params.String("output")).IsMachineReadable() {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(c.Stderr, "change estimate: %v\n", err)
			return 1
		}
		fmt.Fprintln(c.Stdout, string(data))
		return 0
	}
	writeText(c.Stdout, result)
	return 0
}

func (c Command) estimate(opts options, shown, file string) (Result, error) {
	data, err := ReadBounded(file)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", shown, err)
	}
	plan, err := ParsePlan(data)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", shown, err)
	}
	if opts.issue != "" && plan.Issue != opts.issue {
		return Result{}, fmt.Errorf("%s: the plan names issue %q, not %q", shown, plan.Issue, opts.issue)
	}
	table, err := ReadTable(c.Root)
	if err != nil {
		return Result{}, err
	}
	return Estimate(plan, table, shown), nil
}

// ReadBounded reads a plan file of at most 1 MiB.
func ReadBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("no plan file")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxDocumentBytes)
	}
	return data, nil
}

// parseOptions reads `--issue <id>` and `--plan-file <file>`, each in its two
// forms. One of them is required. An issue id is a token the plan format
// accepts, with an optional leading #.
func parseOptions(args []string) (options, error) {
	var opts options
	seen := map[string]bool{}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		if name != "--issue" && name != "--plan-file" {
			return opts, fmt.Errorf("unexpected argument %q; use --issue <id> or --plan-file <file>", Bounded(raw))
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
		issue := strings.TrimPrefix(value, "#")
		if !ValidID(issue) {
			return opts, errors.New("--issue must be an issue id: letters, digits, '.', '_' and '-'")
		}
		opts.issue = issue
	}
	if opts.issue == "" && opts.plan == "" {
		return opts, errors.New("give --issue <id> to read .putnami/<id>/plan.json, or --plan-file <file>")
	}
	return opts, nil
}

// Bounded cuts text to its first 64 bytes, marking the cut with "...", so an
// error message quotes an argument in bounded space.
func Bounded(text string) string {
	if len(text) > 64 {
		return text[:64] + "..."
	}
	return text
}

// writeText writes the estimate for a reader: the plan, then one line per
// slice.
func writeText(w io.Writer, result Result) {
	fmt.Fprintf(w, "Plan %s: %s, %s tokens, %s minutes\n", result.Plan, result.Size,
		result.Tokens.TokenText(), result.Minutes.MinuteText())
	for i, slice := range result.Slices {
		fmt.Fprintf(w, "  %d. %s: %s, %s tokens, %s minutes, %d files counted\n", i+1, slice.Key, slice.Size,
			slice.Tokens.TokenText(), slice.Minutes.MinuteText(), slice.Files)
	}
}

// TokenText writes a token range for a reader, in millions.
func (r Range) TokenText() string {
	return Millions(r.Low) + " to " + Millions(r.High)
}

// Millions writes a token count in millions with one decimal.
func Millions(n int) string {
	return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
}

// MinuteText writes a minute range for a reader.
func (r Range) MinuteText() string {
	return strconv.Itoa(r.Low) + " to " + strconv.Itoa(r.High)
}
