package oci

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestNewRegistryKeychain_InjectsBearerForHostOnly(t *testing.T) {
	kc := NewRegistryKeychain("oci.putnami.dev", "pkt_xyz")

	matchRef, err := name.ParseReference("oci.putnami.dev/app/svc:latest")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := kc.Resolve(matchRef.Context().Registry)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryToken != "pkt_xyz" {
		t.Errorf("expected Bearer pkt_xyz for the host, got %+v", cfg)
	}

	// A different registry must NOT receive the token (defers to the default).
	otherRef, err := name.ParseReference("ghcr.io/app/svc:latest")
	if err != nil {
		t.Fatal(err)
	}
	auth2, err := kc.Resolve(otherRef.Context().Registry)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := auth2.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.RegistryToken == "pkt_xyz" {
		t.Errorf("token leaked to a different registry: %+v", cfg2)
	}
}

func TestNewRegistryKeychain_EmptyTokenDefersToDefault(t *testing.T) {
	kc := NewRegistryKeychain("oci.putnami.dev", "")
	ref, err := name.ParseReference("oci.putnami.dev/app:latest")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := kc.Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryToken != "" {
		t.Errorf("empty token should inject nothing, got %+v", cfg)
	}
}

func TestRegistryHost(t *testing.T) {
	tests := []struct {
		reg  string
		want string
	}{
		{"", ""},
		{"registry.example.com", "registry.example.com"},
		{"us-docker.pkg.dev/project/repo", "us-docker.pkg.dev"},
		{"us-docker.pkg.dev/project/repo/", "us-docker.pkg.dev"},
	}
	for _, tt := range tests {
		t.Run(tt.reg, func(t *testing.T) {
			if got := RegistryHost(tt.reg); got != tt.want {
				t.Errorf("RegistryHost(%q) = %q, want %q", tt.reg, got, tt.want)
			}
		})
	}
}
