package cli

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/credentialprovider"
	"go.putnami.dev/tooling/cli/internal/env"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

func TestProvidersFlagParsesASortedUniqueList(t *testing.T) {
	t.Parallel()
	for args, want := range map[string][]string{
		"--providers install":                            {"install"},
		"--providers publish,install":                    {"install", "publish"},
		"--providers=install,install":                    {"install"},
		"--providers publish --providers install":        {"install", "publish"},
		"--verbose --providers install --max-parallel 2": {"install"},
	} {
		g, _, err := parseFlags(strings.Fields(args))
		if err != nil || !reflect.DeepEqual(g.Providers, want) {
			t.Errorf("parseFlags(%q) = %v, %v; want %v", args, g.Providers, err, want)
		}
	}
	for _, args := range [][]string{{"--providers"}, {"--providers", ""}, {"--providers", "deploy"}, {"--providers", "install,"}, {"--providers", "Install"}} {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%q) accepted an invalid provider list", args)
		}
	}
	// A comma list with spaces arrives as one argument from a shell.
	if g, _, err := parseFlags([]string{"--providers", " install , publish "}); err != nil || !reflect.DeepEqual(g.Providers, []string{"install", "publish"}) {
		t.Errorf("spaced list = %v, %v", g.Providers, err)
	}
}

func TestProvidersNeverBecomeACommandParameter(t *testing.T) {
	t.Parallel()
	base := ParseArgs([]string{"build", "--projects", "app"}, nil, nil)
	parsed := ParseArgs([]string{"build", "--projects", "app", "--providers", "install,publish"}, nil, nil)
	if parsed.Err != nil || !reflect.DeepEqual(parsed.RawJobArgs, base.RawJobArgs) || !reflect.DeepEqual(parsed.JobFlags, base.JobFlags) {
		t.Fatalf("--providers changed task parameters: err=%v args=%v flags=%v", parsed.Err, parsed.RawJobArgs, parsed.JobFlags)
	}
}

func TestProvidersFlagWinsOverTheEnvironment(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "providers-are-opt-in", "flag-wins-over-the-environment")
	if env.Prefix+"PROVIDERS" != credentialprovider.ProvidersEnv {
		t.Fatalf("the CLI reads %sPROVIDERS, the provider launch strips %s", env.Prefix, credentialprovider.ProvidersEnv)
	}
	for name, c := range map[string]struct {
		flag     []string
		fromEnv  string
		want     []string
		wantFrom string
	}{
		"flag and environment": {[]string{"install"}, "publish", []string{"install"}, "--providers"},
		"environment only":     {nil, " publish,install ", []string{"install", "publish"}, "PUTNAMI_PROVIDERS"},
		"neither":              {nil, " ", nil, ""},
	} {
		providers, source, err := invocationProviders(nil, c.flag, c.fromEnv)
		if err != nil || !reflect.DeepEqual(providers, c.want) || source != c.wantFrom {
			t.Errorf("%s: %v from %q, %v; want %v from %q", name, providers, source, err, c.want, c.wantFrom)
		}
	}
	_, _, err := invocationProviders(nil, nil, "install,deploy")
	if err == nil || !strings.Contains(err.Error(), "PUTNAMI_PROVIDERS") || strings.Contains(err.Error(), "--providers") {
		t.Fatalf("an unknown provider in the environment = %v, want a usage error naming PUTNAMI_PROVIDERS only", err)
	}
}

// A bound execution request enables exactly its invocation.providers: the
// executing engine's PUTNAMI_PROVIDERS never adds or removes a purpose.
func TestABoundRequestEnablesExactlyItsProviders(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "providers-are-opt-in", "bound-request-decides-alone")
	var request runner.ExecutionRequest
	request.Invocation.Providers = []string{"install"}
	providers, source, err := invocationProviders(&request, nil, "publish")
	if err != nil || !reflect.DeepEqual(providers, []string{"install"}) || source != providersFromRequest {
		t.Fatalf("bound request naming install, environment naming publish = %v from %q, %v", providers, source, err)
	}
	broker := credentialBroker(providers, source, "", nil, []*extension.ExtensionDescription{credentialDeclarer("@a/x")}, io.Discard)
	if broker == nil || !broker.Enabled(registry.PurposeRead) || broker.Enabled(registry.PurposePublish) {
		t.Fatalf("the bound broker serves read=%v publish=%v; want read only", broker.Enabled(registry.PurposeRead), broker.Enabled(registry.PurposePublish))
	}
	_ = broker.Close()
	providers, _, err = invocationProviders(&runner.ExecutionRequest{}, nil, "install")
	if err != nil || len(providers) != 0 {
		t.Fatalf("bound request with no providers, environment naming install = %v, %v; want none", providers, err)
	}
}

func TestProvidersFlagCatalog(t *testing.T) {
	t.Parallel()
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Long == "--providers" {
			if flag.Type != commandmeta.FlagValue || flag.Category != "Execution" || !reflect.DeepEqual(flag.Values, []string{"install", "publish"}) {
				t.Errorf("providers flag catalog = %+v", flag)
			}
			return
		}
	}
	t.Fatal("--providers missing from the shared parser/help/completion catalog")
}

func credentialDeclarer(name string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: name, Version: "1.0.0",
		Commands: map[string]string{registry.CredentialProviderCommand: "Credentials"},
		Jobs:     map[string]*extension.JobDefinition{registry.CredentialProviderCommand: {Name: registry.CredentialProviderCommand, Command: "/does/not/exist"}},
	}
}

// Off by default, absent provider, and an ambiguous provider. The first two
// build no broker, so nothing is installed or launched. The third builds a
// broker whose first credential fails and names both extensions and the
// source, so a command that downloads nothing still runs. None of them
// installs a credential source, so the test runs beside the package's
// parallel downloads.
func TestInstallCredentialProviders(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-provider", "one-provider-or-none", "errors-name-their-source")
	ambiguous := []*extension.ExtensionDescription{credentialDeclarer("@a/x"), credentialDeclarer("@b/y")}
	if broker := credentialBroker(nil, "", "", nil, ambiguous, io.Discard); broker != nil {
		t.Fatal("flag off with two declarers built a broker; the flag off resolves nothing")
	}
	plain := &extension.ExtensionDescription{Name: "@a/plain", Version: "1.0.0", Commands: map[string]string{"build": "Build"}}
	if broker := credentialBroker([]string{"install"}, providersFromFlag, "", nil, []*extension.ExtensionDescription{plain}, io.Discard); broker != nil {
		t.Fatal("flag on, no provider built a broker")
	}
	broker := credentialBroker([]string{"install"}, providersFromEnv, "", nil, ambiguous, io.Discard)
	if broker == nil {
		t.Fatal("flag on, two declarers built no broker; the first download must fail")
	}
	t.Cleanup(func() { _ = broker.Close() })
	target, _ := url.Parse("https://put.putnami.dev/x")
	_, served, err := broker.Bearer(context.Background(), registry.PurposeRead, target)
	if served || !errors.Is(err, extension.ErrProviderAmbiguous) {
		t.Fatalf("flag on, two declarers: served=%v err=%v; want ErrProviderAmbiguous", served, err)
	}
	for _, want := range []string{"@a/x", "@b/y", "PUTNAMI_PROVIDERS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the ambiguity error does not name %s: %v", want, err)
		}
	}
}

// installCredentialProviders also makes the broker the source of the
// credential a hosted fetch job receives, and its restore removes it. A start
// that fails for another reason than a holder refusal, here two declaring
// extensions, installs the broker on a hosted run too, and its first
// credential fails.
//
// On a hosted run, a provider the run refuses to start as a
// credential holder fails the call, before the first hook, and installs no
// source: one that is not its extension's native runtime, and one that would
// start after repository code.
func TestInstallCredentialProvidersSourcesTheFetchJob(t *testing.T) {
	ambiguous := []*extension.ExtensionDescription{credentialDeclarer("@a/x"), credentialDeclarer("@b/y")}
	for _, bearer := range []string{"", "run-bearer"} {
		restoreCredential := runcredential.SetForTest(bearer)
		restore, err := installCredentialProviders([]string{"install"}, providersFromFlag, "", nil, ambiguous, io.Discard)
		if err != nil {
			restoreCredential()
			t.Fatalf("hosted %t: installCredentialProviders = %v; want the broker installed", bearer != "", err)
		}
		_, err = jobs.ReadJobCredential(context.Background())
		restore()
		restoreCredential()
		if !errors.Is(err, extension.ErrProviderAmbiguous) {
			t.Fatalf("hosted %t, installed: err = %v; want the broker's answer, ErrProviderAmbiguous", bearer != "", err)
		}
		if credential, err := jobs.ReadJobCredential(context.Background()); credential != nil || err != nil {
			t.Fatalf("hosted %t, restored: %v, %v; want no source", bearer != "", credential, err)
		}
	}

	// A provider whose command is an existing program that is not its
	// extension's runtime: this test binary.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	notRuntime := credentialDeclarer("@a/launcher")
	notRuntime.Jobs[registry.CredentialProviderCommand].Command = self
	restoreCredential := runcredential.SetForTest("run-bearer")
	t.Cleanup(restoreCredential)
	var native *runcredential.NativeHolderError
	var custody *runcredential.CustodyError
	// In this order: the second case marks repository code, which stays.
	for _, c := range []struct {
		name    string
		before  func()
		refused func(error) bool
	}{
		{"not the native runtime", func() {}, func(err error) bool { return errors.As(err, &native) }},
		{"after repository code", func() { runcredential.MarkRepositoryCodeStarted("a test hook") },
			func(err error) bool { return errors.As(err, &custody) }},
	} {
		c.before()
		restore, err := installCredentialProviders([]string{"install"}, providersFromFlag, "", nil,
			[]*extension.ExtensionDescription{notRuntime}, io.Discard)
		restore()
		if !c.refused(err) {
			t.Errorf("%s: installCredentialProviders = %v; want the holder refusal", c.name, err)
		}
		if credential, err := jobs.ReadJobCredential(context.Background()); credential != nil || err != nil {
			t.Errorf("%s: a refused provider left a source that answers %v, %v", c.name, credential, err)
		}
	}
}

// resolveExecutionPolicy maps each outcome to the exit code App.Run returns,
// and leaves the effective providers (the flag, else the environment) in the
// global flags, where upgrade reads the list its restart carries. No case
// installs a credential source.
func TestResolveExecutionPolicyExitCodes(t *testing.T) {
	t.Parallel()
	plain := []*extension.ExtensionDescription{{Name: "@a/plain", Version: "1.0.0", Commands: map[string]string{"build": "Build"}}}
	for name, c := range map[string]struct {
		global    GlobalFlags
		fromEnv   string
		want      int
		effective []string
	}{
		"no provider":                 {GlobalFlags{CPUBudgetPolicy: "measured", Providers: []string{"install"}}, "", ExitSuccess, []string{"install"}},
		"no provider, from the env":   {GlobalFlags{CPUBudgetPolicy: "measured"}, "install,publish", ExitSuccess, []string{"install", "publish"}},
		"the flag wins over the env":  {GlobalFlags{CPUBudgetPolicy: "measured", Providers: []string{"publish"}}, "install", ExitSuccess, []string{"publish"}},
		"unknown provider in the env": {GlobalFlags{CPUBudgetPolicy: "measured"}, "deploy", ExitUsage, nil},
		"unknown cpu policy":          {GlobalFlags{CPUBudgetPolicy: "fastest", Providers: []string{"install"}}, "", ExitUsage, []string{"install"}},
	} {
		global := c.global
		stop, code := resolveExecutionPolicy(&global, nil, []string{"build"}, "", plain, c.fromEnv)
		stop()
		if code != c.want {
			t.Errorf("%s: exit %d, want %d", name, code, c.want)
		}
		if !reflect.DeepEqual(global.Providers, c.effective) {
			t.Errorf("%s: providers = %q, want %q", name, global.Providers, c.effective)
		}
	}
}

// The credential provider is installed before the first download a parsed
// command line can reach: in App.Run before the read preparation and the
// workspace bootstrap, and in the bound-request adapter before its bootstrap.
// Downloads before the parse keep the host-keyed credential by design. An
// end-to-end proof would need an installed provider extension and a
// bootstrap served over HTTP; the order of the calls is the invariant.
func TestCredentialProviderIsInstalledBeforeBootstrapDownloads(t *testing.T) {
	t.Parallel()
	for file, c := range map[string]struct {
		function string
		install  string
		after    []string
	}{
		"app.go":            {"Run", "resolveExecutionPolicy", []string{"withInvocationReadPreparation", "EnsureWorkspaceBootstrap"}},
		"runner_execute.go": {"runBoundRequest", "installCredentialProviders", []string{"EnsureWorkspaceBootstrap"}},
	} {
		calls := callOffsets(t, file, c.function)
		install, ok := calls[c.install]
		if !ok {
			t.Fatalf("%s: %s never calls %s", file, c.function, c.install)
		}
		for _, later := range c.after {
			offset, ok := calls[later]
			if !ok {
				t.Fatalf("%s: %s never calls %s", file, c.function, later)
			}
			if offset < install {
				t.Errorf("%s: %s calls %s before %s installs the credential provider", file, c.function, later, c.install)
			}
		}
	}
}

// On a hosted run the credential provider receives the run credential, so it
// starts where it is installed, before the first repository process: a
// provider started afterwards is refused (runcredential.StartHolder).
// TestCredentialProviderIsInstalledBeforeBootstrapDownloads places the install
// before the bootstrap, and credentialprovider's
// TestStartLaunchesTheProviderBeforeRepositoryCode proves that Start launches
// the provider with the run credential.
func TestHostedCredentialProviderStartsWhereItIsInstalled(t *testing.T) {
	t.Parallel()
	calls := callOffsets(t, "providers.go", "installCredentialProviders")
	start, ok := calls["Start"]
	if !ok {
		t.Fatal("installCredentialProviders never starts the credential provider")
	}
	if hosted, ok := calls["Hosted"]; !ok || hosted > start {
		t.Error("installCredentialProviders starts the credential provider without asking whether the run is hosted")
	}
	for _, later := range []string{"InstallRead", "InstallJobRead"} {
		if offset, ok := calls[later]; !ok || offset < start {
			t.Errorf("installCredentialProviders calls %s before it starts the credential provider", later)
		}
	}
}

// callOffsets maps each function name that function calls in file to the
// offset of its first call.
func callOffsets(t *testing.T, file, function string) map[string]token.Pos {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]token.Pos{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				name = callee.Name
			case *ast.SelectorExpr:
				name = callee.Sel.Name
			}
			if _, seen := calls[name]; name != "" && !seen {
				calls[name] = call.Pos()
			}
			return true
		})
	}
	return calls
}
