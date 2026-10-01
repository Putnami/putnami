package events

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/protocol/infra"
)

// TestPluginDescribeOnlyViaApp registers an events plugin with
// app.UseForDescribe and confirms its topic requirements are emitted during
// the describe phase. Because the plugin is never configured or started, no
// transport is selected and the global transport state is left untouched; the
// publish/subscribe topics come from the plugin's construction-time config.
func TestPluginDescribeOnlyViaApp(t *testing.T) {
	out := t.TempDir()
	a := app.New("svc")
	a.UseForDescribe(Events(PluginConfig{Publishes: []string{"order.created"}}))

	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	data, err := os.ReadFile(infra.SidecarPathIn(out, "events"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var m infra.PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if m.Events == nil || !reflect.DeepEqual(m.Events.Publishes, []string{"order.created"}) {
		t.Errorf("events = %+v, want publishes [order.created]", m.Events)
	}
}
