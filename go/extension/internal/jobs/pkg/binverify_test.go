package pkg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
)

func paramsWith(t *testing.T, kv map[string]string) pctx.Params {
	t.Helper()
	p := pctx.Params{}
	for k, v := range kv {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal param %q: %v", k, err)
		}
		p[k] = b
	}
	return p
}

func TestExpectedEmbeddedVersion(t *testing.T) {
	tests := []struct {
		name string
		ctx  *pctx.Context
		want string
	}{
		{
			name: "version-var set uses selected artifact version",
			ctx: &pctx.Context{
				Params: paramsWith(t, map[string]string{"version-var": "main.Version"}),
			},
			want: "0.1.0-aaaa1111",
		},
		{
			name: "no version-var means binary carries no version",
			ctx:  &pctx.Context{Version: &pctx.Version{Full: "0.1.0-aaaa1111"}},
			want: "",
		},
		{
			name: "version-var does not require Version info after selection",
			ctx:  &pctx.Context{Params: paramsWith(t, map[string]string{"version-var": "main.Version"})},
			want: "0.1.0-aaaa1111",
		},
		{
			name: "nil context",
			ctx:  nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expectedEmbeddedVersion(tt.ctx, "0.1.0-aaaa1111"); got != tt.want {
				t.Errorf("expectedEmbeddedVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyBinaryVersion(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh")
	if err := os.WriteFile(fresh, []byte("...header\x00 0.1.0-aaaa1111 \x00footer..."), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyBinaryVersion(fresh, "0.1.0-aaaa1111"); err != nil {
		t.Errorf("verifyBinaryVersion(matching) = %v, want nil", err)
	}

	// A binary that embeds a different (prior commit's) version — the stale
	// cross-compile cache-hit scenario the guard exists to catch.
	stale := filepath.Join(dir, "stale")
	if err := os.WriteFile(stale, []byte("...header\x00 0.1.0-bbbb2222 \x00footer..."), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyBinaryVersion(stale, "0.1.0-aaaa1111"); err == nil {
		t.Error("verifyBinaryVersion(stale) = nil, want error — a binary stamped with a different version must be rejected")
	}

	if err := verifyBinaryVersion(filepath.Join(dir, "missing"), "0.1.0-aaaa1111"); err == nil {
		t.Error("verifyBinaryVersion(missing file) = nil, want error")
	}
}
