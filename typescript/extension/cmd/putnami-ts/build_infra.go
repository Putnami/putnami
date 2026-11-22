package main

import (
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/infraagg"
)

// buildInfra is the workload's infra-aggregation task: the SDK's shared body
// plus the one thing that is TypeScript's to say — the HTTP/2 default a
// Bun-backed workload runs under.
func buildInfra() cli.JobFunc {
	return infraagg.Job(infraagg.Options{RuntimeCompatibility: disableHTTP2})
}

// packageDeployment is the workload's deployment declaration task: the same
// aggregate as buildInfra, under the same compatibility hook, in the canonical
// bytes a release-set member of kind deployment carries.
func packageDeployment() cli.JobFunc {
	return infraagg.DeploymentJob(infraagg.Options{RuntimeCompatibility: disableHTTP2})
}

// disableHTTP2 turns HTTP/2 off for a TypeScript workload's runtime block
// unless the block already states it (ADR 0013).
//
// A Bun-backed service accepts cleartext HTTP/2 with prior knowledge beside
// HTTP/1.1 and its WebSocket upgrades, and serves Connect client and
// bidirectional streams only over HTTP/2. But Bun does not carry WebSockets
// over HTTP/2, and end-to-end HTTP/2 from the platform has not been tested on
// a real deployment, so the default stays off. Putnami Cloud may default a
// service to HTTP/2, so the default is stated EXPLICITLY: a missing statement
// would be read as "take the platform default".
//
// A developer-authored infra/runtime.json that states protocols.http2, true or
// false, keeps its value: that is the workload's own opt-in, taken knowing its
// WebSocket transport may not survive an ingress that speaks HTTP/2 for every
// request. The synthesized defaults state nothing, so they always get false.
//
// This lived in the CLI until an earlier migration, where core recognized a
// TypeScript application by matching its tags ("ts", "typescript") and its
// extension names — a language guess made by the one component that should not
// be making it.
func disableHTTP2(rt *infra.Runtime) {
	if rt == nil {
		return
	}
	if rt.Protocols == nil {
		rt.Protocols = &infra.RuntimeProtocols{}
	}
	if rt.Protocols.HTTP2 != nil {
		return
	}
	http2 := false
	rt.Protocols.HTTP2 = &http2
}
