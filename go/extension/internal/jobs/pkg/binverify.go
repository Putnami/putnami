package pkg

import (
	"bytes"
	"fmt"
	"os"

	pctx "go.putnami.dev/sdk/extension/context"
)

// expectedEmbeddedVersion returns the artifact version the pre-built binary
// must embed, or "" when the project does not stamp a version. The caller
// passes the same stable/prerelease value used for the manifest and package
// identity, so packaging cannot accept a binary with a different identity.
func expectedEmbeddedVersion(ctx *pctx.Context, artifactVersion string) string {
	if ctx == nil || artifactVersion == "" {
		return ""
	}
	if ctx.Params.String("version-var", "versionVar") == "" {
		return ""
	}
	return artifactVersion
}

// verifyBinaryVersion fails when a pre-built binary does not embed expected.
// It scans the bytes for the literal `-X` ldflags value instead of running
// `--version`, so it checks a binary of any GOOS/GOARCH.
func verifyBinaryVersion(binaryPath, expected string) error {
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read binary %s: %w", binaryPath, err)
	}
	if !bytes.Contains(data, []byte(expected)) {
		return fmt.Errorf(
			"binary %s does not embed expected version %q — a stale cross-compile cache hit likely served a prior commit's binary; re-run with --no-cache",
			binaryPath, expected)
	}
	return nil
}
