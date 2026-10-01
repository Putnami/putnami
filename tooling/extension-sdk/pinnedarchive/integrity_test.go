package pinnedarchive

import (
	"net/http"
	"strings"
	"testing"
)

func TestReadAdvertisedIntegrity(t *testing.T) {
	tests := []struct {
		name string
		hdr  http.Header
		want string
	}{
		{
			name: "no header",
			hdr:  http.Header{},
			want: "",
		},
		{
			name: "x-integrity preferred",
			hdr:  http.Header{"X-Integrity": []string{"sha256:abc"}, "Digest": []string{"sha-256=def"}},
			want: "sha256:abc",
		},
		{
			name: "digest sha-256",
			hdr:  http.Header{"Digest": []string{"sha-256=def"}},
			want: "def",
		},
		{
			name: "digest with other algos",
			hdr:  http.Header{"Digest": []string{"md5=zzz, sha-256=def"}},
			want: "def",
		},
		{
			name: "digest only unsupported algo",
			hdr:  http.Header{"Digest": []string{"md5=zzz"}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReadAdvertisedIntegrity(tt.hdr); got != tt.want {
				t.Errorf("ReadAdvertisedIntegrity = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeIntegrity(t *testing.T) {
	const hex64 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"raw hex", hex64, hex64, false},
		{"sha256:", "sha256:" + hex64, hex64, false},
		{"sha-256:", "sha-256:" + hex64, hex64, false},
		{"sha256-", "sha256-" + hex64, hex64, false},
		{"sha-256-", "sha-256-" + hex64, hex64, false},
		{"uppercase normalized", strings.ToUpper(hex64), hex64, false},
		{"too short", "abc", "", true},
		{"non-hex chars", strings.Repeat("z", 64), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeIntegrity(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeIntegrity(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NormalizeIntegrity(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
