package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// These tests count the reads past a line's last release through a package
// counter, so none of them runs in parallel: Go runs every parallel test of
// the package after the sequential ones.

const testRecordIdentity = "test-program"

// recordedRun is one run's reads of the baselines of dirs under patterns,
// through a reader that answers from the record file at path, which the run
// loads at its start and saves at its end, as the engine does.
type recordedRun struct {
	baselines map[string]ReleaseBaseline
	reads     int64 // reads past a last release
	hits      int64 // baselines the record answered
}

func runRecorded(t *testing.T, readerRoot, path string, dirs, patterns []string) recordedRun {
	t.Helper()
	return runRecordedAs(t, readerRoot, path, testRecordIdentity, dirs, patterns)
}

func runRecordedAs(t *testing.T, readerRoot, path, identity string, dirs, patterns []string) recordedRun {
	t.Helper()
	record := fileindex.OpenReleaseRecord(path, identity)
	reader := NewRecordedReleaseBaselineReader(readerRoot, record)
	before := projectReads.Load()
	run := recordedRun{baselines: map[string]ReleaseBaseline{}}
	for _, dir := range dirs {
		for _, pattern := range patterns {
			baseline, err := reader.Read(dir, pattern)
			if err != nil {
				t.Fatalf("Read(%s, %s): %v", dir, pattern, err)
			}
			run.baselines[dir+"\x00"+pattern] = baseline
		}
	}
	run.reads, run.hits = projectReads.Load()-before, record.Hits()
	if err := record.Save(); err != nil {
		t.Fatalf("save the record: %v", err)
	}
	return run
}

// assertFresh fails when a run's baseline differs from the one Git answers
// with no record, the commit and the prefix included.
func assertFresh(t *testing.T, run recordedRun, dirs, patterns []string) {
	t.Helper()
	for _, dir := range dirs {
		for _, pattern := range patterns {
			want, err := ReadReleaseBaseline(dir, pattern)
			if err != nil {
				t.Fatalf("ReadReleaseBaseline(%s, %s): %v", dir, pattern, err)
			}
			if got := run.baselines[dir+"\x00"+pattern]; got != want {
				t.Errorf("%s with %s: got %+v, want %+v", dir, pattern, got, want)
			}
		}
	}
}

func recordPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".putnami", fileindex.ReleaseRecordFilename)
}

// A recorded baseline is the baseline Git answers: over every shape of
// baseline a run records, the run after it answers each one from the record,
// with no read past a last release, and the same value, commit and prefix
// as a fresh read (tooling/cli ADR 0113 §4).
func TestRecordedReleaseBaselinesAreTheFreshOnes(t *testing.T) {
	root, _ := releasedAPIRepo(t)
	writeCommit(t, root, "web/a.go", "feat(web)!: break the web")
	gitDo(t, root, "tag", "web/v1.0.0") // a lightweight tag
	writeCommitWithBody(t, root, "web/b.go", "feat(web): more", "BREAKING CHANGE: the body breaks it")
	writeCommit(t, root, "api/b.go", "fix(api): a fix")
	writeCommit(t, root, "lib/c.go", "feat(lib): no release yet")

	if err := os.MkdirAll(filepath.Join(root, "absent"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []string{root, filepath.Join(root, "api"), filepath.Join(root, "web"), filepath.Join(root, "lib"),
		filepath.Join(root, "absent")}
	patterns := []string{apiPattern, "web/v{version}", "lib/v{version}", "v{version}"}
	// A reader rooted below the work tree's root records the directories it
	// holds, under its own prefix; the others are read as ReadReleaseBaseline
	// reads them.
	for readerRoot, dirs := range map[string][]string{root: dirs, filepath.Join(root, "lib"): {filepath.Join(root, "lib")}} {
		path := recordPath(t)
		first := runRecorded(t, readerRoot, path, dirs, patterns)
		assertFresh(t, first, dirs, patterns)
		if first.hits != 0 || first.reads == 0 {
			t.Fatalf("reader at %s, first run: %d hits and %d reads, want none and some", readerRoot, first.hits, first.reads)
		}
		second := runRecorded(t, readerRoot, path, dirs, patterns)
		assertFresh(t, second, dirs, patterns)
		if second.reads != 0 {
			t.Errorf("reader at %s, second run read %d baselines past their release, want 0", readerRoot, second.reads)
		}
		if second.hits != int64(len(dirs)*len(patterns)) {
			t.Errorf("reader at %s, second run: %d hits, want %d", readerRoot, second.hits, len(dirs)*len(patterns))
		}
	}
}

// moveCase moves one part of the witness between two runs.
type moveCase struct {
	name string
	move func(t *testing.T, root string)
	// keep tells that nothing may be recorded while the part stays moved.
	keep bool
}

// Each part of the witness, moved, makes the next run read the baselines
// again, and that run answers what Git answers.
func TestEachWitnessPartForcesAReread(t *testing.T) {
	cases := []moveCase{
		{name: "HEAD", move: func(t *testing.T, root string) { writeCommit(t, root, "api/c.go", "feat(api)!: break") }},
		{name: "HEAD on a commit that touches no project", move: func(t *testing.T, root string) {
			writeCommit(t, root, "other/c.go", "chore: elsewhere")
		}},
		{name: "a tag of another line", move: func(t *testing.T, root string) {
			gitDo(t, root, "tag", "web/v9.0.0")
		}},
		{name: "a tag moved", move: func(t *testing.T, root string) {
			writeCommit(t, root, "api/c.go", "fix(api): later")
			gitDo(t, root, "tag", "-f", "-a", "api/v0.1.0", "-m", "moved")
			gitDo(t, root, "checkout", "-q", "HEAD~1")
		}},
		{name: "a tag deleted", move: func(t *testing.T, root string) { gitDo(t, root, "tag", "-d", "api/v0.1.0") }},
		{name: "the configuration", move: func(t *testing.T, root string) {
			gitDo(t, root, "config", "i18n.logOutputEncoding", "ISO-8859-1")
		}},
		{name: "a replace ref", keep: true, move: func(t *testing.T, root string) {
			gitDo(t, root, "replace", "HEAD", strings.TrimSpace(runGit(t, root, "rev-parse", "HEAD~1")))
		}},
		{name: "GIT_REPLACE_REF_BASE", keep: true, move: func(t *testing.T, _ string) { t.Setenv("GIT_REPLACE_REF_BASE", "refs/replace/") }},
		{name: "GIT_NO_REPLACE_OBJECTS", keep: true, move: func(t *testing.T, _ string) { t.Setenv("GIT_NO_REPLACE_OBJECTS", "1") }},
		{name: "GIT_GRAFT_FILE", keep: true, move: func(t *testing.T, root string) {
			t.Setenv("GIT_GRAFT_FILE", filepath.Join(root, "no-grafts"))
		}},
		{name: "another Git variable", move: func(t *testing.T, _ string) { t.Setenv("GIT_WITNESS_PROBE", "1") }},
		{name: "HOME", move: func(t *testing.T, _ string) { t.Setenv("HOME", t.TempDir()) }},
		{name: "XDG_CONFIG_HOME", move: func(t *testing.T, _ string) { t.Setenv("XDG_CONFIG_HOME", t.TempDir()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := releasedAPIRepo(t)
			writeCommit(t, root, "api/b.go", "fix(api): a fix")
			dirs := []string{root, filepath.Join(root, "api")}
			patterns := []string{apiPattern}
			path := recordPath(t)
			runRecorded(t, root, path, dirs, patterns)
			if held := runRecorded(t, root, path, dirs, patterns); held.reads != 0 || held.hits != 2 {
				t.Fatalf("unmoved: %d reads and %d hits, want 0 and 2", held.reads, held.hits)
			}

			tc.move(t, root)
			moved := runRecorded(t, root, path, dirs, patterns)
			assertFresh(t, moved, dirs, patterns)
			if moved.hits != 0 {
				t.Errorf("moved: the record answered %d baselines, want none", moved.hits)
			}
			again := runRecorded(t, root, path, dirs, patterns)
			assertFresh(t, again, dirs, patterns)
			if tc.keep && again.hits != 0 {
				t.Errorf("a baseline was recorded while history may be rewritten: %d hits", again.hits)
			}
		})
	}
}

// The root's prefix is part of the witness: a reader rooted elsewhere in the
// work tree reads again.
func TestTheRootPrefixIsWitnessed(t *testing.T) {
	root, api := releasedAPIRepo(t)
	path := recordPath(t)
	runRecorded(t, root, path, []string{api}, []string{apiPattern})
	if run := runRecorded(t, api, path, []string{api}, []string{apiPattern}); run.hits != 0 || run.reads == 0 {
		t.Fatalf("reader at the project: %d hits and %d reads, want a re-read", run.hits, run.reads)
	}
}

// A shallow clone, a repository with no commit and a directory outside every
// work tree record nothing.
func TestStatesWithoutAWitnessRecordNothing(t *testing.T) {
	origin, _ := releasedAPIRepo(t)
	writeCommit(t, origin, "api/b.go", "feat(api): more")
	shallow := filepath.Join(t.TempDir(), "shallow")
	gitDo(t, origin, "clone", "-q", "--depth", "1", "file://"+origin, shallow)
	empty := t.TempDir()
	gitDo(t, empty, "init", "-q")
	outside := outsideRepository(t)

	for _, root := range []string{shallow, empty, outside} {
		path := recordPath(t)
		runRecorded(t, root, path, []string{root}, []string{apiPattern})
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: a record was written (%v)", root, err)
		}
	}
}

// Another program identity, as another CLI build has, and a damaged file
// start with an empty record.
func TestAnotherBuildOrADamagedRecordReadsAgain(t *testing.T) {
	root, api := releasedAPIRepo(t)
	writeCommit(t, root, "api/b.go", "fix(api): a fix")
	dirs, patterns := []string{api}, []string{apiPattern}
	path := recordPath(t)
	runRecorded(t, root, path, dirs, patterns)

	if run := runRecordedAs(t, root, path, "another-build", dirs, patterns); run.hits != 0 || run.reads == 0 {
		t.Errorf("another build: %d hits and %d reads, want a re-read", run.hits, run.reads)
	}
	runRecorded(t, root, path, dirs, patterns)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if run := runRecorded(t, root, path, dirs, patterns); run.hits != 0 || run.reads == 0 {
		t.Errorf("damaged record: %d hits and %d reads, want a re-read", run.hits, run.reads)
	}
}

// A grafts file, a replace ref, loose or packed, and a reftable, where a
// replace ref cannot be seen, count as a rewrite. Git refuses to read a
// history with a grafts file without a configuration that silences it, so the
// grafts file is checked here rather than through a run.
func TestRewritesHistorySeesPackedReplaceRefsAndReftables(t *testing.T) {
	for name, setup := range map[string]func(dir string) error{
		"none": func(string) error { return nil },
		"packed replace ref": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "packed-refs"),
				[]byte("# pack-refs with: peeled fully-peeled sorted\n"+strings.Repeat("a", 40)+" refs/replace/"+strings.Repeat("b", 40)+"\n"), 0o644)
		},
		"packed tags only": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(strings.Repeat("a", 40)+" refs/tags/v1.0.0\n"), 0o644)
		},
		"reftable": func(dir string) error { return os.Mkdir(filepath.Join(dir, "reftable"), 0o755) },
		"grafts": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, "info"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "info", "grafts"), nil, 0o644)
		},
		"loose replace ref": func(dir string) error { return os.MkdirAll(filepath.Join(dir, "refs", "replace"), 0o755) },
	} {
		dir := t.TempDir()
		if err := setup(dir); err != nil {
			t.Fatal(err)
		}
		want := name != "none" && name != "packed tags only"
		if got := rewritesHistory(dir); got != want {
			t.Errorf("%s: rewritesHistory = %v, want %v", name, got, want)
		}
	}
}

// A baseline read while the witness moved is not written: the record takes
// the witness again when it saves. Here a tag moves after the read and comes
// back before the next run, which must still read the baseline itself.
func TestABaselineReadWhileATagMovedIsNotKept(t *testing.T) {
	root, api := releasedAPIRepo(t)
	writeCommit(t, root, "api/b.go", "fix(api): a fix")
	release := strings.TrimSpace(runGit(t, root, "rev-parse", "api/v0.1.0"))
	path := recordPath(t)

	record := fileindex.OpenReleaseRecord(path, testRecordIdentity)
	reader := NewRecordedReleaseBaselineReader(root, record)
	if _, err := reader.Read(api, apiPattern); err != nil {
		t.Fatal(err)
	}
	gitDo(t, root, "tag", "-f", "-a", "api/v0.1.0", "-m", "moved", "HEAD")
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	gitDo(t, root, "update-ref", "refs/tags/api/v0.1.0", release)

	run := runRecorded(t, root, path, []string{api}, []string{apiPattern})
	assertFresh(t, run, []string{api}, []string{apiPattern})
	if run.hits != 0 {
		t.Fatalf("the record answered %d baselines read while a tag moved", run.hits)
	}
}

// One reader answers concurrent reads, from Git and then from the record, as
// a run's tasks ask it; the race detector checks the reader and the record.
func TestConcurrentReadsOfOneReader(t *testing.T) {
	root, _ := releasedAPIRepo(t)
	writeCommit(t, root, "web/a.go", "feat(web)!: break")
	gitDo(t, root, "tag", "web/v1.0.0")
	writeCommit(t, root, "api/b.go", "fix(api): a fix")
	dirs := []string{root, filepath.Join(root, "api"), filepath.Join(root, "web")}
	patterns := []string{apiPattern, "web/v{version}", "lib/v{version}"}
	path := recordPath(t)
	for round := range 2 {
		record := fileindex.OpenReleaseRecord(path, testRecordIdentity)
		reader := NewRecordedReleaseBaselineReader(root, record)
		var wg sync.WaitGroup
		results := make([]ReleaseBaseline, len(dirs)*len(patterns)*4)
		errs := make([]error, len(results))
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				dir, pattern := dirs[i/(len(patterns)*4)], patterns[(i/4)%len(patterns)]
				results[i], errs[i] = reader.Read(dir, pattern)
			}()
		}
		wg.Wait()
		for i := range results {
			dir, pattern := dirs[i/(len(patterns)*4)], patterns[(i/4)%len(patterns)]
			want, err := ReadReleaseBaseline(dir, pattern)
			if err != nil || errs[i] != nil || results[i] != want {
				t.Errorf("round %d, %s with %s: got %+v (%v), want %+v (%v)", round, dir, pattern, results[i], errs[i], want, err)
			}
		}
		if err := record.Save(); err != nil {
			t.Fatal(err)
		}
		if round == 1 && record.Hits() == 0 {
			t.Error("the second round answered nothing from the record")
		}
	}
}

// A tag deleted between the snapshot of the tags and the read of the tags HEAD
// reaches, then put back before the save, leaves no baseline in the record:
// that read is part of the witness. A git placed first on PATH deletes the
// tag just before that read.
func TestATagDeletedBeforeTheReachabilityReadIsNotKept(t *testing.T) {
	root, api := releasedAPIRepo(t)
	writeCommit(t, root, "api/b.go", "fix(api): a fix")
	release := strings.TrimSpace(runGit(t, root, "rev-parse", "refs/tags/api/v0.1.0"))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --merged=*) \"" + realGit + "\" -C \"" + root +
		"\" tag -d api/v0.1.0 >/dev/null 2>&1;; esac; done\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := recordPath(t)
	record := fileindex.OpenReleaseRecord(path, testRecordIdentity)
	reader := NewRecordedReleaseBaselineReader(root, record)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	read, err := reader.Read(api, apiPattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(realGit)+string(os.PathListSeparator)+os.Getenv("PATH"))
	gitDo(t, root, "update-ref", "refs/tags/api/v0.1.0", release)
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	if read.State != ReleaseBaselineUntagged {
		t.Skipf("the deletion did not land between the reads (read %+v)", read)
	}

	run := runRecorded(t, root, path, []string{api}, []string{apiPattern})
	assertFresh(t, run, []string{api}, []string{apiPattern})
	if run.hits != 0 {
		t.Fatalf("the record answered %d baselines read while a tag was deleted", run.hits)
	}
}
