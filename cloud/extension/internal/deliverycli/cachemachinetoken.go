package deliverycli

import (
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/cloud/extension/internal/deliverycli/internal/remotecache"
	registry "go.putnami.dev/protocol/registry"
)

// cloudTokenEnv carries the workspace machine token of a session that holds
// one secret and no signed-in user: a cloud agent session or a CI job.
// clicore.WorkspaceAuth reads the same variable for workspace commands.
const cloudTokenEnv = "PUTNAMI_CLOUD_TOKEN"

// machineTokenPrefix starts every workspace machine token Identity issues.
// A hosted CI capability can travel in the same variable; it is a signed token
// with another shape, and the prefix keeps it away from the cache server.
const machineTokenPrefix = "pkt_"

// cacheMachineToken returns the workspace machine token a provider session
// sends to the cache server, or "" when the session sends another credential.
// The token is used only when all of these hold:
//
//   - no hosted CI launcher started the run: a hosted capability never crosses
//     to the cache server;
//   - the remote cache is active and its token source yields to a machine token
//     (remotecache.Config.TakesMachineToken): PUTNAMI_CACHE_TOKEN, a custom
//     command, a URL source and a metadata identity keep precedence;
//   - no stored user session exists (clicore.MachineTokenOnly): a signed-in
//     user keeps the command recipe, so a token exported for a deploy never
//     replaces it;
//   - the value is exactly one bearer token and has the machine token prefix.
//
// problem is one log line for a value that cannot travel in an Authorization
// header. It never holds the value. A nil env is the process environment.
func cacheMachineToken(cfg *remotecache.Config, hosted bool, env map[string]string) (token, problem string) {
	if hosted || !cfg.Active() || !cfg.TakesMachineToken() || !clicore.MachineTokenOnly(env) {
		return "", ""
	}
	token = clicore.EnvGet(env, cloudTokenEnv)
	if !registry.ValidCredentialBearer(token) {
		return "", cloudTokenEnv + " must contain exactly one bearer token; the remote cache does not use it"
	}
	if !strings.HasPrefix(token, machineTokenPrefix) {
		return "", ""
	}
	return token, ""
}

// applyMachineToken hands the config the session's workspace machine token
// when cacheMachineToken allows one. The token stays in memory: it is not
// written to .putnami/cache.json or to a log.
func (s *providerSession) applyMachineToken(cfg *remotecache.Config) {
	token, problem := cacheMachineToken(cfg, s.hosted, nil)
	if problem != "" {
		s.log(problem)
	}
	cfg.UseMachineToken(token)
}

// cacheReadsMachineToken reports whether a build with the config built (see
// buildCacheConfig), started with env, reads the remote cache with
// PUTNAMI_CLOUD_TOKEN. It takes the decision providerSession.initialize takes,
// so the cache status names the credential a build sends.
func cacheReadsMachineToken(built builtCacheConfig, env map[string]string) bool {
	token, _ := cacheMachineToken(built.cfg, hostedLaunchIn(env), env)
	return token != ""
}
