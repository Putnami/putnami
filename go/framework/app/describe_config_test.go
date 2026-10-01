package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/config"
	protocfg "go.putnami.dev/protocol/config"
)

// configContribPlugin is a plugin that owns config blocks, used to exercise the
// describe-phase config aggregation.
type configContribPlugin struct {
	name string
	defs []config.Descriptor
}

func (p *configContribPlugin) Name() string                           { return p.name }
func (p *configContribPlugin) ConfigDefinitions() []config.Descriptor { return p.defs }

// plainDescribePlugin owns no config — proves Collect[ConfigContributor] skips
// non-contributors.
type plainDescribePlugin struct{ name string }

func (p *plainDescribePlugin) Name() string { return p.name }

func readConfigDeps(t *testing.T, outDir string) []protocfg.Block {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outDir, "config-deps.json"))
	if err != nil {
		t.Fatalf("read config-deps.json: %v", err)
	}
	var doc struct {
		Blocks []protocfg.Block `json:"blocks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal config-deps.json: %v\n%s", err, data)
	}
	return doc.Blocks
}

func findConfigField(fields []protocfg.FieldSchema, name string) (protocfg.FieldSchema, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f, true
		}
	}
	return protocfg.FieldSchema{}, false
}

func TestDescribeConfig_AggregatesContributorBlocks(t *testing.T) {
	type TLS struct {
		CertFile string `json:"certFile"`
		KeyFile  string `json:"keyFile" sensitive:"true"`
	}
	type CoreOptions struct {
		Host    string            `json:"host" default:"localhost"`
		Port    int               `json:"port" env:"CORE_PORT" validate:"required"`
		Timeout time.Duration     `json:"timeout"`
		TLS     TLS               `json:"tls"`
		Tags    []string          `json:"tags"`
		Labels  map[string]string `json:"labels"`
		Ignored string            `json:"-"`
	}
	core := config.Config[CoreOptions]("core")

	a := New("describe-config")
	a.Use(&plainDescribePlugin{name: "plain"})
	a.Use(&configContribPlugin{name: "core-lib", defs: []config.Descriptor{core.Descriptor()}})

	out := t.TempDir()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	blocks := readConfigDeps(t, out)
	if len(blocks) != 1 || blocks[0].Path != "core" {
		t.Fatalf("expected a single core block, got %+v", blocks)
	}
	fields := blocks[0].Fields

	if host, ok := findConfigField(fields, "host"); !ok || host.Default != "localhost" {
		t.Errorf("host = %+v (ok=%v), want default localhost", host, ok)
	}
	if port, ok := findConfigField(fields, "port"); !ok || port.Type != protocfg.FieldTypeInt || port.Env != "CORE_PORT" || !port.Required {
		t.Errorf("port = %+v (ok=%v), want int/env CORE_PORT/required", port, ok)
	}
	if to, ok := findConfigField(fields, "timeout"); !ok || to.Type != protocfg.FieldTypeDuration {
		t.Errorf("timeout = %+v (ok=%v), want duration", to, ok)
	}
	tls, ok := findConfigField(fields, "tls")
	if !ok || tls.Type != protocfg.FieldTypeObject {
		t.Fatalf("tls = %+v (ok=%v), want object", tls, ok)
	}
	if key, ok := findConfigField(tls.Fields, "keyFile"); !ok || !key.Sensitive {
		t.Errorf("tls.keyFile = %+v (ok=%v), want sensitive", key, ok)
	}
	if tags, ok := findConfigField(fields, "tags"); !ok || tags.Type != protocfg.FieldTypeArray || tags.Items == nil || tags.Items.Type != protocfg.FieldTypeString {
		t.Errorf("tags = %+v (ok=%v), want array<string>", tags, ok)
	}
	if labels, ok := findConfigField(fields, "labels"); !ok || labels.Type != protocfg.FieldTypeMap || labels.Keys != protocfg.FieldTypeString || labels.Values == nil {
		t.Errorf("labels = %+v (ok=%v), want map[string]…", labels, ok)
	}
	if _, ok := findConfigField(fields, "Ignored"); ok {
		t.Error(`field tagged json:"-" should be excluded`)
	}
}

func TestDescribeConfig_NoContributorsWritesNothing(t *testing.T) {
	a := New("no-config")
	a.Use(&plainDescribePlugin{name: "plain"})

	out := t.TempDir()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "config-deps.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no config-deps.json, stat err = %v", err)
	}
}

func TestDescribeConfig_DuplicatePathErrors(t *testing.T) {
	type A struct {
		X string `json:"x"`
	}
	type B struct {
		Y string `json:"y"`
	}
	a := New("dup")
	a.Use(&configContribPlugin{name: "lib-a", defs: []config.Descriptor{config.Config[A]("core").Descriptor()}})
	a.Use(&configContribPlugin{name: "lib-b", defs: []config.Descriptor{config.Config[B]("core").Descriptor()}})

	err := a.Describe(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected duplicate-path error")
	}
	if !strings.Contains(err.Error(), "core") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error should name the duplicate path: %v", err)
	}
}

func TestDescribeConfig_NonPrimitiveMapKeyErrors(t *testing.T) {
	type Key struct {
		A string `json:"a"`
	}
	type Opts struct {
		M map[Key]string `json:"m"`
	}
	a := New("badmap")
	a.Use(&configContribPlugin{name: "lib", defs: []config.Descriptor{config.Config[Opts]("bad").Descriptor()}})

	err := a.Describe(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected non-primitive map key error")
	}
	if !strings.Contains(err.Error(), "map key") {
		t.Fatalf("error should mention map key: %v", err)
	}
}
