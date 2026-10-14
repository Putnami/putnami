package agentctx

import (
	"bytes"
	"encoding/json"
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

// contributorSkills are the complete active entry-point inventory.
var contributorSkills = []string{
	"build", "spec", "design", "prototype", "review", "adr", "check", "content-bump",
}

var contributorRemovedSkills = []string{
	"plan", "execute", "fix", "epic", "code-review", "fix-loop",
	"putnami-plan", "putnami-change", "putnami-check", "putnami-review",
}

// contributorWorkers are the worker profiles the extension ships.
var contributorWorkers = []string{"fix-light", "fix-standard", "fix-heavy", "epic-analyst"}

// contributorExplicitOnly are the skills that never run on implicit selection:
// they mutate shared state in bulk or only apply to a configured integration.
var contributorExplicitOnly = []string{"content-bump"}

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
	// Every shipped skill and worker is named here, so an added entry point
	// changes the explicit inventory and its invocation policy on purpose.
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

// stageContributorPackage stages the contributor's manifest and agent content
// at version through the SDK step every packager runs, and returns the stage
// directory and the built content. The contributor declares a runtime, so its
// language packager ships the executable beside this stage.
func stageContributorPackage(t *testing.T, version string) (string, *sdkagentartifact.Result) {
	t.Helper()
	stage := t.TempDir()
	authored, err := os.ReadFile(filepath.Join(contributorRoot(t), "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(authored, &manifest); err != nil {
		t.Fatal(err)
	}
	stampedVersion, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	manifest["version"] = stampedVersion
	stamped, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writePathFile(t, stage, "putnami.extension.json", string(stamped)+"\n")
	content, err := sdkagentartifact.StageExtensionContent(contributorRoot(t), stage, contributorExtension, version)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil {
		t.Fatal("the contributor manifest declares no agent content to stage")
	}
	return stage, content
}

func TestContributorPackagedAndSourceConsumerMigration(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "canonical-consumer-migration", "packaged-canonical-consumer-content")
	stage, content := stageContributorPackage(t, "0.1.0")
	for _, packaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "package"}[packaged], func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "tools", "contributor")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if packaged {
				copyTree(t, stage, dir, "putnami.extension.json", "content")
			} else {
				copyTree(t, contributorRoot(t), dir, "putnami.json", "putnami.extension.json", "src")
			}
			cfg := localContentConfig("/tools/contributor", contributorExtension)
			var report bytes.Buffer
			changed, err := MaterializeLocalAgentContent(root, cfg, &report)
			if err != nil || !changed {
				t.Fatalf("first install changed=%v, err=%v: %s", changed, err, report.String())
			}
			for name, want := range content.Files {
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("consumer %s differs from packaged source: %v", name, err)
				}
			}
			changed, err = MaterializeLocalAgentContent(root, cfg, &report)
			if err != nil || changed {
				t.Fatalf("repeat install changed=%v, err=%v", changed, err)
			}
			for _, host := range []string{".agents", ".claude"} {
				writePathFile(t, root, host+"/skills/build/SKILL.md", "Local build instructions.\n")
			}
			_, err = MaterializeLocalAgentContent(root, cfg, &report)
			if err == nil {
				t.Fatal("installation did not report edited managed content")
			}
			for _, host := range []string{".agents", ".claude"} {
				got, err := os.ReadFile(filepath.Join(root, host, "skills", "build", "SKILL.md"))
				if err != nil || string(got) != "Local build instructions.\n" {
					t.Fatalf("edited %s content was not protected: %v", host, err)
				}
			}
		})
	}
}

func TestManagedUpgradeRemovesUnchangedLegacySkillAndProtectsEditedCopy(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "unchanged"
		if edited {
			name = "edited"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeLocalContentExtension(t, root, "/tools/workflows", "@local/workflows", "build", "Canonical build.\n")
			writePathFile(t, dir, "src/skills/plan/SKILL.md", "---\nname: plan\ndescription: Legacy plan\n---\n\nLegacy plan.\n")
			cfg := localContentConfig("/tools/workflows", "@local/workflows")
			if changed, err := MaterializeLocalAgentContent(root, cfg, nil); err != nil || !changed {
				t.Fatalf("install legacy fixture changed=%v, err=%v", changed, err)
			}
			for _, host := range []string{".agents", ".claude"} {
				if _, statErr := os.Stat(filepath.Join(root, host, "skills", "plan", "SKILL.md")); statErr != nil {
					t.Fatalf("legacy fixture was not installed on %s: %v", host, statErr)
				}
			}
			if edited {
				writePathFile(t, root, ".agents/skills/plan/SKILL.md", "Local protected plan edit.\n")
			}
			if err := os.RemoveAll(filepath.Join(dir, "src", "skills", "plan")); err != nil {
				t.Fatal(err)
			}
			changed, err := MaterializeLocalAgentContent(root, cfg, nil)
			if edited {
				if err == nil {
					t.Fatal("edited managed legacy skill was removed without a conflict")
				}
				got, readErr := os.ReadFile(filepath.Join(root, ".agents", "skills", "plan", "SKILL.md"))
				if readErr != nil || string(got) != "Local protected plan edit.\n" {
					t.Fatalf("edited managed legacy skill was not protected: %q, %v", got, readErr)
				}
				if _, statErr := os.Stat(filepath.Join(root, ".claude", "skills", "plan", "SKILL.md")); statErr != nil {
					t.Fatalf("collision removed the other host's managed copy: %v", statErr)
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("remove unchanged legacy skill changed=%v, err=%v", changed, err)
			}
			for _, host := range []string{".agents", ".claude"} {
				if _, statErr := os.Stat(filepath.Join(root, host, "skills", "plan", "SKILL.md")); !os.IsNotExist(statErr) {
					t.Errorf("unchanged legacy skill survived on %s: %v", host, statErr)
				}
			}
		})
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

func TestContributorReviewMetadataKeepsTheReviewQualityFloor(t *testing.T) {
	profile, err := os.ReadFile(filepath.Join(contributorRoot(t), "src", "agents", "epic-analyst", "claude.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	modelLine := regexp.MustCompile(`(?m)^model: .+$`).FindString(string(profile))
	if modelLine == "" {
		t.Fatal("the analysis profile declares no model")
	}
	result := buildContributorContent(t)
	for _, skill := range []string{"review"} {
		metadata, err := os.ReadFile(filepath.Join(contributorRoot(t), "src", "skills", skill, "agents", "claude.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if got := regexp.MustCompile(`(?m)^model: .+$`).FindString(string(metadata)); got != modelLine {
			t.Errorf("%s model=%s, review floor=%s", skill, got, modelLine)
		}
		if !strings.Contains(string(result.Files[".claude/skills/"+skill+"/SKILL.md"]), modelLine) {
			t.Errorf("%s does not render the declared review floor", skill)
		}
	}
}

func TestContributorShipsOnlyCanonicalEntriesAndActiveUtilities(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "canonical-entry-points", "only-canonical-entry-points-and-active-utilities")
	result := buildContributorContent(t)
	for _, removed := range contributorRemovedSkills {
		if _, err := os.Stat(filepath.Join(contributorRoot(t), "src", "skills", removed)); !os.IsNotExist(err) {
			t.Errorf("removed contributor source %s remains: %v", removed, err)
		}
		for _, host := range []string{".agents", ".claude"} {
			if _, present := result.Files[host+"/skills/"+removed+"/SKILL.md"]; present {
				t.Errorf("removed skill %s still renders on %s", removed, host)
			}
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
		skill := filepath.Join(root, "src", "skills", "build", "SKILL.md")
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
