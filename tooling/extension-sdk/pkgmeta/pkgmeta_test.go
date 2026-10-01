package pkgmeta

import (
	"path/filepath"
	"testing"
)

func TestHasChannel(t *testing.T) {
	tests := []struct {
		name     string
		channels []string
		query    string
		want     bool
	}{
		{"found", []string{"npm", "docker"}, "npm", true},
		{"not found", []string{"npm", "docker"}, "go", false},
		{"empty channels", nil, "npm", false},
		{"empty query", []string{"npm"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &PackageMetadata{Channels: tt.channels}
			got := m.HasChannel(tt.query)
			if got != tt.want {
				t.Errorf("HasChannel(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestPackageOutputDir(t *testing.T) {
	tests := []struct {
		name    string
		wsRoot  string
		project string
		channel string
		want    string
	}{
		{"basic", "/workspace", "my-project", "npm", filepath.FromSlash("/workspace/.putnami/out/my-project/package/npm")},
		{"docker channel", "/ws", "go.putnami.dev/tooling/cli", "docker", filepath.FromSlash("/ws/.putnami/out/go.putnami.dev/tooling/cli/package/docker")},
		{"archives channel", "/ws", "app", "archives", filepath.FromSlash("/ws/.putnami/out/app/package/archives")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PackageOutputDir(tt.wsRoot, tt.project, tt.channel)
			if got != tt.want {
				t.Errorf("PackageOutputDir(%q, %q, %q) = %q, want %q", tt.wsRoot, tt.project, tt.channel, got, tt.want)
			}
		})
	}
}

func TestMetadataPath(t *testing.T) {
	got := MetadataPath("/workspace", "my-project")
	want := filepath.FromSlash("/workspace/.putnami/out/my-project/package/metadata.json")
	if got != want {
		t.Errorf("MetadataPath() = %q, want %q", got, want)
	}
}
