// Package registrycred is the client side of the registry-token credential seam
// (protocol/registry): it asks @putnami/cloud for a registry credential per HOST
// so the framework never embeds a Putnami host list, recipe model, or stored
// credential.
//
// The host is the whole credential request. A caller names the registry host it
// already knows from its own configuration — the OCI registry in a publish target, the
// @putnami scope line in the workspace .npmrc, the declared Go module origin —
// and the cloud maps that host to a registry kind, an account, and a credential.
// Nothing about the package, the action, or the workspace crosses the seam.
//
// # Two shapes, one contract
//
// A publisher holds the credential itself and sets an Authorization header, so
// it calls ResolveToken and gets a bare bearer.
//
// An installer runs `bun install` or `go build`, which read .npmrc and .netrc
// and take no bearer on a pipe, so it calls EnsureNativeCredential: the cloud
// writes the native credential file for the host and the framework never sees
// the token.
//
// # Absence is ordinary
//
// A host the cloud does not manage, a core-only install with no cloud at all, a
// user who is not signed in, and a cloud that predates a shape of the seam are
// all ordinary states. ResolveToken yields an empty token and the caller falls
// back to explicit or native credentials; EnsureNativeCredential leaves the
// machine's existing credentials in place. Neither ever fails a build.
//
// # Hosted runs
//
// A job of a hosted run asks nothing: neither function starts a process when
// the job runs offline or when the engine handed it a job credential
// descriptor, even one without a credential. The child the seam starts is a
// CLI without the run credential that loads the workspace's extensions.
package registrycred

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	registry "go.putnami.dev/protocol/registry"
)

// cloudTokenTimeout bounds the cloud-CLI credential call so a hung or missing
// provider cannot stall a publish.
const cloudTokenTimeout = 30 * time.Second

// cloudCLIName is the fallback launcher the credential seam shells out to when
// it is used outside a CLI-spawned extension. It is a package var so tests can
// point it at a fake binary; extension jobs prefer the absolute executable the
// spawning CLI advertises through registry.CLIExecutableEnv.
var cloudCLIName = "putnami"

// hostedJobHint is the hint ResolveToken returns in a job of a hosted run,
// which starts no credential child.
const hostedJobHint = "a job of a hosted run starts no `putnami cloud registry-token`"

// ResolveToken asks @putnami/cloud for the registry bearer for host, through the
// documented seam `putnami cloud registry-token --host <host>`.
//
// The cloud's own stderr is returned as a hint so a later auth rejection can
// show the cloud-owned guidance verbatim, without core hard-coding a command a
// core-only install lacks.
//
// In a job of a hosted run it starts no process and returns no token (see
// hostedJob), and ResolveTokenWithCLI does the same.
//
// It is a package var so a publisher's unit tests can stub the cloud without a
// real CLI on PATH.
var ResolveToken = resolveTokenCLI

// resolveTokenCLI is the production implementation of the seam. On success it
// returns (bearer, ""); on any failure it returns ("", hint) where hint is the
// cloud's stderr (empty when the cloud binary is absent, so there is nothing
// actionable to relay).
func resolveTokenCLI(host string) (token, hint string) {
	executable, advertised := resolveCloudCLIExecutable()
	return resolveTokenWithCLI(context.Background(), host, executable, advertised)
}

// ResolveTokenWithCLI resolves the host-only credential through the absolute
// executable already selected by the calling CLI. This is the bootstrap path
// used before a private workspace pin can be downloaded: its credential child
// must run these known bytes rather than try to resolve the same missing pin.
// It does not change the parent's environment or consult PATH/CLIExecutableEnv.
// The request is bounded by both ctx and the ordinary credential timeout.
func ResolveTokenWithCLI(ctx context.Context, host, executable string) (token, hint string) {
	if !filepath.IsAbs(executable) {
		return "", "registry-token bootstrap requires an absolute CLI executable"
	}
	return resolveTokenWithCLI(ctx, host, executable, true)
}

func resolveTokenWithCLI(parent context.Context, host, executable string, noRelaunch bool) (token, hint string) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", ""
	}
	// A job of a hosted run starts no credential child (hostedJob).
	if hostedJob() {
		return "", hostedJobHint
	}

	ctx, cancel := context.WithTimeout(parent, cloudTokenTimeout)
	defer cancel()

	// The host is a discrete argv element of a fixed command (no shell is
	// involved), so it cannot inject a command. The executable is the CLI-owned
	// absolute path or the package-owned fallback, never caller input.
	cmd := exec.CommandContext(ctx, executable, //nolint:gosec // G702: argv-form exec of a CLI-owned absolute path or fixed fallback
		registry.SeamParentCommand,
		registry.SeamSubcommand,
		"--"+registry.SeamHostFlag, host,
	)
	// The child is a credential leaf: it must mint one bearer and nothing else.
	// Without this, the CLI's first-use bootstrap runs a full `putnami install`
	// inside the child whenever the lock changed since the last install — which
	// is exactly the state mid-`upgrade`, when the extension phase has rewritten
	// the lock and the artifact phase asks for a credential. That nested install
	// prints its "Workspace setup completed" lines on stdout ahead of the
	// bearer, the read below rejects the value, and the download goes anonymous
	// . The parent command owns workspace state; the child
	// never restores it.
	cmd.Env = append(os.Environ(), "PUTNAMI_NO_AUTO_INSTALL=1")
	if noRelaunch {
		// Preserve the advertised executable's identity even when the workspace
		// lock pins an older CLI: this child is already the exact CLI selected by
		// its spawning runner, so launcher relaunch would replace its bytes.
		cmd.Env = append(cmd.Env, "PUTNAMI_NO_RELAUNCH=1")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", strings.TrimSpace(stderr.String())
	}

	tok := strings.TrimSpace(stdout.String())
	if tok == "" {
		return "", ""
	}
	// The seam contract is a bare bearer on stdout and nothing else. A value
	// carrying internal whitespace is a human status line written to the wrong
	// stream; sent verbatim it would become a malformed `Authorization: Bearer`
	// header the registry rejects as an opaque 401. Treat it as no token.
	if !registry.ValidBearer(tok) {
		return "", "registry-token returned a non-bearer value on stdout (the cloud likely wrote a message instead of the bare token)"
	}
	return tok, ""
}

func resolveCloudCLIExecutable() (executable string, advertised bool) {
	if executable := strings.TrimSpace(os.Getenv(registry.CLIExecutableEnv)); filepath.IsAbs(executable) {
		return executable, true
	}
	return cloudCLIName, false
}
