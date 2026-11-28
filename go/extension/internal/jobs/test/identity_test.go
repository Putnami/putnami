package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// These tests pin the test identity (ADR 0017): the manifest test-exec keys
// on names every binary of the run by the content IDs of its link closure,
// under the flags test-exec compiles the binaries with.

// identityFixture is a listing of a module example.com/a with two packages:
// a, which has an internal test and links example.com/dep, and a/x, which has
// no test. dep, fmt, testing and unsafe are dependencies only.
func identityFixture() []listedPackage {
	return []listedPackage{
		{ImportPath: "example.com/a", Name: "a", BuildID: "aAction/aContent", GoFiles: []string{"a.go"}, Deps: []string{"example.com/dep", "fmt", "unsafe"}},
		{ImportPath: "example.com/a/x", Name: "x", BuildID: "xAction/xContent", GoFiles: []string{"x.go"}, Deps: []string{"fmt"}},
		{ImportPath: "example.com/dep", Name: "dep", BuildID: "depAction/depContent", DepOnly: true, Deps: []string{"fmt"}},
		{ImportPath: "fmt", Name: "fmt", BuildID: "fmtAction/fmtContent", DepOnly: true, Standard: true},
		{ImportPath: "testing", Name: "testing", BuildID: "testingAction/testingContent", DepOnly: true, Standard: true, Deps: []string{"fmt"}},
		{ImportPath: "unsafe", Name: "unsafe", DepOnly: true, Standard: true},
		{ImportPath: "example.com/a [example.com/a.test]", Name: "a", ForTest: "example.com/a", BuildID: "atAction/atContent",
			Deps: []string{"example.com/dep", "fmt", "testing", "unsafe"}},
		{ImportPath: "example.com/a.test", Name: "main", BuildID: "mainAction/mainContent",
			Deps: []string{"example.com/a [example.com/a.test]", "example.com/dep", "fmt", "testing", "unsafe"}},
	}
}

// encodeListing prints entries as `go list -json` does: one object after the
// other.
func encodeListing(t *testing.T, entries []listedPackage) string {
	t.Helper()
	var b strings.Builder
	for _, entry := range entries {
		data, err := json.MarshalIndent(entry, "", "\t")
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

// wantClosureDigest is the closure digest written out independently of
// closureDigest: SHA-256 of the domain line and one "path NUL contentID"
// line per member, sorted by path.
func wantClosureDigest(pairs map[string]string) string {
	paths := make([]string, 0, len(pairs))
	for path := range pairs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := sha256.New()
	h.Write([]byte("go-test-identity-v1\n"))
	for _, path := range paths {
		h.Write([]byte(path + "\x00" + pairs[path] + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// identityGo answers the go commands of the identity and of a solo test run:
// `go env -json`, the module package listing, the identity listing, which it
// records, and `go test`, which it records and passes.
type identityGo struct {
	listing    string
	listingErr error
	listings   [][]string
	tests      [][]string
}

func (g *identityGo) install(t *testing.T) {
	t.Helper()
	mockGoCommandRaw(t, func(_ string, args []string, dir string, _ []string) ([]byte, error) {
		switch {
		case args[0] == "env":
			return []byte(`{"GOARCH": "amd64", "GOOS": "linux", "GOVERSION": "go1.26.9"}` + "\n"), nil
		case args[0] == "list" && slices.Contains(args, "-export"):
			g.listings = append(g.listings, slices.Clone(args))
			return []byte(g.listing), g.listingErr
		case args[0] == "list":
			return []byte("example.com/" + filepath.Base(dir) + "\nexample.com/" + filepath.Base(dir) + "/x\n"), nil
		case args[0] == "test":
			g.tests = append(g.tests, slices.Clone(args))
			return []byte(testEvent("pass", "example.com/"+filepath.Base(dir), "", "") + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected go %v", args)
	})
}

// identityProject lays out a module example.com/a and returns a job context
// for it with params, its command output under a fresh directory.
func identityProject(t *testing.T, params pctx.Params) *pctx.Context {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/a\n\ngo 1.25.7\n")
	if params == nil {
		params = pctx.Params{}
	}
	return &pctx.Context{
		WorkspaceRoot: filepath.Dir(dir),
		OutputPath:    filepath.Join(t.TempDir(), "~test"),
		Project:       pctx.Project{Name: "a", FullPath: dir},
		Params:        params,
	}
}

// runIdentity runs the identity job and returns its manifest's bytes.
func runIdentity(t *testing.T, ctx *pctx.Context) []byte {
	t.Helper()
	status, data, err := RunIdentity(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("RunIdentity status=%q err=%v", status, err)
	}
	if data["manifest"] != IdentityManifestFile {
		t.Fatalf("RunIdentity data = %v, want the manifest port %q", data, IdentityManifestFile)
	}
	manifest, err := os.ReadFile(filepath.Join(ctx.OutputPath, IdentityManifestFile))
	if err != nil {
		t.Fatalf("RunIdentity wrote no manifest: %v", err)
	}
	return manifest
}

func decodeIdentity(t *testing.T, data []byte) IdentityManifest {
	t.Helper()
	var manifest IdentityManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest %s: %v", data, err)
	}
	return manifest
}

func TestIdentityManifestNamesEveryTestBinaryAndPackageByItsLinkClosure(t *testing.T) {
	g := &identityGo{listing: encodeListing(t, identityFixture())}
	g.install(t)
	data := runIdentity(t, identityProject(t, pctx.Params{"coverage": json.RawMessage("false")}))

	if !strings.HasSuffix(string(data), "}\n") {
		t.Fatalf("manifest %q does not end with a newline", data)
	}
	manifest := decodeIdentity(t, data)
	if manifest.Version != 1 || manifest.Go != "go1.26.9" || manifest.GOOS != "linux" || manifest.GOARCH != "amd64" {
		t.Fatalf("manifest header = %+v, want version 1 and the toolchain go env names", manifest)
	}
	if len(manifest.Flags) != 0 || manifest.Unresolved != "" {
		t.Fatalf("manifest = %+v, want no compile flag without coverage and a resolved identity", manifest)
	}

	// The test main's closure: itself, the internal test variant, and every
	// package it links, unsafe with its empty content ID.
	wantTests := map[string]string{"example.com/a.test": wantClosureDigest(map[string]string{
		"example.com/a.test":                 "mainContent",
		"example.com/a [example.com/a.test]": "atContent",
		"example.com/dep":                    "depContent",
		"fmt":                                "fmtContent",
		"testing":                            "testingContent",
		"unsafe":                             "",
	})}
	if !mapsEqual(manifest.Tests, wantTests) {
		t.Fatalf("manifest tests = %v, want %v", manifest.Tests, wantTests)
	}
	// Every package ./... matches has its own closure; a dependency and a test
	// variant are no key of their own.
	wantPackages := map[string]string{
		"example.com/a": wantClosureDigest(map[string]string{
			"example.com/a": "aContent", "example.com/dep": "depContent", "fmt": "fmtContent", "unsafe": "",
		}),
		"example.com/a/x": wantClosureDigest(map[string]string{"example.com/a/x": "xContent", "fmt": "fmtContent"}),
	}
	if !mapsEqual(manifest.Packages, wantPackages) {
		t.Fatalf("manifest packages = %v, want %v", manifest.Packages, wantPackages)
	}

	// A golden of the digest, computed outside Go: a change of the
	// computation is a change of the manifest version.
	if got := manifest.Packages["example.com/a/x"]; got != "a3527de1f99a0f57c423e100cba00e20e453cb7a044ee063c081f0dbec7a2924" {
		t.Fatalf("closure digest of example.com/a/x = %s, want the version 1 golden", got)
	}

	if len(g.listings) != 1 {
		t.Fatalf("identity listings = %q, want one", g.listings)
	}
	args := g.listings[0]
	for _, flag := range []string{"-e", "-trimpath", "-buildvcs=false", "-export", "-deps", "-test"} {
		if !slices.Contains(args, flag) {
			t.Errorf("identity listing %q lacks %s", args, flag)
		}
	}
	if args[len(args)-1] != "./..." {
		t.Errorf("identity listing %q does not select ./..., the packages go test ./... builds", args)
	}
}

// TestIdentityIsIndependentOfListingOrderAndActionIDs pins what moves the
// manifest: a content ID of a package in a closure, and nothing else. The
// order go list prints in, the order of a Deps list and the action ID half
// of a build ID leave it unchanged; a dependency's content ID moves the
// closures that link it, and those alone.
func TestIdentityIsIndependentOfListingOrderAndActionIDs(t *testing.T) {
	base := identityFixture()
	shuffled := slices.Clone(base)
	slices.Reverse(shuffled)
	for i := range shuffled {
		shuffled[i].Deps = slices.Clone(shuffled[i].Deps)
		slices.Reverse(shuffled[i].Deps)
		if action, content, ok := strings.Cut(shuffled[i].BuildID, "/"); ok {
			shuffled[i].BuildID = "other" + action + "/" + content
		}
	}
	edited := identityFixture()
	for i := range edited {
		if edited[i].ImportPath == "example.com/dep" {
			edited[i].BuildID = "depAction/depEdited"
		}
	}

	manifests := map[string][]byte{}
	for name, listing := range map[string][]listedPackage{"base": base, "shuffled": shuffled, "edited": edited} {
		g := &identityGo{listing: encodeListing(t, listing)}
		g.install(t)
		manifests[name] = runIdentity(t, identityProject(t, nil))
	}

	if string(manifests["shuffled"]) != string(manifests["base"]) {
		t.Fatalf("a reordered listing with other action IDs changed the manifest:\n%s\nvs\n%s", manifests["shuffled"], manifests["base"])
	}
	before, after := decodeIdentity(t, manifests["base"]), decodeIdentity(t, manifests["edited"])
	if before.Tests["example.com/a.test"] == after.Tests["example.com/a.test"] ||
		before.Packages["example.com/a"] == after.Packages["example.com/a"] {
		t.Fatalf("a dependency's new content ID left the closures that link it unchanged: %+v", after)
	}
	if before.Packages["example.com/a/x"] != after.Packages["example.com/a/x"] {
		t.Fatalf("a dependency's new content ID moved a/x, which does not link it: %+v", after)
	}
}

// TestIdentityListsWithTheCompileFlagsOfTheTestRun pins the one flag
// derivation: for each parameter set, the identity lists the binaries with
// exactly the compile flags the solo test run passes to `go test`, and the
// manifest names them. A coverage or race variant of a run therefore names
// other binaries than the default run.
func TestIdentityListsWithTheCompileFlagsOfTheTestRun(t *testing.T) {
	t.Setenv(envDatabaseTestBindings, "")
	cases := []struct {
		name   string
		params pctx.Params
		want   []string
	}{
		{name: "default", want: []string{"-cover", "-coverpkg", "example.com/a,example.com/a/x"}},
		{name: "coverage off", params: pctx.Params{"coverage": json.RawMessage("false")}, want: nil},
		{name: "coverage off with a profile", params: pctx.Params{
			"coverage": json.RawMessage("false"), "coverprofile": json.RawMessage(`"c.out"`),
		}, want: []string{"-cover", "-coverpkg", "example.com/a,example.com/a/x"}},
		{name: "atomic", params: pctx.Params{"covermode": json.RawMessage(`"atomic"`)},
			want: []string{"-cover", "-coverpkg", "example.com/a,example.com/a/x", "-covermode", "atomic"}},
		{name: "race", params: pctx.Params{"race": json.RawMessage("true")},
			want: []string{"-cover", "-coverpkg", "example.com/a,example.com/a/x", "-race"}},
		{name: "race without coverage", params: pctx.Params{"race": json.RawMessage("true"), "coverage": json.RawMessage("false")},
			want: []string{"-race"}},
	}

	identities := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &identityGo{listing: encodeListing(t, identityFixture())}
			g.install(t)
			ctx := identityProject(t, tc.params)
			manifest := runIdentity(t, ctx)
			if _, _, err := Run(ctx, jsonl.New(), nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(g.listings) != 1 || len(g.tests) != 1 {
				t.Fatalf("listings %q, test runs %q, want one of each", g.listings, g.tests)
			}

			listed := compileFlagsOf(g.listings[0])
			tested := compileFlagsOf(g.tests[0])
			if !slices.Equal(listed, tc.want) || !slices.Equal(tested, tc.want) {
				t.Fatalf("compile flags: identity listing %q, go test %q, want %q for both", listed, tested, tc.want)
			}
			if got := decodeIdentity(t, manifest).Flags; !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("manifest flags = %q, want %q", got, tc.want)
			}
			identities[tc.name] = string(manifest)
		})
	}
	// Two runs share an identity exactly when they compile with the same
	// flags: a coverage or race variant never shares the default run's.
	for _, a := range cases {
		for _, b := range cases {
			sameFlags := slices.Equal(a.want, b.want)
			sameIdentity := identities[a.name] == identities[b.name]
			if sameFlags != sameIdentity {
				t.Errorf("runs %q and %q: same compile flags %t, same identity %t", a.name, b.name, sameFlags, sameIdentity)
			}
		}
	}
}

// TestBatchMemberCompilesWithTheIdentityFlags pins the batch half of the
// derivation: under coverage-scope project, a member's `go test` passes the
// compile flags its own identity lists with.
func TestBatchMemberCompilesWithTheIdentityFlags(t *testing.T) {
	options := batchOptions{coverage: true, covermode: "count", race: true, useJSON: true}
	mockGoCommandRaw(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte("example.com/a\n"), nil
	})
	flags, err := projectBinaryFlags(options, "go", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	args := batchTestArgs(options, t.TempDir(), "member.out", []string{"example.com/a"}, []string{"./a/..."})
	if got, want := compileFlagsOf(args), flags.args(""); !slices.Equal(got, want) {
		t.Fatalf("batch member compile flags = %q, identity flags = %q", got, want)
	}
	if got := compileFlagsOf(identityListArgs(flags)); !slices.Equal(got, flags.args("")) {
		t.Fatalf("identity listing compile flags = %q, want %q", got, flags.args(""))
	}
}

// compileFlagsOf extracts the compile flags of a `go test` or `go list`
// command line: coverage, -coverpkg with its value, -covermode with its value
// and -race. -coverprofile and every run flag are left out.
func compileFlagsOf(args []string) []string {
	var flags []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-cover", "-race":
			flags = append(flags, args[i])
		case "-coverpkg", "-covermode":
			if i+1 < len(args) {
				flags = append(flags, args[i], args[i+1])
				i++
			}
		}
	}
	return flags
}

// TestIdentityOfABrokenClosureIsUnresolved pins the fail-closed direction: a
// package that does not compile has no content ID, so no closure that links
// it can be named. The manifest then carries a value no other run draws, the
// test run executes under a key of its own and reports the failure, and the
// job reports a skip, which the engine does not store. A closure that does not
// link the broken package keeps its digest.
func TestIdentityOfABrokenClosureIsUnresolved(t *testing.T) {
	original := identityNonce
	identityNonce = func() (string, error) { return "drawn", nil }
	t.Cleanup(func() { identityNonce = original })

	broken := identityFixture()
	for i := range broken {
		switch broken[i].ImportPath {
		case "example.com/dep":
			broken[i].BuildID = ""
			broken[i].Error = &listedError{Err: "dep.go:3:9: undefined: x"}
		case "example.com/a", "example.com/a [example.com/a.test]", "example.com/a.test":
			broken[i].BuildID = ""
			broken[i].DepsErrors = []*listedError{{Err: "dep.go:3:9: undefined: x"}}
		}
	}
	g := &identityGo{listing: encodeListing(t, broken)}
	g.install(t)
	manifest := decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP"))
	if manifest.Unresolved != "drawn" {
		t.Fatalf("manifest unresolved = %q, want the drawn value", manifest.Unresolved)
	}
	if manifest.Tests["example.com/a.test"] != unresolvedIdentity || manifest.Packages["example.com/a"] != unresolvedIdentity {
		t.Fatalf("manifest = %+v, want the closures linking the broken package unresolved", manifest)
	}
	if manifest.Packages["example.com/a/x"] == unresolvedIdentity {
		t.Fatalf("manifest = %+v, want a/x, which does not link it, resolved", manifest)
	}

	// A listing that fails names nothing, and is unresolved too.
	failing := &identityGo{listing: "go: go.mod:3: unknown directive: bogus\n", listingErr: errors.New("exit status 1")}
	failing.install(t)
	manifest = decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP"))
	if manifest.Unresolved != "drawn" || len(manifest.Tests) != 0 || len(manifest.Packages) != 0 {
		t.Fatalf("manifest of a failed listing = %+v, want it unresolved and empty", manifest)
	}

	// A dependency missing from the listing does not resolve either.
	var missing []listedPackage
	for _, entry := range identityFixture() {
		if entry.ImportPath != "example.com/dep" {
			missing = append(missing, entry)
		}
	}
	g = &identityGo{listing: encodeListing(t, missing)}
	g.install(t)
	manifest = decodeIdentity(t, runIdentityAs(t, identityProject(t, nil), "SKIP"))
	if manifest.Unresolved == "" {
		t.Fatalf("manifest = %+v, want a closure naming an unlisted package unresolved", manifest)
	}
}

func TestIdentitySkipsAProjectWithoutGoMod(t *testing.T) {
	g := &identityGo{}
	g.install(t)
	ctx := identityProject(t, nil)
	if err := os.Remove(filepath.Join(ctx.Project.FullPath, "go.mod")); err != nil {
		t.Fatal(err)
	}
	status, _, err := RunIdentity(ctx, jsonl.New(), nil)
	if err != nil || status != "SKIP" {
		t.Fatalf("RunIdentity status=%q err=%v, want SKIP as test-exec", status, err)
	}
	if _, err := os.Stat(filepath.Join(ctx.OutputPath, IdentityManifestFile)); !os.IsNotExist(err) {
		t.Fatalf("a skipped identity wrote a manifest: %v", err)
	}
	if len(g.listings) != 0 {
		t.Fatalf("a skipped identity listed %q", g.listings)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}
