package extensions

import (
	"bytes"
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

const userSkillsExtension = "@acme/skills"

// pinUserSkillsExtension packages an extension whose content ships the skills
// spec and build and names spec for the user scope, and pins it in the user
// scope at userRoot as an install leaves it.
func pinUserSkillsExtension(t *testing.T, userRoot string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"putnami.extension.json":    `{"name":"` + userSkillsExtension + `","agentContent":{"path":"content","source":"src","userScope":["spec"]}}`,
		"putnami.json":              `{"name":"` + userSkillsExtension + `","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["spec"]}}}`,
		"src/skills/spec/SKILL.md":  "---\nname: spec\ndescription: Write a spec\n---\n\n# spec\n",
		"src/skills/build/SKILL.md": "---\nname: build\ndescription: Build\n---\n\n# build\n",
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}
	pkg, err := sdkagentartifact.PackageExtension(root, "1.0.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	// The stable link points into the artifact store, as an install leaves it.
	store := t.TempDir()
	for member, data := range pkg.Members {
		writeFile(t, filepath.Join(store, filepath.FromSlash(member)), string(data))
	}
	link := layout.StableDir(userRoot, layout.Extensions, userSkillsExtension)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(userSkillsExtension, lockfile.LockEntry{Version: "1.0.0", ManifestHash: lockfile.HashBytes(pkg.Manifest)})
	if err := lockfile.WriteLockFile(userRoot, lf); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// The user-scope commands write the named skills under the home directory and
// nothing in the directory they run in, and remove drops them with the pin.
func TestUserScopeCommandsWriteSkillsUnderHomeOnly(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "user-scope-skills", "a-user-scope-install-writes-only-the-named-skills")
	home := t.TempDir()
	original := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = original })
	userRoot := filepath.Join(home, ".putnami", "user")
	pinUserSkillsExtension(t, userRoot)
	caller := t.TempDir()
	writeFile(t, filepath.Join(caller, "README.md"), "a repository\n")
	t.Chdir(caller)

	var out bytes.Buffer
	if err := installUserScopeSkills(userRoot, userSkillsExtension+"@1.0.0", "", &out); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	for _, path := range []string{".claude/skills/spec/SKILL.md", ".agents/skills/spec/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(path))); err != nil {
			t.Errorf("%s was not written under home: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "build")); !os.IsNotExist(err) {
		t.Errorf("an unlisted skill was written: %v", err)
	}
	if got := listFiles(t, caller); strings.Join(got, ",") != "README.md" {
		t.Fatalf("the caller directory changed: %v", got)
	}

	// A structured output keeps its one document: the lines go nowhere.
	out.Reset()
	if err := installUserScopeSkills(userRoot, userSkillsExtension, "json", &out); err != nil || out.Len() != 0 {
		t.Fatalf("structured re-run: %v %q", err, out.String())
	}

	out.Reset()
	if err := ExtensionsRemoveUser(userRoot, []string{"--user", userSkillsExtension}, &out); err != nil {
		t.Fatalf("remove: %v\n%s", err, out.String())
	}
	for _, dir := range []string{".claude", ".agents"} {
		if _, err := os.Stat(filepath.Join(home, dir, "skills")); !os.IsNotExist(err) {
			t.Errorf("remove left %s/skills: %v", dir, err)
		}
	}
	if lf, err := lockfile.ReadLockFile(userRoot); err != nil || lf == nil {
		t.Fatalf("read the lock: %v", err)
	} else if _, pinned := lf.GetExtension(userSkillsExtension); pinned {
		t.Error("remove kept the pin")
	}
	if got := listFiles(t, caller); strings.Join(got, ",") != "README.md" {
		t.Fatalf("the caller directory changed: %v", got)
	}
}
