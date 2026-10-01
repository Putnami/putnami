package main

import (
	"os"

	"go.putnami.dev/app"
	pconfig "go.putnami.dev/config"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
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

	server := http.NewServerPlugin(http.ServerConfig{Port: cfg.Port})
	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{}))

	server.GET("/", func(ctx *http.Context) *http.Response {
		return http.JSON(map[string]string{"Hello": "World"})
	})

	a := app.New("server")
	a.Use(server)
	a.Use(http.NewHealthPlugin())

	if err := a.ListenAndServe(); err != nil {
		logger.Default().Error("application failed", err)
		os.Exit(1)
	}
}
