// Task API demonstrates a Go application using the Putnami framework.
//
// It combines several framework modules into a cohesive REST API:
//   - app:      application lifecycle and plugin orchestration
//   - http:     routing, middleware, and request handling
//   - platform: standard operational endpoints (/healthz, /livez, /readyz, /version)
//   - schema:   struct-tag request validation (see handler.bindAndValidate)
//   - parallel: bounded fan-out for concurrent work (see handler.BatchGet)
//   - events:   typed event publishing and subscription
//   - inject:   constructor-based dependency injection
//   - config:   environment-aware configuration
//   - logger:   structured logging
package main

import (
	"context"
	"log/slog"
	"os"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	"go.putnami.dev/config"
	"go.putnami.dev/events"
	"go.putnami.dev/http"
	"go.putnami.dev/inject"
	"go.putnami.dev/logger"
	"go.putnami.dev/platform"
)

// buildTaskFeature is the single native composition root for task management.
// Tests use the same function as main so the conformance graph cannot drift
// into a separately maintained fixture.
func buildTaskFeature(server *http.ServerPlugin, broker events.Transport) *app.Module {
	taskAPI := api.New(server, api.WithPrefix("/tasks"))
	taskAPI.Register(api.Endpoint("GET", "").
		Returns(api.Type[[]Task]()).
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.List(ctx.Context)
		})))
	taskAPI.Register(api.Endpoint("POST", "").
		Returns(api.Type[Task]()).
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.Create(ctx.Context)
		})))
	taskAPI.Register(api.Endpoint("POST", "/batch").
		Returns(api.Type[[]batchResult]()).
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.BatchGet(ctx.Context)
		})))
	taskAPI.Register(api.Endpoint("GET", "/{id}").
		Returns(api.Type[Task]()).
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.Get(ctx.Context)
		})))
	taskAPI.Register(api.Endpoint("PUT", "/{id}").
		Returns(api.Type[Task]()).
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.Update(ctx.Context)
		})))
	taskAPI.Register(api.Endpoint("DELETE", "/{id}").
		Handle(http.Inject(func(h *TaskHandler, ctx *http.EndpointContext) *http.Response {
			return h.Delete(ctx.Context)
		})))

	taskEvents := events.Events(events.PluginConfig{Transport: broker})
	events.RegisterPublisher(taskEvents, TaskCreatedTopic)
	taskEvents.Register(events.Handle(TaskCreatedTopic,
		func(ctx context.Context, msg *events.Message[Task]) error {
			log := logger.FromContextOrDefault(ctx)
			log.Info("event: task created",
				slog.String("id", msg.Payload.ID),
				slog.String("title", msg.Payload.Title),
			)
			return nil
		},
		events.WithDistribution(events.Broadcast),
	))
	taskEvents.Register(events.Handle(TaskCompletedTopic,
		func(ctx context.Context, msg *events.Message[Task]) error {
			log := logger.FromContextOrDefault(ctx)
			log.Info("event: task completed",
				slog.String("id", msg.Payload.ID),
				slog.String("title", msg.Payload.Title),
			)
			return nil
		},
		events.WithDistribution(events.Broadcast),
	))

	return app.NewModule("tasks").
		Feature(app.Feature{
			ID:      "tasks/manage",
			Name:    "Task management",
			Outcome: "Users can create, list, update and complete tasks",
			Owner:   "samples",
		}).
		Provide(inject.AutoProvide(NewTaskStore)).
		Provide(inject.AutoProvide(NewTaskHandler)).
		Use(taskAPI).
		Use(taskEvents)
}

func main() {
	// Configuration: loaded from environment with sensible defaults.
	serverCfg := config.Config[http.ServerConfig]("server")
	cfg, err := config.Load(serverCfg, config.NewEnvSource("TASK"))
	if err != nil {
		logger.New("main", logger.LevelError).Error("configuration failed", err)
		os.Exit(1)
	}

	// Infrastructure: event broker for async messaging.
	broker := events.NewMemoryBroker()
	publisher := events.NewPublisher(TaskCreatedTopic, broker)

	// HTTP server with middleware.
	server := http.NewServerPlugin(cfg)
	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{
		// Keep the operational probes out of the access log.
		Exclude: []string{"/healthz", "/livez", "/readyz"},
	}))

	// Operational surface: /healthz, /livez, /readyz, /version. Use, below,
	// adds the plugin to the application: it mounts the routes on the
	// application's server and aggregates readiness probes.
	platformPlugin := platform.NewPlugin(platform.Config{
		Version: platform.VersionInfo{Name: "tasks-api", Version: "1.0.0"},
	})

	// Native feature composition. This is the sole functional declaration;
	// routes, injected services, publications, and subscriptions are derived
	// from the ordinary framework registrations inside buildTaskFeature.
	tasks := buildTaskFeature(server, broker)

	// Application wiring.
	application := app.New("tasks-api")

	// Register plugins for lifecycle management.
	application.Use(server)
	application.Use(platformPlugin)
	application.Use(tasks)

	// DI: register infrastructure as values.
	application.Provide(inject.ProvideInstance(broker))
	application.Provide(inject.ProvideInstance(publisher))
	application.Provide(inject.ProvideInstance(logger.New("tasks-api", logger.LevelDebug)))
	application.Provide(config.Provide(serverCfg, config.NewEnvSource("TASK")))

	// Start and block until shutdown signal.
	if err := application.ListenAndServe(); err != nil {
		logger.New("main", logger.LevelError).Error("application failed", err)
		os.Exit(1)
	}
}
