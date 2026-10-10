// Package runtime is the Putnami Cloud destination for the go.putnami.dev/config
// loader. It provides the remote config and secrets sources, the authenticated
// prepared-configuration boot source, and the workload identity that signs
// their requests, read from the GCP metadata server.
//
// A workload enables the sources with a blank import of the activate
// subpackage before it calls config.Load:
//
//	import _ "go.putnami.dev/cloud/runtime/activate"
//
// Discovery reads CONFIG_SERVER_URL. When PUTNAMI_CONFIG_BOOT_BINDING is set,
// the prepared boot source is the only remote source: it posts the opaque
// reference to a fixed path under the origin of CONFIG_SERVER_URL,
// authenticates with a token minted for the exact CONFIG_SERVER_AUDIENCE,
// refuses redirects, and never falls back to a snapshot. Every source
// implements config.ContextSource, so config.LoadContext stops its requests,
// retry waits and snapshot fallback when the caller's context ends.
package runtime
