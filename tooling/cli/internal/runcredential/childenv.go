package runcredential

import (
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/envkeys"
)

// ChildEnv returns env, the environment of a job or a hook this process
// starts, as a hosted run hands it on. A process that holds no run credential
// returns env unchanged.
//
// A hosted run removes every variable that holds a framework credential
// (credentialVariables), including a copy a job's manifest declares, and sets
// extensionproto.OfflineDependenciesEnv to "1" after every other entry, so no
// entry of env overrides it. A process that fetches
// the workspace's dependencies (extensionproto.WorkspaceFetchCommand) is the
// one allowed to download them: it gets no OfflineDependenciesEnv at all.
func ChildEnv(env []string, fetches bool) []string {
	if !Hosted() {
		return env
	}
	return hostedChildEnv(env, fetches)
}

func hostedChildEnv(env []string, fetches bool) []string {
	for _, name := range credentialVariables {
		env = envkeys.Host.Remove(env, name)
	}
	if fetches {
		return envkeys.Host.Remove(env, extensionproto.OfflineDependenciesEnv)
	}
	return envkeys.Host.Set(env, extensionproto.OfflineDependenciesEnv, "1")
}
