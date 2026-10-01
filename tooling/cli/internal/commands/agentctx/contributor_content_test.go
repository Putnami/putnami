package agentctx

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
)

// The contributor extension (tooling/contributor) is the one maintained
// implementation of the contributor workflows. These tests read its authored
// source and the tree the packager's builder produces from it.

const contributorExtension = "@putnami/contributor"

// contributorSkills are the entry points the extension must ship. The last
// four are deprecated aliases (contributorAliases).
var contributorSkills = []string{
	"plan", "execute", "fix", "epic", "check", "code-review", "fix-loop", "content-bump",
	"putnami-plan", "putnami-change", "putnami-check", "putnami-review",
}

// contributorWorkers are the worker profiles the extension ships.
var contributorWorkers = []string{"fix-light", "fix-standard", "fix-heavy", "epic-analyst"}

// contributorAliases maps each deprecated entry point to the canonical skill
// that replaces it.
var contributorAliases = map[string]string{
	"putnami-plan":   "plan",
	"putnami-change": "execute",
	"putnami-check":  "check",
	"putnami-review": "code-review",
}

// contributorExplicitOnly are the skills that never run on implicit selection:
// they mutate shared state in bulk or only apply to a configured integration.
var contributorExplicitOnly = []string{"content-bump", "epic", "fix-loop"}

func contributorRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(driftRepositoryRoot(t), "tooling", "contributor")
}

func buildContributorContent(t *testing.T) *sdkagentartifact.Result {
	t.Helper()
	result, err := sdkagentartifact.BuildExtensionContent(contributorRoot(t), "src", contributorExtension, localContentProbeVersion)
	if err != nil {
		t.Fatalf("build the contributor content: %v", err)
	}
	return result
}

func TestContributorContentRendersEveryEntryPointOnBothHosts(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "one-contributor-extension", "every-skill-and-worker-renders-on-both-hosts")
	result := buildContributorContent(t)
	for _, skill := range contributorSkills {
		for _, host := range []string{".agents/skills/", ".claude/skills/"} {
			path := host + skill + "/SKILL.md"
			if _, ok := result.Files[path]; !ok {
				t.Errorf("the contributor content does not render %s", path)
			}
		}
	}
	for _, worker := range contributorWorkers {
		for _, path := range []string{".claude/agents/" + worker + ".md", ".codex/agents/" + worker + ".toml"} {
			if _, ok := result.Files[path]; !ok {
				t.Errorf("the contributor content does not render %s", path)
			}
		}
	}
	// Every shipped skill and worker is one this test names, so a new entry
	// point is added here, with its aliases and invocation policy, on purpose.
	var extra []string
	for path := range result.Files {
		parts := strings.Split(path, "/")
		if len(parts) >= 3 && parts[0] == ".agents" && parts[1] == "skills" && !contains(contributorSkills, parts[2]) {
			extra = append(extra, parts[2])
		}
		if len(parts) == 3 && parts[0] == ".claude" && parts[1] == "agents" && !contains(contributorWorkers, strings.TrimSuffix(parts[2], ".md")) {
			extra = append(extra, parts[2])
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("the contributor content ships entry points this test does not name: %v", extra)
	}
}

func TestContributorExplicitOnlySkillsDeclareItOnBothHosts(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "explicit-only-invocation", "explicit-only-skills-declare-it-on-both-hosts")
	result := buildContributorContent(t)
	var claudeExplicit, codexExplicit []string
	for _, skill := range contributorSkills {
		claude := string(result.Files[".claude/skills/"+skill+"/SKILL.md"])
		front := claude
		if end := strings.Index(claude[3:], "\n---"); strings.HasPrefix(claude, "---") && end >= 0 {
			front = claude[:end+3]
		}
		if strings.Contains(front, "\ndisable-model-invocation: true") {
			claudeExplicit = append(claudeExplicit, skill)
		}
		if strings.Contains(string(result.Files[".agents/skills/"+skill+"/agents/openai.yaml"]), "allow_implicit_invocation: false") {
			codexExplicit = append(codexExplicit, skill)
		}
	}
	want := strings.Join(contributorExplicitOnly, ",")
	if got := strings.Join(claudeExplicit, ","); !sameSet(got, want) {
		t.Errorf("Claude explicit-only skills = %s, want %s", got, want)
	}
	if got := strings.Join(codexExplicit, ","); !sameSet(got, want) {
		t.Errorf("Codex explicit-only skills = %s, want %s", got, want)
	}
}

func TestContributorAliasesNameAnExistingCanonicalSkill(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "entry-point-aliases", "each-alias-names-an-existing-canonical-skill")
	result := buildContributorContent(t)
	for alias, canonical := range contributorAliases {
		body := string(result.Files[".agents/skills/"+alias+"/SKILL.md"])
		if !strings.Contains(body, "deprecated alias of `"+canonical+"`") {
			t.Errorf("%s does not declare itself a deprecated alias of %s", alias, canonical)
		}
		if !strings.Contains(body, "`.agents/skills/"+canonical+"/SKILL.md`") {
			t.Errorf("%s does not name the canonical workflow file of %s", alias, canonical)
		}
		if _, ok := result.Files[".agents/skills/"+canonical+"/SKILL.md"]; !ok {
			t.Errorf("%s names %s, which the contributor content does not ship", alias, canonical)
		}
		if contains(contributorExplicitOnly, alias) != contains(contributorExplicitOnly, canonical) {
			t.Errorf("%s and %s disagree on explicit-only invocation", alias, canonical)
		}
	}
}

// neutralityRules are what provider-neutral content never carries: a backend
// client invocation, a tracker label, or a private memory store's identity or
// layout. Providers own all three.
var neutralityRules = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"a gh invocation", regexp.MustCompile("(^|[\\s;|&(`$])gh\\s+(api|auth|browse|gist|issue|label|pr|release|repo|run|search|secret|workflow)\\b")},
	{"a tracker label", regexp.MustCompile(`\b(status|source|severity|priority|group|domain|tier)/[a-z]`)},
	{"a hosted repository address", regexp.MustCompile(`(?i)github\.com|gitlab\.com`)},
	{"a private memory identity or layout", regexp.MustCompile(`(?i)agents` + `-memory|missions/|morning\.md|lanes\.md|collaboration/memory`)},
	{"this repository's identity", regexp.MustCompile(`(?i)putnami/putnami|sites/putnami\.dev|\.ai/domains\.json`)},
}

func TestContributorContentIsProviderNeutral(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-content", "no-backend-client-label-or-memory-identity")
	result := buildContributorContent(t)
	names := make([]string, 0, len(result.Files))
	for name := range result.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := string(result.Files[name])
		for _, rule := range neutralityRules {
			if match := rule.pattern.FindString(content); match != "" {
				t.Errorf("%s carries %s: %q", name, rule.name, strings.TrimSpace(match))
			}
		}
	}
	// The rules above only mean something while the helpers actually publish:
	// the finalizer must reach proposals and tasks through the contracts.
	finalizer := string(result.Files[".agents/skills/fix/scripts/finalize-pr.sh"])
	for _, call := range []string{`collab proposals upsert`, `collab proposals status`, `collab tasks transition`} {
		if !strings.Contains(finalizer, call) {
			t.Errorf("the finalizer no longer calls %q", call)
		}
	}
}

// modelIdentity matches a host model name or identifier.
var modelIdentity = regexp.MustCompile(`(?i)\b(gpt-[0-9]|claude-[a-z]|opus|sonnet|haiku|fable|astra|luna)\b|\bsol\b`)

func TestContributorInstructionsNameProfilesNotModels(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "repository-policy", "instructions-name-worker-profiles-not-models")
	root := filepath.Join(contributorRoot(t), "src")
	var bodies []string
	for _, pattern := range []string{"skills/*/SKILL.md", "skills/*/references/*.md", "agents/*/AGENT.md"} {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, matches...)
	}
	if len(bodies) < len(contributorSkills)+len(contributorWorkers) {
		t.Fatalf("found %d instruction bodies, fewer than the entry points; the walk is broken", len(bodies))
	}
	for _, path := range bodies {
		if strings.HasSuffix(path, filepath.Join("plan", "references", "recipes.md")) {
			continue // generated from the recipe indexes; it names no model
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if match := modelIdentity.FindString(string(content)); match != "" {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s names the model %q: instructions select a worker profile, whose host metadata carries the model", rel, match)
		}
	}
}

// fix-loop reconciles and deduplicates a task, then hands exactly that task to
// fix. A backlog-selection flag beside the reference would let fix pick
// another task than the one the loop reconciled.
func TestContributorLoopHandsFixTheTaskItReconciled(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join(contributorRoot(t), "src", "skills", "fix-loop", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	loop := string(content)
	if !strings.Contains(loop, "invoke `/fix <reference>`") {
		t.Error("fix-loop no longer hands fix the task reference it selected")
	}
	if strings.Contains(loop, "--from-audit") {
		t.Error("fix-loop passes --from-audit, a backlog selection, beside the reference it selected")
	}
}

// directScriptRun matches a script run as a command of its own: a path ending
// in .sh that opens a line (after an optional "$ " prompt), a code span, a
// usage string, a command after &&, || or ;, or a $(...) substitution, and is
// followed by an argument, the end of the line or the closing parenthesis.
// "bash <path>.sh" does not match, because bash opens that command.
var directScriptRun = regexp.MustCompile("(?m)(?:^[ \\t]*(?:\\$[ \\t]+)?|`|usage: |(?:&&|\\|\\||;|\\$\\()[ \\t]*)[^\\s`|\"'()]*\\.sh(?:[ \\t]+[^\\s|]|[ \\t]*$|\\))")

func TestDirectScriptRunPattern(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"`scripts/finalize-pr.sh --base <branch>`",
		"  finalize-pr.sh --base <branch>",
		"usage: english-only.sh (files | tasks)",
		"  tree-fingerprint.sh",
		"$ .agents/skills/fix/scripts/finalize-pr.sh --base main",
		`cd "$root" && .agents/skills/fix/scripts/finalize-pr.sh --base main`,
		"fp=$(.agents/skills/fix/scripts/tree-fingerprint.sh)",
	} {
		if !directScriptRun.MatchString(line) {
			t.Errorf("a direct run is not detected: %q", line)
		}
	}
	for _, line := range []string{
		"`bash .agents/skills/fix/scripts/finalize-pr.sh --base <branch>`",
		"$ bash .agents/skills/fix/scripts/finalize-pr.sh --help",
		"fp=$(bash .agents/skills/fix/scripts/tree-fingerprint.sh)",
		"- `tree-fingerprint.sh` forwards to the canonical tree producer.",
		"    english-only.sh | */english-only.sh) return 0 ;;",
		`ENGLISH_ONLY="$(cd "$dir" && pwd || true)/english-only.sh"`,
	} {
		if directScriptRun.MatchString(line) {
			t.Errorf("a run through bash or a script name is reported as a direct run: %q", line)
		}
	}
}

// The agent artifact carries no file mode, so an installed script is never
// executable: every instruction and help text runs it through bash.
func TestContributorContentRunsScriptsThroughBash(t *testing.T) {
	t.Parallel()
	result := buildContributorContent(t)
	names := make([]string, 0, len(result.Files))
	for name := range result.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, match := range directScriptRun.FindAllString(string(result.Files[name]), -1) {
			t.Errorf("%s runs a script without bash: %q", name, strings.TrimSpace(match))
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameSet(a, b string) bool {
	left, right := strings.Split(a, ","), strings.Split(b, ",")
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, ",") == strings.Join(right, ",")
}

func TestContributorContentPolicyRefusesProviderSpecificContent(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "provider-neutral-content", "the-declared-policy-refuses-provider-specific-content")
	source := contributorRoot(t)
	for _, injected := range []string{
		"Open it with `gh pr create --fill`.",
		"Label it status/needs-review.",
		"Write the checkpoint to missions/42.md.",
		"The token is an access token.",
	} {
		root := t.TempDir()
		copyTree(t, source, root, "putnami.json", "putnami.extension.json", "src")
		skill := filepath.Join(root, "src", "skills", "fix", "SKILL.md")
		body, err := os.ReadFile(skill)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(skill, append(body, []byte("\n"+injected+"\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = sdkagentartifact.BuildExtensionContent(root, "src", contributorExtension, localContentProbeVersion)
		if err == nil || !strings.Contains(err.Error(), "forbidden content") {
			t.Errorf("the contributor content policy accepted %q: %v", injected, err)
		}
	}
}

// copyTree copies the named entries of src into dst.
func copyTree(t *testing.T, src, dst string, names ...string) {
	t.Helper()
	for _, name := range names {
		root := filepath.Join(src, name)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, content, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
