package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// --credential-fd is consumed by the global flag pass and binds nothing: the
// arguments every job receives, and so every cache key and run marker, are
// the ones of the same invocation without it.
func TestCredentialFDIsConsumedAndBindsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		with, without []string
	}{
		{[]string{"build", "--credential-fd", "3", "--projects", "api"}, []string{"build", "--projects", "api"}},
		{[]string{"test", "--credential-fd=7", "--coverage"}, []string{"test", "--coverage"}},
		{[]string{"run", "--", "--credential-fd", "3", "arg"}, []string{"run", "--", "arg"}},
		{[]string{"projects", "list", "--credential-fd", "3"}, []string{"projects", "list"}},
	} {
		with, without := ParseArgs(tc.with, nil, nil), ParseArgs(tc.without, nil, nil)
		if with.Err != nil || without.Err != nil {
			t.Fatalf("ParseArgs(%q) = %v; ParseArgs(%q) = %v", tc.with, with.Err, tc.without, without.Err)
		}
		if !reflect.DeepEqual(with.RawJobArgs, without.RawJobArgs) || !reflect.DeepEqual(with.Global, without.Global) {
			t.Errorf("ParseArgs(%q) = %q %+v, want the parse of %q: %q %+v",
				tc.with, with.RawJobArgs, with.Global, tc.without, without.RawJobArgs, without.Global)
		}
	}
}

func TestCredentialFDValueIsADescriptorNumber(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{
		"build --credential-fd three": `invalid --credential-fd value "three"`,
		"build --credential-fd=1":     "invalid --credential-fd value 1: descriptors 0, 1 and 2 are the standard streams",
		"build --credential-fd":       "flag --credential-fd requires a value: --credential-fd <n>",
	} {
		parsed := ParseArgs(strings.Fields(args), nil, nil)
		if parsed.Err == nil || !strings.Contains(parsed.Err.Error(), want) {
			t.Errorf("ParseArgs(%q).Err = %v, want %q", args, parsed.Err, want)
		}
	}
}

// qualifiedCallOffset returns the offset of the first call of pkg.name in
// function of file, and false when function never calls it.
func qualifiedCallOffset(t *testing.T, file, function, pkg, name string) (token.Pos, bool) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found token.Pos
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || found != token.NoPos {
				return found == token.NoPos
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != name {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == pkg {
				found = call.Pos()
				return false
			}
			return true
		})
	}
	return found, found != token.NoPos
}

// The run credential is captured before App.Run can start a process, replace
// this one or read the workspace: enterProcess captures it before it enters
// the agent workspace, and App.Run calls enterProcess right after the early
// capability capture and before the package-root claim, the pinned-CLI
// relaunch, the capability capture and the parse.
func TestRunCredentialIsCapturedBeforeAnythingStarts(t *testing.T) {
	t.Parallel()
	capture, ok := qualifiedCallOffset(t, "app.go", "enterProcess", "runcredential", "Capture")
	if !ok {
		t.Fatal("enterProcess never calls runcredential.Capture")
	}
	if enter, ok := qualifiedCallOffset(t, "app.go", "enterProcess", "launch", "EnterAgentWorkspace"); !ok || enter < capture {
		t.Error("enterProcess enters the agent workspace before runcredential.Capture (or never)")
	}
	calls := callOffsets(t, "app.go", "Run")
	entry, ok := calls["enterProcess"]
	if !ok {
		t.Fatal("App.Run never calls enterProcess")
	}
	if early, ok := calls["captureEarlyReleaseSetProviderCapability"]; !ok || early > entry {
		t.Error("App.Run calls enterProcess before captureEarlyReleaseSetProviderCapability (or never calls it)")
	}
	for _, later := range [][2]string{
		{"workspace", "FindRoot"},
		{"launch", "Relaunch"},
		{"jobs", "CaptureProcessCapabilities"},
		{"sessionreporter", "Capture"},
	} {
		offset, ok := qualifiedCallOffset(t, "app.go", "Run", later[0], later[1])
		if !ok {
			t.Fatalf("App.Run never calls %s.%s", later[0], later[1])
		}
		if offset < entry {
			t.Errorf("App.Run calls %s.%s before enterProcess", later[0], later[1])
		}
	}
	for _, later := range []string{"claimPackageRoot", "takeProvidersEnv", "ParseArgs"} {
		if offset, ok := calls[later]; !ok || offset < entry {
			t.Errorf("App.Run calls %s before enterProcess (or never)", later)
		}
	}
}

// Inspection of the engine is denied before the credential provider is
// installed, on both paths that install it.
func TestCredentialsAreGuardedBeforeTheProviderIsInstalled(t *testing.T) {
	t.Parallel()
	for file, function := range map[string]string{
		"providers.go":      "resolveExecutionPolicy",
		"runner_execute.go": "runBoundRequest",
	} {
		calls := callOffsets(t, file, function)
		guard, ok := calls["guardCredentials"]
		if !ok {
			t.Fatalf("%s: %s never calls guardCredentials", file, function)
		}
		install, ok := calls["installCredentialProviders"]
		if !ok || install < guard {
			t.Errorf("%s: %s installs the credential provider before guardCredentials", file, function)
		}
	}
}
