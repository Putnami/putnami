package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// kitProfile is a stack whose apps hold their marker one directory down and
// whose test step runs the extension's own runtime.
const kitProfile = `{
  "id": "kit",
  "markers": ["Package.kit", "*.app/project.def"],
  "toolchain": {"command": ["kit", "--version"], "pattern": "kit (?<version>\\S+)", "files": [{"path": ".kit-version"}]},
  "inputs": {"sources": ["**/*.src"], "manifests": ["Package.kit", "*.app/project.def"]},
  "excludes": [".build"],
  "tasks": {
    "lint": {"steps": [["kit", "lint"]], "inputs": ["sources", "manifests"]},
    "test": {"steps": [["{runtime}", "convert", "--to", "{output}/out.txt"]], "inputs": ["sources", "manifests"]}
  }
}`

func kitStack(t *testing.T) Stack {
	t.Helper()
	p, err := Decode([]byte(kitProfile))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	detect := func(root string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		all := []Component{
			{Path: "App", Name: "Shop", Root: "App"},
			{Path: "App", Name: "Other", Root: "App"},
			{Path: "pkg", Name: "Pkg", Root: "pkg"},
		}
		var out []Component
		for _, c := range all {
			if slices.Contains(request.Paths, c.Path) {
				out = append(out, c)
			}
		}
		return out, nil
	}
	return Stack{Extension: "@putnami/kit", Profile: p, Detect: detect}
}

func TestANestedMarkerAndExcludesAreValidated(t *testing.T) {
	for name, doc := range map[string]string{
		"deep marker":      strings.Replace(kitProfile, `"*.app/project.def"]`, `"a/b/project.def"]`, 1),
		"recursive marker": strings.Replace(kitProfile, `"*.app/project.def"]`, `"**/project.def"]`, 1),
		"exclude path":     strings.Replace(kitProfile, `[".build"]`, `["a/.build"]`, 1),
		"exclude glob":     strings.Replace(kitProfile, `[".build"]`, `["*.build"]`, 1),
	} {
		if _, err := Decode([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
}

func TestTheProbeKeepsTheFirstComponentOfAPathAndWatchesNoWildcard(t *testing.T) {
	root := t.TempDir()
	s := kitStack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"App", "pkg"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 2 || result.Projects[0].SourceName != "Shop" {
		t.Fatalf("projects = %+v", result.Projects)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeComponentConflict || !strings.Contains(result.Diagnostics[0].Message, "Other") {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	for _, w := range result.Projects[0].WatchedFiles {
		if strings.ContainsAny(w, "*?[") {
			t.Errorf("a wildcard marker is no watched file: %v", result.Projects[0].WatchedFiles)
		}
	}
}

func TestExcludedDirectoriesStayOutOfTheInputs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "App/main.src", "")
	writeFile(t, root, "App/Shop.app/project.def", "")
	writeFile(t, root, "App/.build/gen.src", "")
	writeFile(t, root, "App/deep/.build/gen.src", "")
	got := resolveOne(t, kitStack(t), root, "App", "kit:lint")
	if want := []string{"Shop.app/project.def", "main.src"}; got.Error != "" || !reflect.DeepEqual(got.Files, want) {
		t.Errorf("lint inputs = %+v, want %v", got, want)
	}
	// The basis leaves the skipped directories out too: a build writing
	// there moves no digest.
	project := filepath.Join(root, "App")
	before, err := wsproto.InputBasisDigest(project, got.Basis)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "App/.build/new.src", "x")
	writeFile(t, root, "App/.putnami/out/x.src", "x")
	writeFile(t, root, "App/.git/objects/x.src", "x")
	if after, err := wsproto.InputBasisDigest(project, got.Basis); err != nil || after != before {
		t.Errorf("a write under a skipped directory moved the digest: %v", err)
	}
	writeFile(t, root, "App/main.src", "changed")
	if after, _ := wsproto.InputBasisDigest(project, got.Basis); after == before {
		t.Error("a source change moves the digest")
	}
	// An entry named like a skipped directory that is no directory, such as
	// the .git file of a worktree or a linked build directory, leaves the
	// answer with the basis, and the answer stays valid.
	writeFile(t, root, "App/sub/.git", "gitdir: ../../.git/worktrees/x\n")
	if err := os.Symlink(filepath.Join(root, "App", "main.src"), filepath.Join(root, "App", "sub", ".build")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "App/sub/kept.src", "")
	writeFile(t, root, "App/putnami.json", `{"options": {"@putnami/kit": {"inputs": {"sources": ["**"]}}}}`)
	if got := resolveOne(t, kitStack(t), root, "App", "kit:lint"); got.Error != "" || slices.Contains(got.Files, "sub/.git") || slices.Contains(got.Files, "sub/.build") || !slices.Contains(got.Files, "sub/kept.src") {
		t.Errorf("lint inputs = %+v", got)
	}
	// A directory a pattern names before its wildcard stays in the basis.
	if got := basisSet([]string{".build/**", "src/**"}, []string{".build"}); slices.Contains(got, "!**/.build/**") || !slices.Contains(got, "!**/.git/**") {
		t.Errorf("basisSet = %v", got)
	}
	if got := basisSet([]string{"Package.kit"}, []string{".build"}); !reflect.DeepEqual(got, []string{"Package.kit"}) {
		t.Errorf("a basis with no ** walk stays as it is: %v", got)
	}
}

func TestHasMarkerReadsTheMarkerAloneAsAPattern(t *testing.T) {
	root := t.TempDir()
	s := kitStack(t)
	writeFile(t, root, "a[1]/Shop.app/project.def", "")
	writeFile(t, root, "b*/Package.kit", "")
	if !s.hasMarker(root, "a[1]") || !s.hasMarker(root, "b*") {
		t.Error("a directory whose name holds pattern characters keeps its marker")
	}
	if s.hasMarker(root, "a1") || s.hasMarker(root, "c") {
		t.Error("no marker, no component")
	}
}

func TestAStepRunsTheExtensionRuntimeCommand(t *testing.T) {
	// The shell prints its resolved working directory, so the root is
	// compared resolved: on macOS the temporary directory is a symlink.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "App/.kit-version", "1.0\n")
	bin := t.TempDir()
	writeFile(t, bin, "kit", "#!/bin/sh\necho 'kit 1.0'\n")
	// The runtime stands in for the extension's executable: it logs the
	// command it was asked to run.
	writeFile(t, bin, "runtime", "#!/bin/sh\necho \"$PWD $*\" > \""+filepath.Join(root, "ran")+"\"\n")
	for _, f := range []string{"kit", "runtime"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := kitStack(t)
	s.Runtime = filepath.Join(bin, "runtime")
	meta, _ := json.Marshal(Metadata{Component: "Shop", Root: "App"})
	rec := &recorder{}
	out := filepath.Join(root, "out")
	status, _, _ := s.run(context.Background(), runInput{task: TaskTest, workspaceRoot: root, projectAbs: filepath.Join(root, "App"), metadata: meta, outputRoot: out, stdout: io.Discard}, rec)
	if status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	ran, _ := os.ReadFile(filepath.Join(root, "ran"))
	if want := filepath.Join(root, "App") + " convert --to " + filepath.Join(out, "test", "out.txt") + "\n"; string(ran) != want {
		t.Errorf("ran %q, want %q", ran, want)
	}
	s.Runtime = ""
	if got, err := s.runtimePath(); err != nil || got == "" {
		t.Errorf("{runtime} defaults to the running executable: %q %v", got, err)
	}
}

func TestEntrypointDispatchesARuntimeCommand(t *testing.T) {
	s := kitStack(t)
	var got []string
	s.Commands = map[string]Command{
		"convert": func(args []string, stdout, _ io.Writer) error {
			got = args
			_, err := io.WriteString(stdout, "converted")
			return err
		},
		"broken": func([]string, io.Writer, io.Writer) error { return errors.New("no bundle") },
	}
	var out bytes.Buffer
	if err := s.Entrypoint([]string{"convert", "--to", "x"}, strings.NewReader(""), &out, "1", nil); err != nil || out.String() != "converted" || !reflect.DeepEqual(got, []string{"--to", "x"}) {
		t.Errorf("convert: %v %q %v", err, out.String(), got)
	}
	if err := s.Entrypoint([]string{"broken"}, strings.NewReader(""), &out, "1", nil); err == nil || err.Error() != "broken: no bundle" {
		t.Errorf("a failed command is the entrypoint's error: %v", err)
	}
}
