package storage

import (
	"context"
	"strings"
	"testing"
)

// storageSectionYAML is a CONFIG_DATA document whose "storage" section carries
// the managed bindings document alongside a non-colliding operator-authored
// key, exactly as the control plane's resolve overlay produces it.
const storageSectionYAML = `
storage:
  operatorKey: keep-me
  protocolVersion: 1
  bindings:
    - name: uploads
      backend: memory
      bucket: cfg-provider-bucket
`

// TestBindingsFromConfigDoc_ParsesDocument proves the loose section view
// round-trips through the same strict decode + validation as the env
// transport.
func TestBindingsFromConfigDoc_ParsesDocument(t *testing.T) {
	doc := bindingsConfigDoc{
		ProtocolVersion: 1,
		Bindings: []any{
			map[string]any{"name": "uploads", "backend": "memory", "bucket": "b-uploads"},
		},
		Providers: map[string]any{"gcs": map[string]any{"projectId": "acme"}},
	}
	parsed, err := bindingsFromConfigDoc(doc)
	if err != nil {
		t.Fatalf("bindingsFromConfigDoc: %v", err)
	}
	if parsed == nil || len(parsed.Bindings) != 1 {
		t.Fatalf("parsed = %+v, want one binding", parsed)
	}
	if parsed.Bindings[0].Bucket != "b-uploads" {
		t.Errorf("bucket = %q, want b-uploads", parsed.Bindings[0].Bucket)
	}
	if parsed.Providers.GCS == nil || parsed.Providers.GCS.ProjectID != "acme" {
		t.Errorf("providers = %+v, want gcs projectId acme", parsed.Providers)
	}
}

// TestBindingsFromConfigDoc_NoDocumentFallsThrough returns (nil, nil) when the
// section carries no bindings key or an empty one, so the env transport stays
// in charge.
func TestBindingsFromConfigDoc_NoDocumentFallsThrough(t *testing.T) {
	for name, doc := range map[string]bindingsConfigDoc{
		"absent": {},
		"empty":  {ProtocolVersion: 1, Bindings: []any{}},
	} {
		parsed, err := bindingsFromConfigDoc(doc)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if parsed != nil {
			t.Errorf("%s: expected no document, got %+v", name, parsed)
		}
	}
}

// TestBindingsFromConfigDoc_MalformedDocumentErrors proves a section that
// carries a bindings key the contract rejects fails loudly with the shared
// validation diagnostics instead of silently degrading to the env transport.
func TestBindingsFromConfigDoc_MalformedDocumentErrors(t *testing.T) {
	for name, doc := range map[string]bindingsConfigDoc{
		"non-list bindings":       {ProtocolVersion: 1, Bindings: "not-a-list"},
		"missing protocolVersion": {Bindings: []any{map[string]any{"name": "uploads", "backend": "memory", "bucket": "b"}}},
		"unknown backend kind":    {ProtocolVersion: 1, Bindings: []any{map[string]any{"name": "uploads", "backend": "ftp", "bucket": "b"}}},
	} {
		if _, err := bindingsFromConfigDoc(doc); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestHasManagedBindings proves the transport-agnostic presence predicate: true
// when the "storage" config section carries a bindings document or
// STORAGE_BINDINGS is present, and — crucially — true for a present-but-
// malformed section so a misconfiguration never silently reads as "no managed
// storage" (which would degrade prod to a local/memory backend). Absent from
// both transports is the only false, keeping local serve/test on their local
// path.
func TestHasManagedBindings(t *testing.T) {
	const envBindingsJSON = `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`
	const malformedSection = `
storage:
  bindings:
    - name: uploads
      backend: memory
      bucket: b
`
	const emptyBindings = `
storage:
  protocolVersion: 1
  bindings: []
`
	for name, tc := range map[string]struct {
		config string
		env    string
		want   bool
	}{
		"config section carries document": {config: storageSectionYAML, want: true},
		"env fallback only":               {env: envBindingsJSON, want: true},
		"both transports present":         {config: storageSectionYAML, env: envBindingsJSON, want: true},
		"malformed section is present":    {config: malformedSection, want: true},
		"empty bindings falls to env":     {config: emptyBindings, env: envBindingsJSON, want: true},
		"empty section, no env":           {config: emptyBindings, want: false},
		"neither transport present":       {want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvBindings, tc.env)
			t.Setenv("CONFIG_DATA", tc.config)
			if got := HasManagedBindings(context.Background()); got != tc.want {
				t.Errorf("HasManagedBindings() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPluginConfigure_ConfigSectionWinsOverEnv proves the Backend the DI
// factory provides resolves logical buckets from the config-carried document
// even when STORAGE_BINDINGS points the same bucket elsewhere.
func TestPluginConfigure_ConfigSectionWinsOverEnv(t *testing.T) {
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`)
	t.Setenv("CONFIG_DATA", storageSectionYAML)
	p := NewPlugin()
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	backend, err := p.resolveBackend(context.Background())
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	bb, ok := backend.(*bindingBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bindingBackend", backend)
	}
	target, err := bb.resolve("uploads")
	if err != nil {
		t.Fatalf("resolve uploads: %v", err)
	}
	if target.bucket != "cfg-provider-bucket" {
		t.Errorf("bucket = %q, want the config-section binding cfg-provider-bucket", target.bucket)
	}
}

// TestResolveBackend_BeforeConfigureUsesConfigSection pins the lifecycle ordering
// hazard on the storage twin of the database pool-open bug: Backend is
// registered lazy so its factory usually runs after Configure, but nothing
// stops an eager consumer from resolving it first. The factory must resolve
// the config-carried document itself — lazily and memoized — rather than read
// a field only Configure populates; otherwise it silently falls back to the
// env transport and disagrees with HasManagedBindings.
func TestResolveBackend_BeforeConfigureUsesConfigSection(t *testing.T) {
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`)
	t.Setenv("CONFIG_DATA", storageSectionYAML)
	p := NewPlugin()

	// No Configure call: the resolution must not depend on it having run.
	backend, err := p.resolveBackend(context.Background())
	if err != nil {
		t.Fatalf("resolveBackend before Configure: %v", err)
	}
	bb, ok := backend.(*bindingBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bindingBackend", backend)
	}
	target, err := bb.resolve("uploads")
	if err != nil {
		t.Fatalf("resolve uploads: %v", err)
	}
	if target.bucket != "cfg-provider-bucket" {
		t.Errorf("bucket = %q, want the config-section binding cfg-provider-bucket", target.bucket)
	}
}

// TestResolveBackend_BeforeConfigureMalformedSectionFailsLoud proves a
// malformed config section fails the backend resolution loudly when Configure
// hasn't rejected it first, instead of silently degrading to the env
// transport.
func TestResolveBackend_BeforeConfigureMalformedSectionFailsLoud(t *testing.T) {
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`)
	t.Setenv("CONFIG_DATA", `
storage:
  bindings:
    - name: uploads
      backend: memory
      bucket: b
`)
	p := NewPlugin()
	_, err := p.resolveBackend(context.Background())
	if err == nil {
		t.Fatal("expected the malformed config-section document to fail the resolution")
	}
	if !strings.Contains(err.Error(), "protocolVersion") {
		t.Errorf("error %q should carry the shared protocolVersion diagnostic", err.Error())
	}
}

// TestPluginProvidesResetsConfigDocMemoBetweenLifecyclePasses proves the config
// document memo is scoped to one container build. A Validate or failed Start
// retry collects Provides again; that must re-read config instead of keeping a
// stale nil or error outcome from the previous pass.
func TestPluginProvidesResetsConfigDocMemoBetweenLifecyclePasses(t *testing.T) {
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`)
	p := NewPlugin()

	backend, err := p.resolveBackend(context.Background())
	if err != nil {
		t.Fatalf("resolveBackend with env fallback: %v", err)
	}
	bb, ok := backend.(*bindingBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bindingBackend", backend)
	}
	target, err := bb.resolve("uploads")
	if err != nil {
		t.Fatalf("resolve uploads: %v", err)
	}
	if target.bucket != "env-provider-bucket" {
		t.Fatalf("bucket = %q, want the env binding env-provider-bucket", target.bucket)
	}

	t.Setenv("CONFIG_DATA", `
storage:
  bindings:
    - name: uploads
      backend: memory
      bucket: b
`)
	_ = p.Provides()
	if _, err := p.resolveBackend(context.Background()); err == nil {
		t.Fatal("expected the next lifecycle pass to re-read and reject the malformed config section")
	} else if !strings.Contains(err.Error(), "protocolVersion") {
		t.Errorf("error %q should carry the shared protocolVersion diagnostic", err.Error())
	}

	t.Setenv("CONFIG_DATA", storageSectionYAML)
	_ = p.Provides()
	backend, err = p.resolveBackend(context.Background())
	if err != nil {
		t.Fatalf("resolveBackend after repaired config: %v", err)
	}
	bb, ok = backend.(*bindingBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bindingBackend", backend)
	}
	target, err = bb.resolve("uploads")
	if err != nil {
		t.Fatalf("resolve uploads after repaired config: %v", err)
	}
	if target.bucket != "cfg-provider-bucket" {
		t.Errorf("bucket = %q, want the repaired config-section binding cfg-provider-bucket", target.bucket)
	}
}

// TestPluginConfigure_NoConfigSectionKeepsEnvPath proves that with no managed
// document in config the env transport resolves exactly as before.
func TestPluginConfigure_NoConfigSectionKeepsEnvPath(t *testing.T) {
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"env-provider-bucket"}]}`)
	p := NewPlugin()
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}

	backend, err := p.resolveBackend(context.Background())
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	bb, ok := backend.(*bindingBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bindingBackend", backend)
	}
	target, err := bb.resolve("uploads")
	if err != nil {
		t.Fatalf("resolve uploads: %v", err)
	}
	if target.bucket != "env-provider-bucket" {
		t.Errorf("bucket = %q, want the env binding env-provider-bucket", target.bucket)
	}
}

// TestPluginConfigure_MalformedConfigSectionErrors surfaces a malformed managed
// document at startup with the same validation the env transport uses.
func TestPluginConfigure_MalformedConfigSectionErrors(t *testing.T) {
	t.Setenv("CONFIG_DATA", `
storage:
  bindings:
    - name: uploads
      backend: memory
      bucket: b
`)
	p := NewPlugin()
	err := p.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("expected configure to reject a document with no protocolVersion")
	}
	if !strings.Contains(err.Error(), "protocolVersion") {
		t.Errorf("error %q should carry the shared protocolVersion diagnostic", err.Error())
	}
}

// TestProvides_UnboundErrorNamesBothTransports keeps the fail-closed unbound
// diagnostic and points the operator at both injection transports.
func TestProvides_UnboundErrorNamesBothTransports(t *testing.T) {
	t.Setenv(EnvBindings, "")
	p := NewPlugin()
	_, err := p.provideBackend(nil)
	if err == nil {
		t.Fatal("expected the unbound error when neither transport carries bindings")
	}
	if !strings.Contains(err.Error(), EnvBindings) || !strings.Contains(err.Error(), ConfigSection) {
		t.Errorf("error %q should name both %s and the %q config section", err.Error(), EnvBindings, ConfigSection)
	}
}
