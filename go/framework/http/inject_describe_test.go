package http

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
)

// liveDependency stands for a dependency that needs a live system, such as
// *database.Pool: its constructor fails when no connection is configured.
type liveDependency struct{ name string }

func failingLiveDependency(built *atomic.Int32) func() (*liveDependency, error) {
	return func() (*liveDependency, error) {
		built.Add(1)
		return nil, errors.New("test.no_connection", "requires a connection: set DSN or Connection")
	}
}

func workingLiveDependency(built *atomic.Int32) func() *liveDependency {
	return func() *liveDependency {
		built.Add(1)
		return &liveDependency{name: "live"}
	}
}

func liveHandler() *InjectedHandler {
	return Inject(func(dep *liveDependency, ctx *Context) *Response {
		return JSON(map[string]string{"name": dep.name})
	})
}

func TestInject_DescribeBuildsNoInjectedDependency(t *testing.T) {
	var built atomic.Int32
	server := NewServerPlugin(ServerConfig{})
	server.GET("/live", liveHandler())

	a := app.New("describe-inject")
	a.ProvideFunc(failingLiveDependency(&built))
	a.Use(server)

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("describe must not build an injected dependency: %v", err)
	}
	if got := built.Load(); got != 0 {
		t.Fatalf("describe built the injected dependency %d time(s), want 0", got)
	}
}

func TestInject_StartupStillFailsFastOnAFailingDependency(t *testing.T) {
	var built atomic.Int32
	server := NewServerPlugin(ServerConfig{})
	server.GET("/live", liveHandler())

	a := app.New("startup-inject")
	a.ProvideFunc(failingLiveDependency(&built))
	a.Use(server)

	if err := a.Validate(); err == nil {
		t.Fatal("start-up must fail when an injected dependency cannot be built")
	}
	if got := built.Load(); got == 0 {
		t.Fatal("start-up did not try to build the injected dependency")
	}
}

func TestInject_LazyContainerResolvesSingletonsOnCall(t *testing.T) {
	var built atomic.Int32
	cc := inject.NewContainerContext("lazy")
	if err := cc.Register(inject.AutoProvide(workingLiveDependency(&built))); err != nil {
		t.Fatal(err)
	}
	if err := cc.StartLazy(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if !cc.Lazy() {
		t.Fatal("a context started with StartLazy must report Lazy")
	}

	ih := liveHandler()
	if err := ih.Finalize(cc); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got := built.Load(); got != 0 {
		t.Fatalf("finalize on a lazy container built the dependency %d time(s), want 0", got)
	}

	for range 2 {
		resp := ih.Handle(NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/live", nil)))
		if resp == nil || resp.Status != 200 {
			t.Fatalf("call on a lazy container: response %+v, want 200", resp)
		}
	}
	if got := built.Load(); got != 1 {
		t.Fatalf("singleton built %d time(s) across two calls, want 1", got)
	}
}

func TestInject_LazyContainerReportsAFailingDependencyOnCall(t *testing.T) {
	var built atomic.Int32
	cc := inject.NewContainerContext("lazy-failing")
	if err := cc.Register(inject.AutoProvide(failingLiveDependency(&built))); err != nil {
		t.Fatal(err)
	}
	if err := cc.StartLazy(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	ih := liveHandler()
	if err := ih.Finalize(cc); err != nil {
		t.Fatalf("finalize must not build the dependency: %v", err)
	}
	resp := ih.Handle(NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/live", nil)))
	if resp == nil || resp.Status != 500 {
		t.Fatalf("call with a failing dependency: response %+v, want 500", resp)
	}
}

func TestInject_LazyContainerStillRejectsAMissingProvider(t *testing.T) {
	cc := inject.NewContainerContext("lazy-missing")
	if err := cc.StartLazy(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := liveHandler().Finalize(cc)
	if err == nil {
		t.Fatal("a dependency with no provider must fail finalization, even on a lazy container")
	}
	if got := errors.GetCode(err); got != CodeScope {
		t.Errorf("error code = %q, want %q", got, CodeScope)
	}
}

func TestInject_RefinalizeOnAnEagerContainerResolvesSingletons(t *testing.T) {
	var built atomic.Int32
	newContainer := func(start func(*inject.ContainerContext) error) *inject.ContainerContext {
		cc := inject.NewContainerContext("refinalize")
		if err := cc.Register(inject.AutoProvide(workingLiveDependency(&built))); err != nil {
			t.Fatal(err)
		}
		if err := start(cc); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		return cc
	}
	ih := liveHandler()
	if err := ih.Finalize(newContainer((*inject.ContainerContext).StartLazy)); err != nil {
		t.Fatal(err)
	}
	eager := newContainer((*inject.ContainerContext).Start)
	if err := ih.Finalize(eager); err != nil {
		t.Fatal(err)
	}
	if ih.hasLazy {
		t.Fatal("a handler finalized on an eager container must not resolve singletons on call")
	}
	resp := ih.Handle(NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/live", nil)))
	if resp == nil || resp.Status != 200 {
		t.Fatalf("response %+v, want 200", resp)
	}
}

// livePlugin asks for the live dependency through plugin field auto-wiring.
type livePlugin struct {
	Dep *liveDependency
}

func (*livePlugin) Name() string { return "live-plugin" }

func TestDescribeWiresNoPluginFieldThatNeedsAConstructor(t *testing.T) {
	var built atomic.Int32
	plugin := &livePlugin{}
	a := app.New("describe-wire")
	a.ProvideFunc(failingLiveDependency(&built))
	a.Use(plugin)

	if err := a.Describe(t.TempDir(), nil); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := built.Load(); got != 0 {
		t.Fatalf("describe built a plugin field's dependency %d time(s), want 0", got)
	}
	if plugin.Dep != nil {
		t.Fatal("a plugin field whose provider runs a constructor must stay nil in describe")
	}
}

func TestInject_FinalizeOnAnEagerContainerBuildsEachSingleton(t *testing.T) {
	// A provider marked lazy is not built by the eager Start, so only Finalize
	// can build it: a failure here proves Finalize resolves singletons at
	// start-up on an eager container.
	var built atomic.Int32
	cc := inject.NewContainerContext("eager-finalize")
	if err := cc.Register(inject.AutoProvide(failingLiveDependency(&built), inject.WithLazy())); err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	err := liveHandler().Finalize(cc)
	if err == nil {
		t.Fatal("finalize on an eager container must fail when a singleton cannot be built")
	}
	if got := errors.GetCode(err); got != CodeScope {
		t.Errorf("error code = %q, want %q", got, CodeScope)
	}
	if got := built.Load(); got != 1 {
		t.Fatalf("finalize built the dependency %d time(s), want 1", got)
	}
}

func TestInject_AFailedRefinalizeKeepsTheHandlerWorking(t *testing.T) {
	var built atomic.Int32
	good := inject.NewContainerContext("good")
	if err := good.Register(inject.AutoProvide(workingLiveDependency(&built))); err != nil {
		t.Fatal(err)
	}
	if err := good.Start(); err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	empty := inject.NewContainerContext("empty")
	if err := empty.Start(); err != nil {
		t.Fatal(err)
	}
	defer empty.Close()

	ih := liveHandler()
	if err := ih.Finalize(good); err != nil {
		t.Fatal(err)
	}
	if err := ih.Finalize(empty); err == nil {
		t.Fatal("finalize with no provider must fail")
	}
	resp := ih.Handle(NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/live", nil)))
	if resp == nil || resp.Status != 200 {
		t.Fatalf("handler after a failed refinalize: response %+v, want 200", resp)
	}
}
