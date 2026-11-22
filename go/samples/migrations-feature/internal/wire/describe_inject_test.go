package wire

import (
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/database"
	"go.putnami.dev/http"
)

// poolApp composes a handler that injects *database.Pool with a database
// plugin that has no connection, as the build's describe run sees it.
func poolApp() *app.Application {
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	server.GET("/ping", http.Inject(func(pool *database.Pool, ctx *http.Context) *http.Response {
		return http.Text("ok")
	}))
	a := app.New("describe-inject-pool")
	a.Use(database.NewPlugin(database.PluginConfig{Datasource: "default"}))
	a.Use(server)
	return a
}

// TestDescribeBuildsNoInjectedPool proves the build's describe run succeeds
// for a handler that injects *database.Pool without any database, while a
// real start-up still fails fast on the missing connection.
func TestDescribeBuildsNoInjectedPool(t *testing.T) {
	t.Setenv(database.EnvBinding, "")

	if err := poolApp().Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("describe must not open the injected pool: %v", err)
	}
	if err := poolApp().Validate(); err == nil {
		t.Fatal("start-up must fail when the injected pool has no connection")
	}
}
