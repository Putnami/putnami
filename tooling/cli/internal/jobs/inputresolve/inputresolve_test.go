package inputresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The test binary is also the @test/selectors extension: started with the
// input-resolve control arguments and helperModeEnv set, it answers one
// request in that mode and exits.
const (
	helperModeEnv = "PUTNAMI_INPUT_RESOLVE_HELPER_MODE"
	// helperLogEnv names a file the helper appends one line to per request,
	// holding the number of inputs it was asked.
	helperLogEnv = "PUTNAMI_INPUT_RESOLVE_HELPER_LOG"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" && wsproto.IsInputResolveInvocation(os.Args[1:]) {
		os.Exit(serveHelper(mode))
	}
	os.Exit(m.Run())
}

// serveHelper answers one request in mode and returns the exit status.
//   - list: each input lists the .txt files of its project, with a basis
//     whose one content set selects them.
//   - error: each input is an error answer.
//   - misattributed: the answer names another extension.
//   - hang: no answer before the caller's deadline.
//   - fail: no answer, an error on stderr and a failed exit.
func serveHelper(mode string) int {
	switch mode {
	case "hang":
		time.Sleep(30 * time.Second)
		return 0
	case "fail":
		fmt.Fprint(os.Stderr, "go.mod: unknown directive")
		return 3
	case "misattributed":
		data, err := wsproto.DecodeInputResolveDocument(os.Stdin)
		if err != nil {
			return 2
		}
		var request wsproto.InputResolveRequest
		if err := json.Unmarshal(data, &request); err != nil {
			return 2
		}
		result := wsproto.InputResolveResult{Version: wsproto.InputResolveProtocolVersion, Extension: "@test/other"}
		for _, input := range request.Inputs {
			result.Inputs = append(result.Inputs, wsproto.InputResolveAnswer{Project: input.Project, Selector: input.Selector})
		}
		encoded, _ := json.Marshal(result)
		_, _ = os.Stdout.Write(append(encoded, '\n'))
		return 0
	}
	_, err := wsproto.ServeInputResolve(os.Args[1:], os.Stdin, os.Stdout, func(request wsproto.InputResolveRequest) (wsproto.InputResolveResult, error) {
		if log := os.Getenv(helperLogEnv); log != "" {
			file, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return wsproto.InputResolveResult{}, err
			}
			_, _ = fmt.Fprintf(file, "%d\n", len(request.Inputs))
			_ = file.Close()
		}
		root, err := os.Getwd()
		if err != nil {
			return wsproto.InputResolveResult{}, err
		}
		result := wsproto.InputResolveResult{}
		for _, input := range request.Inputs {
			answer := wsproto.InputResolveAnswer{Project: input.Project, Selector: input.Selector}
			if mode == "error" {
				answer.Error = "embed pattern missing.txt: no matching files"
			} else {
				answer.Files = listText(filepath.Join(root, filepath.FromSlash(input.Project)))
				answer.Basis = wsproto.InputBasis{Content: [][]string{{"**/*.txt"}}}
			}
			result.Inputs = append(result.Inputs, answer)
		}
		return result, nil
	})
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		return 2
	}
	return 0
}

// listText lists the .txt files under dir as sorted slash paths relative to
// it.
func listText(dir string) []string {
	files := []string{}
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".txt") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(files)
	return files
}

// selectorsExtension is the @test/selectors extension, which lists sel:build.
func selectorsExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:    "@test/selectors",
		Runtime: &extension.RuntimeDefinition{InputSelectors: []string{"sel:build"}},
	}
}

// helperResolver is a resolver over a workspace at root whose @test/selectors
// extension is the helper in mode, and the log of the requests it answers.
func helperResolver(t *testing.T, root, mode string, timeout time.Duration) (*Resolver, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "requests")
	return newResolver(root, []*extension.ExtensionDescription{selectorsExtension()}, func(name string) provider {
		return &workspace.ExecProbeProvider{
			Extension:  name,
			Executable: os.Args[0],
			Dir:        root,
			Env:        append(os.Environ(), helperModeEnv+"="+mode, helperLogEnv+"="+log),
			Timeout:    timeout,
		}
	}), log
}

// requests returns the number of requests the helper answered and the number
// of inputs of each.
func requests(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// selectorWorkspace is a workspace root with an app project holding two .txt
// files and a README.
func selectorWorkspace(t *testing.T) (root, app string) {
	t.Helper()
	root = t.TempDir()
	app = filepath.Join(root, "app")
	writeFile(t, filepath.Join(app, "a.txt"), "A")
	writeFile(t, filepath.Join(app, "sub", "b.txt"), "B")
	writeFile(t, filepath.Join(app, "README.md"), "readme")
	return root, app
}

func query(dir string, patterns ...string) store.InputQuery {
	return store.InputQuery{Extension: "@test/selectors", Dir: dir, Selector: "sel:build", Patterns: patterns}
}

// An answer is kept while the digest of its basis holds: an edit the basis
// does not select asks nothing, and an edit or a file it selects asks again
// for that input alone.
func TestResolveInputKeepsAnAnswerWhileItsBasisHolds(t *testing.T) {
	root, app := selectorWorkspace(t)
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	got, err := resolver.ResolveInput(query(app, "src/**"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Files, []string{"a.txt", "sub/b.txt"}) || len(got.Basis.Content) != 1 {
		t.Fatalf("answer = %+v, want the project's .txt files and their basis", got)
	}
	writeFile(t, filepath.Join(app, "README.md"), "edited")
	if _, err := resolver.ResolveInput(query(app, "src/**", "src/**")); err != nil {
		t.Fatal(err)
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("requests = %v, want one: the basis holds and the pattern set is the same", got)
	}
	writeFile(t, filepath.Join(app, "c.txt"), "C")
	got, err = resolver.ResolveInput(query(app, "src/**"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Files, []string{"a.txt", "c.txt", "sub/b.txt"}) {
		t.Fatalf("answer after a basis change = %v, want the new file listed", got.Files)
	}
	writeFile(t, filepath.Join(app, "a.txt"), "edited")
	if _, err := resolver.ResolveInput(query(app, "src/**")); err != nil {
		t.Fatal(err)
	}
	if got := requests(t, log); !slices.Equal(got, []string{"1", "1", "1"}) {
		t.Fatalf("requests = %v, want one per basis change, each for one input", got)
	}
}

// Priming asks each extension once for every input it keeps no answer for,
// and an input it answered is not asked again.
func TestPrimeInputsAsksEachExtensionOnce(t *testing.T) {
	root, app := selectorWorkspace(t)
	writeFile(t, filepath.Join(root, "lib", "l.txt"), "L")
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	queries := []store.InputQuery{
		query(app, "src/**"),
		query(app, "src/**"),
		query(filepath.Join(root, "lib"), "src/**"),
		query(root),
		{Extension: "@test/selectors", Dir: app, Selector: "sel:other"},
	}
	resolver.PrimeInputs(queries)
	resolver.PrimeInputs(queries)
	if got := requests(t, log); !slices.Equal(got, []string{"3"}) {
		t.Fatalf("requests = %v, want one with the three distinct listed inputs", got)
	}
	for _, q := range queries[:4] {
		if _, err := resolver.ResolveInput(q); err != nil {
			t.Fatal(err)
		}
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("requests = %v, want the primed answers read", got)
	}
}

// Concurrent queries for one input wait for one call to the extension.
func TestResolveInputCallsOneExtensionOneAtATime(t *testing.T) {
	root, app := selectorWorkspace(t)
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = resolver.ResolveInput(query(app, "src/**"))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("requests = %v, want one call for eight concurrent queries", got)
	}
}

// An error answer fails the input and is never kept; priming keeps none
// either.
func TestAnErrorAnswerFailsTheInputAndIsNotKept(t *testing.T) {
	root, app := selectorWorkspace(t)
	resolver, log := helperResolver(t, root, "error", 30*time.Second)
	resolver.PrimeInputs([]store.InputQuery{query(app)})
	for range 2 {
		_, err := resolver.ResolveInput(query(app))
		if !errors.Is(err, store.ErrInputSelector) || !strings.Contains(err.Error(), "no matching files") || !strings.Contains(err.Error(), "@test/selectors") {
			t.Fatalf("error answer = %v, want ErrInputSelector naming the extension and the answer", err)
		}
	}
	if got := requests(t, log); len(got) != 3 {
		t.Fatalf("requests = %v, want every read of an error answer to ask again", got)
	}
	if !resolver.SelectsInput(query(app), "README.md") {
		t.Fatal("an input that does not expand selects less than every path")
	}
}

// An answer attributed to another extension, a failed exit and a hung
// extension each fail the input with the transport's classification.
func TestARejectedOrFailedCallFailsTheInput(t *testing.T) {
	root, app := selectorWorkspace(t)
	for mode, want := range map[string]string{
		"misattributed": `attributed to extension "@test/other"`,
		"fail":          "go.mod: unknown directive",
		"hang":          "did not answer within",
	} {
		t.Run(mode, func(t *testing.T) {
			resolver, _ := helperResolver(t, root, mode, 2*time.Second)
			_, err := resolver.ResolveInput(query(app))
			var failure *wsproto.ProbeFailure
			if !errors.Is(err, store.ErrInputSelector) || !errors.As(err, &failure) {
				t.Fatalf("%s = %v, want ErrInputSelector carrying the probe failure", mode, err)
			}
			text := failure.Message
			for _, d := range failure.Diagnostics {
				text += " " + d.Message
			}
			if !strings.Contains(text, want) {
				t.Fatalf("%s failure = %q, want %q", mode, text, want)
			}
		})
	}
}

// A selector the extension does not list, an unknown extension and a project
// outside the workspace are refused before any call; a nil resolver refuses
// every query and primes nothing.
func TestResolveInputRefusesWhatNoExtensionExpands(t *testing.T) {
	root, app := selectorWorkspace(t)
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	for name, q := range map[string]store.InputQuery{
		"an unlisted selector": {Extension: "@test/selectors", Dir: app, Selector: "sel:test"},
		"an unknown extension": {Extension: "@test/unknown", Dir: app, Selector: "sel:build"},
		"another directory":    query(filepath.Dir(root)),
	} {
		if _, err := resolver.ResolveInput(q); !errors.Is(err, store.ErrInputSelector) {
			t.Errorf("%s = %v, want ErrInputSelector", name, err)
		}
	}
	if got := requests(t, log); len(got) != 0 {
		t.Fatalf("requests = %v, want none", got)
	}
	var none *Resolver
	if _, err := none.ResolveInput(query(app)); !errors.Is(err, store.ErrInputSelector) {
		t.Errorf("nil resolver = %v, want ErrInputSelector", err)
	}
	none.PrimeInputs([]store.InputQuery{query(app)})
	if !none.Matcher().SelectsInput(workspace.InputSelectorQuery{Extension: "@test/selectors", Dir: app, Selector: "sel:build"}, "x") {
		t.Error("a nil resolver's matcher selects less than every path")
	}
}

// Change impact reads a listed file, a path the basis selects that does not
// exist yet, and no other path; its priming is the resolver's.
func TestMatcherSelectsTheAnswerAndItsBasis(t *testing.T) {
	root, app := selectorWorkspace(t)
	resolver, log := helperResolver(t, root, "list", 30*time.Second)
	matcher := resolver.Matcher()
	q := workspace.InputSelectorQuery{Extension: "@test/selectors", Dir: app, Selector: "sel:build", Patterns: []string{"src/**"}}
	matcher.PrimeInputs([]workspace.InputSelectorQuery{q})
	for rel, want := range map[string]bool{"a.txt": true, "deleted/gone.txt": true, "README.md": false} {
		if got := matcher.SelectsInput(q, rel); got != want {
			t.Errorf("SelectsInput(%s) = %v, want %v", rel, got, want)
		}
	}
	if got := requests(t, log); len(got) != 1 {
		t.Fatalf("requests = %v, want the primed answer read", got)
	}
}

// New builds each extension's provider from its prepared runtime, keeping the
// first description of a name.
func TestNewAsksThePreparedRuntime(t *testing.T) {
	root, app := selectorWorkspace(t)
	t.Setenv(helperModeEnv, "list")
	t.Setenv(helperLogEnv, filepath.Join(t.TempDir(), "requests"))
	prepared := selectorsExtension()
	prepared.RuntimeExecutable = os.Args[0]
	unprepared := selectorsExtension()
	unprepared.Runtime = nil
	ws := workspace.NewWorkspace(root, nil, nil)
	resolver := New(context.Background(), ws, []*extension.ExtensionDescription{nil, prepared, unprepared}, nil)
	got, err := resolver.ResolveInput(query(app))
	if err != nil || !slices.Equal(got.Files, []string{"a.txt", "sub/b.txt"}) {
		t.Fatalf("answer = %+v, %v", got, err)
	}
}
