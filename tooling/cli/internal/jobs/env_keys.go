package jobs

import (
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
)

// prependPathWith puts dir first on the effective PATH of env, matching names
// with keys, and keeps every other non-empty entry behind it, in order.
func prependPathWith(keys envkeys.Keys, env []string, dir string) []string {
	cleanDir := filepath.Clean(dir)
	parts := []string{cleanDir}
	for _, part := range filepath.SplitList(keys.Last(env, "PATH")) {
		if part == "" || filepath.Clean(part) == cleanDir {
			continue
		}
		parts = append(parts, part)
	}
	return keys.Set(env, "PATH", strings.Join(parts, string(os.PathListSeparator)))
}
