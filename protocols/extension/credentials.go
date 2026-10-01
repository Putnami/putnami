package extension

// Credential custody on a hosted run (ADR 0055).
//
// A hosted run starts the engine with `--credential-fd <n>`. From then on no
// repository-controlled process — `./putnamiw`, a `postinstall` script, a
// test, a hook, or a process one of them leaves running — may read a
// credential. Two variables carry that contract to extension jobs. Neither
// holds a secret.
const (
	// WorkspaceFetchCommand is the lifecycle command that downloads a
	// workspace's dependencies and runs none of their code: no lifecycle
	// script, no build hook, no test. On a hosted run `putnami install` runs it
	// for every extension that provides it, to completion, before any
	// `workspace-install`, and only this job receives the read credential
	// (JobCredentialFDEnv). An extension that fetches nothing does not declare
	// it.
	WorkspaceFetchCommand = "workspace-fetch"

	// JobCredentialFDEnv names the inherited descriptor that carries a job's
	// read credential. Its value is a descriptor number, never the credential.
	// The descriptor holds one JSON line in the credential-provider/v1
	// `credential` result shape (go.putnami.dev/protocol/registry
	// CredentialResult): `{"credential":{"bearer":…,"expiresAt":…,"hosts":[…]}}`,
	// or `{}` when the provider holds no read credential. The job reads it
	// once, closes it before it starts any process, and keeps the bearer out
	// of every environment and every file a later process can read. The
	// engine sets it only for WorkspaceFetchCommand run by the extension's own
	// runtime.
	JobCredentialFDEnv = "PUTNAMI_JOB_CREDENTIAL_FD" // #nosec G101 -- variable name, not a credential

	// OfflineDependenciesEnv is "1" in every job of a hosted run. The
	// dependencies were downloaded by WorkspaceFetchCommand before any
	// repository-controlled process started, so a package manager must not
	// reach the network for them: bun installs from its cache with
	// `--frozen-lockfile`, Go runs with GOPROXY=off and -mod=readonly, and no
	// installer writes a credential into the user's home. Absent means today's
	// behavior.
	OfflineDependenciesEnv = "PUTNAMI_OFFLINE_DEPENDENCIES"
)
