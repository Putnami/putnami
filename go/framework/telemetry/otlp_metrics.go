package telemetry

import (
	"context"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// NewOTLPMetricReader returns a metric Reader that periodically exports the
// MeterProvider's metrics to the collector as OTLP/JSON at /v1/metrics. Wire it
// into Config.MetricReader (or let Config.OTLP do it). The MeterProvider's
// Shutdown — already called by the plugin's Stop — drives the final flush, so
// short-lived containers do not drop their last batch.
func NewOTLPMetricReader(cfg OTLPConfig) sdkmetric.Reader {
	return sdkmetric.NewPeriodicReader(
		newOTLPMetricExporter(cfg),
		sdkmetric.WithInterval(cfg.flushInterval()),
	)
}

// otlpMetricExporter implements sdkmetric.Exporter by rendering metricdata into
// OTLP/JSON and POSTing it. It uses the SDK's default temporality/aggregation
// selectors, so behavior matches the standard OTLP reader.
type otlpMetricExporter struct {
	client *otlpClient
	log    *logger.Logger
}

func newOTLPMetricExporter(cfg OTLPConfig) *otlpMetricExporter {
	return &otlpMetricExporter{
		client: newOTLPClient(cfg),
		log:    logger.Default().Named("telemetry.otlp.metrics"),
	}
}

func (e *otlpMetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

func (e *otlpMetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

// Export renders one resource's metrics and POSTs them. Per the telemetry
// protocol's drop-on-collector-error rule it logs and swallows transport
// failures rather than surfacing them, so a wedged collector never disturbs the
// workload or spams the SDK error handler.
func (e *otlpMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	req := metricsRequest(rm)
	if len(req.ResourceMetrics) == 0 || len(req.ResourceMetrics[0].ScopeMetrics) == 0 {
		return nil
	}
	body, err := otlp.MarshalCanonical(req)
	if err != nil {
		return err // a marshal failure is a bug, not a collector problem
	}
	if err := e.client.post(ctx, otlp.PathMetrics, body); err != nil {
		e.log.Warn("dropped metrics batch: " + err.Error())
	}
	return nil
}

func (e *otlpMetricExporter) ForceFlush(context.Context) error { return nil }

func (e *otlpMetricExporter) Shutdown(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// metricdata → OTLP translation
// ---------------------------------------------------------------------------

// metricsRequest renders a metricdata.ResourceMetrics into an OTLP request.
// Metrics whose data type Putnami does not emit (exponential histogram,
// summary) are skipped.
func metricsRequest(rm *metricdata.ResourceMetrics) otlp.MetricsRequest {
	sms := make([]otlp.ScopeMetrics, 0, len(rm.ScopeMetrics))
	for _, sm := range rm.ScopeMetrics {
		var ms []otlp.Metric
		for _, m := range sm.Metrics {
			if om, ok := convMetric(m); ok {
				ms = append(ms, om)
			}
		}
		if len(ms) == 0 {
			continue
		}
		sms = append(sms, otlp.ScopeMetrics{
			Scope:   otlp.Scope{Name: sm.Scope.Name, Version: sm.Scope.Version},
			Metrics: ms,
		})
	}
	if len(sms) == 0 {
		return otlp.MetricsRequest{}
	}
	return otlp.MetricsRequest{ResourceMetrics: []otlp.ResourceMetrics{{
		Resource:     otlpResourceFromSDK(rm.Resource),
		ScopeMetrics: sms,
	}}}
}

func convMetric(m metricdata.Metrics) (otlp.Metric, bool) {
	om := otlp.Metric{Name: m.Name, Description: m.Description, Unit: m.Unit}
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		om.Sum = &otlp.Sum{DataPoints: numberDP(d.DataPoints), AggregationTemporality: otlpTemporality(d.Temporality), IsMonotonic: d.IsMonotonic}
	case metricdata.Sum[float64]:
		om.Sum = &otlp.Sum{DataPoints: numberDP(d.DataPoints), AggregationTemporality: otlpTemporality(d.Temporality), IsMonotonic: d.IsMonotonic}
	case metricdata.Gauge[int64]:
		om.Gauge = &otlp.Gauge{DataPoints: numberDP(d.DataPoints)}
	case metricdata.Gauge[float64]:
		om.Gauge = &otlp.Gauge{DataPoints: numberDP(d.DataPoints)}
	case metricdata.Histogram[int64]:
		om.Histogram = &otlp.Histogram{DataPoints: histogramDP(d.DataPoints), AggregationTemporality: otlpTemporality(d.Temporality)}
	case metricdata.Histogram[float64]:
		om.Histogram = &otlp.Histogram{DataPoints: histogramDP(d.DataPoints), AggregationTemporality: otlpTemporality(d.Temporality)}
	default:
		return otlp.Metric{}, false
	}
	return om, true
}

func numberDP[N int64 | float64](dps []metricdata.DataPoint[N]) []otlp.NumberDataPoint {
	out := make([]otlp.NumberDataPoint, 0, len(dps))
	for _, dp := range dps {
		p := otlp.NumberDataPoint{
			Attributes:        otlpAttrs(dp.Attributes.ToSlice()),
			StartTimeUnixNano: nanos(dp.StartTime),
			TimeUnixNano:      nanos(dp.Time),
		}
		switch v := any(dp.Value).(type) {
		case int64:
			s := otlp.FormatInt(v)
			p.AsInt = &s
		case float64:
			f := v
			p.AsDouble = &f
		}
		out = append(out, p)
	}
	return out
}

func histogramDP[N int64 | float64](dps []metricdata.HistogramDataPoint[N]) []otlp.HistogramDataPoint {
	out := make([]otlp.HistogramDataPoint, 0, len(dps))
	for _, dp := range dps {
		p := otlp.HistogramDataPoint{
			Attributes:        otlpAttrs(dp.Attributes.ToSlice()),
			StartTimeUnixNano: nanos(dp.StartTime),
			TimeUnixNano:      nanos(dp.Time),
			Count:             otlp.FormatUint(dp.Count),
			ExplicitBounds:    dp.Bounds,
		}
		sum := float64(dp.Sum)
		p.Sum = &sum
		if len(dp.BucketCounts) > 0 {
			bc := make([]string, len(dp.BucketCounts))
			for i, c := range dp.BucketCounts {
				bc[i] = otlp.FormatUint(c)
			}
			p.BucketCounts = bc
		}
		if mn, ok := dp.Min.Value(); ok {
			f := float64(mn)
			p.Min = &f
		}
		if mx, ok := dp.Max.Value(); ok {
			f := float64(mx)
			p.Max = &f
		}
		out = append(out, p)
	}
	return out
}

// otlpTemporality maps the OTel SDK temporality enum to the OTLP wire enum.
// They differ: the SDK numbers Cumulative=1/Delta=2, OTLP numbers Delta=1/
// Cumulative=2, so a cast would invert them.
func otlpTemporality(t metricdata.Temporality) otlp.AggregationTemporality {
	switch t {
	case metricdata.DeltaTemporality:
		return otlp.TemporalityDelta
	case metricdata.CumulativeTemporality:
		return otlp.TemporalityCumulative
	default:
		return otlp.TemporalityUnspecified
	}
}
