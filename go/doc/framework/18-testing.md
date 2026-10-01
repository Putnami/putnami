# Testing

Testing patterns and utilities for the Putnami Go framework.

## Testing with DI

### Fork and override

Fork a `ContainerContext` to create an isolated copy with overridden providers:

```go
import (
    "testing"
    "go.putnami.dev/inject"
)

func TestUserService(t *testing.T) {
    // Fork the production container
    fork := cc.Fork()

    // Override with test doubles
    fork.Override(inject.TokenOf[*Database](), func(r inject.Resolver) (any, error) {
        return NewMockDatabase(), nil
    })
    fork.OverrideValue(inject.TokenOf[*EmailService](), &MockEmailService{})

    // Build and start the forked container
    testCC, err := fork.Start()
    if err != nil {
        t.Fatal(err)
    }
    defer testCC.Close()

    // Resolve and test
    val, _ := testCC.Get(inject.TokenOf[*UserService]())
    userService := val.(*UserService)

    user, err := userService.Create(context.Background(), CreateInput{
        Name:  "Jane",
        Email: "jane@example.com",
    })
    if err != nil {
        t.Fatal(err)
    }
    if user.Name != "Jane" {
        t.Errorf("expected Jane, got %s", user.Name)
    }
}
```

### Manual test container

Create a standalone container for tests:

```go
func TestUserService(t *testing.T) {
    cc := inject.NewContainerContext("test")
    cc.Register(inject.ProvideValue(inject.TokenOf[*Database](), mockDB))
    cc.Register(inject.ProvideInstance(&MockEmailService{}))
    cc.Register(inject.AutoProvide(NewUserService))

    if err := cc.Start(); err != nil {
        t.Fatal(err)
    }
    defer cc.Close()

    val, _ := cc.Get(inject.TokenOf[*UserService]())
    userService := val.(*UserService)
    // ... test
}
```

### Testing scoped services

```go
func TestRequestScope(t *testing.T) {
    cc := inject.NewContainerContext("test")
    cc.Register(inject.AutoProvideScoped(NewRequestContext))

    if err := cc.Start(); err != nil {
        t.Fatal(err)
    }
    defer cc.Close()

    var id1, id2 string

    cc.Scope(context.Background(), func(ctx context.Context, scope inject.ScopeContext) error {
        val, _ := scope.Get(inject.TokenOf[*RequestContext]())
        id1 = val.(*RequestContext).ID
        return nil
    })

    cc.Scope(context.Background(), func(ctx context.Context, scope inject.ScopeContext) error {
        val, _ := scope.Get(inject.TokenOf[*RequestContext]())
        id2 = val.(*RequestContext).ID
        return nil
    })

    if id1 == id2 {
        t.Error("expected different IDs for different scopes")
    }
}
```

## Testing HTTP handlers

### Direct handler testing

Test handlers without starting a server:

```go
import (
    "net/http"
    "net/http/httptest"
    "testing"

    fhttp "go.putnami.dev/http"
)

func TestListUsers(t *testing.T) {
    // Create a mock request
    req := httptest.NewRequest("GET", "/users?page=1", nil)
    w := httptest.NewRecorder()

    // Create context and call handler
    ctx := fhttp.NewContext(w, req)
    resp := listUsersHandler(ctx)

    // Assert response
    body, _ := resp.BodyBytes()
    if resp.Status != 200 {
        t.Errorf("expected 200, got %d", resp.Status)
    }
}
```

### Testing with middleware

```go
func TestProtectedRoute(t *testing.T) {
    handler := fhttp.Chain(
        fhttp.Recovery(),
        requireAuth(),
    )(listUsersHandler)

    // Without auth
    req := httptest.NewRequest("GET", "/users", nil)
    w := httptest.NewRecorder()
    ctx := fhttp.NewContext(w, req)
    resp := handler(ctx)

    if resp.Status != 401 {
        t.Errorf("expected 401 without auth, got %d", resp.Status)
    }

    // With auth
    req = httptest.NewRequest("GET", "/users", nil)
    req.Header.Set("Authorization", "Bearer valid-token")
    w = httptest.NewRecorder()
    ctx = fhttp.NewContext(w, req)
    resp = handler(ctx)

    if resp.Status != 200 {
        t.Errorf("expected 200 with auth, got %d", resp.Status)
    }
}
```

## Testing events

### In-memory broker

Use the memory broker for testing event handlers:

```go
func TestUserCreatedHandler(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    var received bool
    handler := events.Handle(UserCreated,
        func(ctx context.Context, msg *events.Message[UserPayload]) error {
            received = true
            if msg.Payload.Name != "Jane" {
                t.Errorf("expected Jane, got %s", msg.Payload.Name)
            }
            return nil
        },
    )

    broker := events.NewMemoryBroker(events.MemoryBrokerConfig{})
    broker.Subscribe(handler)
    broker.Start(ctx)

    publisher := events.NewPublisher(UserCreated, broker)
    publisher.Publish(ctx, UserPayload{
        ID:    "123",
        Email: "jane@example.com",
        Name:  "Jane",
    })

    // Wait for handler to process
    time.Sleep(100 * time.Millisecond)

    if !received {
        t.Error("handler was not called")
    }

    broker.Stop(ctx)
}
```

## Testing storage

### Memory backend

Use the memory backend for testing file operations:

```go
func TestAvatarUpload(t *testing.T) {
    backend := storage.NewMemoryBackend()

    _, err := backend.Put(context.Background(), "avatars", "user-123/photo.png",
        bytes.NewReader([]byte("fake-image-data")),
        &storage.ObjectMetadata{ContentType: "image/png"},
    )
    if err != nil {
        t.Fatal(err)
    }

    obj, err := backend.Get(context.Background(), "avatars", "user-123/photo.png")
    if err != nil {
        t.Fatal(err)
    }
    defer obj.Body.Close()

    data, _ := io.ReadAll(obj.Body)
    if string(data) != "fake-image-data" {
        t.Error("data mismatch")
    }
}
```

## Testing cache

### Memory cache

```go
func TestCacheAside(t *testing.T) {
    c := cache.NewMemoryCache(cache.MemoryConfig{})
    defer c.Close()

    ctx := context.Background()

    // Miss
    _, found := c.Get(ctx, "key")
    if found {
        t.Error("expected cache miss")
    }

    // Set
    c.Set(ctx, "key", []byte("value"), time.Minute)

    // Hit
    val, found := c.Get(ctx, "key")
    if !found {
        t.Error("expected cache hit")
    }
    if string(val) != "value" {
        t.Errorf("expected 'value', got '%s'", string(val))
    }
}
```

## Testing logging

### Memory sink

Capture and inspect log output:

```go
func TestServiceLogging(t *testing.T) {
    sink := logger.NewMemorySink()
    log := logger.New("test", logger.LevelDebug, sink)

    service := &UserService{log: log}
    service.Create(context.Background(), input)

    if sink.Len() == 0 {
        t.Error("expected log entries")
    }

    last := sink.Last()
    if last.Level != logger.LevelInfo {
        t.Errorf("expected INFO, got %v", last.Level)
    }
}
```

## Testing configuration

```go
func TestConfig(t *testing.T) {
    def := config.Config[ServerConfig]("server")

    cfg, err := config.Load(def,
        config.NewMapSource("test", 50, map[string]any{
            "server": map[string]any{
                "host": "localhost",
                "port": 9999,
            },
        }),
    )
    if err != nil {
        t.Fatal(err)
    }
    if cfg.Port != 9999 {
        t.Errorf("expected port 9999, got %d", cfg.Port)
    }
}
```

## Test organization

Follow Go conventions:

```
mypackage/
├── service.go
├── service_test.go     # Unit tests (colocated)
├── repository.go
└── repository_test.go
```

- Test files use `*_test.go` suffix
- Use the standard `testing` package
- Colocate tests with the code they test
- Use table-driven tests for comprehensive coverage:

```go
func TestValidation(t *testing.T) {
    tests := []struct {
        name    string
        input   map[string]any
        wantErr bool
    }{
        {"valid", map[string]any{"name": "Jane", "email": "jane@example.com"}, false},
        {"missing name", map[string]any{"email": "jane@example.com"}, true},
        {"invalid email", map[string]any{"name": "Jane", "email": "not-email"}, true},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := schema.Validate(reflect.TypeOf(CreateUserInput{}), tt.input)
            if result.HasErrors() != tt.wantErr {
                t.Errorf("HasErrors() = %v, want %v", result.HasErrors(), tt.wantErr)
            }
        })
    }
}
```

## Related guides

- [Dependency Injection](/docs/frameworks/go/dependency-injection) — fork and override
- [Events](/docs/frameworks/go/events) — testing event handlers
- [Storage](/docs/frameworks/go/storage) — memory backend for tests
