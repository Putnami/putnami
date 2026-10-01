package extension

import (
	"strings"
	"testing"
)

func TestValidateRegistryURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		insecure  string // value to set for PUTNAMI_ALLOW_INSECURE_REGISTRY
		wantErr   bool
		wantInErr string
	}{
		{name: "https public", url: "https://put.putnami.dev", wantErr: false},
		{name: "https with path", url: "https://put.putnami.dev/v2", wantErr: false},
		{name: "http public rejected", url: "http://put.putnami.dev", wantErr: true, wantInErr: "plaintext http"},
		{name: "http localhost allowed", url: "http://localhost:9000", wantErr: false},
		{name: "http 127.0.0.1 allowed", url: "http://127.0.0.1:9000", wantErr: false},
		{name: "http ::1 allowed", url: "http://[::1]:9000", wantErr: false},
		{name: "http public allowed with opt-in", url: "http://put.putnami.dev", insecure: "1", wantErr: false},
		{name: "ftp rejected", url: "ftp://put.putnami.dev", wantErr: true, wantInErr: "unsupported scheme"},
		{name: "file rejected", url: "file:///tmp/registry", wantErr: true, wantInErr: "unsupported scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(AllowInsecureRegistryEnv, tt.insecure)
			err := ValidateRegistryURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateRegistryURL(%q) err = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
			if tt.wantErr && tt.wantInErr != "" && !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("error %q should mention %q", err.Error(), tt.wantInErr)
			}
		})
	}
}
