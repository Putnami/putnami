package agentartifact

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

const (
	// defaultClaudeTools is the capability line written when a skill declares
	// no Claude metadata of its own. An agent artifact is only ever
	// materialized into a Putnami workspace, whose guidance makes a Putnami MCP
	// call the first discovery step, so a skill can load those tools
	// (ToolSearch) and call them (mcp__putnami__*) without a permission prompt.
	defaultClaudeTools = "Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*"
	// explicitOpenAILine and explicitClaudeLine are the two host spellings of
	// the same decision: the skill runs only when a user invokes it.
	explicitOpenAIKey = "allow_implicit_invocation"
	explicitClaudeKey = "disable-model-invocation"
)

// nameFormat constrains skill and worker directory names so an emitted path is
// always a plain workspace-relative slash path.
var nameFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// skillSource is one `skills/<name>` directory of the authoring layout. Host
// metadata is optional and is written beside the skill it describes.
type skillSource struct {
	name       string
	document   []byte
	claudeYAML []byte
	openAIYAML []byte
	assets     map[string][]byte
}

// workerSource is one `agents/<name>` directory of the authoring layout. The
// worker body is the single source both hosts receive in full.
type workerSource struct {
	name       string
	body       []byte
	claudeYAML []byte
	codexTOML  []byte
}

// sourceTree is the validated, order-stable content of one authoring layout.
// label is the layout's directory as its author knows it (`src` for an
// agent-artifact project, the declared source of an extension's contribution),
// so every error names the file to edit.
type sourceTree struct {
	label   string
	skills  []*skillSource
	workers []*workerSource
}

// contentPolicy is the per-project publication rule declared in putnami.json.
// It replaces the builder's former hard-coded public-core assertions so the
// maintainer artifact can carry the opposite policy without a code change.
type contentPolicy struct {
	forbiddenContent []string
	requiredSkills   []string
}

func loadContentPolicy(cfg *wsproto.ProjectConfig) (contentPolicy, error) {
	declared, ok := cfg.Options["agent-artifact"]
	if !ok {
		return contentPolicy{}, fmt.Errorf("agent artifact project %s declares no options.agent-artifact content policy", cfg.Name)
	}
	var policy contentPolicy
	for _, key := range sortedKeys(declared) {
		values, err := stringList(declared[key])
		if err != nil {
			return contentPolicy{}, fmt.Errorf("options.agent-artifact.%s: %w", key, err)
		}
		switch key {
		case "forbiddenContent":
			policy.forbiddenContent = values
		case "requiredSkills":
			policy.requiredSkills = values
		default:
			return contentPolicy{}, fmt.Errorf("options.agent-artifact declares an unknown key %q", key)
		}
	}
	// An empty object would satisfy "declared" while stating no rule at all.
	for _, key := range []string{"forbiddenContent", "requiredSkills"} {
		if _, ok := declared[key]; !ok {
			return contentPolicy{}, fmt.Errorf("options.agent-artifact declares no %s; state it, even as an empty list", key)
		}
	}
	return policy, nil
}

// apply reports every violation at once so a policy failure is one review, not
// a sequence of rebuilds.
func (p contentPolicy) apply(files map[string][]byte, skills []*skillSource) error {
	var violations []string
	for _, name := range sortedKeys(files) {
		lower := strings.ToLower(string(files[name]))
		for _, forbidden := range p.forbiddenContent {
			if strings.Contains(lower, strings.ToLower(forbidden)) {
				violations = append(violations, fmt.Sprintf("%s contains forbidden content %q", name, forbidden))
			}
		}
	}
	present := make(map[string]bool, len(skills))
	for _, skill := range skills {
		present[skill.name] = true
	}
	for _, required := range p.requiredSkills {
		if !present[required] {
			violations = append(violations, fmt.Sprintf("required skill %q has no source", required))
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("declared content policy rejected the build:\n  %s", strings.Join(violations, "\n  "))
	}
	return nil
}

// readSourceTree walks the closed authoring layout rooted at root: `skills/`
// and `agents/`. Any other path is a build error: the builder must never absorb
// content nobody declared. label names root in errors.
func readSourceTree(root, label string) (*sourceTree, error) {
	skills := make(map[string]*skillSource)
	workers := make(map[string]*workerSource)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// Hidden entries (.DS_Store, editor swap files, .git) are never
		// artifact content. Skipping them, rather than failing, keeps one commit
		// building the same bytes on every machine.
		if path != root && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("agent artifact source %q is not a regular file", rel)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return placeSource(skills, workers, rel, content)
	})
	if err != nil {
		return nil, fmt.Errorf("validate agent artifact source tree: %w", err)
	}
	if len(skills) == 0 {
		return nil, fmt.Errorf("agent artifact source tree has no skill under %s/skills", label)
	}

	tree := &sourceTree{label: label}
	for _, name := range sortedKeys(skills) {
		skill := skills[name]
		if skill.document == nil {
			return nil, fmt.Errorf("skill %s is missing %s/skills/%s/SKILL.md", name, label, name)
		}
		tree.skills = append(tree.skills, skill)
	}
	for _, name := range sortedKeys(workers) {
		worker := workers[name]
		for _, missing := range []struct {
			file    string
			content []byte
		}{
			{"AGENT.md", worker.body},
			{"claude.yaml", worker.claudeYAML},
			{"codex.toml", worker.codexTOML},
		} {
			if missing.content == nil {
				return nil, fmt.Errorf("worker %s is missing %s/agents/%s/%s", name, label, name, missing.file)
			}
		}
		tree.workers = append(tree.workers, worker)
	}
	return tree, nil
}

func placeSource(skills map[string]*skillSource, workers map[string]*workerSource, rel string, content []byte) error {
	segments := strings.Split(rel, "/")
	switch {
	case len(segments) >= 3 && segments[0] == "skills":
		name := segments[1]
		if !nameFormat.MatchString(name) {
			return fmt.Errorf("agent artifact source %q uses an invalid skill name %q", rel, name)
		}
		skill := skills[name]
		if skill == nil {
			skill = &skillSource{name: name, assets: make(map[string][]byte)}
			skills[name] = skill
		}
		inner := segments[2:]
		switch {
		case len(inner) == 1 && inner[0] == "SKILL.md":
			skill.document = content
			return nil
		case len(inner) == 2 && inner[0] == "agents" && inner[1] == "claude.yaml":
			skill.claudeYAML = content
			return nil
		case len(inner) == 2 && inner[0] == "agents" && inner[1] == "openai.yaml":
			skill.openAIYAML = content
			return nil
		case len(inner) >= 2 && (inner[0] == "references" || inner[0] == "scripts"):
			skill.assets[strings.Join(inner, "/")] = content
			return nil
		}
	case len(segments) == 3 && segments[0] == "agents":
		name := segments[1]
		if !nameFormat.MatchString(name) {
			return fmt.Errorf("agent artifact source %q uses an invalid worker name %q", rel, name)
		}
		worker := workers[name]
		if worker == nil {
			worker = &workerSource{name: name}
			workers[name] = worker
		}
		switch segments[2] {
		case "AGENT.md":
			worker.body = content
			return nil
		case "claude.yaml":
			worker.claudeYAML = content
			return nil
		case "codex.toml":
			worker.codexTOML = content
			return nil
		}
	}
	return fmt.Errorf("undeclared agent artifact source %q", rel)
}

// emitFiles projects the source tree onto the workspace paths each host reads.
// Every host file carries the complete body: no emitted file redirects a
// reader to another path.
func emitFiles(tree *sourceTree) (map[string][]byte, error) {
	files := make(map[string][]byte, len(tree.skills)*2+len(tree.workers)*2)
	for _, skill := range tree.skills {
		front, body, err := splitSkillDocument(skill)
		if err != nil {
			return nil, err
		}
		if err := checkSkillHostMetadata(skill, tree.label); err != nil {
			return nil, err
		}
		prefix := ".agents/skills/" + skill.name
		files[prefix+"/SKILL.md"] = skill.document
		if skill.openAIYAML != nil {
			files[prefix+"/agents/openai.yaml"] = skill.openAIYAML
		}
		for _, asset := range sortedKeys(skill.assets) {
			files[prefix+"/"+asset] = skill.assets[asset]
		}
		files[".claude/skills/"+skill.name+"/SKILL.md"] = claudeSkillDocument(skill, front, body)
	}
	for _, worker := range tree.workers {
		if err := checkWorker(worker, tree.label); err != nil {
			return nil, err
		}
		files[".claude/agents/"+worker.name+".md"] = frontmatterDocument(worker.claudeYAML, worker.body)
		files[".codex/agents/"+worker.name+".toml"] = codexDocument(worker)
	}
	return files, nil
}

// splitSkillDocument reads the neutral frontmatter without a YAML parser: the
// source fence is closed to name and description, so line splitting is exact.
func splitSkillDocument(skill *skillSource) (map[string]string, []byte, error) {
	text := string(skill.document)
	if strings.Contains(text, "\r\n") {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md uses CRLF line endings; the source layout is LF only", skill.name)
	}
	if !strings.HasPrefix(text, "---\n") {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md must open with a --- frontmatter fence", skill.name)
	}
	fence, remainder, closed := strings.Cut(text[len("---\n"):], "\n---\n")
	if !closed {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md frontmatter is not closed", skill.name)
	}
	front := make(map[string]string, 2)
	for line := range strings.SplitSeq(fence, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || (key != "name" && key != "description") {
			return nil, nil, fmt.Errorf("skill %s: SKILL.md frontmatter accepts only name and description, got %q", skill.name, line)
		}
		if _, duplicate := front[key]; duplicate {
			return nil, nil, fmt.Errorf("skill %s: SKILL.md frontmatter declares %s twice", skill.name, key)
		}
		front[key] = strings.TrimSpace(value)
	}
	if front["name"] != skill.name {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md declares name %q", skill.name, front["name"])
	}
	if front["description"] == "" {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md declares no description", skill.name)
	}
	body := strings.TrimLeft(remainder, "\n")
	if body == "" {
		return nil, nil, fmt.Errorf("skill %s: SKILL.md has no body", skill.name)
	}
	return front, []byte(body), nil
}

func checkSkillHostMetadata(skill *skillSource, label string) error {
	if skill.claudeYAML != nil {
		where := fmt.Sprintf("%s/skills/%s/agents/claude.yaml", label, skill.name)
		if err := checkClaudeFrontmatter(skill.name, where, skill.claudeYAML); err != nil {
			return err
		}
	}
	if scalarIs(skill.openAIYAML, explicitOpenAIKey, "false") != scalarIs(skill.claudeYAML, explicitClaudeKey, "true") {
		return fmt.Errorf("skill %s: explicit-only invocation declared for one host only", skill.name)
	}
	return nil
}

func checkWorker(worker *workerSource, label string) error {
	where := fmt.Sprintf("%s/agents/%s/claude.yaml", label, worker.name)
	if err := checkClaudeFrontmatter(worker.name, where, worker.claudeYAML); err != nil {
		return err
	}
	if bytes.Contains(worker.codexTOML, []byte("developer_instructions")) {
		return fmt.Errorf("worker %s: %s/agents/%s/codex.toml already declares developer_instructions", worker.name, label, worker.name)
	}
	if bytes.Contains(worker.body, []byte("'''")) {
		return fmt.Errorf("worker %s: %s/agents/%s/AGENT.md contains ''' and cannot be embedded in TOML", worker.name, label, worker.name)
	}
	if strings.HasPrefix(string(worker.body), "---\n") {
		return fmt.Errorf("worker %s: %s/agents/%s/AGENT.md must carry the body only, not a frontmatter fence", worker.name, label, worker.name)
	}
	if strings.TrimSpace(string(worker.body)) == "" {
		return fmt.Errorf("worker %s: %s/agents/%s/AGENT.md is empty", worker.name, label, worker.name)
	}
	// A key after a [table] header belongs to that table, so the appended
	// developer_instructions would stop being the worker's instructions.
	for line := range strings.SplitSeq(string(worker.codexTOML), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			return fmt.Errorf("worker %s: %s/agents/%s/codex.toml declares a table (%s); developer_instructions is appended after it and would land inside it — keep codex.toml to top-level keys", worker.name, label, worker.name, strings.TrimSpace(line))
		}
	}
	return nil
}

func checkClaudeFrontmatter(name, where string, content []byte) error {
	if hasLine(content, "---") {
		return fmt.Errorf("%s contains a --- fence; the builder writes the fences", where)
	}
	if !hasLine(content, "name: "+name) {
		return fmt.Errorf("%s must declare name: %s", where, name)
	}
	if value, ok := scalarValue(content, "description"); !ok || value == "" {
		return fmt.Errorf("%s must declare a description", where)
	}
	return nil
}

// scalarValue reads a `key: value` line at any indentation without a YAML
// parser: it drops a trailing comment and surrounding quotes. Host metadata is copied
// verbatim, so only the few keys the builder checks are ever read this way.
func scalarValue(content []byte, key string) (string, bool) {
	for line := range strings.SplitSeq(string(content), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimLeft(line, " \t"), key+":")
		if !ok {
			continue
		}
		if comment := strings.Index(rest, " #"); comment >= 0 {
			rest = rest[:comment]
		}
		rest = strings.TrimSpace(rest)
		rest = strings.Trim(rest, `"'`)
		return rest, true
	}
	return "", false
}

func scalarIs(content []byte, key, want string) bool {
	value, ok := scalarValue(content, key)
	return ok && strings.EqualFold(value, want)
}

func claudeSkillDocument(skill *skillSource, front map[string]string, body []byte) []byte {
	yaml := skill.claudeYAML
	if yaml == nil {
		yaml = fmt.Appendf(nil, "name: %s\ndescription: %s\nallowed-tools: %s\n", front["name"], front["description"], defaultClaudeTools)
	}
	return frontmatterDocument(yaml, body)
}

func frontmatterDocument(yaml, body []byte) []byte {
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(terminated(yaml))
	out.WriteString("---\n\n")
	out.Write(terminated(body))
	return out.Bytes()
}

// codexDocument appends the worker body as a TOML multi-line literal string:
// the ”' guard in checkWorker is what makes verbatim embedding safe.
func codexDocument(worker *workerSource) []byte {
	var out bytes.Buffer
	out.Write(terminated(worker.codexTOML))
	out.WriteString("\ndeveloper_instructions = '''\n")
	out.Write(terminated(worker.body))
	out.WriteString("'''\n")
	return out.Bytes()
}

func hasLine(content []byte, want string) bool {
	for line := range strings.SplitSeq(string(content), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func terminated(content []byte) []byte {
	if len(content) == 0 || bytes.HasSuffix(content, []byte("\n")) {
		return content
	}
	return append(append([]byte(nil), content...), '\n')
}

func stringList(value any) ([]string, error) {
	entries, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("must be an array of strings")
	}
	out := make([]string, 0, len(entries))
	for index, entry := range entries {
		text, ok := entry.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("entry %d must be a non-empty string", index)
		}
		out = append(out, text)
	}
	return out, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
