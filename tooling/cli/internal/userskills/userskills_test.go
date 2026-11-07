package userskills

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

const userScopeContentName = "@acme/skills"

// pinUserScopeContent packages an extension whose content ships the skills
// spec, build and review and a worker profile, names userScope as its
// user-scope skills, and pins it in the user scope at userRoot exactly as an
// install leaves it: the stable link holds the package, the lock binds its
// manifest.
func pinUserScopeContent(t *testing.T, userRoot, version, specBody string, userScope ...string) {
	t.Helper()
	root := t.TempDir()
	list := ""
	if len(userScope) > 0 {
		list = `,"userScope":["` + strings.Join(userScope, `","`) + `"]`
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.extension.json", `{"name":"`+userScopeContentName+`","agentContent":{"path":"content","source":"src"`+list+`}}`)
	write("putnami.json", `{"name":"`+userScopeContentName+`","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["spec"]}}}`)
	for _, skill := range []string{"spec", "build", "review"} {
		body := "# " + skill + "\n"
		if skill == "spec" {
			body = specBody
		}
		write("src/skills/"+skill+"/SKILL.md", "---\nname: "+skill+"\ndescription: The "+skill+" skill\n---\n\n"+body)
	}
	write("src/skills/spec/references/formats.md", "Spec Kit, OpenSpec, Kiro, Gherkin.\n")
	write("src/agents/reviewer/AGENT.md", "Review.\n")
	write("src/agents/reviewer/claude.yaml", "name: reviewer\ndescription: Reviewer\n")
	write("src/agents/reviewer/codex.toml", "name = \"reviewer\"\n")
	pkg, err := sdkagentartifact.PackageExtension(root, version)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	dir := layout.StableDir(userRoot, layout.Extensions, userScopeContentName)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for member, data := range pkg.Members {
		path := filepath.Join(dir, filepath.FromSlash(member))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(userScopeContentName, lockfile.LockEntry{Version: version, ManifestHash: lockfile.HashBytes(pkg.Manifest)})
	if err := lockfile.WriteLockFile(userRoot, lf); err != nil {
		t.Fatal(err)
	}
}

// homeTree lists every file under home, by slash path.
func homeTree(t *testing.T, home string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(home, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func userScopeDirs(t *testing.T) (home, userRoot string) {
	t.Helper()
	home = t.TempDir()
	return home, filepath.Join(home, ".putnami", "user")
}

func agentFiles(files []string) []string {
	var out []string
	for _, file := range files {
		if !strings.HasPrefix(file, ".putnami/") {
			out = append(out, file)
		}
	}
	return out
}

func TestUserScopeSkills_InstallWritesOnlyTheNamedSkills(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "user-scope-skills", "a-user-scope-install-writes-only-the-named-skills")
	home, userRoot := userScopeDirs(t)
	pinUserScopeContent(t, userRoot, "1.0.0", "Write the spec.\n", "spec")
	var out bytes.Buffer
	if err := Install(userRoot, home, userScopeContentName, &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	want := []string{
		".agents/skills/spec/SKILL.md",
		".agents/skills/spec/references/formats.md",
		".claude/skills/spec/SKILL.md",
	}
	if got := agentFiles(homeTree(t, home)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files under home:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(filepath.Join(userRoot, ".putnami", "agent-artifacts")); err != nil {
		t.Fatalf("the ownership record lives in the user scope: %v", err)
	}
	if !strings.Contains(out.String(), "Skills spec written") {
		t.Fatalf("output = %q", out.String())
	}

	// An extension that names no user-scope skill writes nothing.
	otherHome, otherRoot := userScopeDirs(t)
	pinUserScopeContent(t, otherRoot, "1.0.0", "Write the spec.\n")
	out.Reset()
	if err := Install(otherRoot, otherHome, userScopeContentName, &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := agentFiles(homeTree(t, otherHome)); len(got) != 0 || out.Len() != 0 {
		t.Fatalf("an extension with no user-scope skill wrote %v and said %q", got, out.String())
	}
}

func TestUserScopeSkills_ReinstallWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "user-scope-skills", "a-user-scope-reinstall-writes-nothing")
	home, userRoot := userScopeDirs(t)
	pinUserScopeContent(t, userRoot, "1.0.0", "Write the spec.\n", "spec")
	if err := Install(userRoot, home, userScopeContentName, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(home, ".claude", "skills", "spec", "SKILL.md")
	before, err := os.Stat(skill)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(userRoot, home, userScopeContentName, &out); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(skill)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || !strings.Contains(out.String(), "already in place") {
		t.Fatalf("a re-run rewrote the skill or did not say so: %q", out.String())
	}

	// A new release updates what it wrote.
	pinUserScopeContent(t, userRoot, "1.1.0", "Write the spec, then read it back.\n", "spec")
	if err := Install(userRoot, home, userScopeContentName, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(skill)
	if err != nil || !strings.Contains(string(data), "read it back") {
		t.Fatalf("the new release did not update the skill: %q, %v", data, err)
	}
}

func TestUserScopeSkills_CollisionWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "user-scope-skills", "a-user-scope-collision-writes-nothing")
	home, userRoot := userScopeDirs(t)
	pinUserScopeContent(t, userRoot, "1.0.0", "Write the spec.\n", "spec")
	mine := filepath.Join(home, ".agents", "skills", "spec", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("my own spec skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Install(userRoot, home, userScopeContentName, &out)
	if !errors.Is(err, ErrCollide) {
		t.Fatalf("err = %v, want a collision", err)
	}
	if got := agentFiles(homeTree(t, home)); len(got) != 1 || got[0] != ".agents/skills/spec/SKILL.md" {
		t.Fatalf("a collision wrote files: %v", got)
	}
	if data, _ := os.ReadFile(mine); string(data) != "my own spec skill\n" {
		t.Fatalf("the person's file changed: %q", data)
	}
	if !strings.Contains(out.String(), ".agents/skills/spec/SKILL.md [unmanaged]") {
		t.Fatalf("the report must name the preserved file: %q", out.String())
	}
}

func TestUserScopeSkills_RemoveRemovesWhatItWrote(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "user-scope-skills", "a-user-scope-remove-removes-what-it-wrote")
	home, userRoot := userScopeDirs(t)
	pinUserScopeContent(t, userRoot, "1.0.0", "Write the spec.\n", "spec")
	if err := Install(userRoot, home, userScopeContentName, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(home, ".claude", "skills", "spec", "SKILL.md")
	if err := os.WriteFile(edited, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Remove(userRoot, home, userScopeContentName, &out); err != nil {
		t.Fatal(err)
	}
	if got := agentFiles(homeTree(t, home)); len(got) != 1 || got[0] != ".claude/skills/spec/SKILL.md" {
		t.Fatalf("after remove, files under home = %v; only the edited one stays", got)
	}
	if !strings.Contains(out.String(), "Kept .claude/skills/spec/SKILL.md") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills")); !os.IsNotExist(err) {
		t.Fatalf("emptied directories stay: %v", err)
	}
	// A second remove finds no record and does nothing.
	out.Reset()
	if err := Remove(userRoot, home, userScopeContentName, &out); err != nil || out.Len() != 0 {
		t.Fatalf("second remove: %v %q", err, out.String())
	}
}
