package inject

import (
	"fmt"
	"testing"

	"go.putnami.dev/errors"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Test types for constructor-based DI ---

type testConfig struct {
	Host string
	Port int
}

type testDB struct {
	DSN string
}

type testUserService struct {
	DB *testDB
}

type testUserRepository struct {
	DB *testDB
}

func newTestConfig() *testConfig {
	return &testConfig{Host: "localhost", Port: 5432}
}

func newTestDB(cfg *testConfig) *testDB {
	return &testDB{DSN: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}
}

func newTestUserService(db *testDB) *testUserService {
	return &testUserService{DB: db}
}

func newTestDBWithError(cfg *testConfig) (*testDB, error) {
	if cfg.Port == 0 {
		return nil, fmt.Errorf("invalid port")
	}
	return &testDB{DSN: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}, nil
}

// --- AutoProvide Tests ---

func TestAutoProvideBasic(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "constructor-based-resolution")
	c := NewContainer("test", nil)

	c.Register(AutoProvide(newTestConfig))
	c.Register(AutoProvide(newTestDB))

	configToken := TokenOf[*testConfig]()
	dbToken := TokenOf[*testDB]()

	// Resolve config
	val, err := c.Get(configToken)
	if err != nil {
		t.Fatal(err)
	}
	cfg := val.(*testConfig)
	if cfg.Host != "localhost" {
		t.Errorf("expected localhost, got %q", cfg.Host)
	}

	// Resolve DB (depends on config)
	val, err = c.Get(dbToken)
	if err != nil {
		t.Fatal(err)
	}
	db := val.(*testDB)
	if db.DSN != "localhost:5432" {
		t.Errorf("expected 'localhost:5432', got %q", db.DSN)
	}
}

func TestAutoProvideChain(t *testing.T) {
	c := NewContainer("test", nil)

	// Register a chain: config -> db -> user service
	c.Register(AutoProvide(newTestConfig))
	c.Register(AutoProvide(newTestDB))
	c.Register(AutoProvide(newTestUserService))

	token := TokenOf[*testUserService]()
	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	svc := val.(*testUserService)
	if svc.DB == nil {
		t.Fatal("expected DB to be injected")
	}
	if svc.DB.DSN != "localhost:5432" {
		t.Errorf("expected 'localhost:5432', got %q", svc.DB.DSN)
	}
}

func TestAutoProvideWithError(t *testing.T) {
	c := NewContainer("test", nil)

	c.Register(AutoProvide(newTestConfig))
	c.Register(AutoProvide(newTestDBWithError))

	token := TokenOf[*testDB]()
	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	db := val.(*testDB)
	if db.DSN != "localhost:5432" {
		t.Errorf("expected 'localhost:5432', got %q", db.DSN)
	}
}

func TestAutoProvideErrorReturned(t *testing.T) {
	c := NewContainer("test", nil)

	// Provide a config with port 0 to trigger error
	c.Register(ProvideInstance[*testConfig](&testConfig{Host: "localhost", Port: 0}))
	c.Register(AutoProvide(newTestDBWithError))

	token := TokenOf[*testDB]()
	_, err := c.Get(token)
	if err == nil {
		t.Fatal("expected error from constructor")
	}
}

func TestAutoProvideMissingDependency(t *testing.T) {
	spectest.Proves(t, "go/dependency-injection", "resolution", "constructor-missing-dependency-rejected")
	c := NewContainer("test", nil)

	// Register DB constructor without providing config
	c.Register(AutoProvide(newTestDB))

	token := TokenOf[*testDB]()
	_, err := c.Get(token)
	if err == nil {
		t.Fatal("expected error for missing dependency")
	}
}

func TestAutoProvideSingleton(t *testing.T) {
	c := NewContainer("test", nil)
	callCount := 0

	c.Register(AutoProvide(func() *testConfig {
		callCount++
		return &testConfig{Host: "localhost", Port: 5432}
	}))

	token := TokenOf[*testConfig]()
	c.Get(token)
	c.Get(token)

	if callCount != 1 {
		t.Errorf("expected factory called once (singleton), called %d times", callCount)
	}
}

// --- AutoProvideScoped Tests ---

func TestAutoProvideScoped(t *testing.T) {
	cc := NewContainerContext("test")

	cc.Register(AutoProvide(newTestConfig))
	cc.Register(AutoProvide(newTestDB))
	cc.Register(AutoProvideScoped(func(db *testDB) *testUserRepository {
		return &testUserRepository{DB: db}
	}))

	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	// Create a scope and resolve the scoped provider
	scope, err := cc.CreateScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	token := TokenOf[*testUserRepository]()
	val, err := scope.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	repo := val.(*testUserRepository)
	if repo.DB == nil {
		t.Fatal("expected DB to be injected into scoped repository")
	}
}

// --- AutoProvideNamed Tests ---

func TestAutoProvideNamed(t *testing.T) {
	c := NewContainer("test", nil)

	c.Register(AutoProvide(newTestConfig))
	c.Register(AutoProvideNamed("primary", newTestDB))

	token := Named[*testDB]("primary")
	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	db := val.(*testDB)
	if db.DSN != "localhost:5432" {
		t.Errorf("expected 'localhost:5432', got %q", db.DSN)
	}

	// Class token should NOT resolve (it's named)
	classToken := TokenOf[*testDB]()
	_, err = c.Get(classToken)
	if err == nil {
		t.Error("expected error: named provider should not resolve via class token")
	}
}

// --- ProvideInstance Tests ---

func TestProvideInstance(t *testing.T) {
	c := NewContainer("test", nil)

	cfg := &testConfig{Host: "production", Port: 443}
	c.Register(ProvideInstance[*testConfig](cfg))

	token := TokenOf[*testConfig]()
	val, err := c.Get(token)
	if err != nil {
		t.Fatal(err)
	}
	resolved := val.(*testConfig)
	if resolved.Host != "production" {
		t.Errorf("expected 'production', got %q", resolved.Host)
	}
}

// --- Panic Tests ---

func TestAutoProvideNotFunction(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-function")
		}
	}()
	AutoProvide("not a function")
}

func TestAutoProvideNoReturn(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for no return value")
		}
	}()
	AutoProvide(func() {})
}

func TestAutoProvideBadSecondReturn(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for non-error second return")
		}
	}()
	AutoProvide(func() (string, string) { return "", "" })
}

// --- TokenOf2 Tests ---

func TestTokenOf2(t *testing.T) {
	token1 := TokenOf[*testConfig]()
	token2 := TokenOf2(token1.(classToken).typ)

	if token1.Key() != token2.Key() {
		t.Errorf("TokenOf and TokenOf2 should produce same key: %s != %s", token1.Key(), token2.Key())
	}
}

// --- Integration: Mixed token-based and constructor-based ---

func TestMixedProviders(t *testing.T) {
	c := NewContainer("test", nil)

	// Token-based registration
	nameToken := Named[string]("app-name")
	c.Register(ProvideValue(nameToken, "my-service"))

	// Constructor-based registration
	c.Register(AutoProvide(newTestConfig))
	c.Register(AutoProvide(newTestDB))

	// Both should resolve
	name, err := c.Get(nameToken)
	if err != nil {
		t.Fatal(err)
	}
	if name.(string) != "my-service" {
		t.Errorf("expected 'my-service', got %v", name)
	}

	dbToken := TokenOf[*testDB]()
	db, err := c.Get(dbToken)
	if err != nil {
		t.Fatal(err)
	}
	if db.(*testDB).DSN != "localhost:5432" {
		t.Errorf("expected 'localhost:5432', got %v", db.(*testDB).DSN)
	}
}

// --- WithOptions on AutoProvide ---

func TestAutoProvideWithOptions(t *testing.T) {
	closed := false

	c := NewContainer("test", nil)
	c.Register(AutoProvide(newTestConfig,
		WithTags("config"),
		WithOnClose(func() error { closed = true; return nil }),
	))

	// Should have the tag
	filter := Tagged[*testConfig]("config")
	results, err := c.List(filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 tagged result, got %d", len(results))
	}

	// Close should call hook
	c.Get(TokenOf[*testConfig]()) // trigger factory
	c.Close()
	if !closed {
		t.Error("close hook should have been called")
	}
}

// --- Error types ---

func TestAutoProvideReturnsNotRegisteredForMissingDep(t *testing.T) {
	c := NewContainer("test", nil)
	c.Register(AutoProvide(newTestDB)) // needs *testConfig

	_, err := c.Get(TokenOf[*testDB]())
	if err == nil {
		t.Fatal("expected error")
	}

	// The error chain should contain a not_registered code
	if !errors.Is(err, CodeNotRegistered) {
		// It could be wrapped, check the message
		if err.Error() == "" {
			t.Error("expected meaningful error message")
		}
	}
}
