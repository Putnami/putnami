package main

import (
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/infraagg"
)

// buildInfra is the workload's infra-aggregation task: the SDK's shared body
// plus the one thing that is TypeScript's to say — what a Bun-backed workload
// can actually serve.
func buildInfra() cli.JobFunc {
	return infraagg.Job(infraagg.Options{RuntimeCompatibility: disableHTTP2})
}

// disableHTTP2 turns HTTP/2 off for every TypeScript workload's runtime block.
//
// Bun-backed services do not serve h2c today, and Putnami Cloud may default a
// service to HTTP/2, so a TS workload must opt out EXPLICITLY — a missing
// statement would be read as "take the platform default", which is the wrong
// one here.
//
// It is applied to a developer-authored infra/runtime.json as well as to the
// synthesized defaults: this is a capability of the runtime, not a preference,
// so `"protocols": {"http2": true}` in a project's own runtime file is a
// request the server cannot honor and must not survive into the manifest a
// deployer acts on.
//
// This lived in the CLI until an earlier migration, where core recognized a
// TypeScript application by matching its tags ("ts", "typescript") and its
// extension names — a language guess made by the one component that should not
// be making it.
func disableHTTP2(rt *infra.Runtime) {
	if rt == nil {
		return
	}
	http2 := false
	if rt.Protocols == nil {
		rt.Protocols = &infra.RuntimeProtocols{}
	}
	rt.Protocols.HTTP2 = &http2
}
