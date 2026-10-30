package oci

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestWrapAuthError(t *testing.T) {
	authErr := errors.New("GET https://oci.example/v2/: 401 Unauthorized")
	other := errors.New("connection refused")

	if got := WrapAuthError(authErr, "run cloud login"); !strings.Contains(got.Error(), "run cloud login") {
		t.Errorf("auth error + hint should wrap with the hint, got %v", got)
	}
	if got := WrapAuthError(authErr, ""); !errors.Is(got, authErr) {
		t.Errorf("no hint → error unchanged, got %v", got)
	}
	if got := WrapAuthError(other, "run cloud login"); !errors.Is(got, other) {
		t.Errorf("non-auth error → unchanged regardless of hint, got %v", got)
	}
	if got := WrapAuthError(nil, "run cloud login"); got != nil {
		t.Errorf("nil error → nil, got %v", got)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"401", "GET https://oci.example/v2/: 401 Unauthorized", true},
		{"unauthorized word", "request unauthorized", true},
		{"invalid credentials", "invalid credentials for registry", true},
		{"empty bearer token", "Failed to push oci.putnami.dev/x:c-1: no token in bearer response:\n{\"token\":\"\"}", true},
		{"unrelated", "connection refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthError(errors.New(tc.msg)); got != tc.want {
				t.Errorf("IsAuthError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestBuildRef(t *testing.T) {
	tests := []struct {
		name      string
		registry  string
		imageName string
		tag       string
		want      string
	}{
		{"no registry", "", "myapp", "v1.0.0", "myapp:v1.0.0"},
		{"with registry", "gcr.io/project", "myapp", "v1.0.0", "gcr.io/project/myapp:v1.0.0"},
		{"registry trailing slash", "gcr.io/project/", "myapp", "latest", "gcr.io/project/myapp:latest"},
		{"gar registry", "europe-west9-docker.pkg.dev/project/repo", "app", "canary", "europe-west9-docker.pkg.dev/project/repo/app:canary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRef(tt.registry, tt.imageName, tt.tag)
			if got != tt.want {
				t.Errorf("BuildRef(%q, %q, %q) = %q, want %q", tt.registry, tt.imageName, tt.tag, got, tt.want)
			}
		})
	}
}

func TestUniqueStrings(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"no duplicates", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"with duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
		{"empty", nil, []string{}},
		{"single", []string{"a"}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UniqueStrings(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("UniqueStrings(%v) = %v (len %d), want %v (len %d)", tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("UniqueStrings(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestTransportOf_DefaultBoundsResponses proves a registry call without a
// per-call transport runs on a transport whose responses must begin within
// RegistryResponseHeaderTimeout, so a stalled public registry cannot hang a
// push, a tag, or a digest lookup.
func TestTransportOf_DefaultBoundsResponses(t *testing.T) {
	for name, options := range map[string][]PushOptions{
		"no options":    nil,
		"nil transport": {{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := transportOf(options)
			if err != nil {
				t.Fatalf("transportOf: %v", err)
			}
			bounded, ok := got.(*http.Transport)
			if !ok {
				t.Fatalf("transportOf returned %T, want *http.Transport", got)
			}
			if bounded.ResponseHeaderTimeout != RegistryResponseHeaderTimeout {
				t.Fatalf("ResponseHeaderTimeout = %v, want %v", bounded.ResponseHeaderTimeout, RegistryResponseHeaderTimeout)
			}
			if got == remote.DefaultTransport {
				t.Fatal("transportOf returned the library default itself instead of a bounded clone")
			}
		})
	}
	if DefaultRegistryTransport() != DefaultRegistryTransport() {
		t.Fatal("DefaultRegistryTransport built a new transport per call; publishers must share one pool")
	}
}

// TestTransportOf_KeepsOverride proves a per-call transport, such as a private
// publication broker's, is used as given.
func TestTransportOf_KeepsOverride(t *testing.T) {
	override := &http.Transport{}
	got, err := transportOf([]PushOptions{{Transport: override}})
	if err != nil {
		t.Fatalf("transportOf: %v", err)
	}
	if got != override {
		t.Fatalf("transportOf returned %v, want the override", got)
	}
	if _, err := transportOf([]PushOptions{{}, {}}); err == nil {
		t.Fatal("transportOf accepted two options values")
	}
}
