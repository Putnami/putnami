package events

import (
	"context"
	stderrors "errors"
	"net/http/httptest"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger/logtest"
)

// This file executes the event boundary's cases of the canonical cross-runtime log
// corpus (protocols/logging/conformance) against the records the REAL memory
// broker emits through the REAL JSON sink. Its TypeScript twin is
// typescript/framework/events/test/logging-cross-language.test.ts.
//
// It also owns the corpus's accumulation case
// (http.terminal.success-with-publishes): publishing is what accumulates onto an
// HTTP terminal record, and this module is the one that depends on
// go.putnami.dev/http (never the reverse), so the only place both halves of that
// case exist is here.
const logConformanceManifest = "../../../protocols/logging/conformance/manifest.json"

// The corpus's fixture topic; its dead-letter topic is the same name + ".dlq".
const conformanceTopicName = "logging.conformance.orders"

// The corpus pins this exact handler-failure message on every failure record, so
// the structured error — not the log message — is what carries it.
const conformanceHandlerError = "conformance handler failure"

// recordingBroker builds a real memory broker whose pinned event loggers render
// through the recorder's JSON sink, so the captured lines are exactly what a log
// aggregator would receive.
func recordingBroker(t *testing.T) (*MemoryBroker, *logtest.Recorder) {
	t.Helper()
	rec := logtest.NewRecorder(t)
	broker := NewMemoryBroker()
	broker.logs = eventLoggersFrom(rec.Root())
	return broker, rec
}

func TestLoggingConformanceEventTerminalSuccess(t *testing.T) {
	want := logtest.LoadCases(t, logConformanceManifest, "event").Case(t, "event.terminal.success")

	topic := NewTopic[string](conformanceTopicName)
	broker, rec := recordingBroker(t)
	handled := make(chan struct{}, 1)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		handled <- struct{}{}
		return nil
	})); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the delivery")
	}
	stopBroker(t, broker)

	logtest.AssertRecord(t, rec.Record(t, want), want)
}

func TestLoggingConformanceEventTerminalFailure(t *testing.T) {
	want := logtest.LoadCases(t, logConformanceManifest, "event").Case(t, "event.terminal.failure")

	topic := NewTopic[string](conformanceTopicName)
	broker, rec := recordingBroker(t)
	failed := make(chan struct{}, 1)
	// MaxRetries 1 = one total delivery attempt: no retry, no DLQ.
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		failed <- struct{}{}
		return stderrors.New(conformanceHandlerError)
	}, WithMaxRetries(1), WithDLQ(false))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the delivery")
	}
	stopBroker(t, broker)

	logtest.AssertRecord(t, rec.Record(t, want), want)
}

// The retry and dead-letter cases share one drive block in the corpus (maxRetries
// 2 + a subscribed DLQ), so they are asserted from one delivery sequence: attempt
// 1 emits the retry record with nextAttempt 2, attempt 2 emits the dead-letter
// with the TRUE final attempt 2. The dead-letter record must appear exactly once
// and never alongside a drop, preserving the terminal-outcome ordering.
func TestLoggingConformanceEventRetryAndDeadLetter(t *testing.T) {
	suite := logtest.LoadCases(t, logConformanceManifest, "event")
	wantRetry := suite.Case(t, "event.terminal.retry")
	wantDLQ := suite.Case(t, "event.terminal.dlq")

	const maxRetries = 2
	topic := NewTopic[string](conformanceTopicName)
	broker, rec := recordingBroker(t)
	signal := newFailingHandlerSignal(maxRetries)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		signal.record()
		return stderrors.New(conformanceHandlerError)
	}, WithMaxRetries(maxRetries), WithDLQ(true),
		WithBaseBackoff(time.Millisecond), WithMaxBackoff(2*time.Millisecond))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	deadLettered := make(chan struct{}, 1)
	if err := broker.Subscribe(&HandlerDefinition{
		Topic:   topic.Name + ".dlq",
		Options: DefaultHandlerOptions(),
		Handler: func(context.Context, *Envelope) error {
			deadLettered <- struct{}{}
			return nil
		},
	}); err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	signal.wait(t)
	select {
	case <-deadLettered:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the dead-letter delivery")
	}
	stopBroker(t, broker)

	logtest.AssertRecord(t, rec.Record(t, wantRetry), wantRetry)
	logtest.AssertRecord(t, rec.Record(t, wantDLQ), wantDLQ)
	if got := len(rec.Records("events.broker", "message dropped")); got != 0 {
		t.Errorf("an accepted dead-letter must not also report a drop, got %d%s", got, rec.Dump())
	}
}

// The accumulation case: two publishes inside ONE request surface on that
// request's single terminal record as an appended list plus an incremented
// counter (never last-write-wins), driven through the real HTTP middleware chain
// and the real publisher.
func TestLoggingConformanceHTTPTerminalSuccessWithPublishes(t *testing.T) {
	want := logtest.LoadCases(t, logConformanceManifest, "http").Case(t, "http.terminal.success-with-publishes")

	rec := logtest.NewRecorder(t)
	topic := NewTopic[string](conformanceTopicName)
	broker := NewMemoryBroker() // real transport; with no subscriber a publish just accepts
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	handler := phttp.Chain(
		phttp.RequestID(),
		phttp.Logging(phttp.LoggerOptions{Logger: rec.Named("http")}),
	)(func(ctx *phttp.Context) *phttp.Response {
		for i := 0; i < 2; i++ {
			if err := Publish(ctx.Context(), broker, topic, "payload"); err != nil {
				t.Errorf("publish %d: %v", i, err)
			}
		}
		return phttp.JSON(map[string]any{"ok": true})
	})
	req := httptest.NewRequest("POST", "/conformance/orders", nil)
	handler(phttp.NewContext(httptest.NewRecorder(), req))
	stopBroker(t, broker)

	logtest.AssertRecord(t, rec.Record(t, want), want)
}
