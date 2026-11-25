package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func writeRuntimeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two prepared runtimes are the same only with the same files, bytes and
// executable bits.
func TestSameRuntimeContents(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(a, "compiled", "putnami-x"), "binary")
	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary")
	if same, err := sameRuntimeContents(a, b); err != nil || !same {
		t.Fatalf("identical runtimes: same=%v err=%v", same, err)
	}

	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary!")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different bytes compare the same")
	}

	writeRuntimeFile(t, b, "compiled/putnami-x", "binary")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different executable bits compare the same")
	}

	writeExecutable(t, filepath.Join(b, "compiled", "putnami-x"), "binary")
	writeRuntimeFile(t, b, "config/extra.json", "{}")
	if same, _ := sameRuntimeContents(a, b); same {
		t.Error("runtimes with different files compare the same")
	}

	if _, err := sameRuntimeContents(a, filepath.Join(b, "missing")); err == nil {
		t.Error("a missing runtime compares without an error")
	}
}

// The base checkout holds the commit's files, not the working tree's, and is
// removed afterwards.
func TestCheckoutBase(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := gittest.Command(root, args...)
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "commit.gpgsign", "false")
	writeRuntimeFile(t, root, "ext/main.go", "package main // base\n")
	writeExecutable(t, filepath.Join(root, "ext", "bin", "prepare"), "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-m", "base")
	writeRuntimeFile(t, root, "ext/main.go", "package main // head\n")

	dir, cleanup, err := checkoutBase(root, "HEAD")
	if err != nil {
		t.Fatalf("checkoutBase: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ext", "main.go"))
	if err != nil || string(got) != "package main // base\n" {
		t.Fatalf("base checkout holds %q (%v), want the committed file", got, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "ext", "bin", "prepare")); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("the committed executable is not executable in the base checkout: %v %v", info, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the base checkout %s survives its cleanup: %v", dir, err)
	}

	if _, _, err := checkoutBase(root, "no-such-ref"); err == nil {
		t.Error("an unknown base checks out without an error")
	}
}

// Only candidates that are extensions preparing from source get an answer,
// and a diff with no base keeps every edge.
func TestCompareExtensionRuntimes_AnswersOnlyForSourcePreparedExtensions(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir(), Projects: []*workspace.Project{{ID: "/app", Name: "app", Path: "app"}}}
	if got := CompareExtensionRuntimes(context.Background(), ws, nil, []string{"/app"}, "abc"); got != nil {
		t.Errorf("a plain project got an answer: %+v", got)
	}
	if got := CompareExtensionRuntimes(context.Background(), nil, nil, []string{"/app"}, "abc"); got != nil {
		t.Errorf("no workspace got an answer: %+v", got)
	}
}

// runtimeComparisonRepo is a git repository holding one in-workspace
// extension, /ext, whose prepared runtime copies bin/runtime-template and
// nothing else, committed once. It returns the workspace, the extension and the
// base commit; the test then edits the working tree as the head.
func runtimeComparisonRepo(t *testing.T) (*workspace.Workspace, *extension.ExtensionDescription, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	extDir := filepath.Join(root, "ext")
	writeRuntimeFixture(t, extDir, "@putnami/test", "1.2.3")
	writeRuntimeFile(t, extDir, extension.ManifestFilename, "{}\n")
	writeRuntimeFile(t, root, "putnami.lock.json", `{"version":5,"cli":{"source":"workspace"}}`+"\n")
	commitRuntimeRepo(t, root, "base")
	base := gitOutput(t, root, "rev-parse", "HEAD")

	ext := runtimeTestExtension(extDir)
	ext.RelPath = "ext"
	ws := &workspace.Workspace{Root: root, Projects: []*workspace.Project{{ID: "/ext", Name: "ext", Path: "ext"}}}
	return ws, ext, base
}

func commitRuntimeRepo(t *testing.T, root, message string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--quiet"}, {"config", "commit.gpgsign", "false"}, {"add", "-A"}, {"commit", "--quiet", "-m", message},
	} {
		cmd := gittest.Command(root, args...)
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := gittest.Command(root, args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// A preparation input that ends up nowhere in the runtime moves the input
// digest and leaves the runtime byte for byte the same.
func TestCompareExtensionRuntimes_AnUnlinkedInputLeavesTheRuntimeUnchanged(t *testing.T) {
	ws, ext, base := runtimeComparisonRepo(t)
	writeRuntimeFile(t, ext.Path, "cmd/source", "source, edited")
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || !got[0].Unchanged {
		t.Fatalf("an unlinked input change compared %+v, want /ext unchanged", got)
	}
}

// A change to what the runtime is built from keeps the edge.
func TestCompareExtensionRuntimes_AMovedRuntimeKeepsTheEdge(t *testing.T) {
	ws, ext, base := runtimeComparisonRepo(t)
	template, err := os.ReadFile(filepath.Join(ext.Path, "bin", "runtime-template"))
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(ext.Path, "bin", "runtime-template"), string(template)+"# moved\n")
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || got[0].Unchanged || got[0].Reason != runtimeReasonDiffers {
		t.Fatalf("a moved runtime compared %+v, want /ext kept because it differs", got)
	}
}

// An extension the base does not hold keeps the edge.
func TestCompareExtensionRuntimes_AnExtensionAbsentAtTheBaseKeepsTheEdge(t *testing.T) {
	ws, ext, base := runtimeComparisonRepo(t)
	added := filepath.Join(ws.Root, "added")
	writeRuntimeFixture(t, added, "@putnami/added", "1.0.0")
	writeRuntimeFile(t, added, extension.ManifestFilename, "{}\n")
	addedExt := runtimeTestExtension(added)
	addedExt.Name, addedExt.RelPath = "@putnami/added", "added"
	ws.Projects = append(ws.Projects, &workspace.Project{ID: "/added", Name: "added", Path: "added"})
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext, addedExt}, []string{"/added"}, base)
	if len(got) != 1 || got[0].Project != "/added" || got[0].Unchanged || got[0].Reason != runtimeReasonNoBaseExtension {
		t.Fatalf("an extension absent at the base compared %+v, want it kept", got)
	}
}

// A run that holds the run credential prepares no runtime, its own or the
// base's: with nothing in the store it compares nothing.
func TestCompareExtensionRuntimes_AHostedRunPreparesNothing(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-hosted-run-compares-runtimes-prepared-before-it", "a-hosted-comparison-prepares-nothing")
	ws, ext, base := runtimeComparisonRepo(t)
	restore := runcredential.SetForTest("bearer")
	defer restore()
	writeRuntimeFile(t, ext.Path, "cmd/source", "source, edited")
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || got[0].Unchanged || got[0].Reason != runtimeReasonHeadNotPrepared {
		t.Fatalf("a hosted run compared %+v, want /ext kept because nothing prepared its runtime", got)
	}
	if ext.RuntimeDigest != "" || ext.RuntimeExecutable != "" {
		t.Errorf("a hosted run prepared the head runtime: digest %q executable %q", ext.RuntimeDigest, ext.RuntimeExecutable)
	}
	if err := runcredential.RequireCustody("test"); err != nil {
		t.Errorf("a hosted comparison recorded repository code: %v", err)
	}
}

// A runtime the two locks pin different prepare toolchains for is another
// runtime, whatever its sources: the edge stays, and neither side is prepared
// with the head's resolution.
func TestCompareExtensionRuntimes_DifferentLockedToolchainsKeepTheEdge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	lock := func(version string) {
		writeRuntimeFile(t, root, "putnami.lock.json", `{"version":5,"toolchains":{"tool":{"version":"`+version+`",`+
			`"integrities":{"`+platform+`":"0000000000000000000000000000000000000000000000000000000000000000"},`+
			`"archives":{"`+platform+`":{"url":"https://example.com/tool-`+version+`.tar.gz","format":"tar.gz","executable":"tool/bin/tool"}}}}}`+"\n")
	}
	extDir := filepath.Join(root, "ext")
	writeRuntimeFixture(t, extDir, "@putnami/test", "1.2.3")
	writeRuntimeFile(t, extDir, extension.ManifestFilename, "{}\n")
	lock("1.0.0")
	commitRuntimeRepo(t, root, "base")
	base := gitOutput(t, root, "rev-parse", "HEAD")
	lock("2.0.0")

	ext := runtimeTestExtension(extDir)
	ext.RelPath = "ext"
	ext.Runtime.Toolchains = map[string]extensionproto.RuntimeToolchain{"compiler": {Lock: "tool", Optional: true}}
	ext.Runtime.Prepare.Toolchains = []string{"compiler"}
	ws := &workspace.Workspace{Root: root, Projects: []*workspace.Project{{ID: "/ext", Name: "ext", Path: "ext"}}}
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || got[0].Unchanged || got[0].Reason != runtimeReasonToolchains {
		t.Fatalf("a toolchain bump compared %+v, want /ext kept because its toolchains differ", got)
	}
}

// The runtimes `putnami extensions prepare` admits before a hosted run are the
// ones the hosted comparison finds: it proves the runtime unchanged without
// preparing anything or recording repository code.
func TestPrepareComparisonRuntimes_LetsAHostedRunCompare(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-hosted-run-compares-runtimes-prepared-before-it", "extensions-prepare-admits-the-runtimes-a-hosted-comparison-looks-up")
	ws, ext, base := runtimeComparisonRepo(t)
	writeRuntimeFile(t, ext.Path, "cmd/source", "source, edited")
	notes, err := PrepareComparisonRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, base)
	if err != nil || len(notes) != 0 {
		t.Fatalf("PrepareComparisonRuntimes: notes %v, err %v", notes, err)
	}

	restore := runcredential.SetForTest("bearer")
	defer restore()
	hosted := runtimeTestExtension(ext.Path)
	hosted.RelPath = "ext"
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{hosted}, []string{"/ext"}, base)
	if len(got) != 1 || !got[0].Unchanged {
		t.Fatalf("a hosted run after the preparation compared %+v, want /ext unchanged", got)
	}
	if hosted.RuntimeDigest != "" || hosted.RuntimeExecutable != "" {
		t.Errorf("the hosted comparison prepared the head runtime: digest %q", hosted.RuntimeDigest)
	}
	if err := runcredential.RequireCustody("test"); err != nil {
		t.Errorf("the hosted comparison recorded repository code: %v", err)
	}
	if _, err := PrepareComparisonRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, base); err == nil {
		t.Error("PrepareComparisonRuntimes ran under the run credential")
	}
}

// An extension whose runtime inputs are the same at the base is not prepared:
// a hosted run answers it unchanged from the two digests alone.
func TestPrepareComparisonRuntimes_SkipsAnUnchangedExtension(t *testing.T) {
	ws, ext, base := runtimeComparisonRepo(t)
	notes, err := PrepareComparisonRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, base)
	if err != nil || len(notes) != 0 {
		t.Fatalf("PrepareComparisonRuntimes: notes %v, err %v", notes, err)
	}
	entries, err := os.ReadDir(os.Getenv("PUTNAMI_ARTIFACT_DIR"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "sha256" {
			if inner, _ := os.ReadDir(filepath.Join(os.Getenv("PUTNAMI_ARTIFACT_DIR"), "sha256")); len(inner) > 0 {
				t.Errorf("an extension with the same inputs at the base was prepared: %d store entries", len(inner))
			}
		}
	}
}

// A hosted run trusts the store only in a workspace whose own launcher
// prepared it: elsewhere it compares nothing.
func TestCompareExtensionRuntimes_AHostedRunOutsideASourceWorkspaceComparesNothing(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-hosted-run-compares-runtimes-prepared-before-it", "a-hosted-comparison-prepares-nothing")
	ws, ext, base := runtimeComparisonRepo(t)
	writeRuntimeFile(t, ws.Root, "putnami.lock.json", `{"version":5}`+"\n")
	restore := runcredential.SetForTest("bearer")
	defer restore()
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || got[0].Unchanged || got[0].Reason != runtimeReasonNotSourceWorkspace {
		t.Fatalf("a hosted run outside a source workspace compared %+v, want /ext kept", got)
	}
}

// The same locked inputs prepare the same runtime: a hosted run says so from
// the two digests, with nothing in the store.
func TestCompareExtensionRuntimes_AHostedRunAnswersEqualDigestsUnchanged(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "a-hosted-run-compares-runtimes-prepared-before-it", "a-hosted-comparison-prepares-nothing")
	ws, ext, base := runtimeComparisonRepo(t)
	restore := runcredential.SetForTest("bearer")
	defer restore()
	got := CompareExtensionRuntimes(context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"/ext"}, base)
	if len(got) != 1 || !got[0].Unchanged {
		t.Fatalf("equal digests compared %+v, want /ext unchanged", got)
	}
	if ext.RuntimeDigest != "" {
		t.Errorf("the hosted comparison prepared the head runtime: %q", ext.RuntimeDigest)
	}
}
