package proto

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	phttp "go.putnami.dev/http"

	"go.putnami.dev/protocol/features/spectest"
)

// configureWithRoutes seeds the plugin with a generated proto document,
// bypassing the api.Plugin → Configure path so describe-mode tests don't
// have to wire the full api orchestrator.
func configureWithRoutes(t *testing.T, p *Plugin, routes []api.DiscoveredRoute) {
	t.Helper()

	doc := Generate(routes, Options{PackageName: p.opts.PackageName, GoPackage: p.opts.GoPackage})
	p.mu.Lock()
	p.doc = &doc
	p.mu.Unlock()
}

func TestDescribeWritesProtoFile(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "describe-writes-the-proto-document-below-the-output-directory")
	p := NewPlugin(PluginOptions{PackageName: "test.v1"})
	configureWithRoutes(t, p, []api.DiscoveredRoute{
		{Method: "GET", Path: "/users"},
		{Method: "POST", Path: "/users", BodySchema: nil},
	})

	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(out, DefaultOutputPath))
	if err != nil {
		t.Fatalf("read proto: %v", err)
	}
	for _, want := range []string{
		`syntax = "proto3";`,
		"package test.v1;",
		"service ApiService {",
		"rpc ListUsers(ListUsersRequest) returns (ListUsersReply);",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("expected proto to contain %q, got:\n%s", want, body)
		}
	}
}

func TestDescribeShortCircuitsWhenNotTargeted(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "the-proto-describe-short-circuits-when-the-project-is-not-targeted")
	p := NewPlugin(PluginOptions{})
	configureWithRoutes(t, p, []api.DiscoveredRoute{
		{Method: "GET", Path: "/things"},
	})

	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"openapi"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, DefaultOutputPath)); !os.IsNotExist(err) {
		t.Errorf("expected no proto when target=openapi, got err=%v", err)
	}
}

func TestDescribeIsNoopBeforeConfigure(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "the-proto-plugin-writes-nothing-before-configure")
	// doc is nil because Configure didn't run — Describe must not panic
	// or fail; describe-mode partial configurations should degrade gracefully.
	p := NewPlugin(PluginOptions{})
	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, DefaultOutputPath)); !os.IsNotExist(err) {
		t.Errorf("expected no file when doc is nil, got err=%v", err)
	}
}

// TestConfigureNeverWritesToDisk protects the "contract artifacts are build
// outputs" requirement: a served workload must not write into its working
// directory, so it cannot fail to start because that directory is read-only.
// Both describe and normal runtime mode take the same Configure path.
func TestConfigureNeverWritesToDisk(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "configuring-the-proto-plugin-never-writes-to-disk")
	for _, describeMode := range []string{"", "all"} {
		t.Run("describe="+describeMode, func(t *testing.T) {
			t.Setenv("PUTNAMI_DESCRIBE", describeMode)

			tmp := t.TempDir()
			out := filepath.Join(tmp, "should-not-exist.proto")
			p := NewPlugin(PluginOptions{Output: out})

			if err := p.Configure(context.Background(), nil); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("Configure must not write the proto artifact, got err=%v", err)
			}
			if p.Document() == nil {
				t.Fatal("Configure must still render the document in memory")
			}
		})
	}
}

// TestWriteToExportsDocument covers the explicit export escape hatch that
// replaces the removed startup write: an empty path falls back to
// PluginOptions.Output.
func TestWriteToExportsDocument(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "nested", "api.proto")
	p := NewPlugin(PluginOptions{PackageName: "test.v1", Output: out})

	if err := p.WriteTo(""); err != nil {
		t.Fatalf("WriteTo before Configure: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("WriteTo must be a no-op before Configure, got err=%v", err)
	}

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.WriteTo(""); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	body, err := os.ReadFile(out) //nolint:gosec // path is test-owned
	if err != nil {
		t.Fatalf("read proto: %v", err)
	}
	if !strings.Contains(string(body), "package test.v1;") {
		t.Errorf("expected rendered document at %s, got:\n%s", out, body)
	}
}

// A provider whose declaration proto3 cannot carry must not start with a
// descriptor that misdescribes it.
func TestConfigureFailsOnADeclarationProto3CannotCarry(t *testing.T) {
	apiPlugin := api.New(protoFakeServer{})
	apiPlugin.Register(api.Endpoint("POST", "/things").
		Body(api.Type[struct {
			Counts map[int]string `json:"counts"`
		}]()).
		HandleRaw(func(_ *phttp.Context) *phttp.Response { return phttp.JSON(nil) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}

	p := NewPlugin(PluginOptions{PackageName: "test.v1"}).From(apiPlugin)
	err := p.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("Configure accepted a declaration proto3 cannot carry")
	}
	if p.Document() != nil {
		t.Errorf("a refused document must not be published: %#v", p.Document())
	}
}

// TestDescribeRemovesAStaleDescriptorWhenNothingIsGenerated pins the removal an
// incremental build depends on.
//
// The Go extension declares <project>/.gen/schema as build-describe's cached
// output and captures it from DISK — the scheduler walks the declared directory,
// it does not read the job's artifact list. A descriptor the current sources no
// longer produce would be recorded under this run's key and restored by every
// later hit on it, while a clean-tree build of the same sources produced an entry
// without it.
func TestDescribeRemovesAStaleDescriptorWhenNothingIsGenerated(t *testing.T) {
	out := t.TempDir()
	stale := filepath.Join(out, DefaultOutputPath)
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("syntax = \"proto3\";"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewPlugin(PluginOptions{}) // never configured, so doc is nil
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a descriptor this run did not produce survived: %v", err)
	}
}

// A run that is not targeted at the proto descriptor is not authoritative for
// it, so it must leave an existing one exactly where it is.
func TestDescribeKeepsTheDescriptorWhenNotTargeted(t *testing.T) {
	out := t.TempDir()
	kept := filepath.Join(out, DefaultOutputPath)
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("syntax = \"proto3\";"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewPlugin(PluginOptions{})
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"openapi"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("an untargeted run removed a descriptor it says nothing about: %v", err)
	}
}
