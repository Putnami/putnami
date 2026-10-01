// Simple API demonstrates a minimal Go application using the Putnami framework.
//
// This is the recommended starting point -- no dependency injection, no events,
// just HTTP handlers with middleware and the operational endpoints.
package main

import (
	"os"

	"go.putnami.dev/app"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	"go.putnami.dev/platform"
)

func main() {
	a, _ := newApp()
	if err := a.ListenAndServe(); err != nil {
		logger.New("main", logger.LevelError).Error("application failed", err)
		os.Exit(1)
	}
}

// newApp composes the application and returns it with its HTTP server. The
// platform plugin answers /livez, /healthz, /readyz and /version on that
// server, and the health plugin answers GET /_/health.
func newApp() (*app.Application, *http.ServerPlugin) {
	server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{
		Exclude: []string{"/_/health", "/livez", "/healthz", "/readyz"},
	}))

	server.GET("/greetings", listGreetings)
	server.POST("/greetings", createGreeting)
	server.GET("/greetings/{id}", getGreeting)
	server.DELETE("/greetings/{id}", deleteGreeting)

	a := app.New("simple-api")
	a.Use(server)
	a.Use(platform.NewPlugin(platform.Config{}))
	a.Use(http.NewHealthPlugin())
	return a, server
}
