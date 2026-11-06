package gittest

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The identity a foreign machine sets in its environment and its user
// configuration.
const (
	ForeignName  = "Foreign Machine"
	ForeignEmail = "foreign@example.invalid"
)

// foreignConfig is a user configuration that changes every commit that reads
// it: it signs commits and tags with a gpg that does not exist, rejects every
// commit with a hook, names another identity and another default branch.
const foreignConfig = `[user]
	name = ` + ForeignName + `
	email = ` + ForeignEmail + `
[commit]
	gpgsign = true
[tag]
	gpgsign = true
	forceSignAnnotated = true
[gpg]
	program = %DIR%/no-gpg
[core]
	hooksPath = %DIR%/hooks
[init]
	defaultBranch = trunk
`

// ForeignMachine gives the rest of the test the git environment of another
// machine: a foreign identity in the GIT_AUTHOR_ and GIT_COMMITTER_
// variables, a foreign user configuration in GIT_CONFIG_GLOBAL,
// XDG_CONFIG_HOME and GIT_CONFIG_COUNT, a global ignore file that ignores
// every file, and a GIT_DIR that is not the fixture. A fixture that reads any
// of them gives another repository, or fails. The test cannot run in parallel.
func ForeignMachine(t testing.TB) {
	t.Helper()
	dir := t.TempDir()
	config := strings.ReplaceAll(foreignConfig, "%DIR%", filepath.ToSlash(dir))
	files := map[string]string{
		"gitconfig":                       config,
		"xdg/git/config":                  config,
		"xdg/git/ignore":                  "*\n",
		"hooks/pre-commit":                "#!/bin/sh\nexit 1\n",
		"hooks/commit-msg":                "#!/bin/sh\nexit 1\n",
		"not-the-fixture.git/placeholder": "",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL":   filepath.Join(dir, "gitconfig"),
		"XDG_CONFIG_HOME":     filepath.Join(dir, "xdg"),
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "user.name",
		"GIT_CONFIG_VALUE_0":  ForeignName,
		"GIT_AUTHOR_NAME":     ForeignName,
		"GIT_AUTHOR_EMAIL":    ForeignEmail,
		"GIT_COMMITTER_NAME":  ForeignName,
		"GIT_COMMITTER_EMAIL": ForeignEmail,
		"GIT_DIR":             filepath.Join(dir, "not-the-fixture.git"),
	} {
		t.Setenv(key, value)
	}
}

// Describe returns what a fixture put in the repository at dir, in a form two
// machines can compare: the git directory, the current branch, every ref,
// every commit's tree, identity and message, every tag's identity and
// message, the index and the untracked files. It leaves out the object IDs and
// dates a fixture does not pin. A dir that holds no repository of its own
// describes as such.
func Describe(t testing.TB, dir string) string {
	t.Helper()
	read := func(args ...string) string {
		out, err := Command(dir, args...).CombinedOutput()
		if err != nil {
			return "error: " + strings.TrimSpace(string(out))
		}
		return strings.TrimSpace(string(out))
	}
	gitDir := read("rev-parse", "--absolute-git-dir")
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found := filepath.Clean(filepath.FromSlash(gitDir)); found != filepath.Join(resolved, ".git") && found != resolved {
		return "no repository at the fixture: " + gitDir
	}
	var out strings.Builder
	section := func(name, body string) {
		out.WriteString("## " + name + "\n" + body + "\n")
	}
	section("bare", read("rev-parse", "--is-bare-repository"))
	section("branch", read("symbolic-ref", "-q", "HEAD"))
	section("refs", read("for-each-ref", "--format=%(refname) %(objecttype) %(taggername) %(taggeremail) %(contents:subject)"))
	section("commits", describeCommits(read("log", "--all", "--format=%H%x00%T%x00%P%x00%an <%ae> %cn <%ce> %G?%x00%B%x1e")))
	if read("rev-parse", "--is-bare-repository") == "false" {
		section("index", read("ls-files", "--stage"))
		section("untracked", read("ls-files", "--others", "--exclude-standard"))
	}
	return out.String()
}

// describeCommits turns a log of NUL-separated commit ID, tree, parent IDs,
// identity and message records into one line per commit: its tree, the trees
// of its parents, its identity and its quoted message, sorted. A commit ID
// and the order of log depend on the dates of a commit, which a fixture does
// not pin, so neither appears.
func describeCommits(log string) string {
	if strings.HasPrefix(log, "error: ") {
		return log
	}
	type commit struct{ tree, parents, rest string }
	commits := map[string]commit{}
	for _, record := range strings.Split(log, "\x1e") {
		fields := strings.SplitN(strings.TrimLeft(record, "\n"), "\x00", 5)
		if len(fields) != 5 {
			continue
		}
		commits[fields[0]] = commit{tree: fields[1], parents: fields[2], rest: fields[3] + " " + strconv.Quote(strings.TrimSpace(fields[4]))}
	}
	lines := make([]string, 0, len(commits))
	for _, c := range commits {
		var parents []string
		for _, id := range strings.Fields(c.parents) {
			if parent, ok := commits[id]; ok {
				parents = append(parents, parent.tree)
			} else {
				parents = append(parents, "shallow")
			}
		}
		lines = append(lines, c.tree+" parents ["+strings.Join(parents, " ")+"] "+c.rest)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// AssertMachineIndependent builds a fixture twice, on this machine and under
// ForeignMachine, and fails the test when the two repositories at the
// returned directory differ. build runs the fixture and returns the directory
// of its repository; a fixture that fails under the foreign machine ends the
// test there. The test cannot run in parallel.
func AssertMachineIndependent(t *testing.T, build func() string) {
	t.Helper()
	want := Describe(t, build())
	ForeignMachine(t)
	if got := Describe(t, build()); got != want {
		t.Fatalf("the fixture differs under a foreign git environment\n--- this machine ---\n%s\n--- foreign machine ---\n%s", want, got)
	}
}
