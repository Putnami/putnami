# Telemetry

`go.putnami.dev/telemetry` provides OpenTelemetry integration for distributed tracing and metrics collection.

## Plugin setup

```go
import (
    "go.putnami.dev/app"
    "go.putnami.dev/telemetry"
)

a := app.New("my-service")
a.Module.Use(telemetry.NewPlugin(telemetry.Config{
    ServiceName:    "my-service",
    ServiceVersion: "1.0.0",
    TraceSampleRate: telemetry.SampleRate(0.1), // Sample 10% of traces
}))
```

The plugin:
- Initializes the OpenTelemetry SDK
- Registers `trace.TracerProvider` and `metric.MeterProvider` in DI
- Shuts down exporters gracefully on application stop

### Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ServiceName` | `string` | — | Service name for telemetry data |
| `ServiceVersion` | `string` | — | Service version |
| `TraceSampleRate` | `*float64` | `telemetry.SampleRate(1.0)` when nil | Trace sampling rate (0.0–1.0), set with `telemetry.SampleRate(rate)` |
| `TraceExporter` | exporter | — | Custom trace exporter |
| `MetricReader` | reader | — | Custom metric reader |

## Tracing

### Creating spans

```go
import "go.putnami.dev/telemetry"

func processOrder(ctx context.Context, orderID string) error {
    ctx, span := telemetry.StartSpan(ctx, "processOrder", "order")
    defer span.End()

    telemetry.SpanAttrs(span, "orderId", orderID)

    if err := chargePayment(ctx, orderID); err != nil {
        telemetry.SetSpanError(span, err)
        return err
    }

    telemetry.SetSpanOK(span)
    return nil
}
```

### Getting a tracer

```go
tracer := telemetry.Tracer("my-service")

ctx, span := tracer.Start(ctx, "operation-name")
defer span.End()
```

### Span helpers

| Function | Description |
|----------|-------------|
| `StartSpan(ctx, name, op)` | Create a child span |
| `SetSpanError(span, err)` | Record an error on the span |
| `SetSpanOK(span)` | Mark the span as successful |
| `SetSpanErrorStructured(span, err)` | Record a structured error (bridges `go.putnami.dev/errors`) |
| `SpanAttrs(span, key, value, ...)` | Set span attributes |
| `TraceIDFromContext(ctx)` | Extract trace ID (32-char hex) |

### Trace ID extraction

```go
traceID := telemetry.TraceIDFromContext(ctx)
// "4bf92f3577b34da6a3ce929d0e0e4736"
```

## Metrics

### Creating instruments

```go
import "go.putnami.dev/telemetry"

meter := telemetry.Meter("my-service")

// Counter
counter, _ := telemetry.Counter(meter, "requests_total")
counter.Add(ctx, 1)

// Histogram
histogram, _ := telemetry.Histogram(meter, "request_duration_ms")
histogram.Record(ctx, 42.5)

// Gauge
gauge, _ := telemetry.Gauge(meter, "active_connections")
gauge.Record(ctx, 15.0)

// Up/down counter
upDown, _ := telemetry.UpDownCounter(meter, "queue_size")
upDown.Add(ctx, 1)  // item added
upDown.Add(ctx, -1) // item removed
```

### Metric helpers

| Function | Returns | Description |
|----------|---------|-------------|
| `Counter(meter, name)` | `metric.Int64Counter` | Monotonically increasing counter |
| `Histogram(meter, name, opts...)` | `metric.Float64Histogram` | Distribution of values |
| `Gauge(meter, name, opts...)` | `metric.Float64Gauge` | Point-in-time value |
| `UpDownCounter(meter, name)` | `metric.Int64UpDownCounter` | Counter that can increase or decrease |

## HTTP middleware

Auto-trace HTTP requests with the telemetry middleware:

```go
import (
    fhttp "go.putnami.dev/http"
    "go.putnami.dev/telemetry"
)

server := fhttp.NewServerPlugin(fhttp.ServerConfig{Port: 3000})
server.Use(telemetry.HTTPMiddleware("my-service"))
```

The middleware automatically:
- Creates a span per request
- Records `http.method`, `http.path`, `http.status_code` attributes
- Emits `http.server.request_count` and `http.server.duration` metrics
- Propagates trace context to downstream services

## Error bridge

When the telemetry plugin is active, structured errors from `go.putnami.dev/errors` automatically emit an `errors_total` counter with labels:

| Label | Source |
|-------|--------|
| `error.code` | Error code |
| `error.category` | Error category |
| `error.source` | Package where the error originated |

```go
// This error automatically increments errors_total{code="db.query", category="infra"}
err := errors.Wrap(dbErr, "db.query").WithCategory(errors.CategoryInfra)
```

## Patterns

### Service-level tracing

```go
type OrderService struct {
    tracer trace.Tracer
    repo   *OrderRepository
}

func NewOrderService(repo *OrderRepository) *OrderService {
    return &OrderService{
        tracer: telemetry.Tracer("OrderService"),
        repo:   repo,
    }
}

func (s *OrderService) Create(ctx context.Context, input CreateOrderInput) (*Order, error) {
    ctx, span := s.tracer.Start(ctx, "CreateOrder")
    defer span.End()

    order, err := s.repo.Create(ctx, input)
    if err != nil {
        telemetry.SetSpanError(span, err)
        return nil, err
    }

    span.SetAttributes(attribute.String("orderId", order.ID))
    return order, nil
}
```

### Custom metrics

```go
type PaymentService struct {
    meter          metric.Meter
    paymentCounter metric.Int64Counter
    amountHist     metric.Float64Histogram
}

func NewPaymentService() *PaymentService {
    meter := telemetry.Meter("payments")
    counter, _ := telemetry.Counter(meter, "payments_total")
    hist, _ := telemetry.Histogram(meter, "payment_amount")
    return &PaymentService{meter: meter, paymentCounter: counter, amountHist: hist}
}

func (s *PaymentService) Process(ctx context.Context, amount float64) error {
    s.paymentCounter.Add(ctx, 1)
    s.amountHist.Record(ctx, amount)
    return nil
}
```

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — HTTP tracing
- [Errors](/docs/frameworks/go/errors) — error metrics
- [Logging](/docs/frameworks/go/logging) — trace-correlated logs
## Support and contract

`go.putnami.dev/telemetry` is `stable` in the workspace support catalog. Its
behavior is defined by the service telemetry specification and two accepted
decision records next to the package source (`go/framework/telemetry/specs/` and
`go/framework/telemetry/doc/adr/`). Export failures are dropped rather than
propagated, so an unreachable collector degrades observability and nothing else.
Before v1.0.0 a minor `0.x` release may still contain a documented breaking
change; strict compatibility between every pre-1.0 minor is not promised.
