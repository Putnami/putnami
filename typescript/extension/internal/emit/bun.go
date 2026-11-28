package emit

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/procstat"
)

//go:embed transpile.mjs
var transpileScript []byte

// transpileTimeout bounds the one bun process of a manifest.
const transpileTimeout = 5 * time.Minute

// BunTranspiler transpiles through the bun binary at bunBin.
func BunTranspiler(bunBin string) Transpiler {
	return func(request TranspileRequest) (map[string]string, error) {
		dir, err := os.MkdirTemp("", "putnami-ts-emit-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		script := filepath.Join(dir, "transpile.mjs")
		if err := os.WriteFile(script, transpileScript, 0o600); err != nil {
			return nil, err
		}
		input, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), transpileTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bunBin, "--no-install", script) //nolint:gosec // the resolved bun and a script this binary embeds
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := procstat.Run(cmd); err != nil {
			return nil, fmt.Errorf("bun transpile: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		out := map[string]string{}
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			return nil, fmt.Errorf("bun transpile answered no manifest: %w", err)
		}
		return out, nil
	}
}
