package app

import "sync"

// DefaultPluginFactory builds a plugin that every application created after
// its registration starts with. It returns nil when the plugin does not apply
// to this process, for example when the environment it reads is not set, and
// the application then starts without it.
type DefaultPluginFactory func() Plugin

var (
	defaultPluginsMu sync.Mutex
	defaultPlugins   []DefaultPluginFactory
)

// RegisterDefaultPlugin makes New add the factory's plugin to every
// application it creates afterwards. Register from a package init(), so a
// blank import of the registering package is the only wiring a workload
// needs:
//
//	func init() {
//	    app.RegisterDefaultPlugin(func() app.Plugin {
//	        if os.Getenv("MY_FEATURE_ENDPOINT") == "" {
//	            return nil
//	        }
//	        return newMyPlugin()
//	    })
//	}
//
// New calls each factory once per application, in registration order, and
// adds the plugins before any plugin the workload adds itself. A nil factory
// is ignored.
func RegisterDefaultPlugin(factory DefaultPluginFactory) {
	if factory == nil {
		return
	}
	defaultPluginsMu.Lock()
	defer defaultPluginsMu.Unlock()
	defaultPlugins = append(defaultPlugins, factory)
}

// useDefaultPlugins adds every registered default plugin that applies to m.
func useDefaultPlugins(m *Module) {
	defaultPluginsMu.Lock()
	factories := append([]DefaultPluginFactory(nil), defaultPlugins...)
	defaultPluginsMu.Unlock()
	for _, factory := range factories {
		if plugin := factory(); plugin != nil {
			m.Use(plugin)
		}
	}
}
