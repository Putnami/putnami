package pkg

import (
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// WritePackageMetadata records the channel this packager produced inside the
// output directory it owns, at <outputDir>/channel.json.
//
// npm and docker packaging run as separate pipeline steps against the same
// project, and each owns its own directory. Writing the record there — rather
// than merging into a file at the package root — is what lets both steps stay
// under ordinary declared capture: the record is captured and restored with the
// artifact it describes, so a cache hit cannot leave the package present and
// its channel unrecorded. The file name and shape live once in the SDK
// (pkgmeta.WriteChannelRecord) so every ecosystem records the same way.
func WritePackageMetadata(outputDir, channel, version string, stable bool) error {
	return pkgmeta.WriteChannelRecord(outputDir, pkgmeta.ChannelRecord{
		Version:  version,
		Stable:   stable,
		Channels: []string{channel},
	})
}
