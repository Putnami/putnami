package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// countingDescriber records how many times Describe ran and whether Configure
// was ever called, so tests can assert a describe-only plugin describes
// without being drawn into the runtime lifecycle. It implements Configurer on
// purpose: if UseForDescribe ever leaked the plugin into the plugin slice, the
// configure phase would flip configured and the assertions would catch it.
type countingDescriber struct {
	name          string
	describeCount int
	configured    bool
}

func (d *countingDescriber) Name() string { return d.name }

func (d *countingDescriber) Configure(context.Context, *Module) error {
	d.configured = true
	return nil
}

func (d *countingDescriber) Describe(ctx *DescribeContext) error {
	if !ctx.Wants(d.name) {
		return nil
	}
	d.describeCount++
	return nil
}

func TestUseForDescribeRunsDescribeWithoutConfigure(t *testing.T) {
	d := &countingDescriber{name: "database"}
	a := New("test")
	a.UseForDescribe(d)

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.describeCount != 1 {
		t.Errorf("describeCount = %d, want 1", d.describeCount)
	}
	if d.configured {
		t.Error("describe-only plugin must not be configured")
	}
}

func TestUseForDescribeExcludedFromRuntimeLifecycle(t *testing.T) {
	d := &countingDescriber{name: "database"}
	a := New("test")
	a.UseForDescribe(d)

	if got := len(a.CollectPlugins()); got != 0 {
		t.Errorf("CollectPlugins() = %d, want 0 (describe-only is not a runtime plugin)", got)
	}

	// A full validate pass runs PreConfigure → build → Configure but must not
	// reach a describe-only plugin.
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if d.configured {
		t.Error("describe-only plugin must not be configured during the runtime lifecycle")
	}
	if d.describeCount != 0 {
		t.Error("describe-only plugin must not describe outside the describe phase")
	}
}

func TestUseForDescribeIsolatedFromCapabilityDiscovery(t *testing.T) {
	d := &countingDescriber{name: "database"}
	a := New("test")
	a.UseForDescribe(d)

	// Collect[Describer] backs every capability walk (health probes, migration
	// sources, …). A describe-only contributor must be invisible to it, or its
	// runtime capabilities would leak into endpoints it never joined.
	if got := Collect[Describer](a.Module); len(got) != 0 {
		t.Errorf("Collect[Describer] = %d, want 0 for a describe-only plugin", len(got))
	}

	only := a.collectDescribeOnly()
	if len(only) != 1 || only[0] != d {
		t.Errorf("collectDescribeOnly() = %v, want [%v]", only, d)
	}
}

func TestUseForDescribeRunsAlongsideRuntimeDescribers(t *testing.T) {
	runtime := &describerStub{name: "openapi", wrote: "schema/openapi.json"}
	only := &countingDescriber{name: "database"}
	a := New("test")
	a.Use(runtime)
	a.UseForDescribe(only)

	out := t.TempDir()
	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !runtime.configure || !runtime.described {
		t.Error("runtime describer should be configured and described")
	}
	if only.describeCount != 1 {
		t.Errorf("describe-only describeCount = %d, want 1", only.describeCount)
	}
	if only.configured {
		t.Error("describe-only plugin must not be configured")
	}
	if _, err := os.Stat(filepath.Join(out, "schema/openapi.json")); err != nil {
		t.Errorf("expected openapi artifact: %v", err)
	}
}

func TestUseForDescribeRespectsTargetFilter(t *testing.T) {
	d := &countingDescriber{name: "database"}
	a := New("test")
	a.UseForDescribe(d)

	if err := a.Describe(t.TempDir(), []string{"openapi"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.describeCount != 0 {
		t.Errorf("describeCount = %d, want 0 when the target filter excludes it", d.describeCount)
	}
}

func TestUseForDescribeDedupesPluginAlsoRegisteredViaUse(t *testing.T) {
	d := &countingDescriber{name: "database"}
	a := New("test")
	a.Use(d)            // full lifecycle, also discovered as a Describer
	a.UseForDescribe(d) // and flagged describe-only

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.describeCount != 1 {
		t.Errorf("describeCount = %d, want 1 (deduped across Use + UseForDescribe)", d.describeCount)
	}
	if !d.configured {
		t.Error("plugin registered via Use should still be configured")
	}
}

func TestUseForDescribeCollectedFromSubModules(t *testing.T) {
	d := &countingDescriber{name: "database"}
	child := NewModule("child")
	child.UseForDescribe(d)

	a := New("test")
	a.Use(child)

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.describeCount != 1 {
		t.Errorf("describeCount = %d, want 1 (describe-only on a sub-module)", d.describeCount)
	}
}

func TestUseForDescribeNilIsNoop(t *testing.T) {
	a := New("test")
	a.UseForDescribe(nil)
	if got := len(a.collectDescribeOnly()); got != 0 {
		t.Errorf("collectDescribeOnly() = %d, want 0 after a nil registration", got)
	}
	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}
}

type emptyDescriberA struct{}

func (*emptyDescriberA) Name() string                    { return "a" }
func (*emptyDescriberA) Describe(*DescribeContext) error { return nil }

type emptyDescriberB struct{}

func (*emptyDescriberB) Name() string                    { return "b" }
func (*emptyDescriberB) Describe(*DescribeContext) error { return nil }

func TestDescribersKeepDistinctZeroSizeDescribers(t *testing.T) {
	a, b := &emptyDescriberA{}, &emptyDescriberB{}
	if !sameAddress(a, b) {
		t.Skip("the runtime gave the two zero-size values distinct addresses; the shared-address case does not arise")
	}
	application := New("test")
	application.Use(a)
	application.UseForDescribe(b)

	got := application.describers()
	if len(got) != 2 || got[0].Name() != "a" || got[1].Name() != "b" {
		names := make([]string, len(got))
		for i, d := range got {
			names[i] = d.Name()
		}
		t.Errorf("describers() = %v, want [a b]: a describe-only describer that shares an address with a runtime describer was dropped", names)
	}
}
