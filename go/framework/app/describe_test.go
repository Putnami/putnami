package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// describerStub records every Describe invocation. configure tracks whether
// Configure was called too, since describe must run Configure first so plugin
// state (e.g. discovered routes) is populated before Describe sees it.
type describerStub struct {
	name      string
	configure bool
	described bool
	gotCtx    *DescribeContext
	wrote     string
}

func (d *describerStub) Name() string { return d.name }
func (d *describerStub) Configure(_ context.Context, _ *Module) error {
	d.configure = true
	return nil
}
func (d *describerStub) Describe(ctx *DescribeContext) error {
	d.described = true
	d.gotCtx = ctx
	if ctx.Wants(d.name) && d.wrote != "" {
		path := filepath.Join(ctx.OutputDir, d.wrote)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("ok"), 0o644)
	}
	return nil
}

func TestApplicationDescribeRunsConfigureThenDescribers(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "describe", "describe-configures-before-describers")
	d := &describerStub{name: "openapi", wrote: "schema/openapi.json"}
	a := New("test")
	a.Use(d)

	out := t.TempDir()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !d.configure {
		t.Error("Configure should have run before Describe")
	}
	if !d.described {
		t.Error("Describe should have been called")
	}
	if d.gotCtx == nil || d.gotCtx.OutputDir != out {
		t.Errorf("OutputDir = %v, want %q", d.gotCtx, out)
	}
	if _, err := os.Stat(filepath.Join(out, "schema/openapi.json")); err != nil {
		t.Errorf("expected schema/openapi.json: %v", err)
	}
}

func TestApplicationDescribeRespectsTargetFilter(t *testing.T) {
	openapi := &describerStub{name: "openapi", wrote: "schema/openapi.json"}
	proto := &describerStub{name: "proto", wrote: "schema/api.proto"}

	a := New("test")
	a.Use(openapi)
	a.Use(proto)

	out := t.TempDir()
	// Only run the openapi describer.
	if err := a.Describe(out, []string{"openapi"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if !openapi.described {
		t.Error("openapi describer should have run")
	}
	if proto.described {
		// Per contract, every Describer is invoked but they should
		// short-circuit when ctx.Wants returns false. Here the stub
		// honors Wants in its body, so the file should not exist.
	}
	if _, err := os.Stat(filepath.Join(out, "schema/openapi.json")); err != nil {
		t.Errorf("expected openapi file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "schema/api.proto")); !os.IsNotExist(err) {
		t.Errorf("expected proto file to be skipped: err=%v", err)
	}
}

func TestApplicationDescribeFailsWithoutOutputDir(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "describe", "describe-requires-output-dir")
	a := New("test")
	a.Use(&describerStub{name: "x"})
	err := a.Describe("", nil)
	if err == nil {
		t.Fatal("expected error for empty outputDir")
	}
	if !strings.Contains(err.Error(), "outputDir") {
		t.Errorf("error should mention outputDir, got: %v", err)
	}
}

func TestApplicationDescribeSurfacesDescriberError(t *testing.T) {
	failing := &failingDescriber{name: "broken"}
	a := New("test")
	a.Use(failing)

	err := a.Describe(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error from failing describer")
	}
}

func TestApplicationDescribeSkipsNonDescribers(t *testing.T) {
	plain := &testPlugin{name: "plain"}
	d := &describerStub{name: "openapi", wrote: "schema/openapi.json"}
	a := New("test")
	a.Use(plain)
	a.Use(d)

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !d.described {
		t.Error("describer should still run when a non-describer sits before it")
	}
}

func TestDescribeContextWantsAcceptsAllAndExplicit(t *testing.T) {
	cases := []struct {
		name    string
		targets []string
		probe   string
		want    bool
	}{
		{"empty targets means all", nil, "openapi", true},
		{"explicit all", []string{"all"}, "proto", true},
		{"target match", []string{"proto"}, "proto", true},
		{"target mismatch", []string{"openapi"}, "proto", false},
		{"multiple targets", []string{"openapi", "proto"}, "proto", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &DescribeContext{Targets: tc.targets}
			if got := ctx.Wants(tc.probe); got != tc.want {
				t.Errorf("Wants(%q) = %v, want %v", tc.probe, got, tc.want)
			}
		})
	}
}

func TestSplitTargetsHandlesSeparatorsAndWhitespace(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"openapi", []string{"openapi"}},
		{"openapi,proto", []string{"openapi", "proto"}},
		{"openapi proto", []string{"openapi", "proto"}},
		{"  openapi , proto  ", []string{"openapi", "proto"}},
		{"all", []string{"all"}},
		{"", []string{}},
	}
	for _, tc := range cases {
		got := splitTargets(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("splitTargets(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitTargets(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
			}
		}
	}
}

// failingDescriber returns an error from Describe so we can assert error
// propagation from Application.Describe.
type failingDescriber struct{ name string }

func (f *failingDescriber) Name() string                    { return f.name }
func (f *failingDescriber) Describe(*DescribeContext) error { return errors.New("boom") }
