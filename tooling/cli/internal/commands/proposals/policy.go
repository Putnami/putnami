package proposals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

type Policy struct {
	Version   int `json:"version"`
	Proposals struct {
		Source string `json:"source"`
	} `json:"proposals"`
	States struct {
		Delivered collab.TaskState `json:"delivered"`
	} `json:"states"`
	Verification struct {
		Language string `json:"language"`
		CIGate   struct {
			Checks         []string `json:"checks"`
			TimeoutMinutes int      `json:"timeoutMinutes"`
			PollSeconds    int      `json:"pollSeconds"`
		} `json:"ciGate"`
	} `json:"verification"`
	Publication struct {
		Draft         bool   `json:"draft"`
		TaskReference string `json:"taskReference"`
		BodyMaxBytes  int    `json:"bodyMaxBytes"`
	} `json:"publication"`
}

func ReadPolicy(root string) (Policy, error) {
	var document struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if err != nil {
		return Policy{}, err
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return Policy{}, fmt.Errorf("workspace policy: %w", err)
	}
	var policy Policy
	if raw := document.Options["@putnami/contributor"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &policy); err != nil {
			return Policy{}, fmt.Errorf("contributor policy: %w", err)
		}
	} else {
		policy.Version = 1
	}
	if policy.Version != 1 {
		return Policy{}, fmt.Errorf("unsupported contributor policy version %d", policy.Version)
	}
	if policy.States.Delivered == "" {
		policy.States.Delivered = collab.TaskStateInProgress
	}
	validState := false
	for _, value := range collab.ValidTaskStates {
		if value == policy.States.Delivered {
			validState = true
		}
	}
	if !validState {
		return Policy{}, fmt.Errorf("invalid delivered task state %q", policy.States.Delivered)
	}
	if policy.Verification.Language != "" && policy.Verification.Language != "en" {
		return Policy{}, fmt.Errorf("unsupported language policy %q", policy.Verification.Language)
	}
	if policy.Publication.BodyMaxBytes < 0 {
		return Policy{}, errors.New("negative publication.bodyMaxBytes")
	}
	if policy.Verification.CIGate.TimeoutMinutes == 0 {
		policy.Verification.CIGate.TimeoutMinutes = 60
	}
	if policy.Verification.CIGate.PollSeconds == 0 {
		policy.Verification.CIGate.PollSeconds = 30
	}
	if policy.Verification.CIGate.TimeoutMinutes < 0 || policy.Verification.CIGate.PollSeconds < 0 {
		return Policy{}, errors.New("invalid ciGate timing")
	}
	return policy, nil
}

var (
	titlePattern     = regexp.MustCompile(`^(feat|fix|docs|test|refactor|perf|build|ci|chore|style)(\([^()]+\))?!?: .+`)
	testPath         = regexp.MustCompile(`(^|/)(testdata|__tests__|tests?)/|_test\.(go|py)$|(^|/)test_[^/]*\.py$|\.(test|spec)\.[cm]?[jt]sx?$|\.test\.sh$`)
	heading          = regexp.MustCompile(`(?m)^#{1,6}\s`)
	agentMarker      = regexp.MustCompile(`(?im)^\s*co-authored-by:.*(anthropic|openai|claude|codex|gpt-[0-9])|generated (with|by) \[?(claude|codex|chatgpt|openai|anthropic)|claude\.ai/code/session_|^\s*claude-session:|chatgpt\.com/codex/`)
	closingDirective = regexp.MustCompile(`(?im)(^|\s)(close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)[:,]?\s+(([^\s#]+/)?[^\s#]+#|#|https?://[^\s]+/issues/)[[:alnum:]_-]+`)
)

func ValidateTitle(title string, paths []string) error {
	if !titlePattern.MatchString(title) {
		return errors.New("title must use Conventional Commits")
	}
	if (strings.HasPrefix(title, "feat") || strings.HasPrefix(title, "fix")) && len(paths) > 0 {
		allTests := true
		for _, path := range paths {
			if !testPath.MatchString(path) {
				allTests = false
				break
			}
		}
		if allTests {
			return errors.New("every changed path is a test; use a test: title")
		}
	}
	if agentMarker.MatchString(title) {
		return errors.New("title names an agent")
	}
	return nil
}

// NormalizeBody joins wrapped prose while keeping blank lines, list items,
// fenced blocks and indented code at their original boundaries.
func NormalizeBody(input string) string {
	var output []string
	held := ""
	flush := func() {
		if held != "" {
			output = append(output, held)
			held = ""
		}
	}
	fence := false
	for _, line := range strings.Split(strings.TrimRight(input, "\n"), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			flush()
			output = append(output, line)
			fence = !fence
			continue
		}
		if fence || trim == "" {
			flush()
			output = append(output, line)
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			flush()
			output = append(output, line)
			continue
		}
		if listItem(line) {
			flush()
			held = strings.TrimRight(line, " \t")
			continue
		}
		if held == "" {
			held = trim
		} else {
			held += " " + trim
		}
	}
	flush()
	return strings.TrimRight(strings.Join(output, "\n"), "\n")
}

func listItem(line string) bool {
	trim := strings.TrimLeft(line, " \t")
	if len(trim) < 2 {
		return false
	}
	if strings.ContainsRune("-*+", rune(trim[0])) {
		return trim[1] == ' ' || trim[1] == '\t'
	}
	i := 0
	for i < len(trim) && trim[i] >= '0' && trim[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(trim) || (trim[i] != '.' && trim[i] != ')') {
		return false
	}
	return trim[i+1] == ' ' || trim[i+1] == '\t'
}

func ValidatePublicationText(title, body, proof string, policy Policy) error {
	if heading.MatchString(body) {
		return errors.New("proposal body must have no Markdown headings")
	}
	if len(body) > 65536 {
		return fmt.Errorf("proposal body exceeds contract limit: %d bytes", len(body))
	}
	if policy.Publication.BodyMaxBytes > 0 && len(body) > policy.Publication.BodyMaxBytes {
		return fmt.Errorf("proposal body is %d bytes; policy allows %d", len(body), policy.Publication.BodyMaxBytes)
	}
	combined := title + "\n\n" + body + "\n" + proof
	if agentMarker.MatchString(combined) {
		return errors.New("proposal title, body or proof names an agent")
	}
	if policy.Verification.Language == "en" && shared.EvaluateLanguage("proposal title, body and proof", combined) != nil {
		return errors.New("proposal title, body or proof has non-English signals")
	}
	return nil
}

func PartialClosing(text string) bool { return closingDirective.MatchString(text) }

func TaskReference(policy Policy, task collab.Task, partial bool) (string, error) {
	if task.Ref.Source == "" || task.Ref.ID == "" {
		return "", errors.New("task reference is incomplete")
	}
	if partial {
		if task.URL != "" {
			return "Refs " + task.URL, nil
		}
		return "Task: " + task.Ref.Source + "/" + task.Ref.ID + " (partial delivery)", nil
	}
	template := policy.Publication.TaskReference
	if template == "" {
		return "", nil
	}
	if strings.Contains(template, "{url}") {
		if task.URL == "" {
			return "", errors.New("task URL required by publication.taskReference")
		}
		return strings.ReplaceAll(template, "{url}", task.URL), nil
	}
	if strings.Contains(template, "{id}") {
		if policy.Proposals.Source == "" || policy.Proposals.Source != task.Ref.Source {
			return "", errors.New("id-only taskReference requires matching task and proposal sources; use {url}")
		}
		if _, err := strconv.Atoi(task.Ref.ID); err != nil {
			return "", errors.New("id-only taskReference requires a numeric task ID")
		}
		return strings.ReplaceAll(template, "{id}", task.Ref.ID), nil
	}
	return "", errors.New("taskReference must contain {url} or {id}")
}

// BoundTaskReference allows an id-only closing template only when the
// resolved proposal binding itself proves the same GitHub repository as the
// task. Policy text alone cannot establish where the provider will publish.
func BoundTaskReference(policy Policy, task collab.Task, partial bool, capabilities collab.Capabilities) (string, error) {
	if !partial && strings.Contains(policy.Publication.TaskReference, "{id}") {
		source, ok := boundGitHubProposalSource(capabilities)
		if !ok || !strings.EqualFold(source, task.Ref.Source) || !strings.EqualFold(source, policy.Proposals.Source) {
			return "", errors.New("id-only taskReference requires the actual proposal binding and task to serve the same source; use {url}")
		}
	}
	return TaskReference(policy, task, partial)
}

func boundGitHubProposalSource(capabilities collab.Capabilities) (string, bool) {
	if capabilities.Status != collab.BindingStatusBound || capabilities.Binding == nil || capabilities.Provider == nil ||
		capabilities.Provider.Name != "@putnami/github-collaboration" || capabilities.Binding.Provider != capabilities.Provider.Name ||
		capabilities.Binding.Version != 1 {
		return "", false
	}
	var settings struct {
		Repository string `json:"repository"`
		Host       string `json:"host"`
	}
	if err := json.Unmarshal(capabilities.Binding.Settings, &settings); err != nil {
		return "", false
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`).MatchString(settings.Repository) {
		return "", false
	}
	host := settings.Host
	if host == "" || strings.EqualFold(host, "github.com") {
		return "github:" + strings.ToLower(settings.Repository), true
	}
	if strings.ContainsAny(host, "/:@?# ") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", false
	}
	return "github:" + strings.ToLower(host) + "/" + strings.ToLower(settings.Repository), true
}
