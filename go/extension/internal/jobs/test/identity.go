package test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
)

// IdentityManifestFile is the command-output file test-identity-exec writes,
// the artifact test-exec folds in place of the project's dependency closure.
const IdentityManifestFile = "test-identity.json"

// identityManifestVersion is the version of the manifest's shape and of the
// closure digest. A change to either changes it, so a manifest of one shape
// never equals a manifest of another.
const identityManifestVersion = 1

// identityDigestDomain opens every closure digest.
const identityDigestDomain = "go-test-identity-v1\n"

// unresolvedIdentity is the closure digest of an entry whose link closure
// holds a package without a content ID: a package that does not compile, or
// one go list could not load.
const unresolvedIdentity = "unresolved"

// IdentityManifest is what test-identity-exec writes: the identity of every
// binary `go test ./...` builds for the project under the run's compile
// flags, as the Go toolchain names its compile actions.
//
// Tests maps each test main package (`<pkg>.test`) to the digest of its link
// closure: the sorted (import path, content ID) pairs of the main package and
// of every package it links, the test variants (`<dep> [<pkg>.test]`)
// included. Packages maps each package `./...` matches to the digest of its
// own link closure, so a test that builds one of the project's packages at
// run time is keyed on what it builds. A content ID is the part of the
// package's build ID after the last "/": the hash of the compiled output, so
// an edit that compiles to the same bytes, such as a comment within a line,
// leaves it unchanged.
//
// The listing runs with -trimpath, which keeps the checkout path out of every
// content ID: two machines whose workspace roots differ list the same
// identity for the same sources. The run itself builds untrimmed binaries,
// whose only difference is the embedded paths (ADR 0017).
//
// Unresolved is set when an entry could not be resolved, or the listing
// failed: a random value, so the manifest never equals another and the test
// run executes and reports the failure itself. Such a manifest is reported as
// a skip (RunIdentity), which the engine never stores for a task that is not
// deterministic, so no later run replays it.
type IdentityManifest struct {
	Version    int               `json:"version"`
	Go         string            `json:"go"`
	GOOS       string            `json:"goos"`
	GOARCH     string            `json:"goarch"`
	Flags      []string          `json:"flags"`
	Tests      map[string]string `json:"tests"`
	Packages   map[string]string `json:"packages"`
	Unresolved string            `json:"unresolved,omitempty"`

	// cause is why a listing that failed left the manifest unresolved.
	cause error
}

// listedPackage is the part of a `go list -json` entry the identity reads.
type listedPackage struct {
	ImportPath string
	Name       string
	BuildID    string
	GoFiles    []string
	CgoFiles   []string
	Deps       []string
	DepOnly    bool
	ForTest    string
	Standard   bool
	Error      *listedError
	DepsErrors []*listedError
}

type listedError struct {
	Err string
}

// identityListFields are the fields RunIdentity asks `go list -json` for.
const identityListFields = "ImportPath,Name,BuildID,GoFiles,CgoFiles,Deps,DepOnly,ForTest,Standard,Error,DepsErrors"

// identityNonce returns the random value of an unresolved manifest. Tests
// replace it.
var identityNonce = func() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// RunIdentity writes the test identity manifest of the project into the
// command output (IdentityManifestFile). It lists the binaries with
// `go list -e -trimpath -export -deps -test` over `./...`, in the
// environment and with the compile flags test-exec runs `go test` with
// (testCompileEnv, projectBinaryFlags), and with -buildvcs=false, since
// `go test` stamps no test binary.
//
// A project without go.mod skips, as test-exec does. A listing that fails, or
// an entry that does not resolve, writes an unresolved manifest and reports a
// skip: test-identity-exec is not deterministic, so the engine stores no entry
// for the skip and replays its manifest to no other run, and it still
// publishes the manifest's digest to test-exec. The test run then executes
// under a key no other run shares and reports the failure itself.
func RunIdentity(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}
	dir := ctx.Project.FullPath
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		emit.Log("info", "No go.mod found, skipping the test identity")
		return "SKIP", nil, nil
	}
	options := readBatchOptions(ctx)
	env := testCompileEnv(dir, options.race, goBinary)
	if goScratch, err := scratch.New("putnami-go-tmp-"); err != nil {
		emit.Diagnostic("warning", "create Go work directory scratch: "+err.Error(), "", 0)
	} else {
		defer func() { _ = goScratch.Remove() }()
		if env, err = withGoTempDir(env, goScratch.Path()); err != nil {
			emit.Diagnostic("warning", err.Error(), "", 0)
		}
	}

	emit.PhaseStart("list")
	manifest, err := identityOf(goBinary, dir, env, options)
	if err != nil {
		emit.PhaseEnd("list", "failed")
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if manifest.cause != nil {
		emit.Log("info", "The test binaries could not be listed; the test run executes and reports why: "+manifest.cause.Error())
	} else if manifest.Unresolved != "" {
		emit.Log("info", "A package of the test binaries does not compile; the test run executes and reports why")
	}
	emit.PhaseEnd("list", "success")

	data, err := encodeIdentityManifest(manifest)
	if err != nil {
		return "FAILED", nil, err
	}
	if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
		return "FAILED", nil, err
	}
	if err := os.WriteFile(filepath.Join(ctx.OutputPath, IdentityManifestFile), data, 0o644); err != nil {
		return "FAILED", nil, err
	}
	emit.Summary(fmt.Sprintf("%d test binaries, %d packages", len(manifest.Tests), len(manifest.Packages)))
	status := "OK"
	if manifest.Unresolved != "" {
		status = "SKIP"
	}
	return status, map[string]any{
		"tests":      len(manifest.Tests),
		"packages":   len(manifest.Packages),
		"unresolved": manifest.Unresolved != "",
		"manifest":   IdentityManifestFile,
	}, nil
}

// identityOf lists the project's test binaries in dir and returns their
// manifest. It fails only when the toolchain does not answer `go env`; a
// listing that fails returns an unresolved manifest.
func identityOf(goBinary, dir string, env []string, options batchOptions) (IdentityManifest, error) {
	manifest := IdentityManifest{
		Version:  identityManifestVersion,
		Flags:    []string{},
		Tests:    map[string]string{},
		Packages: map[string]string{},
	}
	facts, err := identityToolchain(goBinary, dir, env)
	if err != nil {
		return IdentityManifest{}, err
	}
	manifest.Go, manifest.GOOS, manifest.GOARCH = facts.GOVERSION, facts.GOOS, facts.GOARCH

	flags, err := projectBinaryFlags(options, goBinary, dir, env)
	if err == nil {
		manifest.Flags = append(manifest.Flags, flags.args("")...)
		var listed []listedPackage
		listed, err = listTestBinaries(goBinary, dir, env, flags)
		if err == nil {
			manifest.Tests, manifest.Packages = binaryIdentities(listed)
		}
	}
	if err != nil || slices.Contains(mapValues(manifest.Tests), unresolvedIdentity) ||
		slices.Contains(mapValues(manifest.Packages), unresolvedIdentity) {
		nonce, nonceErr := identityNonce()
		if nonceErr != nil {
			return IdentityManifest{}, fmt.Errorf("draw the identity of unresolved test binaries: %w", nonceErr)
		}
		manifest.Unresolved = nonce
		manifest.cause = err
	}
	return manifest, nil
}

// identityToolchainFacts are the toolchain facts the manifest names.
type identityToolchainFacts struct {
	GOVERSION string
	GOOS      string
	GOARCH    string
}

// identityToolchain reads the version and the target platform of the
// toolchain `go test` builds with, in its environment.
func identityToolchain(goBinary, dir string, env []string) (identityToolchainFacts, error) {
	output, err := runGoCommand(goBinary, []string{"env", "-json", "GOVERSION", "GOOS", "GOARCH"}, dir, env)
	if err != nil {
		return identityToolchainFacts{}, fmt.Errorf("go env in %s: %w\n%s", dir, err, strings.TrimSpace(string(output)))
	}
	var facts identityToolchainFacts
	if err := json.Unmarshal(output, &facts); err != nil {
		return identityToolchainFacts{}, fmt.Errorf("read go env in %s: %w", dir, err)
	}
	return facts, nil
}

// identityListArgs are the `go list` arguments of the identity of a run with
// flags: the packages `go test ./...` builds, their dependencies and test
// variants, compiled trimmed so their content IDs name no checkout path.
func identityListArgs(flags binaryFlags) []string {
	args := []string{"list", "-e", "-trimpath", "-buildvcs=false", "-export", "-deps", "-test",
		"-json=" + identityListFields}
	args = append(args, flags.args("")...)
	return append(args, "./...")
}

// listTestBinaries runs the identity listing in dir and decodes its entries.
func listTestBinaries(goBinary, dir string, env []string, flags binaryFlags) ([]listedPackage, error) {
	output, err := runGoCommand(goBinary, identityListArgs(flags), dir, env)
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %w\n%s", dir, err, strings.TrimSpace(string(output)))
	}
	var listed []listedPackage
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var entry listedPackage
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return listed, nil
		} else if err != nil {
			return nil, fmt.Errorf("read go list in %s: %w", dir, err)
		}
		listed = append(listed, entry)
	}
}

// binaryIdentities returns the closure digest of each test main package and
// of each package the listing's pattern matched. A test main is a matched
// `main` package named after a matched package with ".test". A matched
// package without a Go file to compile, one that holds tests alone, builds
// nothing: no binary links it and no test can build it, so it has no entry,
// and its tests are named by their test main.
func binaryIdentities(listed []listedPackage) (tests, packages map[string]string) {
	byPath := make(map[string]listedPackage, len(listed))
	matched := make(map[string]bool)
	for _, entry := range listed {
		byPath[entry.ImportPath] = entry
		if !entry.DepOnly && entry.ForTest == "" {
			matched[entry.ImportPath] = true
		}
	}
	tests, packages = map[string]string{}, map[string]string{}
	for path := range matched {
		entry := byPath[path]
		if root, ok := strings.CutSuffix(path, ".test"); ok && entry.Name == "main" && matched[root] {
			tests[path] = closureDigest(entry, byPath)
			continue
		}
		if len(entry.GoFiles) == 0 && len(entry.CgoFiles) == 0 {
			continue
		}
		packages[path] = closureDigest(entry, byPath)
	}
	return tests, packages
}

// closureDigest is the digest of the sorted (import path, content ID) pairs
// of entry and every package it links, or unresolvedIdentity when one of them
// has no content ID.
func closureDigest(entry listedPackage, byPath map[string]listedPackage) string {
	members := append([]string{entry.ImportPath}, entry.Deps...)
	slices.Sort(members)
	members = slices.Compact(members)
	h := sha256.New()
	h.Write([]byte(identityDigestDomain))
	for _, path := range members {
		id, ok := contentID(byPath, path)
		if !ok {
			return unresolvedIdentity
		}
		fmt.Fprintf(h, "%s\x00%s\n", path, id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// contentID returns the content ID of the listed package at path: the part of
// its build ID after the last "/". A package that did not load or compile has
// none; a standard pseudo-package such as unsafe compiles to nothing and has
// an empty one.
func contentID(byPath map[string]listedPackage, path string) (string, bool) {
	entry, ok := byPath[path]
	if !ok || entry.Error != nil || len(entry.DepsErrors) > 0 {
		return "", false
	}
	if entry.BuildID == "" {
		return "", entry.Standard
	}
	return entry.BuildID[strings.LastIndex(entry.BuildID, "/")+1:], true
}

// encodeIdentityManifest renders the manifest as indented JSON with its maps
// sorted, followed by a newline: the same bytes for the same identity.
func encodeIdentityManifest(manifest IdentityManifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func mapValues(m map[string]string) []string {
	values := make([]string, 0, len(m))
	for _, value := range m {
		values = append(values, value)
	}
	return values
}
