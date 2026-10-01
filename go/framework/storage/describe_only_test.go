package storage

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/protocol/infra"
)

// TestPluginDescribeOnlyViaApp registers a storage plugin with
// app.UseForDescribe and confirms its bucket requirements are emitted during
// the describe phase. Storage has no runtime lifecycle, so describe-only is
// simply the supported way to contribute its scratch fragment from a build
// that does not otherwise wire the plugin.
func TestPluginDescribeOnlyViaApp(t *testing.T) {
	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	reg.Register(&BucketDefinition{Name: "uploads"})

	out := t.TempDir()
	a := app.New("svc")
	a.UseForDescribe(&Plugin{registry: reg})

	if err := a.Describe(out, nil); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	data, err := os.ReadFile(infra.SidecarPathIn(out, "storage"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var m infra.PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	if !reflect.DeepEqual(m.Storage, []infra.StorageBucket{{Name: "uploads", Access: infra.StorageAccessReadWrite}}) {
		t.Errorf("storage = %+v, want [{uploads readwrite}]", m.Storage)
	}
}
