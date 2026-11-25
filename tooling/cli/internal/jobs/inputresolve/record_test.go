package inputresolve

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// recordingResolver is a resolver of one run over the workspace at root, whose
// @test/selectors extension is the helper listing .txt files, implemented by
// digest, and that keeps its answers under storeRoot. It returns the log of
// the requests the helper answers.
func recordingResolver(t *testing.T, storeRoot, root, digest string) (*Resolver, string) {
	t.Helper()
	return recordingResolverUnder(t, storeRoot, root, digest, "probe-a")
}

// recordingResolverUnder is recordingResolver in a workspace whose probe
// digest is probeDigest.
func recordingResolverUnder(t *testing.T, storeRoot, root, digest, probeDigest string) (*Resolver, string) {
	t.Helper()
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	ext := selectorsExtension()
	ext.RuntimeDigest = digest
	resolver.records = newRecords(storeRoot, root, map[string]*extension.ExtensionDescription{ext.Name: ext},
		func() string { return probeDigest })
	return resolver, log
}

func resolveFiles(t *testing.T, resolver *Resolver, q store.InputQuery) []string {
	t.Helper()
	got, err := resolver.ResolveInput(q)
	if err != nil {
		t.Fatal(err)
	}
	return got.Files
}

func TestARecordedInputAnswerIsReadAgain(t *testing.T) {
	storeRoot := t.TempDir()
	root, app := selectorWorkspace(t)
	first, log := recordingResolver(t, storeRoot, root, "digest-a")
	if got := resolveFiles(t, first, query(app, "src/**")); !slices.Equal(got, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("files = %v", got)
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("the first run asked %d times, want 1", len(got))
	}

	// The next runs read the record, through a query and through a prime.
	second, log := recordingResolver(t, storeRoot, root, "digest-a")
	if got := resolveFiles(t, second, query(app, "src/**")); !slices.Equal(got, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("recorded files = %v", got)
	}
	third, _ := recordingResolver(t, storeRoot, root, "digest-a")
	third.PrimeInputs([]store.InputQuery{query(app, "src/**")})
	if !third.holds(answerKey("@test/selectors", mustRequest(t, third, query(app, "src/**")))) {
		t.Fatal("a prime did not keep the recorded answer")
	}
	if got := requests(t, log); len(got) != 0 {
		t.Fatalf("a run with a recorded answer asked %d times, want 0", len(got))
	}

	// A file the basis selects changes: the record no longer holds.
	writeFile(t, filepath.Join(app, "c.txt"), "C")
	fourth, log := recordingResolver(t, storeRoot, root, "digest-a")
	if got := resolveFiles(t, fourth, query(app, "src/**")); !slices.Equal(got, []string{"a.txt", "c.txt", "sub/b.txt"}) {
		t.Fatalf("files after an edit = %v", got)
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("a run after an edit asked %d times, want 1", len(got))
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-recorded-input-answer-is-read-again")
}

func TestAChangedExtensionOrRequestAsksAgain(t *testing.T) {
	root, app := selectorWorkspace(t)
	storeRoot := t.TempDir()
	first, _ := recordingResolver(t, storeRoot, root, "digest-a")
	resolveFiles(t, first, query(app, "src/**"))

	for _, run := range []struct {
		name   string
		digest string
		probe  string
		query  store.InputQuery
	}{
		{"another implementation", "digest-b", "probe-a", query(app, "src/**")},
		{"other patterns", "digest-a", "probe-a", query(app, "lib/**")},
		// The workspace's projects or edges moved: the answer can follow
		// them outside its basis.
		{"another project graph", "digest-a", "probe-b", query(app, "src/**")},
	} {
		// Each row reads a store whose one record the first run wrote, so it
		// misses for its own reason alone.
		rowStore := t.TempDir()
		recorded, _ := recordingResolver(t, rowStore, root, "digest-a")
		resolveFiles(t, recorded, query(app, "src/**"))
		resolver, log := recordingResolverUnder(t, rowStore, root, run.digest, run.probe)
		resolveFiles(t, resolver, run.query)
		if got := requests(t, log); len(got) != 1 {
			t.Errorf("%s: asked %d times, want 1", run.name, len(got))
		}
	}

	// A record whose digest does not hold is not read.
	resolver, _ := recordingResolver(t, storeRoot, root, "digest-a")
	resolver.records.write("@test/selectors", answerKey("@test/selectors", mustRequest(t, resolver, query(app, "src/**"))),
		kept{expansion: store.InputExpansion{Files: []string{"a.txt", "gone.txt", "sub/b.txt"}}, digest: "any"})
	if _, ok := resolver.recorded("@test/selectors", answerKey("@test/selectors", mustRequest(t, resolver, query(app, "src/**"))), app); ok {
		t.Fatal("a record whose digest does not hold was read")
	}

	// An extension with no implementation identity is never recorded.
	none, log := helperResolver(t, root, "list", 30*time.Second)
	none.records = newRecords(storeRoot, root, map[string]*extension.ExtensionDescription{"@test/selectors": selectorsExtension()},
		func() string { return "probe-a" })
	resolveFiles(t, none, query(app, "src/**"))
	again, againLog := helperResolver(t, root, "list", 30*time.Second)
	again.records = none.records
	resolveFiles(t, again, query(app, "src/**"))
	if got := len(requests(t, log)) + len(requests(t, againLog)); got != 2 {
		t.Fatalf("an extension with no identity was asked %d times over two runs, want 2", got)
	}
	spectest.Proves(t, "cli/job-planning-execution", "unchanged-planning-reads-records",
		"a-changed-extension-or-request-asks-again")
}

func TestARecordListingAFileThatIsGoneIsNotRead(t *testing.T) {
	storeRoot := t.TempDir()
	root, app := selectorWorkspace(t)
	resolver, _ := recordingResolver(t, storeRoot, root, "digest-a")
	key := answerKey("@test/selectors", mustRequest(t, resolver, query(app, "src/**")))
	// The basis selects no listed file, so its digest holds while one is gone.
	basis := wsproto.InputBasis{Content: [][]string{{"README.md"}}}
	digest, err := wsproto.InputBasisDigest(app, basis)
	if err != nil {
		t.Fatal(err)
	}
	resolver.records.write("@test/selectors", key,
		kept{expansion: store.InputExpansion{Files: []string{"a.txt", "gone.txt"}, Basis: basis}, digest: digest})
	if _, ok := resolver.recorded("@test/selectors", key, app); ok {
		t.Fatal("a record listing a file that is gone was read")
	}
	resolver.records.write("@test/selectors", key,
		kept{expansion: store.InputExpansion{Files: []string{"a.txt"}, Basis: basis}, digest: digest})
	if _, ok := resolver.recorded("@test/selectors", key, app); !ok {
		t.Fatal("a record whose files and basis hold was not read")
	}
}

func TestANewerAnswerReplacesTheRecordOfItsRequest(t *testing.T) {
	storeRoot := t.TempDir()
	root, app := selectorWorkspace(t)
	for _, digest := range []string{"digest-a", "digest-b", "digest-c"} {
		resolver, _ := recordingResolver(t, storeRoot, root, digest)
		resolveFiles(t, resolver, query(app, "src/**"))
	}
	var files int
	_ = filepath.WalkDir(filepath.Join(storeRoot, "input-resolve"), func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Fatalf("three implementations answering one request left %d records, want 1", files)
	}
}

func TestAMalformedInputRecordIsNotRead(t *testing.T) {
	storeRoot := t.TempDir()
	root, app := selectorWorkspace(t)
	first, _ := recordingResolver(t, storeRoot, root, "digest-a")
	resolveFiles(t, first, query(app, "src/**"))
	key := answerKey("@test/selectors", mustRequest(t, first, query(app, "src/**")))
	path, _, ok := first.records.file("@test/selectors", key)
	if !ok {
		t.Fatal("no record path")
	}
	for _, content := range []string{"", "{", `{"schema":1,"key":"other","digest":"d"}`, `{"schema":2}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := first.records.read("@test/selectors", key); ok {
			t.Errorf("record %q was read", content)
		}
	}
}

func mustRequest(t *testing.T, r *Resolver, q store.InputQuery) wsproto.InputResolveQuery {
	t.Helper()
	asked, err := r.request(q)
	if err != nil {
		t.Fatal(err)
	}
	return asked
}
