// Package profiles holds the ecosystem profiles the framework itself owns.
//
// Every ecosystem is declared by the extension that publishes it, but `oci` has
// no single owner: the TypeScript and the Go extensions both publish container
// images through the SDK's shared Docker publisher, and neither may declare the
// profile without making the other's declaration a duplicate. The SDK ships it
// instead, and both extensions reference it with `uses: ["oci"]`.
package profiles

import (
	"encoding/json"

	extproto "go.putnami.dev/protocol/extension"
)

// OCI is the container-image ecosystem: a repository path as coordinate, an
// opaque tag as version, and a native channel projection — a channel name IS an
// OCI tag, so `channel set` retags the image in the registry itself.
var OCI = extproto.OwnedProfile{
	Owner: "putnami-extension-sdk",
	Profile: extproto.EcosystemProfile{
		ID:         "oci",
		Coordinate: extproto.PatternRule{Pattern: `^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`},
		Version:    extproto.VersionRule{Pattern: `^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`, Ordering: extproto.OrderingString},
		Channel:    extproto.ChannelNative,
		Registries: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"publish":{"type":"string"}}}`),
		Publish:    "publish",
	},
}

// Builtin returns every profile the framework owns itself, for a CLI resolving
// the profiles of its installed extensions.
func Builtin() []extproto.OwnedProfile { return []extproto.OwnedProfile{OCI} }
