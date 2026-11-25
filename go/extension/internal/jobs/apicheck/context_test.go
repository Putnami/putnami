package apicheck

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// contextFact is the release baseline the job context would carry for the
// fixture project: the baseline Git gives, and a directory that holds the
// regular files the tag holds at the project, as the CLI extracts them
// (go.putnami.dev/protocol/job ADR 0002). It reads Git, so the caller builds
// it before it takes git off PATH.
func (f *fixture) contextFact(project string) *job.ReleaseBaseline {
	f.t.Helper()
	base, err := gitBaseline(f.root, project)
	if err != nil {
		f.t.Fatalf("read the baseline: %v", err)
	}
	fact := &job.ReleaseBaseline{State: base.state, Pattern: base.pattern, HasTags: base.hasTags, Tag: base.tag}
	if base.state != job.ReleaseBaselineTagged {
		return fact
	}
	if fact.Breaking, err = base.breaking(); err != nil {
		f.t.Fatal(err)
	}
	entries, err := treeAtTag(project, base.release.(gitRelease).rev, ".", treeListing{recursive: true})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(entries) == 0 {
		// The project directory is absent at the tag: the context names no
		// directory.
		return fact
	}
	fact.Directory = filepath.Join(f.t.TempDir(), "baseline")
	for _, entry := range entries {
		if !entry.regular {
			continue
		}
		contents, err := readBlobs(project, []string{entry.object})
		if err != nil {
			f.t.Fatal(err)
		}
		target := filepath.Join(fact.Directory, filepath.FromSlash(entry.name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(target, contents[0], 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return fact
}

// withoutGit runs fn with no git on PATH.
func withoutGit(t *testing.T, fn func()) {
	t.Helper()
	saved := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	defer func() { _ = os.Setenv("PATH", saved) }()
	fn()
}

// The check reads its line's release baseline and the files its tag holds
// from the job context when it carries them, and starts no git: each case
// gives the report the check gives reading Git, with git off PATH.
func TestTheContextBaselineGivesTheReportGitGives(t *testing.T) {
	spectest.Proves(t, feature, "go-jobs-read-the-tree-from-their-context",
		"the-api-check-reads-the-release-baseline-from-the-context")
	cases := map[string]func(t *testing.T) (*fixture, string){
		"a removed function": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.write("lib/greet/greet.go", waveOnlySource)
			f.commit("feat: drop the greeting")
			return f, f.project
		},
		"a declared break": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.write("lib/greet/greet.go", waveOnlySource)
			f.commit("feat!: drop the greeting")
			return f, f.project
		},
		"a compatible change": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.write("lib/greet/more.go", "package lib\n\n// More is new.\nfunc More() {}\n")
			f.commit("feat: more")
			return f, f.project
		},
		"directories the check never reads at the tag": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			exported := "package x\n\n// Gone goes.\nfunc Gone() {}\n"
			for _, dir := range []string{"vendor/v", "testdata/t", ".hidden/h", "internal/i", "_scratch/s", "dist/d", "nested"} {
				f.write("lib/greet/"+dir+"/x.go", exported)
			}
			f.write("lib/greet/nested/go.mod", "module example.com/nested\n\ngo 1.25\n")
			f.write("lib/greet/kept/x.go", exported)
			f.commit("feat: unread packages")
			f.git("tag", "lib/v0.5.0")
			for _, dir := range []string{"vendor/v", "testdata/t", ".hidden/h", "internal/i", "_scratch/s", "dist/d", "nested", "kept"} {
				f.write("lib/greet/"+dir+"/x.go", "package x\n")
			}
			f.commit("feat: drop them")
			return f, f.project
		},
		"an untagged line": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.git("tag", "-d", firstTag)
			return f, f.project
		},
		"an untagged line among other tags": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.git("tag", "-d", firstTag)
			f.git("tag", "other/v1.0.0")
			return f, f.project
		},
		"a project the tag does not hold": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			f.write("lib/later/putnami.json", `{"name":"`+projectName+`","tags":["go"]}`+"\n")
			f.write("lib/later/go.mod", "module "+projectName+"\n\ngo 1.25\n")
			f.write("lib/later/greet.go", greetSource)
			f.commit("feat: a later project")
			return f, filepath.Join(f.root, "lib", "later")
		},
		"a released command surface with a removed flag": func(t *testing.T) (*fixture, string) {
			f := newSurfaceFixture(t)
			f.write(surfaceLocation, surfaceDocument(t, styleFlag()))
			f.commit("feat: drop --loud")
			return f, f.project
		},
		"a dropped command surface": func(t *testing.T) (*fixture, string) {
			f := newSurfaceFixture(t)
			f.write("lib/greet/putnami.json", `{"name":"`+projectName+`","tags":["go"]}`+"\n")
			f.options = Options{}
			f.commit("feat: no surface")
			return f, f.project
		},
		"a declaration the tag holds as a symbolic link": func(t *testing.T) (*fixture, string) {
			f := newFixture(t, catalog("package", "stable"))
			config := filepath.Join(f.project, "putnami.json")
			f.write("lib/greet/real.json", `{"options":{"validate":{"command-surface":"`+surfaceFile+`"}}}`)
			if err := os.Remove(config); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real.json", config); err != nil {
				t.Fatal(err)
			}
			f.commit("feat: a linked declaration")
			f.git("tag", surfaceTag)
			if err := os.Remove(config); err != nil {
				t.Fatal(err)
			}
			declareSurface(f, surfaceFile)
			f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
			f.commit("feat: the greet command")
			return f, f.project
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f, project := build(t)
			fromGit, err := Check(f.root, project, projectName, f.tags, f.options)
			if err != nil {
				t.Fatalf("from Git: %v", err)
			}
			fact := f.contextFact(project)
			var fromContext *Report
			withoutGit(t, func() {
				fromContext, err = CheckBaseline(f.root, project, projectName, f.tags, f.options, fact)
			})
			if err != nil {
				t.Fatalf("from the context: %v", err)
			}
			if !reflect.DeepEqual(fromContext, fromGit) {
				t.Fatalf("from the context: %+v\nfrom Git:         %+v", fromContext, fromGit)
			}
		})
	}
}

// The states a context baseline carries before any tag give the reports the
// check gives reading Git, with no git at all.
func TestAContextBaselineWithoutATagNeedsNoRepository(t *testing.T) {
	spectest.Proves(t, feature, "go-jobs-read-the-tree-from-their-context",
		"the-api-check-reads-the-release-baseline-from-the-context")
	root := t.TempDir()
	for state, want := range map[string]string{
		job.ReleaseBaselineUnmanaged: "the workspace is not in a git repository",
		job.ReleaseBaselineNoCommit:  "the repository has no commit",
		job.ReleaseBaselineShallow:   "the clone is shallow, so the tags and history of line lib/v{version} are incomplete",
	} {
		var report *Report
		var err error
		withoutGit(t, func() {
			report, err = CheckBaseline(root, root, projectName, []string{"go"}, Options{},
				&job.ReleaseBaseline{State: state, Pattern: "lib/v{version}"})
		})
		if err != nil || !strings.HasPrefix(report.Warning+report.Note, want) {
			t.Errorf("%s: report = %+v, %v; want %q", state, report, err, want)
		}
	}
	if _, err := CheckBaseline(root, root, projectName, []string{"go"}, Options{}, &job.ReleaseBaseline{State: "lost"}); err == nil {
		t.Error("an unknown state gave a report, want an error")
	}
}

// The task body reads the baseline from the job context's tree member.
func TestRunReadsTheReleaseBaselineFromTheJobContext(t *testing.T) {
	spectest.Proves(t, feature, "go-jobs-read-the-tree-from-their-context",
		"the-api-check-reads-the-release-baseline-from-the-context")
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")
	fact := f.contextFact(f.project)
	ctx := &pctx.Context{
		WorkspaceRoot: f.root,
		Project:       pctx.Project{Name: projectName, Path: "lib/greet", FullPath: f.project, Tags: f.tags},
		Tree:          &job.Tree{Directory: "lib/greet", ReleaseBaseline: fact},
	}
	var status string
	var data map[string]any
	var runErr error
	var output string
	withoutGit(t, func() {
		output = captureStdout(t, func() {
			status, data, runErr = Run(ctx, jsonl.NewForVersion(1), nil)
		})
	})
	if runErr != nil || status != "FAILED" || data["incompatible"] != 1 || data["tag"] != firstTag {
		t.Fatalf("status = %q, data = %v, err = %v; want the removed function failing", status, data, runErr)
	}
	if found := diagnostics(decodeEvents(t, output), runtime.SeverityError); len(found) != 1 ||
		!strings.Contains(found[0].Message, "func Greet removed since lib/v0.4.0") {
		t.Fatalf("errors = %+v, want the removed function", found)
	}
}
