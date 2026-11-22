# Build a Postgres API in Go

You will build an HTTP API in Go that stores items in Postgres, test it against a real database, and run it as a Docker image.

## Where the docs are

- On putnami.dev: [Persistence](/docs/frameworks/go/persistence), [Migrations](/docs/frameworks/go/migrations), [Testing](/docs/frameworks/go/testing#test-against-postgres), [Configuration](/docs/frameworks/go/configuration) and [Package & Docker](/docs/frameworks/go/extension/package-and-docker).
- On your machine: every `go.putnami.dev` module ships its API reference as `AI.md`, with its `doc/` folder, in the Go module cache Putnami uses: `~/.putnami/cache/go/mod/go.putnami.dev/<module>@<version>/AI.md`, such as `database@v0.4.0/AI.md`. `PUTNAMI_HOME` moves that cache to `$PUTNAMI_HOME/cache/go/mod`, and `PUTNAMI_GO_CACHE_DIR` to `$PUTNAMI_GO_CACHE_DIR/mod`.

## Steps

### 1) Create the workspace and the API project

`putnami init` sets up the workspace in the current directory:

```bash
mkdir shop && cd shop
putnami init --extension go --project api
```

The `go-server` template, the Go default, creates the project in `api/`: `main.go` composes the application and serves `GET /`, and `main_test.go` tests it. `putnami serve api` runs it.

### 2) Add the database modules

```bash
putnami deps add go.putnami.dev/database go.putnami.dev/migration --projects api
```

`go.mod` keeps a module that nothing imports yet as an indirect requirement, and the next `putnami build` drops it: write the code of the next steps before you build.

### 3) Write the migration

Create `api/migrations/001_create_items.up.sql`:

```sql
CREATE TABLE items (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL
);
```

A `.down.sql` file of the same name makes the migration reversible.

### 4) Connect the service and add the routes

Replace `api/main.go`. The framework reads no `DATABASE_URL` variable: `ServerConfig` maps it, and the database plugin takes it as its DSN. A database binding for the `default` datasource, which a deploy target injects and the test below sets, wins over it. With `AutoApply`, the service applies its migrations at startup, before it listens.

```go
package main

import (
	"embed"
	"os"
	"strconv"

	"go.putnami.dev/app"
	pconfig "go.putnami.dev/config"
	"go.putnami.dev/database"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	"go.putnami.dev/platform"
)

type ServerConfig struct {
	Port        int    `json:"port" default:"3000" env:"PORT"`
	DatabaseURL string `json:"databaseUrl" env:"DATABASE_URL"`
}

var RuntimeConfig = pconfig.Config[ServerConfig]("server")

//go:embed migrations/*.sql
var migrationsFS embed.FS

// itemsPlugin contributes the migrations of the items table.
type itemsPlugin struct{}

func (itemsPlugin) Name() string { return "items" }

func (itemsPlugin) MigrationSources() []migration.Source {
	return []migration.Source{
		database.NewSQLSource("items", database.Datasource{Name: "default", Schema: "public"}, migrationsFS),
	}
}

type Item struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

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

func newApp(cfg ServerConfig) (*app.Application, *http.ServerPlugin) {
	server := http.NewServerPlugin(http.ServerConfig{Port: cfg.Port})
	server.Use(http.Recovery())
	server.Use(http.RequestID())
	server.Use(http.Logging(http.LoggerOptions{
		Exclude: []string{"/_/health", "/livez", "/healthz", "/readyz"}, // keep probes out of the access log
	}))

	server.POST("/items", http.Inject(func(pool *database.Pool, ctx *http.Context) *http.Response {
		var item Item
		if err := ctx.Body(&item); err != nil || item.Name == "" {
			return http.JSONStatus(400, map[string]string{"message": "name is required"})
		}
		err := pool.QueryRow(ctx.Context(), "INSERT INTO items (name) VALUES ($1) RETURNING id", item.Name).Scan(&item.ID)
		if err != nil {
			return http.ErrorResponse(err)
		}
		return http.JSONStatus(201, item)
	}))

	server.GET("/items/{id}", http.Inject(func(pool *database.Pool, ctx *http.Context) *http.Response {
		id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
		if err != nil {
			return http.NotFound()
		}
		item := Item{ID: id}
		err = pool.QueryRow(ctx.Context(), "SELECT name FROM items WHERE id = $1", id).Scan(&item.Name)
		if database.IsNoRows(err) {
			return http.NotFound()
		}
		if err != nil {
			return http.ErrorResponse(err)
		}
		return http.JSON(item)
	}))

	server.DELETE("/items/{id}", http.Inject(func(pool *database.Pool, ctx *http.Context) *http.Response {
		id, err := strconv.ParseInt(ctx.Param("id"), 10, 64)
		if err != nil {
			return http.NotFound()
		}
		tag, err := pool.Exec(ctx.Context(), "DELETE FROM items WHERE id = $1", id)
		if err != nil {
			return http.ErrorResponse(err)
		}
		if tag.RowsAffected() == 0 {
			return http.NotFound()
		}
		return http.NoContent()
	}))

	a := app.New("server")
	a.Use(server)
	a.Use(platform.NewPlugin(platform.Config{}))
	a.Use(http.NewHealthPlugin())
	a.Use(database.NewPlugin(database.PluginConfig{
		Datasource: "default",
		Pool:       database.PoolConfig{DSN: cfg.DatabaseURL},
		Migration:  &database.MigrationConfig{AutoApply: true},
	}))
	a.Use(itemsPlugin{})
	return a, server
}
```

Never open the pool or query in `main` itself: the build runs `main` to describe the service, with no database.

### 5) Run it locally

`putnami serve api` starts no database: give it one in `DATABASE_URL`, such as `DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' putnami serve api`, or under `server.databaseUrl` in `api/conf/.env.local.yaml`.

### 6) Build

```bash
putnami build --projects api
```

The build writes `api/infra/requirements.json`, which says the service needs a Postgres database. Commit it: `putnami test` reads it to give the tests a database.

### 7) Test against Postgres

`putnami test` starts a throwaway Postgres, in Docker or from a local install, and `testprovider` creates an isolated database for the suite. Replace `api/main_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
)

var handler http.Handler

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	res, err := testprovider.Provision(ctx, testprovider.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = res.Cleanup() }()

	binding, err := json.Marshal(res.Binding)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = os.Setenv(database.EnvBinding, string(binding)) // the plugin connects to the test database
	_ = os.Setenv("PORT", "0")                          // the server listens on a free port

	a, server := newApp(ServerConfig{})
	a.Run(func(context.Context) error { return nil }) // Start returns once the plugins started
	if err := a.Start(ctx); err != nil {                // applies the migrations
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = a.Stop(ctx) }()
	handler = server.Handler()
	return m.Run()
}

func TestCreateReadDelete(t *testing.T) {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/items", bytes.NewBufferString(`{"name":"apple"}`)))
	if rec.Code != 201 {
		t.Fatalf("POST /items = %d %s", rec.Code, rec.Body)
	}
	var created Item
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/items/%d", created.ID)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("DELETE", path, nil))
	if rec.Code != 204 {
		t.Fatalf("DELETE %s = %d", path, rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 404 {
		t.Fatalf("GET %s after DELETE = %d", path, rec.Code)
	}
}
```

```bash
putnami test --projects api
```

When no Postgres answers, `testprovider.Provision` returns the error and the suite fails: start Docker, or install Postgres.

### 8) Package the image

`putnami package` builds only the channels you turn on, and fails for a named project with none. Turn the image channel on in `api/putnami.json`, beside what the template wrote:

```json
{
  "options": {
    "package": {
      "docker": true,
      "port": 8080
    }
  }
}
```

```bash
putnami package --projects api --docker-load --docker-tag local
```

The image is tagged `api:c-<content hash>`, and `--docker-tag local` adds `api:local`; `--docker-load` loads it into Docker. Add `--platform linux/arm64` on Apple silicon. The image sets `PORT` to the package port, 8080 when none is set.

### 9) Run the image

Run Postgres and the service on one Docker network, and pass the connection in `DATABASE_URL`. Start the service once Postgres accepts connections: it connects and migrates before it listens, and stops when the database does not answer.

```bash
docker network create shop
docker run -d --name db --network shop -e POSTGRES_PASSWORD=postgres postgres:17
until docker exec db pg_isready -h 127.0.0.1 -U postgres; do sleep 1; done
docker run --rm --network shop -p 8080:8080 \
  -e DATABASE_URL='postgres://postgres:postgres@db:5432/postgres?sslmode=disable' \
  api:local
```

The service applies its migration at startup. In another shell:

```bash
curl -s -X POST localhost:8080/items -d '{"name":"apple"}'
curl -s localhost:8080/items/1
```

## Result

You now have a Go API that stores its data in Postgres, applies its migrations, passes its tests against a real database and runs as a Docker image.
