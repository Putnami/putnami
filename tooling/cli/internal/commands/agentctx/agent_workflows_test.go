package agentctx

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Shared references in shipped contributor entries must resolve on both
// hosts. The native finalizer, language, machine admission and tree commands
// have behavioral tests at their Go API boundaries.
func TestContributorResourceLinks(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(cwd, "..", "..", "..", "..", ".."))
	links := regexp.MustCompile(`\]\(([^)]+)\)`)
	for _, host := range []string{".agents", ".claude"} {
		for _, skill := range contributorSkills {
			path := filepath.Join(root, host, "skills", skill, "SKILL.md")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range links.FindAllSubmatch(body, -1) {
				target := string(match[1])
				if strings.Contains(target, "://") || strings.HasPrefix(target, "#") {
					continue
				}
				target = strings.SplitN(target, "#", 2)[0]
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), target)); err != nil {
					t.Errorf("%s references unavailable resource %s: %v", path, target, err)
				}
			}
		}
	}
}
