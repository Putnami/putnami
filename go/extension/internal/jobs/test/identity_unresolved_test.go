package test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runIdentityAs runs the identity job, requires status, and returns its
// manifest's bytes.
func runIdentityAs(t *testing.T, ctx *pctx.Context, status string) []byte {
	t.Helper()
	got, data, err := RunIdentity(ctx, jsonl.New(), nil)
	if err != nil || got != status {
		t.Fatalf("RunIdentity status=%q err=%v, want %s", got, err, status)
	}
	if data["manifest"] != IdentityManifestFile || data["unresolved"] != (status == "SKIP") {
		t.Fatalf("RunIdentity data = %v, want the manifest port %q and unresolved %v", data, IdentityManifestFile, status == "SKIP")
	}
	manifest, err := os.ReadFile(filepath.Join(ctx.OutputPath, IdentityManifestFile))
	if err != nil {
		t.Fatalf("RunIdentity wrote no manifest: %v", err)
	}
	return manifest
}

// An unresolved identity is reported as a skip, a result the engine never
// stores for test-identity-exec, which is not deterministic: no later run
// replays its random value, so its test run's key is shared by no other run.
// It still writes the manifest whose digest the test step folds. A resolved
// identity succeeds.
func TestAnUnresolvedIdentityIsReportedAsASkip(t *testing.T) {
	broken := identityFixture()
	for i := range broken {
		if broken[i].ImportPath == "example.com/dep" {
			broken[i].BuildID = ""
			broken[i].Error = &listedError{Err: "dep.go:3:9: undefined: x"}
		}
	}
	unresolved := &identityGo{listing: encodeListing(t, broken)}
	unresolved.install(t)
	if manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP")); manifest.Unresolved == "" {
		t.Fatalf("manifest = %+v, want it unresolved", manifest)
	}

	failing := &identityGo{listing: "go: go.mod:3: unknown directive: bogus\n", listingErr: errors.New("exit status 1")}
	failing.install(t)
	if manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP")); manifest.Unresolved == "" {
		t.Fatalf("manifest of a failed listing = %+v, want it unresolved", manifest)
	}

	resolved := &identityGo{listing: encodeListing(t, identityFixture())}
	resolved.install(t)
	if manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "OK")); manifest.Unresolved != "" {
		t.Fatalf("manifest = %+v, want it resolved", manifest)
	}
}

// A matched package that holds tests alone has no Go file to compile, and go
// list reports it with an error: it builds nothing, so it leaves the identity
// resolved, and its tests are named by their test main. A package that has a
// file to compile and does not compile still leaves it unresolved.
func TestATestOnlyPackageLeavesTheIdentityResolved(t *testing.T) {
	listing := append(identityFixture(),
		listedPackage{ImportPath: "example.com/a/scripts", Name: "scripts",
			Error: &listedError{Err: "no non-test Go files in /work/a/scripts"}},
		listedPackage{ImportPath: "example.com/a/scripts [example.com/a/scripts.test]", Name: "scripts",
			ForTest: "example.com/a/scripts", BuildID: "stAction/stContent", Deps: []string{"fmt", "testing"}},
		listedPackage{ImportPath: "example.com/a/scripts.test", Name: "main", BuildID: "smAction/smContent",
			Deps: []string{"example.com/a/scripts [example.com/a/scripts.test]", "fmt", "testing"}},
	)
	g := &identityGo{listing: encodeListing(t, listing)}
	g.install(t)
	manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "OK"))
	if manifest.Unresolved != "" {
		t.Fatalf("manifest = %+v, want a test-only package to leave it resolved", manifest)
	}
	if _, listed := manifest.Packages["example.com/a/scripts"]; listed {
		t.Fatalf("packages = %v, want no entry for a package that builds nothing", manifest.Packages)
	}
	if manifest.Tests["example.com/a/scripts.test"] == "" || manifest.Tests["example.com/a/scripts.test"] == unresolvedIdentity {
		t.Fatalf("tests = %v, want the test-only package's test main named", manifest.Tests)
	}

	broken := append(identityFixture(), listedPackage{ImportPath: "example.com/a/y", Name: "y", GoFiles: []string{"y.go"},
		Error: &listedError{Err: "y.go:1:1: expected 'package', found 'EOF'"}})
	g = &identityGo{listing: encodeListing(t, broken)}
	g.install(t)
	if manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP")); manifest.Unresolved == "" {
		t.Fatalf("manifest = %+v, want a package that does not compile to leave it unresolved", manifest)
	}
}
