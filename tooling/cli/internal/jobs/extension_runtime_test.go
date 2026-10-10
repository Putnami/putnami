package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	model "go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestExtensionRuntimeDigestDeterministicAndComplete(t *testing.T) {
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n", 0o644)
	ext := runtimeTestExtension(root)

	first, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := extensionRuntimeDigest(ext)
	if err != nil || first != second {
		t.Fatalf("determinism: %q, %q, %v", first, second, err)
	}

	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n// changed\n", 0o644)
	changedInput, _ := extensionRuntimeDigest(ext)
	if changedInput == first {
		t.Fatal("declared input content did not invalidate runtime digest")
	}
	ext.Name = "@putnami/other"
	changedIdentity, _ := extensionRuntimeDigest(ext)
	if changedIdentity == changedInput {
		t.Fatal("extension identity did not invalidate runtime digest")
	}
	ext.Runtime.Prepare.Args = append(ext.Runtime.Prepare.Args, "--changed")
	changedDeclaration, _ := extensionRuntimeDigest(ext)
	if changedDeclaration == changedIdentity {
		t.Fatal("prepare declaration did not invalidate runtime digest")
	}
}

func TestCollectRuntimeInputsRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "real"), "x", 0o644)
	if err := os.Symlink("real", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := collectRuntimeInputs(root, []string{"**"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("collectRuntimeInputs error = %v, want symlink rejection", err)
	}
}

func TestRuntimeReplacementCoveragePackageIsStagedAndDigested(t *testing.T) {
	root := t.TempDir()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require example.dev/sdk v0.0.0

replace example.dev/sdk => ../sdk
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	coverageSource := filepath.Join(sdkRoot, "coverage", "coverage.go")
	mustWriteRuntimeFile(t, coverageSource, "package coverage\n\nconst Enabled = true\n", 0o644)

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	before, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}

	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	stagedExtension, err := stageRuntimeSourceView(extensionRoot, t.TempDir(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	stagedCoverage := filepath.Join(filepath.Dir(stagedExtension), "sdk", "coverage", "coverage.go")
	if _, err := os.Stat(stagedCoverage); err != nil {
		t.Fatalf("required replacement package coverage/ was not staged: %v", err)
	}

	mustWriteRuntimeFile(t, coverageSource, "package coverage\n\nconst Enabled = false\n", 0o644)
	after, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("coverage/ replacement package edit did not invalidate runtime digest")
	}
}

// vendor/ and dist/ are named in the project-walk exclusions the replacement
// enumeration borrows its install-directory names from, and both are carved back
// out: vendor/ is build input whenever a module vendors its dependencies, and
// dist/ is ordinary source in a replaced module. Dropping either carve-out would
// silently remove the tree from the staged view and leave the digest unable to
// explain the GOWORK=off build failure that follows, so pin each name on both
// sides — staged, and digest-sensitive.
func TestRuntimeReplacementVendorAndDistStayStagedAndDigested(t *testing.T) {
	for _, directory := range []string{"vendor", "dist"} {
		t.Run(directory, func(t *testing.T) {
			root := t.TempDir()
			extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
			source := filepath.Join(sdkRoot, directory, "example.dev", "dep", "dep.go")
			mustWriteRuntimeFile(t, source, "package dep\n\nconst Enabled = true\n", 0o644)

			ext := runtimeTestExtension(extensionRoot)
			ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
			before, err := extensionRuntimeDigest(ext)
			if err != nil {
				t.Fatal(err)
			}

			stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
			staged := filepath.Join(stagedRoot, "sdk", directory, "example.dev", "dep", "dep.go")
			if _, err := os.Stat(staged); err != nil {
				t.Fatalf("replacement %s/ is build input and was not staged: %v", directory, err)
			}

			mustWriteRuntimeFile(t, source, "package dep\n\nconst Enabled = false\n", 0o644)
			after, err := extensionRuntimeDigest(ext)
			if err != nil {
				t.Fatal(err)
			}
			if after == before {
				t.Fatalf("a replacement %s/ edit did not invalidate the runtime digest", directory)
			}
		})
	}
}

func TestRuntimeReplacementGeneratedStateIsExcludedFromDigestAndStaging(t *testing.T) {
	root := t.TempDir()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require example.dev/sdk v0.0.0

replace example.dev/sdk => ../sdk
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "sdk.go"), "package sdk\n", 0o644)

	versionStamp := filepath.Join(sdkRoot, ".gen", "version.json")
	mustWriteRuntimeFile(t, versionStamp, `{"buildTime":"2026-07-30T12:00:00Z"}`, 0o644)

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	before, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}

	mustWriteRuntimeFile(t, versionStamp, `{"buildTime":"2026-07-30T12:01:00Z"}`, 0o644)
	after, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("scheduler-owned replacement .gen state invalidated runtime digest")
	}

	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	stagedExtension, err := stageRuntimeSourceView(extensionRoot, t.TempDir(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	stagedStamp := filepath.Join(filepath.Dir(stagedExtension), "sdk", ".gen", "version.json")
	if _, err := os.Stat(stagedStamp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scheduler-owned replacement .gen state was staged: %v", err)
	}
}

func writeRuntimeReplacementFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require example.dev/sdk v0.0.0

replace example.dev/sdk => ../sdk
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "sdk.go"), "package sdk\n", 0o644)
	return extensionRoot, sdkRoot
}

func stageRuntimeReplacementView(t *testing.T, extensionRoot string, ext *extension.ExtensionDescription) string {
	t.Helper()
	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	stagedExtension, err := stageRuntimeSourceView(extensionRoot, t.TempDir(), inputs)
	if err != nil {
		t.Fatalf("stageRuntimeSourceView: %v", err)
	}
	return filepath.Dir(stagedExtension)
}

// A provider workload commits clients/ts next to clients/go, so any workspace
// install plants node_modules/.bin/<name> as a symlink inside a module a CLI
// library replaces. That install output is neither module source nor input to a
// Go build: staging must not refuse it, and the digest must not follow it.
func TestRuntimeReplacementInstallOutputIsExcludedFromDigestAndStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	goClient := filepath.Join(sdkRoot, "clients", "go", "client.go")
	mustWriteRuntimeFile(t, goClient, "package client\n", 0o644)
	tsPackage := filepath.Join(sdkRoot, "clients", "ts")
	mustWriteRuntimeFile(t, filepath.Join(tsPackage, "package.json"), `{"name":"example-client"}`, 0o644)
	mustWriteRuntimeFile(t,
		filepath.Join(tsPackage, "node_modules", "@putnami", "client", "bin", "generate.js"),
		"#!/usr/bin/env node\n", 0o755)
	binDir := filepath.Join(tsPackage, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join("..", "@putnami", "client", "bin", "generate.js"),
		filepath.Join(binDir, "putnami-client-generate"),
	); err != nil {
		t.Fatal(err)
	}

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	installed, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatalf("digest refused an installed replacement module: %v", err)
	}

	stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
	stagedSDK := filepath.Join(stagedRoot, "sdk")
	if _, err := os.Lstat(filepath.Join(stagedSDK, "clients", "ts", "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package-manager install output was staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagedSDK, "clients", "go", "client.go")); err != nil {
		t.Fatalf("the generated Go client beside the install output was not staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagedSDK, "clients", "ts", "package.json")); err != nil {
		t.Fatalf("committed TypeScript client source was not staged: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(tsPackage, "node_modules")); err != nil {
		t.Fatal(err)
	}
	uninstalled, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if uninstalled != installed {
		t.Fatal("node_modules install state moved the prepared-runtime digest")
	}

	mustWriteRuntimeFile(t, goClient, "package client\n\nconst Changed = true\n", 0o644)
	edited, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if edited == uninstalled {
		t.Fatal("a client source edit beside the pruned install output did not invalidate the digest")
	}
}

// A symlink that stays inside the replaced module is staged as the same link,
// so the staged view and the digest describe one tree.
func TestRuntimeReplacementContainedSymlinkIsStagedAsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "NOTICE"), "notice\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "OTHER"), "other\n", 0o644)
	link := filepath.Join(sdkRoot, "internal", "NOTICE")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "NOTICE"), link); err != nil {
		t.Fatal(err)
	}

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	before, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatalf("digest refused a contained symlink: %v", err)
	}

	stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
	stagedLink := filepath.Join(stagedRoot, "sdk", "internal", "NOTICE")
	info, err := os.Lstat(stagedLink)
	if err != nil {
		t.Fatalf("contained symlink was not staged: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("contained symlink was staged as a copied file, not as the link the digest hashed")
	}
	target, err := os.Readlink(stagedLink)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("..", "NOTICE") {
		t.Fatalf("staged link target = %q, want the source link target", target)
	}
	// The recreated link must resolve inside the staged copy, never back into
	// the source tree the build does not own.
	resolved, err := filepath.EvalSymlinks(stagedLink)
	if err != nil {
		t.Fatalf("staged link does not resolve inside the staged view: %v", err)
	}
	stagedSDK, err := filepath.EvalSymlinks(filepath.Join(stagedRoot, "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resolved, stagedSDK+string(filepath.Separator)) {
		t.Fatalf("staged link resolved to %q, outside the staged replacement %q", resolved, stagedSDK)
	}
	if content, err := os.ReadFile(stagedLink); err != nil || string(content) != "notice\n" {
		t.Fatalf("staged link content = %q, %v", content, err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "OTHER"), link); err != nil {
		t.Fatal(err)
	}
	after, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("repointing a contained symlink did not invalidate the prepared-runtime digest")
	}
}

// A contained link to a directory is staged through dirlink, so on Windows it
// becomes a junction that needs no Developer Mode. Unix keeps the symlink.
func TestRuntimeReplacementContainedDirectoryLinkIsStagedAsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the source fixture needs a symlink, which Windows restricts")
	}
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "docs", "NOTICE"), "notice\n", 0o644)
	if err := os.Symlink("docs", filepath.Join(sdkRoot, "alias")); err != nil {
		t.Fatal(err)
	}

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	if _, err := extensionRuntimeDigest(ext); err != nil {
		t.Fatalf("digest refused a contained directory link: %v", err)
	}
	stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
	stagedLink := filepath.Join(stagedRoot, "sdk", "alias")
	info, err := os.Lstat(stagedLink)
	if err != nil {
		t.Fatalf("contained directory link was not staged: %v", err)
	}
	if !dirlink.IsLink(stagedLink, info) {
		t.Fatalf("contained directory link was staged with mode %v, not as a link", info.Mode())
	}
	resolved, err := dirlink.Resolve(stagedLink)
	if err != nil {
		t.Fatal(err)
	}
	stagedDocs, err := dirlink.Resolve(filepath.Join(stagedRoot, "sdk", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != stagedDocs {
		t.Fatalf("staged directory link resolved to %q, want the staged %q", resolved, stagedDocs)
	}
	if content, err := os.ReadFile(filepath.Join(stagedLink, "NOTICE")); err != nil || string(content) != "notice\n" {
		t.Fatalf("read through the staged directory link = %q, %v", content, err)
	}
}

// A replaced module can contain another replaced module, so the same path is
// staged twice. The second staging must reproduce the first rather than fail on
// an existing link, the way the file copy's rename already does.
func TestRuntimeReplacementNestedModuleStagesContainedSymlinkTwice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := t.TempDir()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	innerRoot := filepath.Join(sdkRoot, "inner")
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require (
	example.dev/inner v0.0.0
	example.dev/sdk v0.0.0
)

replace example.dev/sdk => ../sdk

replace example.dev/inner => ../sdk/inner
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "cmd", "main.go"), "package main\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(innerRoot, "go.mod"), "module example.dev/inner\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(innerRoot, "NOTICE"), "notice\n", 0o644)
	if err := os.Symlink("NOTICE", filepath.Join(innerRoot, "LICENSE")); err != nil {
		t.Fatal(err)
	}

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	if _, err := extensionRuntimeDigest(ext); err != nil {
		t.Fatalf("digest refused nested replacements: %v", err)
	}
	stagedRoot := stageRuntimeReplacementView(t, extensionRoot, ext)
	target, err := os.Readlink(filepath.Join(stagedRoot, "sdk", "inner", "LICENSE"))
	if err != nil {
		t.Fatalf("nested replacement link was not staged: %v", err)
	}
	if target != "NOTICE" {
		t.Fatalf("staged link target = %q, want %q", target, "NOTICE")
	}
}

// Containment is the reason the refusal exists: the prepared runtime is
// content-addressed, so a link that leaves the replaced module would let the
// build read bytes no digest covers. The digest and the staging walk must
// refuse it identically, or a tree the digest accepts fails at staging time.
func TestRuntimeReplacementEscapingSymlinkIsRefusedByDigestAndStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	for _, testCase := range []struct {
		name   string
		target func(root string) string
	}{
		{name: "relative", target: func(string) string { return filepath.Join("..", "outside", "secret.go") }},
		{name: "absolute", target: func(root string) string { return filepath.Join(root, "outside", "secret.go") }},
		{name: "through a contained directory", target: func(string) string {
			return filepath.Join("internal", "..", "..", "outside", "secret.go")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
			mustWriteRuntimeFile(t, filepath.Join(root, "outside", "secret.go"), "package secret\n", 0o644)
			mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "internal", "keep.go"), "package internal\n", 0o644)
			if err := os.Symlink(testCase.target(root), filepath.Join(sdkRoot, "secret.go")); err != nil {
				t.Fatal(err)
			}

			ext := runtimeTestExtension(extensionRoot)
			ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
			_, digestErr := extensionRuntimeDigest(ext)
			if digestErr == nil || !strings.Contains(digestErr.Error(), "outside the replacement root") {
				t.Fatalf("digest error = %v, want an escaping-symlink refusal", digestErr)
			}

			inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
			if err != nil {
				t.Fatal(err)
			}
			viewRoot := t.TempDir()
			_, stageErr := stageRuntimeSourceView(extensionRoot, viewRoot, inputs)
			if stageErr == nil || !strings.Contains(stageErr.Error(), "outside the replacement root") {
				t.Fatalf("staging error = %v, want an escaping-symlink refusal", stageErr)
			}
			if _, err := os.Lstat(filepath.Join(viewRoot, "sdk", "secret.go")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an escaping symlink reached the staged view: %v", err)
			}
		})
	}
}

// The report is a prepare-runtime refusal, so pin the whole path: a self-hosted
// extension whose go.mod replaces a provider workload directory must prepare,
// and its prepare must run against a staged view that carries the replaced
// module's Go source, keeps a contained link, and drops the install output.
func TestPrepareRuntimeStagesReplacedWorkloadWithInstalledClientPackage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	extensionRoot := filepath.Join(root, "extension")
	sdkRoot := filepath.Join(root, "sdk")
	writeRuntimeFixtureWithControls(t, extensionRoot, "@putnami/test", "1.2.3", runtimeFixtureControls{
		prepareAssertions: `test -f ../sdk/clients/go/client.go || { printf 'prepare: the staged view lost the replaced module Go client\n' >&2; exit 5; }
test ! -e ../sdk/clients/ts/node_modules || { printf 'prepare: the staged view carries package-manager install output\n' >&2; exit 6; }
test -L ../sdk/NOTICE.link || { printf 'prepare: the staged view lost a contained link\n' >&2; exit 7; }
`,
	})
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require example.dev/sdk v0.0.0

replace example.dev/sdk => ../sdk
`, 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "clients", "go", "client.go"), "package client\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "NOTICE"), "notice\n", 0o644)
	if err := os.Symlink("NOTICE", filepath.Join(sdkRoot, "NOTICE.link")); err != nil {
		t.Fatal(err)
	}
	tsPackage := filepath.Join(sdkRoot, "clients", "ts")
	mustWriteRuntimeFile(t,
		filepath.Join(tsPackage, "node_modules", "@putnami", "client", "bin", "generate.js"),
		"#!/usr/bin/env node\n", 0o755)
	binDir := filepath.Join(tsPackage, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join("..", "@putnami", "client", "bin", "generate.js"),
		filepath.Join(binDir, "putnami-client-generate"),
	); err != nil {
		t.Fatal(err)
	}

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	executable, err := prepareOrLoadExtensionRuntime(
		context.Background(), artifactstore.New(t.TempDir()), ext)
	if err != nil {
		t.Fatalf("prepare-runtime refused a replaced workload directory: %v", err)
	}
	if info, statErr := os.Stat(executable); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("prepared runtime %q: %v", executable, statErr)
	}
}

func TestRuntimeSynchronizationPrecedesTaskTimeoutAndInvocation(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	extensionRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	counter := filepath.Join(t.TempDir(), "prepares")
	artifacts := artifactstore.New(t.TempDir())
	writeRuntimeFixtureWithControls(t, extensionRoot, "@putnami/test", "1.2.3", runtimeFixtureControls{
		counterPath:  counter,
		prepareDelay: 50 * time.Millisecond,
	})

	ext := runtimeTestExtension(extensionRoot)
	def := &extension.JobDefinition{
		Name:      "test",
		Command:   "{" + extensionproto.TemplateVarExtensionRuntime + "}",
		TimeoutMs: 1,
	}
	ext.Jobs = map[string]*extension.JobDefinition{"test": def}
	project := &workspace.Project{ID: "/project", Name: "project", Path: "."}
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{project})

	started := time.Now()
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{ext}, nil, artifacts,
	); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("fixture preparation completed in %s; it did not exceed the 1ms task timeout", elapsed)
	}
	if ext.RuntimeExecutable == "" {
		t.Fatal("runtime synchronization did not publish the resolved executable")
	}

	// Make a second preparation impossible. Invocation must remain lookup-only:
	// changing the declaration cannot trigger a new build under the task timer.
	ext.Runtime.Prepare.Command = "putnami-prepare-must-not-run-during-invocation"
	job := &ScheduledJob{Project: project, Extension: ext, JobDef: def}
	invocationCtx, cancel, inv, err := prepareJobInvocation(
		context.Background(), ws, job, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("first invocation tried to prepare the runtime: %v", err)
	}
	cancel()
	_ = os.Remove(inv.contextFile)
	if invocationCtx == nil || inv.command != ext.RuntimeExecutable {
		t.Fatalf("invocation runtime = %q, want synchronized %q", inv.command, ext.RuntimeExecutable)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "prepare\n"); got != 1 {
		t.Fatalf("prepare count after first invocation = %d, want exactly one during synchronization", got)
	}
}

func TestCommandScopedRuntimeSynchronizationPreservesBootstrap(t *testing.T) {
	ext := &extension.ExtensionDescription{
		Name: "@putnami/bootstrap",
		Path: t.TempDir(),
		Runtime: &extension.RuntimeDefinition{
			Executable: "compiled/missing",
		},
		Jobs: map[string]*extension.JobDefinition{
			"workspace-install": {
				Name:        "workspace-install",
				CommandName: "workspace-install",
				Command:     "{extensionRoot}/bin/workspace-install",
			},
			"build": {
				Name:        "build",
				CommandName: "build",
				Command:     "{extensionRuntime}",
			},
		},
	}
	ws := &workspace.Workspace{Root: t.TempDir()}

	if err := SynchronizeExtensionRuntimesForCommands(
		context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"workspace-install"}, nil,
	); err != nil {
		t.Fatalf("runtime-free bootstrap command synchronized an unavailable runtime: %v", err)
	}
	if ext.RuntimeExecutable != "" {
		t.Fatalf("bootstrap command resolved runtime %q, want no runtime work", ext.RuntimeExecutable)
	}

	err := SynchronizeExtensionRuntimesForCommands(
		context.Background(), ws, []*extension.ExtensionDescription{ext}, []string{"build"}, nil,
	)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeExecutableMissing {
		t.Fatalf("runtime-backed command error = %v, want %s", err, extensionproto.FailureRuntimeExecutableMissing)
	}
}

func TestCommandScopedRuntimeSynchronizationIncludesCommandDependencies(t *testing.T) {
	ext := &extension.ExtensionDescription{
		Name: "@putnami/dependency",
		Path: t.TempDir(),
		Runtime: &extension.RuntimeDefinition{
			Executable: "compiled/missing",
		},
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				Name:             "publish",
				CommandName:      "publish",
				Command:          "/bin/true",
				CommandDependsOn: []string{"build"},
			},
			"build": {
				Name:        "build",
				CommandName: "build",
				Command:     "{extensionRuntime}",
			},
		},
	}
	err := SynchronizeExtensionRuntimesForCommands(
		context.Background(), &workspace.Workspace{Root: t.TempDir()},
		[]*extension.ExtensionDescription{ext}, []string{"publish"}, nil,
	)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeExecutableMissing {
		t.Fatalf("dependency runtime error = %v, want %s", err, extensionproto.FailureRuntimeExecutableMissing)
	}
}

func TestCommandScopedRuntimeSynchronizationIncludesSessionPrerequisiteClosure(t *testing.T) {
	orchestrator := &extension.ExtensionDescription{
		Name: "@putnami/orchestrator",
		Jobs: map[string]*extension.JobDefinition{
			"deploy": {
				Name:        "deploy",
				CommandName: "deploy",
				Command:     "/bin/true",
				SessionPrerequisites: []extension.SessionPrerequisiteDefinition{{
					Command:   "publish",
					DependsOn: []string{"lint", "test", "build", "validate", "validate-workspace"},
				}},
			},
		},
	}
	runtimeExtension := &extension.ExtensionDescription{
		Name: "@putnami/runtime",
		Path: t.TempDir(),
		Runtime: &extension.RuntimeDefinition{
			Executable: "compiled/missing",
		},
		Jobs: map[string]*extension.JobDefinition{
			"lint": {
				Name:        "lint",
				CommandName: "lint",
				Command:     "{extensionRuntime}",
			},
		},
	}
	active := commandDependencyClosure(
		[]*extension.ExtensionDescription{orchestrator, runtimeExtension}, []string{"deploy"},
	)
	for _, command := range []string{"deploy", "publish", "lint", "test", "build", "validate", "validate-workspace"} {
		if !active[command] {
			t.Errorf("session-prerequisite closure omitted %q", command)
		}
	}

	err := SynchronizeExtensionRuntimesForCommands(
		context.Background(), &workspace.Workspace{Root: t.TempDir()},
		[]*extension.ExtensionDescription{orchestrator, runtimeExtension}, []string{"deploy"}, nil,
	)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeExecutableMissing {
		t.Fatalf("session-prerequisite gate runtime error = %v, want %s", err, extensionproto.FailureRuntimeExecutableMissing)
	}
}

func TestSynchronizeExtensionRuntimesScansEveryHookField(t *testing.T) {
	// cacheClean/cacheGC left this list: the cache
	// lifecycle is a reserved COMMAND now, so its {extensionRuntime} use is
	// found by the job scan instead (TestSynchronizeExtensionRuntimesScansCacheCommands).
	hookNames := []string{"preBuild", "onInstall"}
	fields := []string{"command", "cwd", "args", "env"}
	for _, hookName := range hookNames {
		for _, field := range fields {
			t.Run(hookName+"/"+field, func(t *testing.T) {
				hook := &extension.HookDefinition{Command: "/bin/true"}
				switch field {
				case "command":
					hook.Command = "{extensionRuntime}"
				case "cwd":
					hook.Cwd = "{extensionRuntime}/work"
				case "args":
					hook.Args = []string{"--runtime={extensionRuntime}"}
				case "env":
					hook.Env = map[string]string{"RUNTIME": "{extensionRuntime}"}
				}
				hooks := &extension.ManifestHooks{}
				switch hookName {
				case "preBuild":
					hooks.PreBuild = hook
				case "onInstall":
					hooks.OnInstall = hook
				}
				ext := &extension.ExtensionDescription{
					Name:  "@putnami/no-runtime",
					Hooks: hooks,
				}

				err := SynchronizeExtensionRuntimes(
					context.Background(), &workspace.Workspace{Root: t.TempDir()},
					[]*extension.ExtensionDescription{ext}, nil,
				)
				var runtimeErr *extensionRuntimeError
				if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeNotDeclared {
					t.Fatalf("hook runtime error = %v, want %s", err, extensionproto.FailureRuntimeNotDeclared)
				}
				if !strings.Contains(err.Error(), `hook "`+hookName+`"`) {
					t.Errorf("hook error does not identify %s: %v", hookName, err)
				}
			})
		}
	}
}

// TestSynchronizeExtensionRuntimesScansCacheCommands replaces the cacheClean /
// cacheGC arms of the hook scan.
//
// The migration to typed commands must not lose the diagnostic: an extension
// whose cache command says {extensionRuntime} while declaring no runtime is
// still a manifest that cannot be executed, and it must be reported as such
// rather than silently never collecting anything.
func TestSynchronizeExtensionRuntimesScansCacheCommands(t *testing.T) {
	for _, command := range extensionproto.ReservedCacheCommands {
		t.Run(command, func(t *testing.T) {
			ext := &extension.ExtensionDescription{
				Name: "@putnami/no-runtime",
				Jobs: map[string]*extension.JobDefinition{
					command: {
						Name:        command,
						CommandName: command,
						Command:     "{extensionRuntime}",
						Args:        []string{command},
					},
				},
			}
			err := SynchronizeExtensionRuntimes(
				context.Background(), &workspace.Workspace{Root: t.TempDir()},
				[]*extension.ExtensionDescription{ext}, nil,
			)
			var runtimeErr *extensionRuntimeError
			if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeNotDeclared {
				t.Fatalf("cache command runtime error = %v, want %s", err, extensionproto.FailureRuntimeNotDeclared)
			}
			if !strings.Contains(err.Error(), command) {
				t.Errorf("error does not identify the %s command: %v", command, err)
			}
		})
	}
}

func TestSynchronizeFirstPartyRuntimesFromCleanArtifactStore(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("first-party prepare scripts require bash")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{name: "@putnami/go", rel: filepath.Join("go", "extension")},
		{name: "@putnami/python", rel: filepath.Join("python", "extension")},
		{name: "@putnami/typescript", rel: filepath.Join("typescript", "extension")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			extensionRoot := filepath.Join(repoRoot, tc.rel)
			manifest, err := extension.LoadManifest(filepath.Join(extensionRoot, extension.ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			ext := extension.Resolve(manifest, extensionRoot)
			ext.Name = tc.name
			ext.LocalSource = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			artifacts := artifactstore.New(t.TempDir())
			if err := synchronizeExtensionRuntimes(
				ctx, []*extension.ExtensionDescription{ext}, nil, artifacts,
			); err != nil {
				t.Fatalf("clean runtime synchronization: %v", err)
			}
			if err := validateRuntimeExecutable(ext.RuntimeExecutable); err != nil {
				t.Fatalf("synchronized runtime: %v", err)
			}
			if ext.RuntimeDigest == "" {
				t.Fatal("synchronization did not retain the cache-key runtime digest")
			}
		})
	}
}

func TestPreparedAndPackagedRuntimeBehavioralParity(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	sourceRoot := t.TempDir()
	packageRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRuntimeFixture(t, sourceRoot, "@putnami/test", "1.2.3")

	prepared := runtimeTestExtension(sourceRoot)
	ws := workspace.NewWorkspace(workspaceRoot, nil, []*workspace.Project{
		{ID: "/project", Name: "project", Path: "project"},
	})
	artifacts := artifactstore.New(t.TempDir())
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{prepared}, nil, artifacts,
	); err != nil {
		t.Fatal(err)
	}

	packagedPath := filepath.Join(packageRoot, filepath.FromSlash(prepared.Runtime.Executable))
	mustCopyRuntimeFile(t, prepared.RuntimeExecutable, packagedPath)
	packaged := &extension.ExtensionDescription{
		Name:    prepared.Name,
		Version: prepared.Version,
		Path:    packageRoot,
		Runtime: &extension.RuntimeDefinition{Executable: prepared.Runtime.Executable},
	}
	if err := synchronizeExtensionRuntimes(
		context.Background(), []*extension.ExtensionDescription{packaged}, nil, artifacts,
	); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, ext *extension.ExtensionDescription, subcommand string, cancelOnFirstEvent bool) *JobResult {
		t.Helper()
		project := ws.Projects[0]
		job := &ScheduledJob{
			Project:   project,
			Extension: ext,
			JobDef: &extension.JobDefinition{
				Name:      "parity",
				Command:   "{extensionRuntime}",
				Args:      []string{subcommand},
				Cwd:       "{projectRoot}",
				TimeoutMs: unboundedJobTimeoutMs,
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var once sync.Once
		result, err := RunJob(ctx, ws, job, nil, nil, nil, func(RawJobEvent) {
			if cancelOnFirstEvent {
				once.Do(cancel)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	preparedResult := run(t, prepared, "parity", false)
	packagedResult := run(t, packaged, "parity", false)
	if preparedResult.Status != "success" || packagedResult.Status != "success" {
		t.Fatalf("statuses = %q/%q, want success", preparedResult.Status, packagedResult.Status)
	}
	physicalProjectRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	for label, result := range map[string]*JobResult{"prepared": preparedResult, "packaged": packagedResult} {
		if result.Data["cwd"] != physicalProjectRoot || result.Data["structured"] != true {
			t.Errorf("%s result data = %#v, want cwd and structured payload", label, result.Data)
		}
		if len(result.Events) != 3 {
			t.Errorf("%s events = %d, want meta/log/result", label, len(result.Events))
		}
	}
	if !reflect.DeepEqual(preparedResult.Data, packagedResult.Data) {
		t.Errorf("prepared/package result payload drift:\n%#v\n%#v", preparedResult.Data, packagedResult.Data)
	}
	preparedTypes := []string{preparedResult.Events[0].Type, preparedResult.Events[1].Type, preparedResult.Events[2].Type}
	packagedTypes := []string{packagedResult.Events[0].Type, packagedResult.Events[1].Type, packagedResult.Events[2].Type}
	if !reflect.DeepEqual(preparedTypes, packagedTypes) {
		t.Errorf("prepared/package event vocabulary drift: %v != %v", preparedTypes, packagedTypes)
	}

	preparedCanceled := run(t, prepared, "cancel", true)
	packagedCanceled := run(t, packaged, "cancel", true)
	if preparedCanceled.Status != "canceled" || packagedCanceled.Status != "canceled" {
		t.Errorf("cancellation statuses = %q/%q, want canceled", preparedCanceled.Status, packagedCanceled.Status)
	}
	if !preparedCanceled.FirstEventObserved || !packagedCanceled.FirstEventObserved {
		t.Error("cancellation fixtures did not preserve structured output before termination")
	}
}

func TestResolveExtensionRuntimeConcurrentPrepareAndReuse(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	counter := filepath.Join(t.TempDir(), "prepares")
	artifacts := artifactstore.New(t.TempDir())
	writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
		counterPath: counter,
	})
	ext := runtimeTestExtension(root)

	var paths [8]string
	var errs [8]error
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = prepareOrLoadExtensionRuntime(context.Background(), artifacts, ext)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare %d: %v", i, err)
		}
		if paths[i] != paths[0] {
			t.Fatalf("path %d = %q, want %q", i, paths[i], paths[0])
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "prepare\n"); got != 1 {
		t.Fatalf("prepare count = %d, want exactly one", got)
	}
}

// TestResolveExtensionRuntimeConcurrentStress is the higher-fan-out variant of
// the test above: one goroutine's handshake subprocess died on
// SIGSEGV during a full-suite run, and 30 focused re-runs could not reproduce it
// — isolation is exactly what hides it. So this variant stays in the DEFAULT
// suite (no build tag, no -run gate) where it contends with everything else, and
// it widens the window the single-digest test cannot reach: several distinct
// digests admit CONCURRENTLY, so one goroutine's staging tree is created,
// exec'd, published and cleaned up while others exec from already-published
// trees. It is -count-friendly (all state is per-run temp dirs) and deliberately
// cheap — the fixtures are shell scripts, so the cost is process spawns, not
// compilation.
func TestResolveExtensionRuntimeConcurrentStress(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	artifactDir := t.TempDir()
	artifacts := artifactstore.New(artifactDir)
	counter := filepath.Join(t.TempDir(), "prepares")

	// Sized to stay well inside the 10s handshake deadline in
	// validateRuntimeHandshake even on a loaded machine: 24 goroutines is 3x the
	// single-digest test's fan-out, while the cost driver (one prepare subprocess
	// and one freshly-created executable per digest) stays at 3.
	const runtimes = 3
	const waiters = 8
	exts := make([]*extension.ExtensionDescription, runtimes)
	for i := range exts {
		root := t.TempDir()
		writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
			counterPath: counter,
		})
		// A per-fixture declared input makes the digests distinct, so the admits
		// do not collapse onto a single digest lock: several stage/publish/cleanup
		// cycles overlap instead of serializing behind one winner.
		mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "variant"), fmt.Sprintf("variant %d\n", i), 0o644)
		exts[i] = runtimeTestExtension(root)
	}

	paths := make([][]string, runtimes)
	errs := make([][]error, runtimes)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range exts {
		paths[i] = make([]string, waiters)
		errs[i] = make([]error, waiters)
		for j := range waiters {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				<-start // release everyone at once, maximizing overlap
				paths[i][j], errs[i][j] = prepareOrLoadExtensionRuntime(context.Background(), artifacts, exts[i])
			}(i, j)
		}
	}
	close(start)
	wg.Wait()

	seen := make(map[string]int, runtimes)
	for i := range exts {
		for j := range waiters {
			if errs[i][j] != nil {
				t.Fatalf("runtime %d waiter %d: %v", i, j, errs[i][j])
			}
			if paths[i][j] != paths[i][0] {
				t.Fatalf("runtime %d waiter %d path = %q, want %q", i, j, paths[i][j], paths[i][0])
			}
		}
		// Publication must survive every sibling's staging cleanup: the executable
		// each waiter was handed is still an executable regular file afterwards.
		if err := validateRuntimeExecutable(paths[i][0]); err != nil {
			t.Fatalf("runtime %d executable %q: %v", i, paths[i][0], err)
		}
		seen[paths[i][0]]++
	}
	if len(seen) != runtimes {
		t.Fatalf("distinct prepared runtimes = %d, want %d (digests collided)", len(seen), runtimes)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "prepare\n"); got != runtimes {
		t.Fatalf("prepare count = %d, want exactly %d (one per digest)", got, runtimes)
	}
	// Every staging tree is reclaimed: none is left behind, and none took the
	// published bytes with it (asserted above).
	entries, err := os.ReadDir(filepath.Join(artifactDir, "tmp"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "staging-") {
			t.Errorf("leftover staging dir after concurrent admits: %s", entry.Name())
		}
	}
}

func TestRuntimeHandshakeRetriesTransientTextFileBusy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ETXTBSY exec semantics are Linux-specific")
	}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	answer := fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		ext.Name, ext.Version, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	executable := filepath.Join(t.TempDir(), "runtime")
	mustWriteRuntimeFile(t, executable, "#!/bin/sh\nprintf '%s\\n' '"+answer+"'\n", 0o755)

	writer, err := os.OpenFile(executable, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		closed <- writer.Close()
	}()

	err = runRuntimeHandshake(context.Background(), executable, ext, nil, (&handshakeClockProbe{}).clock)
	if closeErr := <-closed; closeErr != nil {
		t.Fatalf("close held executable: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("handshake after transient ETXTBSY: %v", err)
	}
}

func TestRuntimePrepareRetriesTransientTextFileBusy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ETXTBSY exec semantics are Linux-specific")
	}
	root := t.TempDir()
	script := filepath.Join(root, "prepare")
	mustWriteRuntimeFile(t, script, "#!/bin/sh\nprintf 'ready\\n'\n", 0o755)

	writer, err := os.OpenFile(script, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		closed <- writer.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, state, err := runRuntimePrepareCommand(ctx, script, nil, root, os.Environ())
	if closeErr := <-closed; closeErr != nil {
		t.Fatalf("close held executable: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("prepare after transient ETXTBSY: %v", err)
	}
	if state == nil || !state.Success() {
		t.Fatalf("prepare state = %+v, want success", state)
	}
	if got := string(out); got != "ready\n" {
		t.Fatalf("prepare output = %q, want ready", got)
	}
}

// TestRuntimeHandshakeFailureIsSelfDiagnosing pins the diagnostic payload
// needed on the next occurrence: a handshake subprocess that dies on a
// SIGNAL reports the executable's on-disk identity, so the report itself says
// whether the file was intact and which tree it came from — without that, a
// "signal: segmentation fault" is indistinguishable from a file whose bytes
// moved under an in-flight exec.
func TestRuntimeHandshakeFailureIsSelfDiagnosing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	executable := filepath.Join(root, "runtime")
	mustWriteRuntimeFile(t, executable,
		"#!/bin/sh\nprintf '%s\\n' 'dying' >&2\nkill -s SEGV $$\n", 0o755)
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}

	err := validateRuntimeHandshake(context.Background(), executable, ext, nil)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeHandshakeFailed {
		t.Fatalf("signal-death error = %v, want %s", err, extensionproto.FailureRuntimeHandshakeFailed)
	}
	info, statErr := os.Lstat(executable)
	if statErr != nil {
		t.Fatal(statErr)
	}
	for _, want := range []string{
		"signal: segmentation fault", // the exact shape previously reported
		executable,
		`stderr="dying"`,
		fmt.Sprintf("size=%d", info.Size()),
		"mtime=" + info.ModTime().UTC().Format(time.RFC3339Nano),
		"stat=",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("signal-death message %q missing %q", err.Error(), want)
		}
	}

	// The tree really disappearing must be reported, not swallowed by the stat.
	missing := filepath.Join(root, "gone")
	err = validateRuntimeHandshake(context.Background(), missing, ext, nil)
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeHandshakeFailed {
		t.Fatalf("absent-executable error = %v, want %s", err, extensionproto.FailureRuntimeHandshakeFailed)
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "stat failed") {
		t.Errorf("absent-executable message %q must name the path and the failed stat", err.Error())
	}
}

func TestRuntimeHandshakeFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	executable := filepath.Join(root, "runtime")
	mustWriteRuntimeFile(t, executable, "#!/bin/sh\nprintf '%s\\n' '{\"extension\":\"wrong\"}'\n", 0o755)
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	err := validateRuntimeHandshake(context.Background(), executable, ext, nil)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeIdentityMismatch {
		t.Fatalf("handshake error = %v, want %s", err, extensionproto.FailureRuntimeIdentityMismatch)
	}

	mustWriteRuntimeFile(t, executable, "#!/bin/sh\nprintf '%s\\n' 'not-json'\n", 0o755)
	err = validateRuntimeHandshake(context.Background(), executable, ext, nil)
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeHandshakeFailed {
		t.Fatalf("malformed error = %v, want %s", err, extensionproto.FailureRuntimeHandshakeFailed)
	}
	// Truncated output is the other "bytes moved under the exec" signature, so it
	// carries the same on-disk identity as a signal death.
	if !strings.Contains(err.Error(), executable) || !strings.Contains(err.Error(), "size=") {
		t.Errorf("malformed message %q must name the executable and its on-disk size", err.Error())
	}

	wrongTarget := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' '{\"extension\":\"@putnami/test\",\"version\":\"1.2.3\",\"platform\":\"wrong/target\",\"cliContract\":4,\"runtimeProtocol\":%d,\"runtimeABI\":%d}'\n",
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	mustWriteRuntimeFile(t, executable, wrongTarget, 0o755)
	err = validateRuntimeHandshake(context.Background(), executable, ext, nil)
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeIdentityMismatch {
		t.Fatalf("target error = %v, want %s", err, extensionproto.FailureRuntimeIdentityMismatch)
	}
}

// handshakeClockProbe is a runtimeHandshakeClock a test controls. With fire
// set, the deadline has elapsed by the time it is armed; without it, the
// channel is nil and never delivers, so the runtime's own outcome decides.
// armedPids records the process each arming received, which proves the clock
// started only once the runtime process existed.
type handshakeClockProbe struct {
	fire      bool
	onArm     func()
	armedPids []int
}

func (p *handshakeClockProbe) clock(process *os.Process) (<-chan time.Time, func()) {
	pid := 0
	if process != nil {
		pid = process.Pid
	}
	p.armedPids = append(p.armedPids, pid)
	if p.onArm != nil {
		p.onArm()
	}
	if !p.fire {
		return nil, func() {}
	}
	expired := make(chan time.Time)
	close(expired)
	return expired, func() {}
}

// TestRuntimeHandshakeTimeoutIsItsOwnVerdict pins the handshake timeout without a wall clock.
// A handshake still running when its deadline fires is reported as
// runtime.handshake_timeout. A runtime that exits non-zero, dies on a signal,
// or prints malformed runtime-info keeps runtime.handshake_failed, whatever the
// machine's load. The clock is armed once, with a started process, so the time
// the operating system spends admitting a freshly written binary never counts.
//
// The blocked runtime cannot answer before the probe's deadline, which has
// elapsed by the time it is armed, and the other runtimes face a deadline that
// never fires. So no case depends on how fast the host runs.
//
// Every case's executable is written before the parallel subtests start, so
// no subtest's fork ever inherits another subtest's still-open write
// descriptor; a fork that has not exec'd yet when a sibling exec's its own
// freshly written file would otherwise earn ETXTBSY.
func TestRuntimeHandshakeTimeoutIsItsOwnVerdict(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	if runtimeHandshakeDeadline != 10*time.Second {
		t.Fatalf("runtimeHandshakeDeadline = %s; the value is settled: moving it moves the load at which a healthy runtime is refused",
			runtimeHandshakeDeadline)
	}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Version: "1.2.3"}
	answer := fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		ext.Name, ext.Version, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)

	cases := []struct {
		name       string
		script     string
		fire       bool
		cancel     bool
		wantCode   string
		executable string
	}{
		{
			// exec hands the process holding the pipes to sleep, so the kill
			// reaches it and Wait returns.
			name:     "a blocked runtime at its deadline times out",
			script:   "#!/bin/sh\nexec sleep 3600\n",
			fire:     true,
			wantCode: extensionproto.FailureRuntimeHandshakeTimeout,
		},
		{
			name:     "a runtime that exits non-zero fails",
			script:   "#!/bin/sh\nprintf '%s\\n' 'broken' >&2\nexit 3\n",
			wantCode: extensionproto.FailureRuntimeHandshakeFailed,
		},
		{
			// The incident's own signature, "signal: killed" with an empty stderr,
			// produced by something other than the deadline stays a failure.
			name:     "a runtime killed by another cause fails",
			script:   "#!/bin/sh\nkill -s KILL $$\n",
			wantCode: extensionproto.FailureRuntimeHandshakeFailed,
		},
		{
			name:     "a runtime that prints malformed runtime-info fails",
			script:   "#!/bin/sh\nprintf '%s\\n' 'not-json'\n",
			wantCode: extensionproto.FailureRuntimeHandshakeFailed,
		},
		{
			name:     "a canceled caller interrupts the handshake instead of timing it out",
			script:   "#!/bin/sh\nexec sleep 3600\n",
			fire:     true,
			cancel:   true,
			wantCode: extensionproto.FailureRuntimeHandshakeFailed,
		},
		{
			name:   "a runtime that answers is accepted",
			script: "#!/bin/sh\nprintf '%s\\n' '" + answer + "'\n",
		},
	}
	for i := range cases {
		cases[i].executable = filepath.Join(t.TempDir(), "runtime")
		mustWriteRuntimeFile(t, cases[i].executable, cases[i].script, 0o755)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executable := tc.executable
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &handshakeClockProbe{fire: tc.fire}
			if tc.cancel {
				probe.onArm = cancel
			}

			err := runRuntimeHandshake(ctx, executable, ext, nil, probe.clock)

			if len(probe.armedPids) == 0 {
				t.Fatalf("clock never armed; handshake error = %v", err)
			}
			if len(probe.armedPids) != 1 || probe.armedPids[0] <= 0 {
				t.Fatalf("clock armed with pids %v, want exactly one arming with the started runtime process", probe.armedPids)
			}
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("handshake error = %v, want the answering runtime accepted", err)
				}
				return
			}
			var runtimeErr *extensionRuntimeError
			if !errors.As(err, &runtimeErr) || runtimeErr.code != tc.wantCode {
				t.Fatalf("handshake error = %v, want %s", err, tc.wantCode)
			}
			message := err.Error()
			if tc.wantCode != extensionproto.FailureRuntimeHandshakeTimeout {
				if strings.Contains(message, "timed out") {
					t.Errorf("%s message %q reads as a timeout", tc.wantCode, message)
				}
				return
			}
			for _, want := range []string{
				"timed out",
				"counted from process start",
				"not a malformed build",
				"less loaded",
				"CPU used",
				executable,
			} {
				if !strings.Contains(message, want) {
					t.Errorf("timeout message %q missing %q", message, want)
				}
			}
		})
	}
}

func TestExtensionRuntimeMutationFailsAndRecovers(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	mutated := filepath.Join(root, "cmd", "source")
	mutationTrigger := filepath.Join(t.TempDir(), "mutate-once")
	mustWriteRuntimeFile(t, mutationTrigger, "mutate\n", 0o644)
	artifacts := artifactstore.New(t.TempDir())
	writeRuntimeFixtureWithControls(t, root, "@putnami/test", "1.2.3", runtimeFixtureControls{
		mutationTrigger: mutationTrigger,
		mutationTarget:  mutated,
	})
	ext := runtimeTestExtension(root)

	_, err := prepareOrLoadExtensionRuntime(context.Background(), artifacts, ext)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed ||
		!strings.Contains(err.Error(), "changed during preparation") {
		t.Fatalf("mutation error = %v, want typed mutation failure", err)
	}

	if _, err := prepareOrLoadExtensionRuntime(context.Background(), artifacts, ext); err != nil {
		t.Fatalf("recovery after unpublished mutation failure: %v", err)
	}
}

func TestExtensionRuntimeInterruptionAndAbsentToolchain(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell runtime fixture")
	}
	root := t.TempDir()
	artifacts := artifactstore.New(t.TempDir())
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")
	ext := runtimeTestExtension(root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := prepareOrLoadExtensionRuntime(ctx, artifacts, ext)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed {
		t.Fatalf("interruption error = %v, want %s", err, extensionproto.FailureRuntimePrepareFailed)
	}
	if _, err := prepareOrLoadExtensionRuntime(context.Background(), artifacts, ext); err != nil {
		t.Fatalf("recovery after interruption: %v", err)
	}

	missing := runtimeTestExtension(root)
	missing.Runtime.Prepare.Command = "putnami-toolchain-that-does-not-exist"
	_, err = prepareOrLoadExtensionRuntime(context.Background(), artifacts, missing)
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimePrepareFailed {
		t.Fatalf("absent toolchain error = %v, want %s", err, extensionproto.FailureRuntimePrepareFailed)
	}
}

// TestExtensionRuntimeDigestFollowsThePrepareToolchainIdentity pins the input
// that makes toolchain pinning observable: the prepared binary is stored under
// a digest that includes WHICH compiler produced it. Without it, upgrading the
// locked toolchain would keep serving the artifact the previous one built.
func TestExtensionRuntimeDigestFollowsThePrepareToolchainIdentity(t *testing.T) {
	root := t.TempDir()
	writeRuntimeFixture(t, root, "@putnami/test", "1.2.3")
	ext := runtimeTestExtension(root)
	ext.Runtime.Toolchains = map[string]extensionproto.RuntimeToolchain{
		"compiler": {Lock: "compiler"},
	}
	ext.Runtime.Prepare.Toolchains = []string{"compiler"}
	ext.RuntimeToolchains = map[string]model.RuntimeToolchainResolution{
		"compiler": {Available: true, Identity: "identity-a"},
	}

	first, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	ext.RuntimeToolchains["compiler"] = model.RuntimeToolchainResolution{Available: true, Identity: "identity-b"}
	second, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("runtime digest did not move with the resolved compiler identity: %q", first)
	}
	spectest.Proves(t, "cli/runtime-toolchain-pinning", "identity-keys-results",
		"a-prepared-runtime-digest-follows-the-resolved-identity")
}

func runtimeTestExtension(root string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:        "@putnami/test",
		Version:     "1.2.3",
		Path:        root,
		LocalSource: true,
		Runtime: &extension.RuntimeDefinition{
			Executable: "bin/runtime",
			Prepare: &extension.RuntimePrepare{
				Command: "{extensionRoot}/bin/prepare",
				Args:    []string{"--output", "{runtimeOutput}"},
				Inputs:  []string{"bin/**", "cmd/**"},
			},
		},
	}
}

type runtimeFixtureControls struct {
	// prepareAssertions is shell run at the very start of the prepare, inside
	// the staged source view, so a test can assert on the tree the preparation
	// actually receives and fail the prepare when it is wrong.
	prepareAssertions string
	counterPath       string
	barrierPath       string
	barrierCount      int
	prepareDelay      time.Duration
	cpuWorkIterations int
	releasePath       string
	mutationTrigger   string
	mutationTarget    string
}

func writeRuntimeFixture(t *testing.T, root, name, version string) {
	t.Helper()
	writeRuntimeFixtureWithControls(t, root, name, version, runtimeFixtureControls{})
}

// writeRuntimeFixtureWithControls embeds test-owned coordination paths in the
// generated prepare script. Unlike process environment, these immutable
// per-fixture dependencies cannot bleed between parallel top-level tests. The
// environment-backed defaults remain for TestRuntimeSyncHelperProcess, where
// separate child processes deliberately use environment as their transport.
func writeRuntimeFixtureWithControls(
	t *testing.T,
	root, name, version string,
	controls runtimeFixtureControls,
) {
	t.Helper()
	info := fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":4,"runtimeProtocol":%d,"runtimeABI":%d}`,
		name, version, runtime.GOOS+"/"+runtime.GOARCH,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	runtimeScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n" +
		"  printf '%s\\n' '" + info + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"parity\" ]; then\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"extension\":\"@putnami/test\",\"job\":\"parity\"}}'\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"log\",\"level\":\"info\",\"message\":\"structured\"}'\n" +
		"  printf '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\",\"data\":{\"cwd\":\"%s\",\"structured\":true}}}\\n' \"$PWD\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"cancel\" ]; then\n" +
		"  printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"extension\":\"@putnami/test\",\"job\":\"cancel\"}}'\n" +
		"  trap 'exit 0' TERM INT\n" +
		"  while :; do sleep 1; done\n" +
		"fi\n" +
		"exit 0\n"
	// The barrier is TestIndependentRuntimeSyncsRunConcurrently's rendezvous: the
	// prepare announces itself and refuses to finish until the whole cohort has
	// announced too. Under a serial stage the first prepare waits for peers that
	// cannot start, times out and EXITS NON-ZERO, so a regression is a failure
	// rather than a slow pass. The environment form is retained for the child-
	// process fixture; ordinary tests embed their test-owned path instead.
	counterStep := `if [ -n "${RUNTIME_PREPARE_COUNTER:-}" ]; then
  printf 'prepare\n' >> "$RUNTIME_PREPARE_COUNTER"
fi
`
	if controls.counterPath != "" {
		counterStep = "printf 'prepare\\n' >> " + shellQuote(controls.counterPath) + "\n"
	}
	barrierStep := `if [ -n "${RUNTIME_BARRIER:-}" ]; then
  printf 'here\n' >> "$RUNTIME_BARRIER"
  waited=0
  while [ "$(wc -l < "$RUNTIME_BARRIER" | tr -d ' ')" -lt "$RUNTIME_BARRIER_N" ]; do
    waited=$((waited + 1))
    if [ "$waited" -gt 300 ]; then
      printf 'prepare: cohort of %s never assembled; the stage ran serially\n' "$RUNTIME_BARRIER_N" >&2
      exit 3
    fi
    sleep 0.05
  done
fi
`
	if controls.barrierPath != "" {
		path := shellQuote(controls.barrierPath)
		barrierStep = fmt.Sprintf(`printf 'here\n' >> %s
waited=0
while [ "$(wc -l < %s | tr -d ' ')" -lt %d ]; do
  waited=$((waited + 1))
  if [ "$waited" -gt 300 ]; then
    printf 'prepare: cohort of %%s never assembled; the stage ran serially\n' %s >&2
    exit 3
  fi
  sleep 0.05
done
`, path, path, controls.barrierCount, shellQuote(fmt.Sprint(controls.barrierCount)))
	}
	delayStep := `[ -z "${RUNTIME_PREPARE_SLEEP:-}" ] || sleep "$RUNTIME_PREPARE_SLEEP"
`
	if controls.prepareDelay > 0 {
		delayStep = fmt.Sprintf("sleep %.6f\n", controls.prepareDelay.Seconds())
	}
	cpuWorkStep := ""
	if controls.cpuWorkIterations > 0 {
		cpuWorkStep = fmt.Sprintf(`i=0
while [ "$i" -lt %d ]; do
  i=$((i + 1))
done
`, controls.cpuWorkIterations)
	}
	releaseStep := ""
	if controls.releasePath != "" {
		release := shellQuote(controls.releasePath)
		releaseStep = fmt.Sprintf(`waited=0
while [ ! -f %s ]; do
  waited=$((waited + 1))
  if [ "$waited" -gt 6000 ]; then
    printf 'prepare: release was never published\n' >&2
    exit 4
  fi
  sleep 0.01
done
`, release)
	}
	mutationStep := `[ -z "${RUNTIME_MUTATE_FILE:-}" ] || printf 'mutation\n' >> "$RUNTIME_MUTATE_FILE"
`
	if controls.mutationTrigger != "" {
		mutationStep = fmt.Sprintf(`if [ -f %s ]; then
  rm -f %s
  printf 'mutation\n' >> %s
fi
`, shellQuote(controls.mutationTrigger), shellQuote(controls.mutationTrigger),
			shellQuote(controls.mutationTarget))
	}
	// The prepare runs the runtime it wrote once, as fixtureproc.Write does
	// for each program it places, so the handshake that follows within its
	// deadline does not also pay for the host's first-launch check of the new
	// file.
	prepare := `#!/bin/sh
set -eu
` + controls.prepareAssertions + counterStep + barrierStep + delayStep + releaseStep + cpuWorkStep + `output="$2"
mkdir -p "$output/bin"
cp bin/runtime-template "$output/bin/runtime"
chmod +x "$output/bin/runtime"
"$output/bin/runtime" __putnami runtime-info >/dev/null
` + mutationStep
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "prepare"), prepare, 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "bin", "runtime-template"), runtimeScript, 0o755)
	mustWriteRuntimeFile(t, filepath.Join(root, "cmd", "source"), "source", 0o644)
}

func mustCopyRuntimeFile(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteRuntimeFile(t, target, string(data), 0o755)
}

func mustWriteRuntimeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// An installed extension's Windows archive carries its runtime at the declared
// path with ".exe" (compiled/putnami-go.exe), while every manifest declares
// compiled/putnami-go. The CLI resolves the declared path under the name the
// packagers write for the machine's OS, and leaves it unchanged elsewhere.
func TestRuntimeExecutablePathResolvesTheWindowsExecutable(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-executables-by-name", "an-installed-runtime-resolves-under-its-exe-name")
	root := filepath.Join("store", "extension")
	for _, tc := range []struct {
		goos, executable, want string
	}{
		{goos: "windows", executable: "compiled/putnami-go", want: filepath.Join(root, "compiled", "putnami-go.exe")},
		{goos: "windows", executable: "compiled/putnami-go.exe", want: filepath.Join(root, "compiled", "putnami-go.exe")},
		{goos: "linux", executable: "compiled/putnami-go", want: filepath.Join(root, "compiled", "putnami-go")},
		{goos: "darwin", executable: "compiled/putnami-go", want: filepath.Join(root, "compiled", "putnami-go")},
	} {
		if got := runtimeExecutablePath(root, tc.executable, tc.goos); got != tc.want {
			t.Errorf("runtimeExecutablePath(%q, %q, %q) = %q, want %q", root, tc.executable, tc.goos, got, tc.want)
		}
	}
}

// A runtime missing on Windows because the extension ships the declared file
// without ".exe" stays runtime.executable_missing, and the error says why. The
// declared file is never run in place of the Windows name.
func TestUnsuffixedRuntimeHintNamesTheWindowsExecutable(t *testing.T) {
	spectest.Proves(t, "cli/windows-consumers", "windows-executables-by-name", "an-unsuffixed-runtime-is-named-not-run")
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "compiled", "putnami-script"), "#!/bin/sh\n", 0o755)

	hint := unsuffixedRuntimeHint(root, "compiled/putnami-script", "windows")
	for _, want := range []string{"compiled/putnami-script exists without the .exe suffix", "windows/amd64 executable at compiled/putnami-script.exe"} {
		if !strings.Contains(hint, want) {
			t.Errorf("windows hint = %q, want it to contain %q", hint, want)
		}
	}
	for _, tc := range []struct{ name, goos, executable string }{
		{name: "linux keeps the declared name", goos: "linux", executable: "compiled/putnami-script"},
		{name: "darwin keeps the declared name", goos: "darwin", executable: "compiled/putnami-script"},
		{name: "no unsuffixed file", goos: "windows", executable: "compiled/putnami-absent"},
		{name: "declared with the suffix", goos: "windows", executable: "compiled/putnami-script.exe"},
	} {
		if hint := unsuffixedRuntimeHint(root, tc.executable, tc.goos); hint != "" {
			t.Errorf("%s: hint = %q, want none", tc.name, hint)
		}
	}
}
