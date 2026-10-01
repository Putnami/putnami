package jobs

import (
	"path/filepath"
	"sync"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// The two layouts of one project manifest. They decode to the same document,
// and a formatter rewrites the first into the second.
const (
	expandedManifest  = "{\n  \"name\": \"proj\",\n  \"tags\": [\n    \"ts\",\n    \"e2e\"\n  ]\n}\n"
	collapsedManifest = "{\n  \"name\": \"proj\",\n  \"tags\": [\"ts\", \"e2e\"]\n}\n"
	// tunedManifest adds only an execution-only tasks block to collapsedManifest.
	tunedManifest = "{\n  \"name\": \"proj\",\n  \"tags\": [\"ts\", \"e2e\"],\n  \"tasks\": { \"lint\": { \"cpuWeight\": 4 } }\n}\n"
)

// formatterJob is a declared source rewriter keyed on every JSON file of its
// project, the shape of a TypeScript format phase.
func formatterJob() *ScheduledJob {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~format", "format", &extension.TaskDeclaration{MutatesSources: true}, sources)
	job.JobDef.FilePatterns = []string{"**/*.json"}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	return job
}

// A formatter that only changes the layout of a keyed project config has
// rewritten a keyed input. The phase is marked, and its entry is never restored
// as green, whether the config is keyed through the project's patterns or
// through the workspace-relative ones.
func TestSourceRewriterDetectsProjectConfigReformat(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		job      func() *ScheduledJob
	}{
		{
			name:     "project manifest",
			manifest: filepath.Join(captureTestProject, wsproto.ConfigFilename),
			job:      formatterJob,
		},
		{
			name:     "workspace-keyed project config",
			manifest: filepath.Join("shared", wsproto.ConfigFilename),
			job: func() *ScheduledJob {
				job := formatterJob()
				job.JobDef.TaskCachePolicy.Key = &extension.TaskCacheKey{
					Files:          []string{"index.ts"},
					WorkspaceFiles: []string{"shared/" + wsproto.ConfigFilename},
				}
				return job
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := tc.job()
			f := newCaptureFixture(t, job)
			writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "index.ts"), "const a = 1;\n")
			manifest := filepath.Join(f.ws.Root, tc.manifest)
			writeFileAt(t, manifest, expandedManifest)

			hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
			if err != nil || hash == "" {
				t.Fatalf("compute pre-format key: %q (err=%v)", hash, err)
			}
			sourceHash, err := sourceInputDigest(f.ws, job, nil, f.cache)
			if err != nil || sourceHash == "" {
				t.Fatalf("compute pre-format source digest: %q (err=%v)", sourceHash, err)
			}

			writeFileAt(t, manifest, collapsedManifest)

			var mu sync.Mutex
			result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
				true, hash, sourceHash, &mu, map[string]string{})
			if !result.SourceMutated {
				t.Fatal("a layout-only rewrite of a keyed project config was recorded as clean")
			}
			entry, err := f.cache.LookupTaskEntry(hash)
			if err != nil || entry == nil {
				t.Fatalf("lookup mutation marker: entry=%v err=%v", entry, err)
			}
			if hit := f.sched.restoreDeclaredCacheHit(t.Context(), job, hash, entry, &mu, map[string]string{}); hit != nil {
				t.Fatalf("the entry of a phase that reformatted a keyed config restored as green: %+v", hit)
			}
		})
	}
}

// A source rewriter keys a project config by its bytes, so a clean verdict
// recorded for the formatted layout never answers for another layout of the
// same document.
func TestSourceRewriterKeysProjectConfigBytes(t *testing.T) {
	rewriter := formatterJob()
	f := newCaptureFixture(t, rewriter)
	manifest := filepath.Join(f.ws.Root, captureTestProject, wsproto.ConfigFilename)
	key := func(body string) string {
		t.Helper()
		writeFileAt(t, manifest, body)
		f.cache.InvalidateFileHashes()
		hash, err := computeJobCacheHash(f.ws, rewriter, nil, nil, f.cache, nil)
		if err != nil || hash == "" {
			t.Fatalf("compute rewriter key: %q (err=%v)", hash, err)
		}
		return hash
	}

	formatted := key(collapsedManifest)
	sourceHash, err := sourceInputDigest(f.ws, rewriter, nil, f.cache)
	if err != nil || sourceHash == "" {
		t.Fatalf("compute source digest: %q (err=%v)", sourceHash, err)
	}
	var mu sync.Mutex
	result := f.sched.finalizeExecutedJob(t.Context(), rewriter, capturedResult(nil),
		true, formatted, sourceHash, &mu, map[string]string{})
	if result.SourceMutated {
		t.Fatal("a formatter that left an already-formatted config unchanged was marked as mutating")
	}
	if hit := f.hitOrFail(t, rewriter, formatted); !hit.CacheHit || hit.SourceMutated {
		t.Fatalf("clean formatter restore = %+v, want an ordinary warm hit", hit)
	}

	expanded := key(expandedManifest)
	if expanded == formatted {
		t.Fatal("the unformatted config addressed the clean verdict recorded for the formatted one")
	}
	if entry, err := f.cache.LookupTaskEntry(expanded); err != nil || entry != nil {
		t.Fatalf("the unformatted config found an entry: entry=%v err=%v", entry, err)
	}
}

// How a task reads a project config decides which of its bytes are in the
// key. A task that rewrites its sources reads every keyed config as text,
// whatever pattern selects it. A read-only task that sweeps the config up with
// a glob (`**/*.json`, as a linter does) reads it as text too. A task whose
// pattern names the file reads it as configuration: layout and execution-only
// tuning leave its key alone.
func TestProjectConfigKeyFollowsHowTheTaskReadsIt(t *testing.T) {
	namedRewriter := formatterJob()
	namedRewriter.JobDef.Name = "lint~format-named"
	namedRewriter.JobDef.FilePatterns = []string{wsproto.ConfigFilename}
	globRewriter := formatterJob()
	linter := declaredJob("lint~all", "all", &extension.TaskDeclaration{})
	linter.JobDef.FilePatterns = []string{"**/*.json"}
	linter.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	reader := declaredJob("build~transpile", "transpile", nil)
	reader.JobDef.FilePatterns = []string{wsproto.ConfigFilename, "**/*.ts"}
	jobs := []*ScheduledJob{namedRewriter, globRewriter, linter, reader}
	f := newCaptureFixture(t, jobs...)
	manifest := filepath.Join(f.ws.Root, captureTestProject, wsproto.ConfigFilename)

	keys := func(body string) []string {
		t.Helper()
		writeFileAt(t, manifest, body)
		f.cache.InvalidateFileHashes()
		out := make([]string, len(jobs))
		for i, job := range jobs {
			hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
			if err != nil || hash == "" {
				t.Fatalf("compute %s key: %q (err=%v)", job.JobDef.Name, hash, err)
			}
			out[i] = hash
		}
		return out
	}

	formatted := keys(collapsedManifest)
	for _, edit := range []struct{ what, body string }{
		{"a layout-only edit", expandedManifest},
		{"an execution-only tasks block", tunedManifest},
	} {
		edited := keys(edit.body)
		for i, job := range jobs[:3] {
			if edited[i] == formatted[i] {
				t.Errorf("%s left the key of %s unmoved; it reads the config as text", edit.what, job.JobDef.Name)
			}
		}
		if edited[3] != formatted[3] {
			t.Errorf("%s moved the key of %s; it reads the config as configuration", edit.what, reader.JobDef.Name)
		}
	}
}
