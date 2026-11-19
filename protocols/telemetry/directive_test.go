package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// DirectiveFixture is fixtures/conventions/export-directives.json, which the
// Go and TypeScript runtimes read too.
type DirectiveFixture struct {
	Header string `json:"header"`
	Keys   struct {
		TraceRatio string `json:"traceRatio"`
		TraceFloor string `json:"traceFloor"`
		LogLevel   string `json:"logLevel"`
		TTL        string `json:"ttl"`
	} `json:"keys"`
	MaxTTLSeconds int64 `json:"maxTtlSeconds"`
	ParseCases    []struct {
		Value     string            `json:"value"`
		Directive *FixtureDirective `json:"directive"`
	} `json:"parseCases"`
	LimitCases []struct {
		Name      string        `json:"name"`
		Own       FixtureLimits `json:"own"`
		Directive string        `json:"directive"`
		Effective FixtureLimits `json:"effective"`
	} `json:"limitCases"`
	TraceScenarios []struct {
		Name     string           `json:"name"`
		Settings TraceSampling    `json:"settings"`
		Events   []DirectiveEvent `json:"events"`
	} `json:"traceScenarios"`
	LogScenarios []struct {
		Name   string           `json:"name"`
		Level  string           `json:"level"`
		Events []DirectiveEvent `json:"events"`
	} `json:"logScenarios"`
	RetryAfter struct {
		Status     int   `json:"status"`
		MaxSeconds int64 `json:"maxSeconds"`
		Cases      []struct {
			Status       int    `json:"status"`
			Value        string `json:"value"`
			NowSecond    int64  `json:"nowSecond"`
			DelaySeconds int64  `json:"delaySeconds"`
		} `json:"cases"`
	} `json:"retryAfter"`
}

// FixtureDirective is a parsed directive as the fixture writes it.
type FixtureDirective struct {
	TraceRatio *float64 `json:"traceRatio"`
	TraceFloor *int64   `json:"traceFloor"`
	LogLevel   string   `json:"logLevel"`
	TTLSeconds int64    `json:"ttlSeconds"`
}

// FixtureLimits are the trace settings and the log level of a service.
type FixtureLimits struct {
	Traces   TraceSampling `json:"traces"`
	LogLevel string        `json:"logLevel"`
}

// DirectiveEvent is a directive header arriving, a root span, or a log
// record, at a second of a scenario.
type DirectiveEvent struct {
	AtSecond  int64  `json:"atSecond"`
	Directive string `json:"directive"`
	TraceID   string `json:"traceId"`
	Kept      bool   `json:"kept"`
	Record    string `json:"record"`
	Emitted   bool   `json:"emitted"`
}

func readDirectiveFixture(t *testing.T) DirectiveFixture {
	t.Helper()
	data, err := os.ReadFile("fixtures/conventions/export-directives.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture DirectiveFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

func mustParseDirective(t *testing.T, value string) ExportDirective {
	t.Helper()
	directive, ok := ParseExportDirective(value)
	if !ok {
		t.Fatalf("ParseExportDirective(%q) is void", value)
	}
	return directive
}

func TestDirective_TheHeaderConventionMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "the-export-directive-header", "the-header-convention-matches-its-fixture")
	fixture := readDirectiveFixture(t)
	if fixture.Header != HeaderExportDirective {
		t.Errorf("header = %q, want %q", fixture.Header, HeaderExportDirective)
	}
	if fixture.Keys.TraceRatio != DirectiveTraceRatio || fixture.Keys.TraceFloor != DirectiveTraceFloor ||
		fixture.Keys.LogLevel != DirectiveLogLevel || fixture.Keys.TTL != DirectiveTTL {
		t.Errorf("keys = %+v", fixture.Keys)
	}
	if fixture.MaxTTLSeconds != MaxDirectiveTTLSeconds {
		t.Errorf("maxTtlSeconds = %d, want %d", fixture.MaxTTLSeconds, MaxDirectiveTTLSeconds)
	}
	if len(fixture.ParseCases) == 0 || len(fixture.LimitCases) == 0 {
		t.Fatal("the fixture holds no parse case or no limit case")
	}
	for _, c := range fixture.ParseCases {
		got, ok := ParseExportDirective(c.Value)
		if c.Directive == nil {
			if ok {
				t.Errorf("ParseExportDirective(%q) = %+v, want a void header", c.Value, got)
			}
			continue
		}
		if !ok {
			t.Errorf("ParseExportDirective(%q) is void, want %+v", c.Value, *c.Directive)
			continue
		}
		want := *c.Directive
		if !equalFloat(got.TraceRatio, want.TraceRatio) || !equalInt(got.TraceFloorPerMinute, want.TraceFloor) ||
			got.LogLevel != want.LogLevel || got.TTL != time.Duration(want.TTLSeconds)*time.Second {
			t.Errorf("ParseExportDirective(%q) = %+v, want %+v", c.Value, got, want)
		}
	}
	for _, c := range fixture.LimitCases {
		directive := mustParseDirective(t, c.Directive)
		if got := directive.LimitTraceSampling(c.Own.Traces); got != c.Effective.Traces {
			t.Errorf("%s: traces = %+v, want %+v", c.Name, got, c.Effective.Traces)
		}
		if got := directive.LimitLogLevel(c.Own.LogLevel); got != c.Effective.LogLevel {
			t.Errorf("%s: log level = %q, want %q", c.Name, got, c.Effective.LogLevel)
		}
	}
}

func TestDirective_TheRetryAfterConventionMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "too-many-requests-delays-a-signal", "the-retry-after-convention-matches-its-fixture")
	fixture := readDirectiveFixture(t)
	if fixture.RetryAfter.Status != 429 || fixture.RetryAfter.MaxSeconds != MaxRetryAfterSeconds {
		t.Errorf("retryAfter = status %d, max %d", fixture.RetryAfter.Status, fixture.RetryAfter.MaxSeconds)
	}
	if len(fixture.RetryAfter.Cases) == 0 {
		t.Fatal("the fixture holds no Retry-After case")
	}
	for _, c := range fixture.RetryAfter.Cases {
		got := RetryAfterDelay(c.Status, c.Value, time.Unix(c.NowSecond, 0))
		if want := time.Duration(c.DelaySeconds) * time.Second; got != want {
			t.Errorf("RetryAfterDelay(%d, %q) = %v, want %v", c.Status, c.Value, got, want)
		}
	}
}

// TestDirective_TheBudgetAppliesTheTraceScenarios runs each trace scenario
// through TraceBudget.Restrict, the rule the Go sampler applies.
func TestDirective_TheBudgetAppliesTheTraceScenarios(t *testing.T) {
	for _, scenario := range readDirectiveFixture(t).TraceScenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			budget, err := NewTraceBudget(scenario.Settings)
			if err != nil {
				t.Fatal(err)
			}
			for i, event := range scenario.Events {
				at := time.Unix(event.AtSecond, 0)
				if event.Directive != "" {
					if directive, ok := ParseExportDirective(event.Directive); ok {
						budget.Restrict(directive, at)
					}
					continue
				}
				if got := budget.Decide(traceIDOf(t, event.TraceID), at); got != event.Kept {
					t.Errorf("event %d at %ds: kept = %v, want %v", i, event.AtSecond, got, event.Kept)
				}
			}
			if budget.Settings() != scenario.Settings {
				t.Errorf("a directive changed the settings to %+v", budget.Settings())
			}
		})
	}
}

func TestDirective_InForceReturnsToTheSettingsAtTheTTL(t *testing.T) {
	budget, err := NewTraceBudget(TraceSampling{Budget: 1, Ratio: 0.5, FloorPerMinute: 2})
	if err != nil {
		t.Fatal(err)
	}
	budget.Restrict(mustParseDirective(t, "trace-ratio=0.1, trace-floor=1, ttl=10"), time.Unix(100, 0))
	if got := budget.InForce(time.Unix(109, 0)); got != (TraceSampling{Budget: 1, Ratio: 0.1, FloorPerMinute: 1}) {
		t.Errorf("in force at 109 = %+v", got)
	}
	if got := budget.InForce(time.Unix(110, 0)); got != budget.Settings() {
		t.Errorf("in force at 110 = %+v, want the settings", got)
	}
}

func TestDirective_LimitLogLevelKeepsAnUnknownOwnLevelBelowTheDirective(t *testing.T) {
	if got := (ExportDirective{}).LimitLogLevel("info"); got != "info" {
		t.Errorf("no level = %q, want info", got)
	}
	if got := (ExportDirective{LogLevel: "warn"}).LimitLogLevel(""); got != "warn" {
		t.Errorf("unknown own level = %q, want warn", got)
	}
}

func equalFloat(got, want *float64) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}

func equalInt(got, want *int64) bool {
	return (got == nil) == (want == nil) && (got == nil || *got == *want)
}
