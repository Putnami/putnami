package pkg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// observedEmitter is an emitter whose log and diagnostic messages the test
// reads back.
func observedEmitter() (*jsonl.Emitter, func() []string) {
	emit := jsonl.New()
	var mu sync.Mutex
	var messages []string
	emit.Observe(func(message string) {
		mu.Lock()
		defer mu.Unlock()
		messages = append(messages, message)
	})
	return emit, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), messages...)
	}
}

// linkWarnings are the messages warnLinksLeavingArchive emitted.
func linkWarnings(messages []string) []string {
	var warnings []string
	for _, message := range messages {
		if strings.HasPrefix(message, "archived ") {
			warnings = append(warnings, message)
		}
	}
	return warnings
}

func packageTestExtension(t *testing.T, workspaceRoot, projectRoot string, emit *jsonl.Emitter) map[string][]byte {
	t.Helper()
	outputRoot := t.TempDir()
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
		Params: pctx.Params{"platforms": []byte(`["linux/amd64"]`)},
	}
	if ok := createExtensionArchives(ctx, emit, "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}
	return readArchiveEntries(t, filepath.Join(outputRoot, "archives", "putnami-testext-linux-x64.tar.gz"))
}

// TestCreateExtensionArchivesShipsDocAndSpecs holds the archive to the files
// its guidance links: the doc and specs trees travel beside README.md and
// AI.md, byte for byte, so the links resolve where an agent reads them.
func TestCreateExtensionArchivesShipsDocAndSpecs(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-guidance-is-self-contained", "an-extension-archive-ships-its-doc-and-specs-trees")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "ext", "testext")
	guide := "# Guide\n\n## Install\n\nRun it.\n"

	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)
	mustWrite(t, filepath.Join(projectRoot, "README.md"), "# testext\n\nSee [install](doc/guide.md#install) and [the ADR](doc/adr/0001-why.md).\n")
	mustWrite(t, filepath.Join(projectRoot, "AI.md"), "# Guidance\n\nThe [specification](specs/toolchain.json) states the promise.\n")
	mustWrite(t, filepath.Join(projectRoot, "doc", "guide.md"), guide)
	mustWrite(t, filepath.Join(projectRoot, "doc", "adr", "0001-why.md"), "# Why\n")
	mustWrite(t, filepath.Join(projectRoot, "specs", "toolchain.json"), "{}\n")

	emit, messages := observedEmitter()
	entries := packageTestExtension(t, workspaceRoot, projectRoot, emit)

	want := []string{
		"AI.md",
		"README.md",
		"doc/adr/0001-why.md",
		"doc/guide.md",
		"putnami.extension.json",
		"specs/toolchain.json",
	}
	if got := archiveLayout(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("archive layout = %v, want %v", got, want)
	}
	if got := string(entries["doc/guide.md"]); got != guide {
		t.Errorf("archived doc/guide.md = %q, want the authored bytes %q", got, guide)
	}
	if warnings := linkWarnings(messages()); len(warnings) != 0 {
		t.Errorf("packaging warned on links the archive carries: %v", warnings)
	}
}

// TestCreateExtensionArchivesWarnsOnALinkLeavingTheArchive reads the shipped
// guidance with the archive as its whole tree: a link to a workspace file the
// archive does not carry, or to a missing one, warns and names the file and
// the target, while a putnami.dev page is not checked. Packaging still
// succeeds.
func TestCreateExtensionArchivesWarnsOnALinkLeavingTheArchive(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-guidance-is-self-contained", "a-shipped-link-that-leaves-the-archive-warns")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "ext", "testext")

	// The targets exist in the workspace, so the repository's own link check
	// accepts them; the archive carries neither.
	mustWrite(t, filepath.Join(workspaceRoot, "RELEASE.md"), "# Release\n")
	mustWrite(t, filepath.Join(projectRoot, "internal", "notes.md"), "# Notes\n")
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)
	mustWrite(t, filepath.Join(projectRoot, "README.md"), "# testext\n\nSee [the release policy](../../RELEASE.md) and [the site](https://putnami.dev/docs/support).\n")
	mustWrite(t, filepath.Join(projectRoot, "AI.md"), "# Guidance\n\nRead [the notes](internal/notes.md).\n")

	emit, messages := observedEmitter()
	packageTestExtension(t, workspaceRoot, projectRoot, emit)

	warnings := linkWarnings(messages())
	if len(warnings) != 2 {
		t.Fatalf("link warnings = %v, want one for each of the two links the archive cannot resolve", warnings)
	}
	for i, want := range []struct{ file, target string }{
		{"AI.md", "internal/notes.md"},
		{"README.md", "../../RELEASE.md"},
	} {
		if !strings.HasPrefix(warnings[i], "archived "+want.file+",") || !strings.Contains(warnings[i], want.target) {
			t.Errorf("warning %d = %q, want it to name %s and the target %s", i, warnings[i], want.file, want.target)
		}
	}
}

// TestCreateExtensionArchivesBundlesFrameworkGuidanceOnlyForGo keeps the Go
// framework guidance in the Go extension's archive: another extension the Go
// packager archives, such as the TypeScript one, carries only its own.
func TestCreateExtensionArchivesBundlesFrameworkGuidanceOnlyForGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-guidance-is-self-contained", "only-the-go-extension-bundles-go-framework-guidance")
	for _, tc := range []struct {
		name      string
		framework bool
	}{
		{goExtensionName, true},
		{"@putnami/typescript", false},
		{"@putnami/testext", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspaceRoot := t.TempDir()
			projectRoot := filepath.Join(workspaceRoot, "ext", "testext")
			mustWrite(t, filepath.Join(workspaceRoot, "go", "framework", "http", "AI.md"), "# http framework\n")
			mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"),
				strings.Replace(conformingManifest, `"name": "@putnami/testext"`, `"name": "`+tc.name+`"`, 1))

			entries := packageTestExtension(t, workspaceRoot, projectRoot, jsonl.New())

			_, got := entries["framework-docs/http/AI.md"]
			if got != tc.framework {
				t.Errorf("%s archive carries framework-docs/http/AI.md = %v, want %v (layout %v)", tc.name, got, tc.framework, archiveLayout(entries))
			}
		})
	}
}

// TestShippedExtensionGuidanceResolvesInsideTheArchive stages the real Go and
// TypeScript extensions as packaging does and requires every relative link of
// their README.md and AI.md to resolve inside the stage: a link a fresh agent
// follows in the installed archive is never a dead end.
func TestShippedExtensionGuidanceResolvesInsideTheArchive(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-guidance-is-self-contained", "the-go-and-typescript-extension-guidance-resolves-inside-their-archives")
	workspaceRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"go/extension", "typescript/extension"} {
		t.Run(rel, func(t *testing.T) {
			projectRoot := filepath.Join(workspaceRoot, filepath.FromSlash(rel))
			if _, err := os.Stat(filepath.Join(projectRoot, "putnami.extension.json")); err != nil {
				t.Fatalf("the workspace has no extension at %s: %v", rel, err)
			}
			stageDir := t.TempDir()
			stageCommonFiles(projectRoot, stageDir, jsonl.New())
			if stagedManifestName(stageDir) == goExtensionName {
				stageFrameworkDocs(workspaceRoot, stageDir, jsonl.New())
			}
			findings, err := archiveLinkFindings(stageDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				file, _ := filepath.Rel(stageDir, finding.File)
				t.Errorf("%s/%s:%d: %s", rel, filepath.ToSlash(file), finding.Line, finding.Message)
			}
		})
	}
}
