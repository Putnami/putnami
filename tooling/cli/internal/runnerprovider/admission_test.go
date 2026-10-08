package runnerprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runnersource"
	"go.putnami.dev/tooling/cli/internal/store"
)

func admissionRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "--initial-branch=main")
	write(".gitignore", "app/conf/local.txt\n.gen\nnode_modules\n.putnami\ndist\n")
	write("app/conf/tracked.txt", "tracked")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	write("app/conf/local.txt", "local")
	write("app/conf/new.txt", "untracked, not ignored")
	write("app/.gen/generated.txt", "generated")
	write("app/node_modules/dep.js", "dep")
	write(".putnami/state.json", "{}")
	write("app/dist/bundle.js", "built")
	return root
}

func TestGoEmbedSourceLinkReferentIsCapturedAndMaterialized(t *testing.T) {
	root := admissionRepo(t)
	ignore, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ignore.WriteString("app/.source.txt\n"); err != nil {
		_ = ignore.Close()
		t.Fatal(err)
	}
	if err := ignore.Close(); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/.source.txt", "package app\nimport _ \"embed\"\n//go:embed assets/payload.sql\nvar payload string\n")
	write("app/assets/payload.sql", "SELECT 1;\n")
	if err := os.Symlink(".source.txt", filepath.Join(root, "app/embed.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	collected, err := store.CollectKeyFiles(filepath.Join(root, "app"), []string{"**/*.go", "go-embed:build"})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, file := range collected {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, filepath.ToSlash(rel))
	}
	if !slices.Contains(files, "app/.source.txt") || !slices.Contains(files, "app/assets/payload.sql") {
		t.Fatalf("collector omitted source referent or embed payload: %v", files)
	}
	inputs := jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{Key: "/app:build~describe", Files: files}}}
	admission, err := AdmitInputs(context.Background(), root, inputs)
	if err != nil || !slices.Equal(admission.Bound, []string{"app/.source.txt"}) {
		t.Fatalf("source referent admission = %+v, %v", admission, err)
	}
	sourceStore, err := runnersource.OpenStore(filepath.Join(t.TempDir(), "source-store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sourceStore.Close() })
	snapshot, err := sourceStore.Capture(context.Background(), root, admission.Bound)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := sourceStore.Materialize(t.TempDir(), snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"app/.source.txt":        "package app\nimport _ \"embed\"\n//go:embed assets/payload.sql\nvar payload string\n",
		"app/embed.go":           "package app\nimport _ \"embed\"\n//go:embed assets/payload.sql\nvar payload string\n",
		"app/assets/payload.sql": "SELECT 1;\n",
	} {
		got, err := os.ReadFile(filepath.Join(materialized, rel))
		if err != nil || string(got) != want {
			t.Fatalf("materialized %s = %q, %v", rel, got, err)
		}
	}
}

func TestGoEmbedSemanticInputsUnderLifecycleRootsMustTravelOrRefuse(t *testing.T) {
	write := func(t *testing.T, root, rel, contents string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(t *testing.T, root string, outputs ...string) jobs.PortablePlanInputs {
		t.Helper()
		selection, err := store.CollectKeyFileSelection(filepath.Join(root, "app"), []string{"**/*.go", "go-embed:build"})
		if err != nil {
			t.Fatal(err)
		}
		relative := func(paths []string) []string {
			var result []string
			for _, p := range paths {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					t.Fatal(err)
				}
				result = append(result, filepath.ToSlash(rel))
			}
			return result
		}
		return jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{
			Key: "/app:build~describe", Files: relative(selection.Files), GoEmbedFiles: relative(selection.GoEmbed),
		}}, Outputs: outputs}
	}
	for _, test := range []struct {
		name, source, payload, output, omitted string
		link                                   bool
	}{
		{"ignored source referent", ".gen/source.txt", "assets/payload.sql", "", "app/.gen/source.txt", true},
		{"ignored embedded payload", "embed.go", ".gen/payload.sql", "", "app/.gen/payload.sql", false},
		{"ignored declared output", "embed.go", "conf/local.txt", "app/conf", "app/conf/local.txt", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := admissionRepo(t)
			src := "package app\nimport _ \"embed\"\n//go:embed " + test.payload + "\nvar payload string\n"
			if test.link {
				write(t, root, "app/"+test.source, src)
				if err := os.Symlink(test.source, filepath.Join(root, "app/embed.go")); err != nil {
					t.Skipf("source-file symlinks unavailable: %v", err)
				}
			} else {
				write(t, root, "app/embed.go", src)
			}
			if test.payload != "conf/local.txt" {
				write(t, root, "app/"+test.payload, "SELECT 1;\n")
			}
			inputs := plan(t, root, test.output)
			if !slices.Contains(inputs.Tasks[0].GoEmbedFiles, test.omitted) {
				t.Fatalf("collector lost semantic provenance for %s: %+v", test.omitted, inputs.Tasks[0])
			}
			_, err := AdmitInputs(context.Background(), root, inputs)
			if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), test.omitted) || !strings.Contains(err.Error(), "/app:build~describe") {
				t.Fatalf("ignored semantic input %s was admitted: %v", test.omitted, err)
			}
		})
	}
	for _, test := range []struct {
		name, payload, output string
		tracked               bool
	}{
		{"tracked lifecycle input", ".gen/payload.sql", "", true},
		{"nonignored declared output", "generated/payload.sql", "app/generated", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := admissionRepo(t)
			write(t, root, "app/embed.go", "package app\nimport _ \"embed\"\n//go:embed "+test.payload+"\nvar payload string\n")
			write(t, root, "app/"+test.payload, "SELECT 1;\n")
			if test.tracked {
				cmd := exec.Command("git", "-C", root, "add", "-f", "app/"+test.payload)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("track lifecycle input: %v: %s", err, out)
				}
			}
			inputs := plan(t, root, test.output)
			admission, err := AdmitInputs(context.Background(), root, inputs)
			if err != nil || len(admission.Bound) != 0 {
				t.Fatalf("capturable semantic input was refused: %+v, %v", admission, err)
			}
			sourceStore, err := runnersource.OpenStore(filepath.Join(t.TempDir(), "source-store"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sourceStore.Close() })
			snapshot, err := sourceStore.Capture(context.Background(), root, admission.Bound)
			if err != nil {
				t.Fatal(err)
			}
			want := "app/" + test.payload
			found := false
			for _, entry := range snapshot.Manifest.Entries {
				found = found || entry.Path == want
			}
			if !found {
				t.Fatalf("capturable semantic input %s absent from snapshot", want)
			}
		})
	}
}

func TestGoSourceAliasesSelectedByGoInputsCaptureTheirReferents(t *testing.T) {
	for _, test := range []struct {
		name, link, target string
	}{
		{"lexical out source", "app/out/p/source.go", "../../.source.txt"},
		{"ordinary underscore source", "app/_fixtures/source.go", "../.source.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := admissionRepo(t)
			ignore, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ignore.WriteString("app/.source.txt\napp/out/\n"); err != nil {
				t.Fatal(err)
			}
			if err := ignore.Close(); err != nil {
				t.Fatal(err)
			}
			source := "package app\nconst Value = 1\n"
			if err := os.WriteFile(filepath.Join(root, "app/.source.txt"), []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, filepath.FromSlash(test.link))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target, link); err != nil {
				t.Skipf("source-file symlinks unavailable: %v", err)
			}
			selection, err := store.CollectKeyFileSelection(filepath.Join(root, "app"), []string{"**/*.go", "go-embed:build"})
			if err != nil {
				t.Fatal(err)
			}
			var files, semantic []string
			for _, paths := range []struct {
				absolute []string
				into     *[]string
			}{{selection.Files, &files}, {selection.GoEmbed, &semantic}} {
				for _, p := range paths.absolute {
					rel, err := filepath.Rel(root, p)
					if err != nil {
						t.Fatal(err)
					}
					*paths.into = append(*paths.into, filepath.ToSlash(rel))
				}
			}
			if !slices.Contains(files, test.link) || !slices.Contains(semantic, "app/.source.txt") {
				t.Fatalf("source alias or referent omitted: files=%v semantic=%v", files, semantic)
			}
			inputs := jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{Key: "/app:build~describe", Files: files, GoEmbedFiles: semantic}}}
			admission, err := AdmitInputs(context.Background(), root, inputs)
			if err != nil || !slices.Contains(admission.Bound, "app/.source.txt") {
				t.Fatalf("source alias admission = %+v, %v", admission, err)
			}
			sourceStore, err := runnersource.OpenStore(filepath.Join(t.TempDir(), "source-store"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sourceStore.Close() })
			snapshot, err := sourceStore.Capture(context.Background(), root, admission.Bound)
			if err != nil {
				t.Fatal(err)
			}
			materialized, err := sourceStore.Materialize(t.TempDir(), snapshot.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(materialized, filepath.FromSlash(test.link))); err != nil || string(got) != source {
				t.Fatalf("materialized source alias = %q, %v", got, err)
			}
		})
	}
}

func TestGoSourceAliasUnderIgnoredLifecycleRootMustBeTrackedOrRefused(t *testing.T) {
	root := admissionRepo(t)
	ignore, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ignore.WriteString("app/.source.txt\n"); err != nil {
		t.Fatal(err)
	}
	if err := ignore.Close(); err != nil {
		t.Fatal(err)
	}
	source := "package app\nconst Value = 1\n"
	if err := os.WriteFile(filepath.Join(root, "app/.source.txt"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "app/.gen/source.go")
	if err := os.Symlink("../.source.txt", link); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	plan := func() jobs.PortablePlanInputs {
		t.Helper()
		selection, err := store.CollectKeyFileSelection(filepath.Join(root, "app"), []string{"**/*.go", "go-embed:build"})
		if err != nil {
			t.Fatal(err)
		}
		relative := func(paths []string) []string {
			var names []string
			for _, p := range paths {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					t.Fatal(err)
				}
				names = append(names, filepath.ToSlash(rel))
			}
			return names
		}
		return jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{
			Key: "/app:build~describe", Files: relative(selection.Files), GoEmbedFiles: relative(selection.GoEmbed),
		}}}
	}
	inputs := plan()
	for _, name := range []string{"app/.gen/source.go", "app/.source.txt"} {
		if !slices.Contains(inputs.Tasks[0].GoEmbedFiles, name) {
			t.Fatalf("missing semantic source path %s: %+v", name, inputs.Tasks[0])
		}
	}
	if _, err := AdmitInputs(context.Background(), root, inputs); !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "app/.gen/source.go") {
		t.Fatalf("ignored lifecycle source alias was admitted: %v", err)
	}
	cmd := exec.Command("git", "-C", root, "add", "-f", "app/.gen/source.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("track source alias: %v: %s", err, out)
	}
	admission, err := AdmitInputs(context.Background(), root, plan())
	if err != nil || !slices.Equal(admission.Bound, []string{"app/.source.txt"}) {
		t.Fatalf("tracked lifecycle source alias admission = %+v, %v", admission, err)
	}
	sourceStore, err := runnersource.OpenStore(filepath.Join(t.TempDir(), "source-store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sourceStore.Close() })
	snapshot, err := sourceStore.Capture(context.Background(), root, admission.Bound)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := sourceStore.Materialize(t.TempDir(), snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(materialized, "app/.gen/source.go")); err != nil || string(got) != source {
		t.Fatalf("tracked lifecycle source alias was not captured: %q, %v", got, err)
	}
}

func TestAdmitInputsBindsOnlyRequiredIgnoredFiles(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	inputs := jobs.PortablePlanInputs{
		Tasks: []jobs.PortableTaskInputs{
			{Key: "/app:build~compile", Files: []string{"app/.gen/generated.txt", "app/conf/local.txt", "app/conf/new.txt", "app/conf/tracked.txt", "app/dist/bundle.js", "app/node_modules/dep.js"}},
			{Key: "/app:test~run", Files: []string{".putnami/state.json", "app/conf/local.txt"}},
		},
		Outputs: []string{"app/dist"},
	}
	admission, err := AdmitInputs(context.Background(), root, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(admission.Bound, ",") != "app/conf/local.txt" {
		t.Fatalf("bound = %v; tracked, untracked non-ignored, lifecycle-recreated and declared-output paths must not bind", admission.Bound)
	}
	if len(admission.Unbound) != 0 {
		t.Fatalf("a declared input was reported as unbound: %+v", admission.Unbound)
	}
	// Without the declared output, the ignored build directory IS a declared
	// input the key hashes, and it binds.
	inputs.Outputs = nil
	admission, err = AdmitInputs(context.Background(), root, inputs)
	if err != nil || strings.Join(admission.Bound, ",") != "app/conf/local.txt,app/dist/bundle.js" {
		t.Fatalf("bound without outputs = %v (%v)", admission.Bound, err)
	}
	if empty, err := AdmitInputs(context.Background(), root, jobs.PortablePlanInputs{}); err != nil || empty.Bound != nil || empty.Unbound != nil {
		t.Fatalf("an empty plan admitted %+v (%v)", empty, err)
	}
	if _, err := AdmitInputs(context.Background(), t.TempDir(), inputs); err == nil || errors.Is(err, ErrInadmissible) {
		t.Fatalf("a Git failure must be an error of its own class, not a usage refusal: %v", err)
	}
}

// The whole-tree fallback of a task with no declared file input is not a
// statement of what it reads: nothing it selects is bound — not even a path
// past the protocol's size limit, which must not refuse the request — and
// every skipped path is reported with its task and its remedy.
func TestAdmitInputsReportsFallbackInputsInsteadOfBindingThem(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	oversize, err := os.Create(filepath.Join(root, "app", "dist", "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := oversize.Truncate(runner.MaxSourceFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := oversize.Close(); err != nil {
		t.Fatal(err)
	}
	fallback := []string{"app/.gen/generated.txt", "app/conf/local.txt", "app/conf/tracked.txt", "app/dist/huge.bin"}
	for index := range 6 {
		name := fmt.Sprintf("app/dist/artifact-%d.bin", index)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte("built"), 0o644); err != nil {
			t.Fatal(err)
		}
		fallback = append(fallback, name)
	}
	admission, err := AdmitInputs(context.Background(), root, jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{
		{Key: "/app:test~run", FallbackFiles: fallback},
		{Key: "/app:build~compile", Files: []string{"app/conf/local.txt"}},
	}})
	if err != nil {
		t.Fatalf("an oversize ignored artifact in the fallback set refused the request: %v", err)
	}
	// The path one task declares is bound even though another only reaches it
	// through its fallback: a declaration decides, wherever it comes from.
	if strings.Join(admission.Bound, ",") != "app/conf/local.txt" {
		t.Fatalf("bound = %v", admission.Bound)
	}
	if len(admission.Unbound) != 1 || admission.Unbound[0].Task != "/app:test~run" {
		t.Fatalf("unbound = %+v", admission.Unbound)
	}
	// Tracked, lifecycle-recreated and bound paths are not reported: only
	// ignored files that genuinely do not travel are.
	if got := strings.Join(admission.Unbound[0].Paths, ","); got != "app/dist/artifact-0.bin,app/dist/artifact-1.bin,app/dist/artifact-2.bin,app/dist/artifact-3.bin,app/dist/artifact-4.bin,app/dist/artifact-5.bin,app/dist/huge.bin" {
		t.Fatalf("unbound paths = %q", got)
	}
	var report strings.Builder
	admission.ReportUnbound(&report)
	text := report.String()
	for _, want := range []string{"/app:test~run", "declares no file inputs", "7 git-ignored file(s)", "app/dist/artifact-0.bin", "and 2 more", "filePatterns"} {
		if !strings.Contains(text, want) {
			t.Errorf("the diagnostic does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "app/conf/local.txt") || strings.Contains(text, "app/dist/artifact-5.bin") {
		t.Errorf("the diagnostic names a bound path or exceeds its cap:\n%s", text)
	}
	var silent strings.Builder
	Admission{Bound: []string{"a"}}.ReportUnbound(&silent)
	if silent.Len() != 0 {
		t.Errorf("an admission with nothing unbound printed %q", silent.String())
	}
}

func TestAdmitInputsRefusesWhatCannotTravel(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	_, err := AdmitInputs(context.Background(), root, jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{
		{Key: "/app:build~compile", Files: []string{"app/conf/local.txt"}},
		{Key: "/app:test~run", Env: []string{"DATABASE_URL", "TARGET"}},
	}})
	if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "/app:test~run") || !strings.Contains(err.Error(), "DATABASE_URL, TARGET") {
		t.Fatalf("env-keyed task: %v", err)
	}
	// A directory is never bound: paths only. The projection never names one,
	// and a caller that does is refused with the task and the path.
	_, err = AdmitInputs(context.Background(), root, jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{
		{Key: "/app:build~compile", Files: []string{"app/dist"}},
	}})
	if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "/app:build~compile") || !strings.Contains(err.Error(), "app/dist") || !strings.Contains(err.Error(), "regular file or symlink") {
		t.Fatalf("directory: %v", err)
	}
}

func TestVerifyAdmissionRefusesADivergentBoundSet(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	inputs := jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{
		{Key: "/app:build~compile", Files: []string{"app/.gen/generated.txt", "app/conf/local.txt", "app/conf/tracked.txt"}},
	}}
	if err := VerifyAdmission(root, []string{"app/conf/local.txt"}, inputs); err != nil {
		t.Fatalf("the admitted set was refused: %v", err)
	}
	if err := VerifyAdmission(root, nil, inputs); err != nil {
		t.Fatalf("an empty admitted set was refused: %v", err)
	}
	for name, admitted := range map[string][]string{
		"not an input": {"app/conf/missing.txt"},
		"recreated":    {"app/.gen/generated.txt"},
	} {
		if err := VerifyAdmission(root, admitted, inputs); err == nil || !strings.Contains(err.Error(), "admitted inputs differ") || !strings.Contains(err.Error(), admitted[0]) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(root, "app", "conf", "local.txt")); err != nil {
		t.Fatal(err)
	}
	inputs.Tasks[0].Files = append(inputs.Tasks[0].Files, "app/conf/local.txt")
	if err := VerifyAdmission(root, []string{"app/conf/local.txt"}, inputs); err == nil || !strings.Contains(err.Error(), "admitted inputs differ") {
		t.Errorf("an absent bound input was accepted: %v", err)
	}
	if err := VerifyAdmission(root, nil, jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{Key: "/app:test~run", Env: []string{"TARGET"}}}}); !errors.Is(err, ErrInadmissible) {
		t.Errorf("an env-keyed re-planned graph was accepted: %v", err)
	}
	// A path the executing tree only reaches through the whole-tree fallback
	// is not a declaration, so it cannot admit a bound path here either.
	fallbackOnly := jobs.PortablePlanInputs{Tasks: []jobs.PortableTaskInputs{{Key: "/app:test~run", FallbackFiles: []string{"app/conf/tracked.txt"}}}}
	if err := VerifyAdmission(root, []string{"app/conf/tracked.txt"}, fallbackOnly); err == nil || !strings.Contains(err.Error(), "not a declared input") {
		t.Errorf("a fallback-only path admitted a bound input: %v", err)
	}
	manifest := runner.SourceManifest{Version: 1, Entries: []runner.SourceEntry{
		{Path: "a", Kind: "file", Digest: runner.BlobDigest(nil), Mode: "0644", Bound: true},
		{Path: "b", Kind: "symlink", Target: "a"},
		{Path: "c", Kind: "symlink", Target: "a", Bound: true},
	}}
	if got := strings.Join(boundEntries(manifest), ","); got != "a,c" {
		t.Errorf("boundEntries = %q", got)
	}
	if _, err := bindSource(runner.ExecutionRequest{Source: runner.SourceBlock{Bound: []string{"a"}}}, root, runnersource.Snapshot{Manifest: manifest}); err == nil || !strings.Contains(err.Error(), "differs from what was admitted") {
		t.Errorf("a snapshot whose bound set differs from the admitted one was bound: %v", err)
	}
}
