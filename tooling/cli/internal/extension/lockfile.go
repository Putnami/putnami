package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// HashFile computes the SHA-256 hash of a file and returns it hex-encoded.
func HashFile(path string) (string, error) {
	return lockfile.HashFile(path)
}

// HashFileContext computes the same bare lowercase SHA-256 as HashFile while
// honoring cancellation between streamed chunks. It is used by bounded read
// preparation for archives and manifests; ordinary callers retain HashFile's
// historical behavior.
func HashFileContext(ctx context.Context, path string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
