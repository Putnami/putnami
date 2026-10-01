package publish

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
)

// smokeTestTimeout caps the smoke test runtime. `go mod download` on a
// freshly-published module typically completes in well under a second against a
// colocated registry; the timeout exists so a hung registry surfaces as a test
// failure rather than blocking the release pipeline.
const smokeTestTimeout = 60 * time.Second

// goModDownload is the subprocess invocation the smoke test runs. Overridable
// from tests so the package-level test suite can drive the smoke-test flow
// without a live registry.
var goModDownload = runGoModDownload

// smokeTestGoModule verifies that the just-published module can be fetched from
// the registry the same way every downstream consumer will: `go mod download
// <module>@<version>` in a fresh GOMODCACHE pointed at the registry, with the sum
// DB disabled (the sum DB does not know about freshly-published private modules).
//
// This is a release gate: the publish endpoint can accept a payload that
// downstream `go mod download` still rejects. It returns the error without
// emitting diagnostics; the caller renders the failure. The subprocess
// stdout/stderr is appended to the returned error.
func smokeTestGoModule(registryURL, registryToken, modulePath, version string) error {
	proxy, err := goProxyURL(registryURL, registryToken)
	if err != nil {
		return fmt.Errorf("build GOPROXY url: %w", err)
	}

	cache, err := os.MkdirTemp("", "putnami-go-smoke-")
	if err != nil {
		return fmt.Errorf("create temp GOMODCACHE: %w", err)
	}
	defer func() {
		// GOMODCACHE entries are written read-only; chmod recursively so the
		// cleanup can remove them.
		_ = chmodWritable(cache)
		_ = os.RemoveAll(cache)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTimeout)
	defer cancel()

	// The machine-global GOCACHE this subprocess points at is concurrently
	// deletable; retry the known-transient ENOENT class instead of failing the
	// release pipeline on it.
	outBytes, err := toolchain.RunWithGoCacheRetry(func() ([]byte, error) {
		downloadOut, downloadErr := goModDownload(ctx, proxy, cache, modulePath, version)
		return []byte(downloadOut), downloadErr
	})
	out := redactGoRegistrySecret(string(outBytes), registryToken)
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			return fmt.Errorf("go mod download %s@%s: %w", modulePath, version, err)
		}
		return fmt.Errorf("go mod download %s@%s: %w\n%s", modulePath, version, err, trimmed)
	}
	return nil
}

func redactGoRegistrySecret(text, token string) string {
	if token == "" {
		return text
	}
	secrets := make([]string, 0, 4)
	secrets = append(secrets, token, url.PathEscape(token), url.QueryEscape(token))
	encodedUserinfo := strings.TrimPrefix(url.UserPassword("", token).String(), ":")
	secrets = append(secrets, encodedUserinfo)
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return text
}

// goProxyURL renders the GOPROXY value for the smoke test. When a registry token
// is configured, it is embedded as HTTP basic auth so `go mod download` can
// authenticate; otherwise the URL is returned verbatim. The `,off` fallback keeps
// the downloader from falling through to proxy.golang.org for a private module.
func goProxyURL(registryURL, token string) (string, error) {
	if token == "" {
		return registryURL + ",off", nil
	}
	u, err := url.Parse(registryURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("x-access-token", token)
	return u.String() + ",off", nil
}

// runGoModDownload is the production implementation of the subprocess invocation.
func runGoModDownload(ctx context.Context, proxy, cache, modulePath, version string) (string, error) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, goBinary, "mod", "download", "-x", modulePath+"@"+version)
	cmd.Env = goModDownloadEnvironment(os.Environ(), goBinary, proxy, cache)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// goModDownloadEnvironment makes the private read path an exact, inspectable
// contract. Values inherited from the host are removed rather than duplicated:
// the subprocess must never depend on duplicate-environment precedence for its
// no-fallback/no-SumDB guarantees.
func goModDownloadEnvironment(base []string, goBinary, proxy, cache string) []string {
	env := toolchain.GoCommandEnv(base, goBinary)
	blocked := map[string]struct{}{
		"GOPROXY": {}, "GONOPROXY": {}, "GOSUMDB": {}, "GOMODCACHE": {},
	}
	filtered := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		_, override := blocked[key]
		if !override && !privateGoSmokeCapabilityEnv(key) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered,
		"GOPROXY="+proxy,
		// The whole point of this job is to prove the registry serves what was
		// just published, so the download has to go through the URL above. Clear
		// the GONOPROXY the shared environment applies when it supplies its own
		// default proxy: it would send the origin's modules directly to its
		// vanity endpoint and the smoke test would pass without ever asking the
		// registry — or ever using the token embedded in the URL.
		//
		// "none", NOT "". Go falls GONOPROXY back to GOPRIVATE whenever GONOPROXY
		// is empty, and GOPRIVATE also lives in Go's env file (`go env -w`), which
		// this environment cannot see or filter. An empty value therefore cleared
		// nothing on any machine carrying the documented private-module setting:
		// the download went to the vanity endpoint unauthenticated and the origin
		// answered 401. "none" is Go's match-nothing sentinel and is the only
		// value that actually overrides the file.
		"GONOPROXY=none",
		"GOSUMDB=off",
		"GOMODCACHE="+cache,
	)
}

func privateGoSmokeCapabilityEnv(name string) bool {
	lower := strings.ToLower(name)
	return (strings.HasPrefix(lower, "putnami_") && strings.HasSuffix(lower, "_token")) ||
		lower == "go_registry_token" || lower == "npm_token" || lower == "node_auth_token" ||
		lower == "node_options" || lower == "node_path" || lower == "bun_options" ||
		lower == "http_proxy" || lower == "https_proxy" || lower == "all_proxy" || lower == "no_proxy" ||
		lower == "ld_preload" || lower == "dyld_insert_libraries" || lower == "dyld_library_path" ||
		lower == "ssh_auth_sock" || lower == "gpg_agent_info"
}

// chmodWritable walks dir and adds the owner-write bit to every entry so
// os.RemoveAll can delete the read-only files Go's module cache writes.
func chmodWritable(dir string) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode()|0o200)
	})
}
