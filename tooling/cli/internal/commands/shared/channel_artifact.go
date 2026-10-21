package shared

// ChannelArtifact is the install argument that resolves name on channel for
// one install: the bare name for no channel, which resolves as the workspace
// configures it, else name@channel. The workspace config keeps the bare name
// either way, and the lock records the exact version the channel resolved.
func ChannelArtifact(name, channel string) string {
	if channel == "" {
		return name
	}
	return name + "@" + channel
}
