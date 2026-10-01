package platform

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestVersionFromGenerated_FullPayload verifies the generated .gen/version.json
// shape (name/version/sha/branch/buildTime/isDirty) is mapped into VersionInfo,
// surfacing exactly the fields runtime/debug.ReadBuildInfo cannot supply.
func TestVersionFromGenerated_FullPayload(t *testing.T) {
	const data = `{
		"name": "my-service",
		"version": "0.1.0-abc1234",
		"suffix": "abc1234",
		"sha": "abc1234def5678",
		"branch": "feature/foo",
		"isDirty": false,
		"buildTime": "2026-06-21T12:00:00Z"
	}`
	got, err := VersionFromGenerated([]byte(data))
	if err != nil {
		t.Fatalf("VersionFromGenerated: %v", err)
	}
	want := VersionInfo{
		Name:      "my-service",
		Version:   "0.1.0-abc1234",
		SHA:       "abc1234def5678",
		Branch:    "feature/foo",
		BuildTime: "2026-06-21T12:00:00Z",
	}
	if got != want {
		t.Errorf("VersionFromGenerated = %+v, want %+v", got, want)
	}
}

// TestVersionFromGenerated_DirtyMarksSHA verifies a dirty build is surfaced on
// the wire (which has no boolean dirty field) by appending "+dirty" to the SHA.
func TestVersionFromGenerated_DirtyMarksSHA(t *testing.T) {
	const data = `{"name":"svc","version":"0.0.0-abc-deadbee","sha":"abc1234","branch":"main","isDirty":true,"buildTime":"2026-06-21T12:00:00Z"}`
	got, err := VersionFromGenerated([]byte(data))
	if err != nil {
		t.Fatalf("VersionFromGenerated: %v", err)
	}
	if got.SHA != "abc1234+dirty" {
		t.Errorf("dirty SHA = %q, want %q", got.SHA, "abc1234+dirty")
	}
	// The generated version string already encodes dirtiness and must pass
	// through untouched.
	if got.Version != "0.0.0-abc-deadbee" {
		t.Errorf("Version = %q, want it unchanged", got.Version)
	}
	if got.Branch != "main" {
		t.Errorf("Branch = %q, want main", got.Branch)
	}
}

// TestVersionFromGenerated_DirtyWithoutSHA verifies the "+dirty" marker is only
// appended when a SHA is present (no bare "+dirty" with an empty commit).
func TestVersionFromGenerated_DirtyWithoutSHA(t *testing.T) {
	const data = `{"name":"svc","version":"0.0.0","isDirty":true}`
	got, err := VersionFromGenerated([]byte(data))
	if err != nil {
		t.Fatalf("VersionFromGenerated: %v", err)
	}
	if got.SHA != "" {
		t.Errorf("SHA = %q, want empty (no bare +dirty marker)", got.SHA)
	}
}

// TestVersionFromGenerated_InvalidJSON verifies malformed bytes are rejected
// with the dedicated error code rather than silently yielding an empty struct.
func TestVersionFromGenerated_InvalidJSON(t *testing.T) {
	_, err := VersionFromGenerated([]byte("not json"))
	if err == nil {
		t.Fatal("expected an error for malformed version metadata, got nil")
	}
}

// TestVersionFromGenerated_FeedsResolveVersion is the integration check: a
// VersionInfo built from generated data, passed through resolveVersion as
// Config.Version would be, retains branch/buildTime (caller fields win over
// any runtime/debug fallback).
func TestVersionFromGenerated_FeedsResolveVersion(t *testing.T) {
	const data = `{"name":"svc","version":"1.2.3","sha":"cafe","branch":"release/9","buildTime":"2026-06-21T00:00:00Z"}`
	info, err := VersionFromGenerated([]byte(data))
	if err != nil {
		t.Fatalf("VersionFromGenerated: %v", err)
	}
	resolved := resolveVersion(info)
	if resolved.Branch != "release/9" {
		t.Errorf("Branch = %q, want release/9 (caller field must win)", resolved.Branch)
	}
	if resolved.BuildTime != "2026-06-21T00:00:00Z" {
		t.Errorf("BuildTime = %q, want the generated value", resolved.BuildTime)
	}
	if resolved.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", resolved.Version)
	}
}

// TestMergeBuildInfo verifies the precedence contract of mergeBuildInfo:
// caller-supplied (configured) fields always win over derived (build-info)
// values; an empty configured field is filled from derived; both-empty
// fields stay empty.
func TestMergeBuildInfo(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "version-fallback", "an-empty-configured-field-falls-back-to-the-embedded-build-information")
	cases := []struct {
		name       string
		configured VersionInfo
		derived    VersionInfo
		want       VersionInfo
	}{
		{
			name: "configured wins when all fields set",
			configured: VersionInfo{
				Name:      "my-service",
				Version:   "1.0.0",
				SHA:       "abc123",
				BuildTime: "2026-01-01T00:00:00Z",
			},
			derived: VersionInfo{
				Name:      "derived-name",
				Version:   "9.9.9",
				SHA:       "fff000",
				BuildTime: "2020-06-15T12:00:00Z",
			},
			want: VersionInfo{
				Name:      "my-service",
				Version:   "1.0.0",
				SHA:       "abc123",
				BuildTime: "2026-01-01T00:00:00Z",
			},
		},
		{
			name:       "empty configured is filled from derived",
			configured: VersionInfo{},
			derived: VersionInfo{
				Name:      "go.putnami.dev/platform",
				Version:   "v0.1.2",
				SHA:       "deadbeef",
				BuildTime: "2025-11-01T08:30:00Z",
			},
			want: VersionInfo{
				Name:      "go.putnami.dev/platform",
				Version:   "v0.1.2",
				SHA:       "deadbeef",
				BuildTime: "2025-11-01T08:30:00Z",
			},
		},
		{
			name: "both empty stays empty",
			want: VersionInfo{},
		},
		{
			name: "partial configured — only Name set, rest filled from derived",
			configured: VersionInfo{
				Name: "my-service",
			},
			derived: VersionInfo{
				Name:      "should-not-win",
				Version:   "v2.3.4",
				SHA:       "c0ffee",
				BuildTime: "2024-03-10T10:00:00Z",
			},
			want: VersionInfo{
				Name:      "my-service",
				Version:   "v2.3.4",
				SHA:       "c0ffee",
				BuildTime: "2024-03-10T10:00:00Z",
			},
		},
		{
			name: "configured SHA and BuildTime win; Name and Version fall back",
			configured: VersionInfo{
				SHA:       "explicit-sha",
				BuildTime: "explicit-time",
			},
			derived: VersionInfo{
				Name:      "derived-name",
				Version:   "v1.0.0",
				SHA:       "derived-sha",
				BuildTime: "derived-time",
			},
			want: VersionInfo{
				Name:      "derived-name",
				Version:   "v1.0.0",
				SHA:       "explicit-sha",
				BuildTime: "explicit-time",
			},
		},
		{
			name: "Branch field is preserved unchanged (not populated from derived)",
			configured: VersionInfo{
				Branch: "feature/foo",
			},
			derived: VersionInfo{
				Name: "derived-name",
			},
			want: VersionInfo{
				Name:   "derived-name",
				Branch: "feature/foo",
			},
		},
		{
			name: "derived with empty fields does not overwrite configured",
			configured: VersionInfo{
				Name:      "my-service",
				Version:   "v1.0.0",
				SHA:       "abc",
				BuildTime: "2026-01-01T00:00:00Z",
			},
			derived: VersionInfo{},
			want: VersionInfo{
				Name:      "my-service",
				Version:   "v1.0.0",
				SHA:       "abc",
				BuildTime: "2026-01-01T00:00:00Z",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeBuildInfo(c.configured, c.derived)
			if got != c.want {
				t.Errorf("mergeBuildInfo(%+v, %+v) = %+v, want %+v",
					c.configured, c.derived, got, c.want)
			}
		})
	}
}
