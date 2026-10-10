// Package activate registers the Putnami Cloud config and secrets sources with
// the go.putnami.dev/config loader. Blank-import it from a workload's main
// package to enable CONFIG_SERVER_URL-driven config and secrets resolution:
//
//	import _ "go.putnami.dev/cloud/runtime/activate"
//
// Without this import, config.Load reads only local YAML, CONFIG_DATA and
// env-tag overrides, and configuration held only by the config server is
// absent at runtime.
//
// Registration runs from init(), before the workload calls config.Load.
// Importing the parent package for its types does not touch the loader's
// global discoverer registry; only this blank import does.
package activate

import (
	config "go.putnami.dev/config"

	cloudruntime "go.putnami.dev/cloud/runtime"
)

func init() {
	cloudruntime.Register(config.RegisterSourceDiscoverer)
}
