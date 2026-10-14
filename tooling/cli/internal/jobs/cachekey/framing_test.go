package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strings"
	"testing"
)

// TestFramingHashesThePinnedBytes pins the bytes every cache key and content
// digest built from these fields hashes. The digests are the SHA-256 of the
// byte layouts written out by hand: each label length and value size is an
// unsigned 64-bit little-endian integer before its bytes. A change here moves
// every key that hashes a field.
func TestFramingHashesThePinnedBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(t *testing.T, h hash.Hash)
		want  string
	}{
		{
			// 05 00 00 00 00 00 00 00 "label" 05 00 00 00 00 00 00 00 "value"
			name:  "one field",
			write: func(_ *testing.T, h hash.Hash) { HashField(h, "label", []byte("value")) },
			want:  "9ef868d6fcc4907c29962ded4119f1a13f7dbef5865c21b97618185c9510c78b",
		},
		{
			// 05 00 00 00 00 00 00 00 "empty" 00 00 00 00 00 00 00 00
			name:  "an empty value",
			write: func(_ *testing.T, h hash.Hash) { HashField(h, "empty", nil) },
			want:  "6151da3378e6f56e35d42cfe3e0a8f1f58fe6217e6f52d9234dfa810c945bb1e",
		},
		{
			// 06 .. "schema" 02 .. "v1" 05 .. "bytes" 03 .. "abc"
			name: "a field then a stream",
			write: func(t *testing.T, h hash.Hash) {
				HashField(h, "schema", []byte("v1"))
				if err := HashStream(h, "bytes", 3, strings.NewReader("abc")); err != nil {
					t.Fatal(err)
				}
			},
			want: "83714fbf3a150fbaf27975dfad0cfdfec42cd7de5c89893621df43a0f5adef81",
		},
	}
	for _, tc := range cases {
		h := sha256.New()
		tc.write(t, h)
		if got := hex.EncodeToString(h.Sum(nil)); got != tc.want {
			t.Errorf("%s: digest = %s, want %s", tc.name, got, tc.want)
		}
	}
}
