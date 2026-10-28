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
	gitexec "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/runnersource"
	"go.putnami.dev/tooling/cli/internal/store"
)

func admissionRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitexec.Executable(), append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
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

// listedInputs expands each selector to the files it lists, relative to the
// directory the selection reads.
type listedInputs map[string][]string

func (l listedInputs) ExpandInput(_, selector string) (store.InputExpansion, error) {
	files, ok := l[selector]
	if !ok {
		return store.InputExpansion{}, fmt.Errorf("%w: %s lists nothing", store.ErrInputSelector, selector)
	}
	return store.InputExpansion{Files: files}, nil
}

func (l listedInputs) Identity() string { return fmt.Sprint(map[string][]string(l)) }

// writeAdmissionFile writes contents at rel under root.
func writeAdmissionFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// appendIgnore appends lines to the repository's .gitignore.
func appendIgnore(t *testing.T, root, lines string) {
	t.Helper()
	ignore, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ignore.WriteString(lines); err != nil {
		_ = ignore.Close()
		t.Fatal(err)
	}
	if err := ignore.Close(); err != nil {
		t.Fatal(err)
	}
}

// selectorPlan collects the app project's selection of **/*.go and sel:build
// as one task's portable inputs, its paths relative to root.
func selectorPlan(t *testing.T, root string, expander store.InputExpander, outputs ...string) cachekey.PortablePlanInputs {
	t.Helper()
	selection, err := store.CollectKeyFileSelection(filepath.Join(root, "app"), []string{"**/*.go", "sel:build"}, expander)
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
	return cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{{
		Key: "/app:build~describe", Files: relative(selection.Files), SelectedFiles: relative(selection.Selected),
	}}, Outputs: outputs}
}

// captureAndMaterialize captures bound from root and materializes the
// snapshot in a fresh directory.
func captureAndMaterialize(t *testing.T, root string, bound []string) string {
	t.Helper()
	sourceStore, err := runnersource.OpenStore(filepath.Join(t.TempDir(), "source-store"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sourceStore.Close() })
	snapshot, err := sourceStore.Capture(context.Background(), root, bound)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := sourceStore.Materialize(t.TempDir(), snapshot.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return materialized
}

// An ignored file a selector lists binds, travels and materializes beside the
// link that reads it.
func TestListedLinkReferentIsCapturedAndMaterialized(t *testing.T) {
	root := admissionRepo(t)
	appendIgnore(t, root, "app/.source.txt\n")
	source := "package app\n"
	writeAdmissionFile(t, root, "app/.source.txt", source)
	writeAdmissionFile(t, root, "app/assets/payload.sql", "SELECT 1;\n")
	if err := os.Symlink(".source.txt", filepath.Join(root, "app/embed.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	inputs := selectorPlan(t, root, listedInputs{"sel:build": {".source.txt", "assets/payload.sql"}})
	files := inputs.Tasks[0].Files
	if !slices.Contains(files, "app/.source.txt") || !slices.Contains(files, "app/assets/payload.sql") || !slices.Contains(files, "app/embed.go") {
		t.Fatalf("collector omitted the link, its referent or a listed payload: %v", files)
	}
	admission, err := AdmitInputs(context.Background(), root, inputs)
	if err != nil || !slices.Equal(admission.Bound, []string{"app/.source.txt"}) {
		t.Fatalf("listed referent admission = %+v, %v", admission, err)
	}
	materialized := captureAndMaterialize(t, root, admission.Bound)
	for rel, want := range map[string]string{"app/.source.txt": source, "app/embed.go": source, "app/assets/payload.sql": "SELECT 1;\n"} {
		got, err := os.ReadFile(filepath.Join(materialized, rel))
		if err != nil || string(got) != want {
			t.Fatalf("materialized %s = %q, %v", rel, got, err)
		}
	}
}

// A file a selector lists under a lifecycle root or a declared output travels
// only when Git tracks it or nothing ignores it; otherwise the admission
// refuses the task that requires it.
func TestListedInputsUnderLifecycleRootsMustTravelOrRefuse(t *testing.T) {
	for _, test := range []struct {
		name, listed, output string
	}{
		{"ignored listed file", ".gen/payload.sql", ""},
		{"ignored declared output", "conf/local.txt", "app/conf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := admissionRepo(t)
			writeAdmissionFile(t, root, "app/embed.go", "package app\n")
			if test.listed != "conf/local.txt" {
				writeAdmissionFile(t, root, "app/"+test.listed, "SELECT 1;\n")
			}
			inputs := selectorPlan(t, root, listedInputs{"sel:build": {test.listed}}, test.output)
			omitted := "app/" + test.listed
			if !slices.Contains(inputs.Tasks[0].SelectedFiles, omitted) {
				t.Fatalf("collector lost the listing of %s: %+v", omitted, inputs.Tasks[0])
			}
			_, err := AdmitInputs(context.Background(), root, inputs)
			if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), omitted) || !strings.Contains(err.Error(), "/app:build~describe") {
				t.Fatalf("ignored listed input %s was admitted: %v", omitted, err)
			}
		})
	}
	for _, test := range []struct {
		name, listed, output string
		tracked              bool
	}{
		{"tracked lifecycle input", ".gen/payload.sql", "", true},
		{"nonignored declared output", "generated/payload.sql", "app/generated", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := admissionRepo(t)
			writeAdmissionFile(t, root, "app/embed.go", "package app\n")
			writeAdmissionFile(t, root, "app/"+test.listed, "SELECT 1;\n")
			if test.tracked {
				cmd := exec.Command(gitexec.Executable(), "-C", root, "add", "-f", "app/"+test.listed)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("track lifecycle input: %v: %s", err, out)
				}
			}
			inputs := selectorPlan(t, root, listedInputs{"sel:build": {test.listed}}, test.output)
			admission, err := AdmitInputs(context.Background(), root, inputs)
			if err != nil || len(admission.Bound) != 0 {
				t.Fatalf("capturable listed input was refused: %+v, %v", admission, err)
			}
			materialized := captureAndMaterialize(t, root, admission.Bound)
			if _, err := os.Stat(filepath.Join(materialized, "app", filepath.FromSlash(test.listed))); err != nil {
				t.Fatalf("capturable listed input %s absent from the snapshot: %v", test.listed, err)
			}
		})
	}
}

// A link a selector lists under an ignored lifecycle root is refused until Git
// tracks it; its listed referent then binds and the link materializes.
func TestListedLinkUnderIgnoredLifecycleRootMustBeTrackedOrRefused(t *testing.T) {
	root := admissionRepo(t)
	appendIgnore(t, root, "app/.source.txt\n")
	source := "package app\n"
	writeAdmissionFile(t, root, "app/.source.txt", source)
	if err := os.Symlink("../.source.txt", filepath.Join(root, "app/.gen/source.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	expander := listedInputs{"sel:build": {".gen/source.go", ".source.txt"}}
	inputs := selectorPlan(t, root, expander)
	for _, name := range []string{"app/.gen/source.go", "app/.source.txt"} {
		if !slices.Contains(inputs.Tasks[0].SelectedFiles, name) {
			t.Fatalf("missing listed path %s: %+v", name, inputs.Tasks[0])
		}
	}
	if _, err := AdmitInputs(context.Background(), root, inputs); !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "app/.gen/source.go") {
		t.Fatalf("ignored lifecycle link was admitted: %v", err)
	}
	cmd := exec.Command(gitexec.Executable(), "-C", root, "add", "-f", "app/.gen/source.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("track the link: %v: %s", err, out)
	}
	admission, err := AdmitInputs(context.Background(), root, selectorPlan(t, root, expander))
	if err != nil || !slices.Equal(admission.Bound, []string{"app/.source.txt"}) {
		t.Fatalf("tracked lifecycle link admission = %+v, %v", admission, err)
	}
	materialized := captureAndMaterialize(t, root, admission.Bound)
	if got, err := os.ReadFile(filepath.Join(materialized, "app/.gen/source.go")); err != nil || string(got) != source {
		t.Fatalf("tracked lifecycle link was not captured: %q, %v", got, err)
	}
}

func TestAdmitInputsBindsOnlyRequiredIgnoredFiles(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	inputs := cachekey.PortablePlanInputs{
		Tasks: []cachekey.PortableTaskInputs{
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
	if empty, err := AdmitInputs(context.Background(), root, cachekey.PortablePlanInputs{}); err != nil || empty.Bound != nil || empty.Unbound != nil {
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
	admission, err := AdmitInputs(context.Background(), root, cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{
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
	_, err := AdmitInputs(context.Background(), root, cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{
		{Key: "/app:build~compile", Files: []string{"app/conf/local.txt"}},
		{Key: "/app:test~run", Env: []string{"DATABASE_URL", "TARGET"}},
	}})
	if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "/app:test~run") || !strings.Contains(err.Error(), "DATABASE_URL, TARGET") {
		t.Fatalf("env-keyed task: %v", err)
	}
	// A directory is never bound: paths only. The projection never names one,
	// and a caller that does is refused with the task and the path.
	_, err = AdmitInputs(context.Background(), root, cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{
		{Key: "/app:build~compile", Files: []string{"app/dist"}},
	}})
	if !errors.Is(err, ErrInadmissible) || !strings.Contains(err.Error(), "/app:build~compile") || !strings.Contains(err.Error(), "app/dist") || !strings.Contains(err.Error(), "regular file or symlink") {
		t.Fatalf("directory: %v", err)
	}
}

func TestVerifyAdmissionRefusesADivergentBoundSet(t *testing.T) {
	t.Parallel()
	root := admissionRepo(t)
	inputs := cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{
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
	if err := VerifyAdmission(root, nil, cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{{Key: "/app:test~run", Env: []string{"TARGET"}}}}); !errors.Is(err, ErrInadmissible) {
		t.Errorf("an env-keyed re-planned graph was accepted: %v", err)
	}
	// A path the executing tree only reaches through the whole-tree fallback
	// is not a declaration, so it cannot admit a bound path here either.
	fallbackOnly := cachekey.PortablePlanInputs{Tasks: []cachekey.PortableTaskInputs{{Key: "/app:test~run", FallbackFiles: []string{"app/conf/tracked.txt"}}}}
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
