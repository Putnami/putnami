package telemetry

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"go.putnami.dev/app"
	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

func TestFromEnvironmentNeedsACollectorAndHonorsTheOptOut(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "default-from-environment", "no-endpoint-or-the-opt-out-means-no-plugin")
	t.Setenv(EnvOTLPEndpoint, "")
	if p := FromEnvironment(EnvironmentOptions{}); p != nil {
		t.Fatalf("FromEnvironment without %s = %v, want nil", EnvOTLPEndpoint, p)
	}
	t.Setenv(EnvOTLPEndpoint, "https://collector.example")
	t.Setenv(EnvSDKDisabled, "TRUE")
	if p := FromEnvironment(EnvironmentOptions{}); p != nil {
		t.Fatalf("FromEnvironment with %s=TRUE = %v, want nil", EnvSDKDisabled, p)
	}
	t.Setenv(EnvSDKDisabled, "false")
	if p := FromEnvironment(EnvironmentOptions{}); p == nil {
		t.Fatalf("FromEnvironment with an endpoint and %s=false = nil, want a plugin", EnvSDKDisabled)
	}
}

// serveOnce starts a, serves one GET /hello through server while it runs,
// then stops it.
func serveOnce(t *testing.T, a *app.Application, server *putnami_http.ServerPlugin) {
	t.Helper()
	a.Run(func(context.Context) error {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/hello", nil))
		if rec.Code != 200 {
			t.Errorf("GET /hello = %d, want 200", rec.Code)
		}
		return nil
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func helloServer() *putnami_http.ServerPlugin {
	server := putnami_http.NewServerPlugin(putnami_http.ServerConfig{Port: 0})
	server.GET("/hello", func(_ *putnami_http.Context) *putnami_http.Response { return putnami_http.JSON("hi") })
	return server
}

func TestFromEnvironmentExportsRequestMetricsAndASpanWithNoOtherWiring(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "default-from-environment", "the-environment-alone-exports-request-metrics-and-a-span")
	saveOTelGlobals(t)
	col := &testCollector{}
	srv := col.server(t)
	t.Setenv(EnvOTLPEndpoint, srv.URL+"/")
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service=team/workloads/hello")

	audience := ""
	plugin := FromEnvironment(EnvironmentOptions{
		BearerTokenSource: func(endpoint string) func(context.Context) (string, error) {
			audience = endpoint
			return func(context.Context) (string, error) { return "id-token", nil }
		},
	})
	if audience != srv.URL {
		t.Fatalf("token audience = %q, want the endpoint without its trailing slash %q", audience, srv.URL)
	}
	server := helloServer()
	a := app.New("hello")
	a.Use(plugin)
	a.Use(server)
	serveOnce(t, a, server)

	var metrics, traces string
	col.mu.Lock()
	for _, req := range col.reqs {
		if req.auth != "Bearer id-token" {
			t.Errorf("%s push carries %q, want the token source's bearer", req.path, req.auth)
		}
		switch req.path {
		case "/v1/metrics":
			metrics += string(req.body)
		case "/v1/traces":
			traces += string(req.body)
		}
	}
	col.mu.Unlock()
	for _, want := range []string{"http.server.request_count", "http.server.duration", "/hello", "team/workloads/hello"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("the metrics pushed on stop do not carry %q:\n%s", want, metrics)
		}
	}
	for _, want := range []string{"GET /hello", "team/workloads/hello"} {
		if !strings.Contains(traces, want) {
			t.Errorf("the traces pushed on stop do not carry %q:\n%s", want, traces)
		}
	}
}

func TestFromEnvironmentLeavesTheProvidersToAnExplicitPluginAndCountsOnce(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "default-from-environment", "an-explicit-plugin-owns-the-providers-and-each-request-counts-once")
	saveOTelGlobals(t)
	col := &testCollector{}
	srv := col.server(t)
	t.Setenv(EnvOTLPEndpoint, srv.URL)
	t.Setenv(EnvSDKDisabled, "")

	reader := sdkmetric.NewManualReader()
	server := helloServer()
	// The workload instruments its server itself too.
	server.Use(HTTPMiddleware("hello"))
	a := app.New("hello")
	a.Use(FromEnvironment(EnvironmentOptions{}))
	a.Use(NewPlugin(Config{ServiceName: "hello", MetricReader: reader}))
	a.Use(server)
	// A validation pass configures every plugin once before Start does again.
	if err := a.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var count int64
	a.Run(func(ctx context.Context) error {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/hello", nil))
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Errorf("collect: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "http.server.request_count" {
					for _, dp := range sum.DataPoints {
						count += dp.Value
					}
				}
			}
		}
		return nil
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if count != 1 {
		t.Fatalf("http.server.request_count = %d for one request, want 1", count)
	}
	if n := col.count(); n != 0 {
		t.Fatalf("the environment plugin pushed %d times while an explicit plugin owns the providers, want 0", n)
	}
}

// envPushHelper names the variable that turns TestHelperEnvironmentPush into
// the body of a child process.
const envPushHelper = "PUTNAMI_TELEMETRY_PUSH_HELPER"

// TestHelperEnvironmentPush is not a test of its own: run as a child process
// with envPushHelper set, it serves one request with the telemetry the
// environment gives it, so its stop pushes to the collector the environment
// names.
func TestHelperEnvironmentPush(t *testing.T) {
	if os.Getenv(envPushHelper) != "1" {
		t.Skip("runs only as the child process of a process-instance-identity test")
	}
	server := helloServer()
	a := app.New("hello")
	a.Use(FromEnvironment(EnvironmentOptions{}))
	a.Use(server)
	serveOnce(t, a, server)
}

// pushedResource is the resource of one OTLP/JSON resourceMetrics or
// resourceSpans entry.
type pushedResource struct {
	Resource struct {
		Attributes []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string `json:"stringValue"`
			} `json:"value"`
		} `json:"attributes"`
	} `json:"resource"`
}

// instanceIDsPushedByTwoProcesses starts two processes from one environment,
// extended with extra, and returns the service.instance.id of every resource
// they pushed to a test collector.
func instanceIDsPushedByTwoProcesses(t *testing.T, extra ...string) []string {
	t.Helper()
	col := &testCollector{}
	srv := col.server(t)
	env := append(os.Environ(), envPushHelper+"=1", EnvOTLPEndpoint+"="+srv.URL, EnvSDKDisabled+"=")
	env = append(env, extra...)
	for range 2 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperEnvironmentPush$", "-test.count=1")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child process: %v\n%s", err, out)
		}
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	var ids []string
	for _, req := range col.reqs {
		var body struct {
			ResourceMetrics []pushedResource `json:"resourceMetrics"`
			ResourceSpans   []pushedResource `json:"resourceSpans"`
		}
		if err := json.Unmarshal(req.body, &body); err != nil {
			t.Fatalf("%s push is not OTLP/JSON: %v", req.path, err)
		}
		for _, pushed := range append(body.ResourceMetrics, body.ResourceSpans...) {
			for _, a := range pushed.Resource.Attributes {
				if a.Key == otlp.AttrServiceInstanceID {
					ids = append(ids, req.path+"="+a.Value.StringValue)
				}
			}
		}
	}
	return ids
}

func TestTwoProcessesFromOneEnvironmentPushDistinctInstanceIDs(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "process-instance-identity", "two-processes-from-one-environment-push-distinct-instance-ids")
	ids := instanceIDsPushedByTwoProcesses(t, "OTEL_RESOURCE_ATTRIBUTES=service=team/workloads/hello,revision=hello-00001")
	// Each process pushes its metrics and its traces once, on stop.
	if len(ids) != 4 {
		t.Fatalf("pushes carrying service.instance.id = %v, want 4 (metrics and traces of two processes)", ids)
	}
	perProcess := map[string]bool{}
	for _, id := range ids {
		_, value, _ := strings.Cut(id, "=")
		if value == "" {
			t.Fatalf("a push carries an empty service.instance.id: %v", ids)
		}
		perProcess[value] = true
	}
	if len(perProcess) != 2 {
		t.Fatalf("service.instance.id values = %v, want one per process, so 2 distinct values", ids)
	}
}

func TestAnInstanceIDFromTheEnvironmentIsKept(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "process-instance-identity", "an-instance-id-from-the-environment-is-kept")
	ids := instanceIDsPushedByTwoProcesses(t, "OTEL_RESOURCE_ATTRIBUTES=service.name=hello,service.instance.id=hello-7f9c")
	if len(ids) != 4 {
		t.Fatalf("pushes carrying service.instance.id = %v, want 4 (metrics and traces of two processes)", ids)
	}
	for _, id := range ids {
		if _, value, _ := strings.Cut(id, "="); value != "hello-7f9c" {
			t.Fatalf("service.instance.id values = %v, want the value OTEL_RESOURCE_ATTRIBUTES sets in every push", ids)
		}
	}
}

func TestAnEmptyInstanceIDFromTheEnvironmentNamesTheProcess(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=hello,service.instance.id=")
	res := Config{fromEnvironment: true}.resource()
	v, ok := res.Set().Value(otlp.AttrServiceInstanceID)
	if !ok || v.AsString() != processInstanceID() {
		t.Fatalf("service.instance.id = %q (present %v), want the process's own %q", v.AsString(), ok, processInstanceID())
	}
}
