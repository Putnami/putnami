// Simple API demonstrates a minimal Go application using the Putnami framework.
//
// This is the recommended starting point -- no dependency injection, no events,
// just HTTP handlers with middleware and a health check.
package main

import (
	"os"

	"go.putnami.dev/app"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
)

func main() {
	server := http.NewServerPlugin(http.ServerConfig{Port: 8080})

	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{
		Exclude: []string{"/_/health"},
	}))

	server.GET("/greetings", listGreetings)
	server.POST("/greetings", createGreeting)
	server.GET("/greetings/{id}", getGreeting)
	server.DELETE("/greetings/{id}", deleteGreeting)

	a := app.New("simple-api")
	a.Use(server)
	a.Use(http.NewHealthPlugin())

	if err := a.ListenAndServe(); err != nil {
		logger.New("main", logger.LevelError).Error("application failed", err)
		os.Exit(1)
	}
}
