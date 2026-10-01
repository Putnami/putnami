package main

import (
	"os"

	"go.putnami.dev/app"
	pconfig "go.putnami.dev/config"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	"go.putnami.dev/platform"
)

type ServerConfig struct {
	Port int `json:"port" default:"3000" env:"PORT"`
}

var RuntimeConfig = pconfig.Config[ServerConfig]("server")

func main() {
	cfg, err := pconfig.Load(RuntimeConfig)
	if err != nil {
		logger.Default().Error("config failed", err)
		os.Exit(1)
	}

	a, _ := newApp(cfg)
	if err := a.ListenAndServe(); err != nil {
		logger.Default().Error("application failed", err)
		os.Exit(1)
	}
}

// newApp composes the application and returns it with its HTTP server. The
// platform plugin answers /livez, /healthz, /readyz and /version on that
// server, and the health plugin answers GET /_/health.
func newApp(cfg ServerConfig) (*app.Application, *http.ServerPlugin) {
	server := http.NewServerPlugin(http.ServerConfig{Port: cfg.Port})
	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{
		Exclude: []string{"/_/health", "/livez", "/healthz", "/readyz"}, // keep probes out of the access log
	}))

	server.GET("/", func(ctx *http.Context) *http.Response {
		return http.JSON(map[string]string{"Hello": "World"})
	})

	a := app.New("server")
	a.Use(server)
	a.Use(platform.NewPlugin(platform.Config{}))
	a.Use(http.NewHealthPlugin())
	return a, server
}
