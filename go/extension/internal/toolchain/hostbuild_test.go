package toolchain

import (
	"reflect"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
)

func TestBuildArgsIsEmptyWhenNothingIsConfigured(t *testing.T) {
	if args := (HostBuild{}).BuildArgs(""); len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

func TestBuildArgsEmitsEveryConfiguredFlagInOrder(t *testing.T) {
	host := HostBuild{
		Mod: "mod", Gcflags: "gcf", Asmflags: "asmf", Tags: "tag1",
		Race: true, Trimpath: true, Installsuffix: "suffix", Parallelism: "4", Buildmode: "pie",
	}
	want := []string{
		"-mod", "mod",
		"-gcflags", "gcf",
		"-asmflags", "asmf",
		"-ldflags", "ldf",
		"-tags", "tag1",
		"-race",
		"-trimpath",
		"-installsuffix", "suffix",
		"-p", "4",
		"-buildmode", "pie",
	}
	if got := host.BuildArgs("ldf"); !reflect.DeepEqual(got, want) {
		t.Errorf("BuildArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// TestArgsNeverCarriesLdflags is the cache-sharing half of the shared
// configuration: -ldflags reaches the LINK action alone, so the describe build
// leaves it out and still shares every compile action with the stamped compile
// build. Carrying it would re-link the describe binary on every version bump
// for no change in what it describes.
func TestArgsNeverCarriesLdflags(t *testing.T) {
	host := HostBuild{Tags: "tag1"}
	args := host.Args()
	for _, arg := range args {
		if arg == "-ldflags" {
			t.Fatalf("Args carries -ldflags: %v", args)
		}
	}
	withLdflags := host.BuildArgs("-X main.Version=1.2.3")
	if len(withLdflags) != len(args)+2 {
		t.Errorf("BuildArgs = %v, want exactly Args (%v) plus -ldflags", withLdflags, args)
	}
}

func TestReadonlySelectsModReadonlyAndAnExplicitModWins(t *testing.T) {
	if got := (HostBuild{Readonly: true}).Args(); !reflect.DeepEqual(got, []string{"-mod", "readonly"}) {
		t.Errorf("args = %v, want [-mod readonly]", got)
	}
	if got := (HostBuild{Mod: "vendor", Readonly: true}).Args(); !reflect.DeepEqual(got, []string{"-mod", "vendor"}) {
		t.Errorf("args = %v, want the explicit mod to win", got)
	}
}

// TestResolveHostBuildReadsTheResolvedParameterBag is the one that makes the
// two steps agree in a real run. A task's argv is fixed by its manifest entry
// (`build --phase compile`, `build-describe`), so a command flag or a project
// option reaches the job as a resolved PARAMETER — and describe, which passes
// no argv flags at all, must read the same values the compile phase does.
func TestResolveHostBuildReadsTheResolvedParameterBag(t *testing.T) {
	ctx := &pctx.Context{Params: pctx.Params{
		"tags":      []byte(`"integration"`),
		"gcflags":   []byte(`"-N -l"`),
		"asmflags":  []byte(`"-D=x"`),
		"race":      []byte(`true`),
		"trimpath":  []byte(`true`),
		"buildmode": []byte(`"pie"`),
		"cgo":       []byte(`true`),
		"mod":       []byte(`"vendor"`),
		"p":         []byte(`4`),
	}}

	fromParams := ResolveHostBuild(ctx, nil)
	want := HostBuild{
		Mod: "vendor", Gcflags: "-N -l", Asmflags: "-D=x", Tags: "integration",
		Race: true, Trimpath: true, Parallelism: "4", Buildmode: "pie", CGO: "true",
	}
	if fromParams != want {
		t.Errorf("resolved = %+v\nwant     = %+v", fromParams, want)
	}

	// The compile phase resolves the same bag through the same helper, which is
	// what "describe and compile build the same program" means concretely.
	if describe, compile := ResolveHostBuild(ctx, nil), ResolveHostBuild(ctx, map[string]string{"phase": "compile"}); describe != compile {
		t.Errorf("describe resolved %+v but compile resolved %+v from one parameter bag", describe, compile)
	}
}

func TestArgvFlagsWinOverParameters(t *testing.T) {
	ctx := &pctx.Context{Params: pctx.Params{"tags": []byte(`"fromparams"`), "race": []byte(`true`)}}
	got := ResolveHostBuild(ctx, map[string]string{"tags": "fromargv", "race": "false"})
	if got.Tags != "fromargv" {
		t.Errorf("tags = %q, want the argv value", got.Tags)
	}
	if got.Race {
		t.Error("race = true, want the argv --no-race to win")
	}
}

// TestCGOIsTriState pins the difference between "say nothing" and "say false":
// an absent `cgo` leaves the host default, which is what lets this build's
// `net` be the object the next build reads.
func TestCGOIsTriState(t *testing.T) {
	absent := ResolveHostBuild(&pctx.Context{Params: pctx.Params{}}, nil)
	if absent.CGO != "" {
		t.Errorf("CGO = %q for an absent parameter, want the host default", absent.CGO)
	}
	if got := cgoEnabled(t, absent.Env([]string{"CGO_ENABLED=1"}, t.TempDir(), "")); got != "1" {
		t.Errorf("CGO_ENABLED = %q, want the inherited host value", got)
	}

	off := ResolveHostBuild(&pctx.Context{Params: pctx.Params{"cgo": []byte(`false`)}}, nil)
	if off.CGO != "false" {
		t.Fatalf("CGO = %q, want %q", off.CGO, "false")
	}
	if got := cgoEnabled(t, off.Env([]string{"CGO_ENABLED=1"}, t.TempDir(), "")); got != "0" {
		t.Errorf("CGO_ENABLED = %q, want cgo:false to force it off", got)
	}

	on := ResolveHostBuild(&pctx.Context{Params: pctx.Params{"cgo": []byte(`true`)}}, nil)
	if got := cgoEnabled(t, on.Env([]string{"CGO_ENABLED=0"}, t.TempDir(), "")); got != "1" {
		t.Errorf("CGO_ENABLED = %q, want cgo:true to force it on", got)
	}
}

// TestRaceForcesCGOOn repeats the rule `go test -race` applies, so a describe
// scheduled beside a race test builds what the test builds. `-race` with cgo
// off has no outcome other than `-race requires cgo`, so it wins over an
// explicit cgo:false.
func TestRaceForcesCGOOn(t *testing.T) {
	host := ResolveHostBuild(&pctx.Context{Params: pctx.Params{"race": []byte(`true`), "cgo": []byte(`false`)}}, nil)
	if got := cgoEnabled(t, host.Env([]string{"CGO_ENABLED=0"}, t.TempDir(), "")); got != "1" {
		t.Errorf("CGO_ENABLED = %q under -race, want 1", got)
	}
}

func TestHostBuildInvocationReturnsTheFlagsAndTheEnvironmentTogether(t *testing.T) {
	ctx := &pctx.Context{Params: pctx.Params{"tags": []byte(`"integration"`), "cgo": []byte(`false`)}}
	args, env := HostBuildInvocation(ctx, nil, []string{"CGO_ENABLED=1"}, t.TempDir(), "")
	if !reflect.DeepEqual(args, []string{"-tags", "integration"}) {
		t.Errorf("args = %v, want [-tags integration]", args)
	}
	if got := cgoEnabled(t, env); got != "0" {
		t.Errorf("CGO_ENABLED = %q, want 0", got)
	}
}

func cgoEnabled(t *testing.T, env []string) string {
	t.Helper()
	value := ""
	for _, entry := range env {
		if after, ok := strings.CutPrefix(entry, "CGO_ENABLED="); ok {
			value = after
		}
	}
	return value
}

// TestHostBuildParamNamesCoversEveryReadParameter keeps the exported list and
// the resolver from drifting apart, in both directions.
//
// The list is what the Go extension's manifest contract test walks to decide
// which parameters a build task must pin or declare, so a name missing from it
// is a parameter that changes a build nobody keys on, and a name that
// no longer resolves anything is a declaration nobody needs.
//
// Forward: setting one parameter alone must move the resolved configuration.
// Backward: every field of HostBuild must be reachable — a new field with no
// name in the list would resolve to its zero value forever.
func TestHostBuildParamNamesCoversEveryReadParameter(t *testing.T) {
	// One value per name that is non-zero for that name's type, so "the
	// configuration moved" is unambiguous.
	values := map[string][]byte{
		"mod":           []byte(`"vendor"`),
		"readonly":      []byte(`true`),
		"gcflags":       []byte(`"-N -l"`),
		"asmflags":      []byte(`"-D=x"`),
		"tags":          []byte(`"integration"`),
		"race":          []byte(`true`),
		"trimpath":      []byte(`true`),
		"installsuffix": []byte(`"suffix"`),
		"p":             []byte(`4`),
		"buildmode":     []byte(`"pie"`),
		"cgo":           []byte(`true`),
	}

	for _, name := range HostBuildParamNames {
		raw, ok := values[name]
		if !ok {
			t.Errorf("no probe value for declared parameter %q; add one so this test can prove it is read", name)
			continue
		}
		resolved := ResolveHostBuild(&pctx.Context{Params: pctx.Params{name: raw}}, nil)
		if resolved == (HostBuild{}) {
			t.Errorf("parameter %q resolved to the zero configuration; HostBuildParamNames names a parameter "+
				"ResolveHostBuild does not read", name)
		}
	}

	if got, want := len(HostBuildParamNames), reflect.TypeOf(HostBuild{}).NumField(); got != want {
		t.Errorf("HostBuildParamNames has %d names for %d HostBuild fields; a field with no parameter name "+
			"resolves to its zero value forever, and a name with no field is never read", got, want)
	}

	// The whole bag at once must fill every field: a name silently shadowed by
	// another would show up here as a zero field.
	all := pctx.Params{}
	for name, raw := range values {
		all[name] = raw
	}
	resolved := reflect.ValueOf(ResolveHostBuild(&pctx.Context{Params: all}, nil))
	for i := range resolved.NumField() {
		if resolved.Field(i).IsZero() {
			t.Errorf("field %s is zero with every parameter set; it has no working parameter name",
				resolved.Type().Field(i).Name)
		}
	}
}
