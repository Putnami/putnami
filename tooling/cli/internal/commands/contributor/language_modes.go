package contributor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/cli/internal/git"
)

// CallProvider uses the normal collaboration route, including its capability,
// custody and error handling. The language checker only invokes reads.
type CallProvider func(context.Context, string, string, json.RawMessage) collab.Envelope

type LanguageRunner struct {
	Root  string
	Stdin io.Reader
	Out   io.Writer
	Err   io.Writer
	Call  CallProvider
}

func (r LanguageRunner) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Root}, args...)...)
	cmd.Env = git.RepositoryEnvironment(os.Environ())
	return cmd.Output()
}

type languageScan struct {
	runner    LanguageRunner
	units     int
	offenders int
}

func (s *languageScan) add(label string, content []byte) {
	s.units++
	if hit := EvaluateLanguage(label, string(content)); hit != nil {
		s.offenders++
		fmt.Fprintf(s.runner.Out, "%s: non-English signals: %s\n", hit.Label, strings.Join(hit.Signals, ", "))
		for _, line := range hit.Lines {
			fmt.Fprintf(s.runner.Out, "%s:%s\n", hit.Label, line)
		}
	}
}

// RunLanguage returns 0 for no violations, 1 for violations, and 2 for usage
// or tool failure. The same pure evaluator is used by proposal publication.
func (r LanguageRunner) RunLanguage(ctx context.Context, mode string, args []string) int {
	if r.Out == nil {
		r.Out = os.Stdout
	}
	if r.Err == nil {
		r.Err = os.Stderr
	}
	if r.Stdin == nil {
		r.Stdin = os.Stdin
	}
	scan := &languageScan{runner: r}
	var err error
	switch mode {
	case "text":
		err = scan.text(args)
	case "files":
		err = scan.files(args)
	case "commits":
		err = scan.commits(args)
	case "tasks":
		err = scan.tasks(ctx, args)
	default:
		err = fmt.Errorf("unknown mode %q (text, files, commits, tasks)", mode)
	}
	if err != nil {
		fmt.Fprintf(r.Err, "contributor language: %v\n", err)
		return 2
	}
	if scan.offenders > 0 {
		fmt.Fprintf(r.Err, "contributor language: %d of %d scanned unit(s) are not in English\n", scan.offenders, scan.units)
		return 1
	}
	fmt.Fprintf(r.Err, "contributor language: %d scanned unit(s), no offender\n", scan.units)
	return 0
}

func (s *languageScan) text(args []string) error {
	label := ""
	if len(args) >= 2 && args[0] == "--label" {
		label, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		args = []string{"-"}
	}
	for _, path := range args {
		if strings.HasPrefix(path, "-") && path != "-" {
			return fmt.Errorf("unknown text argument %q", path)
		}
		var data []byte
		var err error
		if path == "-" {
			data, err = io.ReadAll(io.LimitReader(s.runner.Stdin, 16<<20))
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return err
		}
		name := path
		if path == "-" {
			name = "stdin"
		}
		if label != "" {
			name = label
		}
		s.add(name, data)
	}
	return nil
}

func (s *languageScan) files(args []string) error {
	paths, err := s.filePaths(args)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if ExcludedLanguagePath(path) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.runner.Root, path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if strings.IndexByte(string(data), 0) >= 0 {
			continue
		}
		s.add(path, data)
	}
	return nil
}

func (s *languageScan) filePaths(args []string) ([]string, error) {
	if len(args) > 0 && args[0] == "--diff" {
		if len(args) != 2 {
			return nil, fmt.Errorf("files --diff needs one revision range")
		}
		data, err := s.runner.git("diff", "--no-relative", "--name-only", "--diff-filter=d", "-z", args[1])
		if err != nil {
			return nil, fmt.Errorf("invalid revision range %q: %w", args[1], err)
		}
		return nulFields(data), nil
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	gitArgs := append([]string{"ls-files", "-z", "--"}, args...)
	data, err := s.runner.git(gitArgs...)
	if err != nil {
		return nil, err
	}
	return nulFields(data), nil
}

func (s *languageScan) commits(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("commits needs one revision range")
	}
	data, err := s.runner.git("rev-list", args[0])
	if err != nil {
		return fmt.Errorf("invalid revision range %q: %w", args[0], err)
	}
	for _, sha := range strings.Fields(string(data)) {
		message, err := s.runner.git("log", "-1", "--format=%B", sha)
		if err != nil {
			return err
		}
		s.add("commit "+sha[:min(7, len(sha))], message)
	}
	return nil
}

func (s *languageScan) tasks(ctx context.Context, args []string) error {
	limit := 1000
	if len(args) != 0 {
		if len(args) != 2 || args[0] != "--limit" {
			return fmt.Errorf("tasks accepts only --limit <positive integer>")
		}
		var err error
		limit, err = strconv.Atoi(args[1])
		if err != nil || limit < 1 {
			return fmt.Errorf("--limit must be a positive integer")
		}
	}
	if s.runner.Call == nil {
		return fmt.Errorf("tasks provider unavailable")
	}
	cursor, processed := "", 0
	for processed < limit {
		page, err := s.taskPage(ctx, cursor)
		if err != nil {
			return err
		}
		for _, task := range page.Items {
			if processed >= limit {
				break
			}
			processed++
			body := task.Title + "\n\n" + task.Body
			if strings.Contains(body, "audit-fp: workspace/language/english-only") {
				continue
			}
			s.add("task "+task.Ref.ID, []byte(body))
		}
		if page.Page.Next == "" {
			break
		}
		if page.Page.Next == cursor {
			return fmt.Errorf("tasks provider repeated cursor")
		}
		cursor = page.Page.Next
	}
	return nil
}

func (s *languageScan) taskPage(ctx context.Context, cursor string) (collab.TaskListResult, error) {
	input, _ := json.Marshal(collab.TaskFindInput{States: []collab.TaskState{collab.TaskStateOpen, collab.TaskStateInProgress, collab.TaskStateBlocked}, Page: &collab.PageRequest{Size: 100, Cursor: cursor}})
	envelope := s.runner.Call(ctx, collab.ContractTasks, collab.OperationFind, input)
	if envelope.Outcome != collab.OutcomeOK {
		return collab.TaskListResult{}, fmt.Errorf("tasks find answered %s: %v", envelope.Outcome, envelope.Error)
	}
	var page collab.TaskListResult
	if err := json.Unmarshal(envelope.Result, &page); err != nil {
		return page, err
	}
	return page, nil
}

func nulFields(data []byte) []string {
	text := strings.TrimSuffix(string(data), "\x00")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\x00")
}
