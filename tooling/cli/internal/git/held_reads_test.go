package git

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/sdk/extension/procstat"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// These tests install the process's held answers, so none of them runs in
// parallel.

// heldCalls are the calls of the held table the tests ask, each as a caller
// asks it.
var heldCalls = [][]string{
	{"rev-parse", "--abbrev-ref", "HEAD"},
	{"rev-parse", "--short", "HEAD"},
	{"rev-parse", "HEAD"},
	{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"},
	{"rev-parse", "--is-shallow-repository"},
	{"rev-parse", "--git-common-dir"},
	{"rev-parse", "--show-toplevel"},
	{"show", "-s", "--format=%ct", "--end-of-options", "HEAD"},
	{"tag", "--points-at", "HEAD"},
	{"for-each-ref", "--format=%(refname)", "refs/tags"},
	{"status", "--porcelain"},
	{"diff", "HEAD", "--no-color"},
	{"status", "--porcelain=v2", "-z", "--no-renames", "--ignore-submodules=none", "-uno"},
	{"ls-files", "--others", "--exclude-standard", "-z"},
	{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z"},
}

// ignoreQuestion is what the tests ask the ignore rules (IgnoresDirectories).
const ignoreQuestion = "build\x00out\x00docs\x00"

// heldRun is one process's reads of heldCalls in dir through a record at
// path, opened with a clock an hour ahead so that the files the test just
// wrote are recordable: the answers, and the git processes they started.
func heldRun(t *testing.T, dir, path string) (map[string]string, int64) {
	t.Helper()
	forgetProcessReads()
	record := fileindex.OpenRecordAt(path, testRecordIdentity, time.Now().Add(time.Hour).UnixNano())
	restoreRecord := fileindex.UseRecord(record)
	restore := UseHeldReads(record)
	before := procstat.Take().Of(procstat.Git).Starts
	answers := map[string]string{}
	for _, args := range heldCalls {
		stdout, stderr, err := gitread.RunCapture(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, stderr)
		}
		answers[strings.Join(args, " ")] = stdout
	}
	answers["check-ignore"] = ignoredAnswer(t, dir)
	started := procstat.Take().Of(procstat.Git).Starts - before
	restore()
	restoreRecord()
	if err := record.Save(); err != nil {
		t.Fatalf("save the record: %v", err)
	}
	return answers, started
}

// gitAnswers is what Git answers for heldCalls in dir, with no record.
func gitAnswers(t *testing.T, dir string) map[string]string {
	t.Helper()
	answers := map[string]string{}
	for _, args := range heldCalls {
		stdout, stderr, err := gitread.RunCapture(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, stderr)
		}
		answers[strings.Join(args, " ")] = stdout
	}
	restore := gitread.UseHeld(nil)
	answers["check-ignore"] = ignoredAnswer(t, dir)
	restore()
	return answers
}

func ignoredAnswer(t *testing.T, dir string) string {
	t.Helper()
	ignored, err := IgnoresDirectories(dir, strings.Split(strings.TrimSuffix(ignoreQuestion, "\x00"), "\x00"))
	if err != nil {
		t.Fatalf("IgnoresDirectories: %v", err)
	}
	var names []string
	for name := range ignored {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// antedateTree moves the modification time of every file and directory of the
// repository at dir an hour back, as for files written before the run: the
// configuration reading is kept only for files older than a second
// (readConfig).
func antedateTree(t *testing.T, dir string) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		return os.Chtimes(path, past, past)
	})
	if err != nil {
		t.Fatal(err)
	}
	// The index records the old stats: Git refreshes it once, as any status
	// before the run would. A status that rewrites the index is not kept. A
	// deleted file makes the refresh exit 1 after it wrote the index.
	_ = gittest.Command(dir, "update-index", "-q", "--refresh").Run()
}

// forgetProcessReads drops what this process kept of a repository, as a new
// process starts: the configuration reading and the parsed index.
func forgetProcessReads() {
	configCache.Range(func(key, _ any) bool { configCache.Delete(key); return true })
	indexCache.Range(func(key, _ any) bool { indexCache.Delete(key); return true })
	witnesses.Range(func(key, _ any) bool { witnesses.Delete(key); return true })
	walks.Range(func(key, _ any) bool { walks.Delete(key); return true })
	witnessTops.Range(func(key, _ any) bool { witnessTops.Delete(key); return true })
}

func assertSameAnswers(t *testing.T, step string, got, want map[string]string) {
	t.Helper()
	for call, answer := range want {
		if got[call] != answer {
			t.Errorf("%s: git %s answered %q from the record, Git answers %q", step, call, got[call], answer)
		}
	}
}

// TestAnUnchangedRepositoryAnswersItsGitReadsFromTheRecord holds that a
// second run in an unchanged repository starts no git process for the held
// calls and answers as Git does, and that each change Git would see gives
// Git's new answer on the next run (tooling/cli ADR 0136).
func TestAnUnchangedRepositoryAnswersItsGitReadsFromTheRecord(t *testing.T) {
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, ".gitignore"), "build/\n")
	write(t, filepath.Join(dir, "build", "out.txt"), "out\n")
	write(t, filepath.Join(dir, "tracked.txt"), "one\n")
	runGit(t, dir, "add", ".gitignore", "tracked.txt")
	runGit(t, dir, "commit", "-m", "chore: tracked")
	runGit(t, dir, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
	write(t, filepath.Join(dir, "untracked.txt"), "u\n")
	path := filepath.Join(t.TempDir(), "input-record")
	antedateTree(t, dir)

	first, started := heldRun(t, dir, path)
	if started == 0 {
		t.Fatal("the first run started no git process: it had no record to answer from")
	}
	assertSameAnswers(t, "first run", first, gitAnswers(t, dir))
	second, started := heldRun(t, dir, path)
	if started != 0 {
		t.Errorf("an unchanged repository started %d git process(es), want none", started)
	}
	assertSameAnswers(t, "unchanged", second, gitAnswers(t, dir))

	for _, change := range []struct {
		name   string
		change func()
	}{
		{"a tracked file rewritten with the same size", func() { write(t, filepath.Join(dir, "tracked.txt"), "two\n") }},
		{"the change staged", func() { runGit(t, dir, "add", "tracked.txt") }},
		{"a new untracked file in a new directory", func() { write(t, filepath.Join(dir, "docs", "new.md"), "n\n") }},
		{"a second file in the untracked directory", func() { write(t, filepath.Join(dir, "docs", "more.md"), "m\n") }},
		{"an untracked file rewritten", func() { write(t, filepath.Join(dir, "untracked.txt"), "uu\n") }},
		{"a path asked of the ignore rules created", func() { write(t, filepath.Join(dir, "out", "x.txt"), "x\n") }},
		{"a new file under an ignored directory", func() { write(t, filepath.Join(dir, "build", "more.txt"), "m\n") }},
		{"a new ignore rule", func() { write(t, filepath.Join(dir, ".gitignore"), "build/\ndocs/\n") }},
		{"a commit", func() { runGit(t, dir, "commit", "-am", "chore: two") }},
		{"a lightweight tag at HEAD", func() { runGit(t, dir, "tag", "v1.1.0") }},
		{"a branch switch", func() { runGit(t, dir, "switch", "-c", "topic") }},
		{"a tag deleted", func() { runGit(t, dir, "tag", "-d", "v1.1.0") }},
		{"the tags packed", func() { runGit(t, dir, "pack-refs", "--all") }},
		{"a tracked file deleted", func() {
			if err := os.Remove(filepath.Join(dir, "tracked.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a longer abbreviation", func() { runGit(t, dir, "config", "core.abbrev", "12") }},
		{"a detached HEAD", func() { runGit(t, dir, "checkout", "-q", "--detach") }},
		{"a new commit on the detached HEAD", func() {
			write(t, filepath.Join(dir, "third.txt"), "3\n")
			runGit(t, dir, "add", "third.txt")
			runGit(t, dir, "commit", "-m", "chore: three")
		}},
	} {
		change.change()
		antedateTree(t, dir)
		got, _ := heldRun(t, dir, path)
		assertSameAnswers(t, change.name, got, gitAnswers(t, dir))
		// The run after the change is unchanged again.
		again, started := heldRun(t, dir, path)
		if started != 0 {
			t.Errorf("after %s, the next unchanged run started %d git process(es), want none", change.name, started)
		}
		assertSameAnswers(t, change.name+", then unchanged", again, gitAnswers(t, dir))
	}
}

// TestAReadOnlyRecordKeepsNoGitRead holds that a run that only plans, whose
// record is read-only, takes no witness: it asks Git what it would ask with
// no record.
func TestAReadOnlyRecordKeepsNoGitRead(t *testing.T) {
	dir := initGitRepo(t)
	forgetProcessReads()
	record := fileindex.OpenRecordReadOnly(filepath.Join(t.TempDir(), "input-record"), testRecordIdentity, time.Now().UnixNano())
	restore := UseHeldReads(record)
	defer restore()
	before := procstat.Take().Of(procstat.Git).Starts
	if _, err := gitread.Run(dir, "rev-parse", "--short", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if started := procstat.Take().Of(procstat.Git).Starts - before; started != 1 {
		t.Errorf("a read-only record started %d git process(es) for one call, want 1", started)
	}
}

// TestTheHeldCallsAreTheirShapes holds which calls the record holds: the
// history calls only as the version lines and linetag spell them, with a
// full object name.
func TestTheHeldCallsAreTheirShapes(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		args []string
		want readClass
	}{
		{[]string{"rev-parse", "--verify", "--end-of-options", sha + "^{commit}"}, classRepository},
		{[]string{"rev-parse", "--verify", "--end-of-options", "main^{commit}"}, classNone},
		{[]string{"log", "--format=%H%x00%s%x00%b\x1e", "--no-show-signature", sha + "..HEAD", "--", "go", "tooling"}, classRepository},
		{[]string{"log", "--format=%H%x00%s%x00%b\x1e", "--no-show-signature", sha + "..HEAD"}, classRepository},
		{[]string{"log", "--format=%H%x00%s%x00%b\x1e", "--no-show-signature", "v1..HEAD"}, classNone},
		{[]string{"diff-tree", "-r", "--root", "--no-commit-id", "--name-only", "--no-renames", "-z", "--end-of-options", sha, "--", "go"}, classRepository},
		{[]string{"for-each-ref", "--merged=" + sha, "--format=%(refname) %(objectname)", "refs/tags"}, classRepository},
		{[]string{"status", "--porcelain"}, classWorkTree},
		{[]string{"check-ignore", "-z", "--stdin"}, classIgnore},
		{[]string{"commit", "-m", "x"}, classNone},
	} {
		if got := classify(tc.args); got != tc.want {
			t.Errorf("classify(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// TestAChangeWhileTheRecordIsOpenIsSeen holds that an answer the record holds
// is checked with stats taken at the call: repository code that changes the
// repository between two calls of one run, a hook or a task, gets Git's new
// answer from the same open record.
func TestAChangeWhileTheRecordIsOpenIsSeen(t *testing.T) {
	dir := initGitRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "one\n")
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "chore: tracked")
	antedateTree(t, dir)
	path := filepath.Join(t.TempDir(), "input-record")
	heldRun(t, dir, path)

	forgetProcessReads()
	record := fileindex.OpenRecordAt(path, testRecordIdentity, time.Now().Add(time.Hour).UnixNano())
	restoreRecord := fileindex.UseRecord(record)
	defer restoreRecord()
	restore := UseHeldReads(record)
	defer restore()
	ask := func() map[string]string {
		answers := map[string]string{}
		for _, args := range heldCalls {
			stdout, _, err := gitread.RunCapture(dir, args...)
			if err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
			answers[strings.Join(args, " ")] = stdout
		}
		return answers
	}
	ask()
	// The record has now stated every part once in this process.
	write(t, filepath.Join(dir, "tracked.txt"), "two\n")
	runGit(t, dir, "commit", "-am", "chore: two")
	got := ask()
	restoreBare := gitread.UseHeld(nil)
	want := gitAnswers(t, dir)
	restoreBare()
	delete(want, "check-ignore")
	assertSameAnswers(t, "a commit while the record is open", got, want)
}

// TestALinkedConfigurationFileIsFollowed holds that a configuration file that
// is a link witnesses the file it resolves to: an edit there gives Git's new
// answer.
func TestALinkedConfigurationFileIsFollowed(t *testing.T) {
	dir := initGitRepo(t)
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles")
	write(t, filepath.Join(dotfiles, "gitconfig"), "[core]\n\tabbrev = 10\n")
	if err := os.Symlink(filepath.Join(dotfiles, "gitconfig"), filepath.Join(home, ".gitconfig")); err != nil {
		t.Skipf("no symbolic link: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	antedateTree(t, dir)
	antedateTree(t, home)
	path := filepath.Join(t.TempDir(), "input-record")
	heldRun(t, dir, path)
	if _, started := heldRun(t, dir, path); started != 0 {
		t.Fatalf("the unchanged repository started %d git process(es), want none", started)
	}
	write(t, filepath.Join(dotfiles, "gitconfig"), "[core]\n\tabbrev = 16\n")
	antedateTree(t, home)
	got, _ := heldRun(t, dir, path)
	short := strings.Join([]string{"rev-parse", "--short", "HEAD"}, " ")
	if want := gitAnswers(t, dir)[short]; got[short] != want || len(strings.TrimSpace(want)) != 16 {
		t.Errorf("after an edit of the linked configuration, --short HEAD answered %q, Git answers %q", got[short], want)
	}
}
