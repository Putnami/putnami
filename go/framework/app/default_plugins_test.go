package app

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// withDefaultPlugins runs the test with only the given factories registered
// and restores the process registry afterwards.
func withDefaultPlugins(t *testing.T, factories ...DefaultPluginFactory) {
	t.Helper()
	defaultPluginsMu.Lock()
	saved := defaultPlugins
	defaultPlugins = nil
	defaultPluginsMu.Unlock()
	t.Cleanup(func() {
		defaultPluginsMu.Lock()
		defaultPlugins = saved
		defaultPluginsMu.Unlock()
	})
	for _, f := range factories {
		RegisterDefaultPlugin(f)
	}
}

func TestNewStartsWithRegisteredDefaultPlugins(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "default-plugins", "new-adds-each-default-plugin-before-the-workloads-own")
	calls := 0
	withDefaultPlugins(t,
		func() Plugin { calls++; return &testPlugin{name: "first-default"} },
		func() Plugin { return &testPlugin{name: "second-default"} },
	)

	a := New("svc")
	a.Use(&testPlugin{name: "own"})

	plugins := a.CollectPlugins()
	names := make([]string, 0, len(plugins))
	for _, po := range plugins {
		names = append(names, po.Plugin.Name())
	}
	want := []string{"first-default", "second-default", "own"}
	if len(names) != len(want) {
		t.Fatalf("plugins = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("plugins = %v, want %v", names, want)
		}
	}

	New("other")
	if calls != 2 {
		t.Fatalf("the factory ran %d times for 2 applications, want 2", calls)
	}
}

func TestNewSkipsADefaultPluginThatDoesNotApply(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "default-plugins", "a-factory-that-returns-nil-adds-nothing")
	withDefaultPlugins(t, func() Plugin { return nil })
	RegisterDefaultPlugin(nil)

	if got := New("svc").CollectPlugins(); len(got) != 0 {
		t.Fatalf("an application with no applicable default plugin has %d plugins, want 0", len(got))
	}
}
