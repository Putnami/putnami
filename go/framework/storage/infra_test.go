package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/app"
	protocaps "go.putnami.dev/protocol/capabilities"
	"go.putnami.dev/protocol/infra"
)

func TestBuildInfraManifest(t *testing.T) {
	tests := []struct {
		name    string
		buckets []*BucketDefinition
		wantNil bool
		want    []infra.StorageBucket
	}{
		{
			name:    "zero buckets",
			wantNil: true,
		},
		{
			name:    "single bucket defaults to readwrite",
			buckets: []*BucketDefinition{{Name: "uploads"}},
			want:    []infra.StorageBucket{{Name: "uploads", Access: infra.StorageAccessReadWrite}},
		},
		{
			name: "explicit access and public are preserved",
			buckets: []*BucketDefinition{
				{Name: "public-assets", Options: BucketOptions{Access: AccessRead, Public: true}},
			},
			want: []infra.StorageBucket{{Name: "public-assets", Access: infra.StorageAccessRead, Public: true}},
		},
		{
			name: "bucket with retention",
			buckets: []*BucketDefinition{
				{Name: "audit-logs", Options: BucketOptions{Retention: "90d"}},
			},
			want: []infra.StorageBucket{{Name: "audit-logs", Access: infra.StorageAccessReadWrite, Retention: "90d"}},
		},
		{
			name: "multi bucket sorted by name carries access and public",
			buckets: []*BucketDefinition{
				{Name: "uploads"},
				{Name: "audit-logs", Options: BucketOptions{Access: AccessWrite, Retention: "30d"}},
				{Name: "avatars", Options: BucketOptions{Public: true}},
			},
			want: []infra.StorageBucket{
				{Name: "audit-logs", Access: infra.StorageAccessWrite, Retention: "30d"},
				{Name: "avatars", Access: infra.StorageAccessReadWrite, Public: true},
				{Name: "uploads", Access: infra.StorageAccessReadWrite},
			},
		},
		{
			name: "blank retention is omitted",
			buckets: []*BucketDefinition{
				{Name: "uploads", Options: BucketOptions{Retention: "   "}},
			},
			want: []infra.StorageBucket{{Name: "uploads", Access: infra.StorageAccessReadWrite}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, diags := buildInfraManifest(tt.buckets)
			if len(diags) != 0 {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if tt.wantNil {
				if m != nil {
					t.Fatalf("expected nil manifest, got %+v", m)
				}
				return
			}
			if m == nil {
				t.Fatal("expected manifest, got nil")
			}
			if m.ProtocolVersion != infra.ProtocolVersion {
				t.Errorf("protocolVersion = %d, want %d", m.ProtocolVersion, infra.ProtocolVersion)
			}
			if m.Schema != infra.PerProjectSchemaURL {
				t.Errorf("schema = %q, want %q", m.Schema, infra.PerProjectSchemaURL)
			}
			if !reflect.DeepEqual(m.Storage, tt.want) {
				t.Errorf("storage = %+v, want %+v", m.Storage, tt.want)
			}
		})
	}
}

func TestPluginDesignInfraRequirementsAreSortedNativeBuckets(t *testing.T) {
	registry := &Registry{buckets: make(map[string]*BucketDefinition)}
	registry.Register(&BucketDefinition{Name: "uploads"})
	registry.Register(&BucketDefinition{Name: "audit-logs"})
	registry.Register(&BucketDefinition{Name: "avatars"})

	requirements := (&Plugin{registry: registry}).DesignInfraRequirements()
	got := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		if requirement.Kind != protocaps.InfraKindStorage {
			t.Fatalf("infra kind = %q, want storage", requirement.Kind)
		}
		got = append(got, requirement.Name)
	}
	want := []string{"audit-logs", "avatars", "uploads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("storage requirements = %v, want %v", got, want)
	}
}

func TestBuildInfraManifestOmitsRetentionInJSON(t *testing.T) {
	m, diags := buildInfraManifest([]*BucketDefinition{{Name: "uploads"}})
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); strings.Contains(got, "retention") {
		t.Errorf("expected retention field to be omitted, got %s", got)
	}
}

func TestBuildInfraManifestInvalidBucketName(t *testing.T) {
	m, diags := buildInfraManifest([]*BucketDefinition{{Name: "Not A Valid Bucket"}})
	if m != nil {
		t.Fatalf("expected nil manifest for invalid bucket name, got %+v", m)
	}
	if len(diags) == 0 {
		t.Fatal("expected a diagnostic for the invalid bucket name")
	}
}

func TestPluginDescribeWritesSidecar(t *testing.T) {
	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	reg.Register(&BucketDefinition{Name: "audit-logs", Options: BucketOptions{Retention: "90d"}})
	reg.Register(&BucketDefinition{Name: "uploads"})
	plugin := &Plugin{registry: reg}

	out := t.TempDir()
	if err := plugin.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	path := infra.SidecarPathIn(out, "storage")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	var m infra.PerProjectManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal sidecar: %v", err)
	}
	want := []infra.StorageBucket{
		{Name: "audit-logs", Access: infra.StorageAccessReadWrite, Retention: "90d"},
		{Name: "uploads", Access: infra.StorageAccessReadWrite},
	}
	if !reflect.DeepEqual(m.Storage, want) {
		t.Errorf("storage = %+v, want %+v", m.Storage, want)
	}
}

func TestPluginDescribeNoBucketsWritesNothing(t *testing.T) {
	plugin := &Plugin{registry: &Registry{buckets: make(map[string]*BucketDefinition)}}
	out := t.TempDir()
	if err := plugin.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	path := infra.SidecarPathIn(out, "storage")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar, stat err = %v", err)
	}
}

func TestPluginDescribeDoesNotTouchOtherProducerSidecars(t *testing.T) {
	out := t.TempDir()
	// Each producer owns its own sidecar; storage must not read or write the
	// database producer's file even when both exist in the same project.
	databasePath := infra.SidecarPathIn(out, "database")
	if err := infra.WriteSidecarIn(out, "database", infra.PerProjectManifest{
		Databases: []infra.Database{{Name: "primary", Engine: infra.EnginePostgres}},
	}); err != nil {
		t.Fatal(err)
	}
	databaseBefore, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	reg.Register(&BucketDefinition{Name: "uploads"})
	if err := (&Plugin{registry: reg}).Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	databaseAfter, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("database sidecar disappeared: %v", err)
	}
	if !bytes.Equal(databaseBefore, databaseAfter) {
		t.Errorf("database sidecar mutated by storage Describe:\nbefore: %s\nafter:  %s", databaseBefore, databaseAfter)
	}
}

func TestPluginDescribeSkipsUnwantedTarget(t *testing.T) {
	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	reg.Register(&BucketDefinition{Name: "uploads"})
	plugin := &Plugin{registry: reg}

	out := t.TempDir()
	ctx := &app.DescribeContext{OutputDir: out, Targets: []string{"openapi"}}
	if err := plugin.Describe(ctx); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	path := infra.SidecarPathIn(out, "storage")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar when target not requested, stat err = %v", err)
	}
}

func TestPluginDescribeInvalidBucketReturnsError(t *testing.T) {
	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	reg.Register(&BucketDefinition{Name: "INVALID BUCKET"})
	plugin := &Plugin{registry: reg}
	if err := plugin.Describe(&app.DescribeContext{OutputDir: t.TempDir()}); err == nil {
		t.Fatal("expected error for invalid bucket name")
	}
}
