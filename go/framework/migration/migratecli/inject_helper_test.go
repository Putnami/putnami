package migratecli

import (
	"go.putnami.dev/inject"
	"go.putnami.dev/migration"
)

// injectTokenForRegistry returns the DI token used by app.buildContainer
// to register the per-app *migration.Registry. Lifted into a helper so
// the test stays independent of the inject API import surface.
func injectTokenForRegistry() inject.Token {
	return inject.TokenOf[*migration.Registry]()
}
