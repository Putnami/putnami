package events

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
)

// The tests below pin the event boundary's log contract
// (protocols/logging/conformance): one terminal record per delivery under
// events.handler, structured retry/dead-letter/drop records under events.broker,
// and attempt counts that report the TRUE delivery count. They drive the real
// memory broker and read the records back through a memory sink — the log
// helpers are never called directly, so a transport that stops routing through
// the shared boundary fails these tests.

// brokerWithSink builds a memory broker whose event loggers write to a memory
// sink. The root logger is unnamed so the derived names are exactly the pinned
// "events.handler" / "events.broker" of the contract.
func brokerWithSink() (*MemoryBroker, *logger.MemorySink) {
	sink := logger.NewMemorySink()
	broker := NewMemoryBroker()
	broker.logs = eventLoggersFrom(logger.New("", logger.LevelDebug, sink))
	return broker, sink
}

// stopBroker drains the broker before the sink is read. Records are written by
// dispatch goroutines, so the drain is both the "all records emitted" barrier and
// the happens-before edge that makes reading sink.Entries race-free.
func stopBroker(t *testing.T, broker *MemoryBroker) {
	t.Helper()
	if err := broker.Stop(context.Background()); err != nil {
		t.Fatalf("stop broker: %v", err)
	}
}

// eventGroupOf returns a record's "event" group the way the JSON sink renders it:
// an attr-supplied group replaces the same-named field-bag key, so the attr wins
// when both are present.
func eventGroupOf(t *testing.T, entry logger.LogEntry) map[string]any {
	t.Helper()
	for _, attr := range entry.Attrs {
		if attr.Key != "event" {
			continue
		}
		group, ok := attr.Value.Any().(map[string]any)
		if !ok {
			t.Fatalf("event attr is not a map: %T", attr.Value.Any())
		}
		return group
	}
	if group, ok := entry.Context["event"].(map[string]any); ok {
		return group
	}
	t.Fatalf("no event group on record %q", entry.Message)
	return nil
}

// recordsOf returns every entry emitted by loggerName with the given message.
func recordsOf(sink *logger.MemorySink, loggerName, message string) []logger.LogEntry {
	var out []logger.LogEntry
	for _, entry := range sink.Entries {
		if entry.Logger == loggerName && entry.Message == message {
			out = append(out, entry)
		}
	}
	return out
}

// oneRecordOf returns the single entry emitted by loggerName with message,
// failing when the count is not exactly one.
func oneRecordOf(t *testing.T, sink *logger.MemorySink, loggerName, message string) logger.LogEntry {
	t.Helper()
	found := recordsOf(sink, loggerName, message)
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %q record from %q, got %d (all: %s)", message, loggerName, len(found), allRecords(sink))
	}
	return found[0]
}

// allRecords renders the captured records for failure messages.
func allRecords(sink *logger.MemorySink) string {
	var b strings.Builder
	for _, entry := range sink.Entries {
		b.WriteString("\n  [" + entry.Level.String() + "] " + entry.Logger + " " + entry.Message)
	}
	return b.String()
}

// errorAttrOf returns the structured error carried as an attr (the Warn path,
// which cannot use the ErrorCtx error parameter).
func errorAttrOf(t *testing.T, entry logger.LogEntry) *logger.ErrorInfo {
	t.Helper()
	for _, attr := range entry.Attrs {
		if attr.Key != "error" {
			continue
		}
		info, ok := attr.Value.Any().(*logger.ErrorInfo)
		if !ok {
			t.Fatalf("error attr is not *logger.ErrorInfo: %T", attr.Value.Any())
		}
		return info
	}
	t.Fatalf("no structured error attr on record %q", entry.Message)
	return nil
}

// failingHandlerSignal signals once, after the handler has been invoked want
// times, so a test can wait for the delivery sequence it drives instead of
// sleeping.
type failingHandlerSignal struct {
	mu    sync.Mutex
	calls int
	want  int
	done  chan struct{}
}

func newFailingHandlerSignal(want int) *failingHandlerSignal {
	return &failingHandlerSignal{want: want, done: make(chan struct{})}
}

func (s *failingHandlerSignal) record() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == s.want {
		close(s.done)
	}
}

func (s *failingHandlerSignal) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %d handler invocations", s.want)
	}
}

// A handled delivery emits exactly one terminal record: INFO "message handled"
// from events.handler, with the closed event group of the contract and the
// delivery's trace id (the message id when the envelope carries no trace).
func TestEventTerminalRecordOnSuccess(t *testing.T) {
	topic := NewTopic[string]("logging.terminal.success")
	broker, sink := brokerWithSink()

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
	if err := Publish(context.Background(), broker, topic, "payload", WithMessageID("m-success")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for delivery")
	}
	stopBroker(t, broker)

	entry := oneRecordOf(t, sink, "events.handler", "message handled")
	if entry.Level != logger.LevelInfo {
		t.Errorf("level = %v, want INFO", entry.Level)
	}
	if entry.Error != nil {
		t.Errorf("successful delivery must not carry an error, got %+v", entry.Error)
	}
	// No span is active, so the delivery seeds the trace from the message id
	// (matching the TypeScript dispatch pipeline).
	if entry.TraceID != "m-success" {
		t.Errorf("traceId = %q, want the message id", entry.TraceID)
	}

	group := eventGroupOf(t, entry)
	if len(group) != 5 {
		t.Errorf("event group must be exactly {topic,messageId,attempt,outcome,durationMs}, got %v", group)
	}
	if group["topic"] != topic.Name {
		t.Errorf("topic = %v, want %q", group["topic"], topic.Name)
	}
	if group["messageId"] != "m-success" {
		t.Errorf("messageId = %v, want m-success", group["messageId"])
	}
	if group["attempt"] != 1 {
		t.Errorf("attempt = %v, want 1", group["attempt"])
	}
	if group["outcome"] != outcomeSuccess {
		t.Errorf("outcome = %v, want success", group["outcome"])
	}
	if _, ok := group["durationMs"].(int64); !ok {
		t.Errorf("durationMs missing or not int64: %T %v", group["durationMs"], group["durationMs"])
	}
	if _, ok := group["duration"]; ok {
		t.Error("event group must use durationMs, not duration")
	}
}

// A failed delivery emits the same terminal record at ERROR with outcome
// "failure" and the structured error — never the error text in the message.
func TestEventTerminalRecordOnFailure(t *testing.T) {
	topic := NewTopic[string]("logging.terminal.failure")
	// MaxRetries 1 means one total delivery attempt: no retry follows.
	broker, sink := brokerWithSink()

	handlerErr := stderrors.New("conformance handler failure")
	failed := make(chan struct{}, 1)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		failed <- struct{}{}
		return handlerErr
	}, WithMaxRetries(1), WithDLQ(false))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload", WithMessageID("m-failure")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for delivery")
	}
	stopBroker(t, broker)

	entry := oneRecordOf(t, sink, "events.handler", "message handling failed")
	if entry.Level != logger.LevelError {
		t.Errorf("level = %v, want ERROR", entry.Level)
	}
	if entry.Error == nil {
		t.Fatal("expected the structured error on the failure terminal record")
	}
	if entry.Error.Message != handlerErr.Error() {
		t.Errorf("error.message = %q, want %q", entry.Error.Message, handlerErr.Error())
	}
	if strings.Contains(entry.Message, handlerErr.Error()) {
		t.Errorf("message must stay a stable constant, got %q", entry.Message)
	}

	group := eventGroupOf(t, entry)
	if group["outcome"] != outcomeFailure {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
	if group["attempt"] != 1 {
		t.Errorf("attempt = %v, want 1", group["attempt"])
	}
	if _, ok := group["durationMs"].(int64); !ok {
		t.Errorf("durationMs missing or not int64: %T %v", group["durationMs"], group["durationMs"])
	}
}

// The first failure of a retryable delivery emits a WARNING retry record from
// events.broker carrying the failed attempt, the scheduled nextAttempt, the
// delay, maxRetries, and the structured error as an attr.
func TestEventRetryRecordOnFirstFailure(t *testing.T) {
	topic := NewTopic[string]("logging.terminal.retry")
	broker, sink := brokerWithSink()

	handlerErr := stderrors.New("conformance handler failure")
	signal := newFailingHandlerSignal(2)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		signal.record()
		return handlerErr
	}, WithMaxRetries(2), WithDLQ(false),
		WithBaseBackoff(time.Millisecond), WithMaxBackoff(2*time.Millisecond))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload", WithMessageID("m-retry")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	signal.wait(t)
	stopBroker(t, broker)

	entry := oneRecordOf(t, sink, "events.broker", "message retry scheduled")
	if entry.Level != logger.LevelWarn {
		t.Errorf("level = %v, want WARN (a rescheduled delivery is recoverable)", entry.Level)
	}
	if info := errorAttrOf(t, entry); info.Message != handlerErr.Error() {
		t.Errorf("error.message = %q, want %q", info.Message, handlerErr.Error())
	}

	group := eventGroupOf(t, entry)
	if group["topic"] != topic.Name || group["messageId"] != "m-retry" {
		t.Errorf("event group identity = %v", group)
	}
	if group["attempt"] != 1 {
		t.Errorf("attempt = %v, want the failed delivery's attempt 1", group["attempt"])
	}
	if group["nextAttempt"] != 2 {
		t.Errorf("nextAttempt = %v, want 2", group["nextAttempt"])
	}
	if group["maxRetries"] != 2 {
		t.Errorf("maxRetries = %v, want 2 (same unit as attempt)", group["maxRetries"])
	}
	if _, ok := group["delayMs"].(int64); !ok {
		t.Errorf("delayMs missing or not int64: %T %v", group["delayMs"], group["delayMs"])
	}
	if group["outcome"] != outcomeFailure {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
	// The terminal record of each delivery is still emitted exactly once per
	// delivery, independently of the retry record.
	if got := len(recordsOf(sink, "events.handler", "message handling failed")); got != 2 {
		t.Errorf("terminal failure records = %d, want one per delivery (2)%s", got, allRecords(sink))
	}
}

// Regression for the attempt off-by-one: the exhausted-retry drop must
// report the TRUE final attempt. The pre-fix code logged env.Attempt+1 here (4 of
// 3 attempts), disagreeing with dlq.original_attempt, which recorded the raw
// value for the very same failure.
func TestEventDropRecordReportsTrueFinalAttempt(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "terminal-outcome", "exhausted-retries-without-a-dlq-report-drop")
	const maxRetries = 3
	topic := NewTopic[string]("logging.terminal.drop")
	broker, sink := brokerWithSink()

	handlerErr := stderrors.New("conformance handler failure")
	signal := newFailingHandlerSignal(maxRetries)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		signal.record()
		return handlerErr
	}, WithMaxRetries(maxRetries), WithDLQ(false),
		WithBaseBackoff(time.Millisecond), WithMaxBackoff(2*time.Millisecond))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload", WithMessageID("m-drop")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	signal.wait(t)
	stopBroker(t, broker)

	entry := oneRecordOf(t, sink, "events.broker", "message dropped")
	if entry.Level != logger.LevelError {
		t.Errorf("level = %v, want ERROR (the message is lost)", entry.Level)
	}
	if entry.Error == nil || entry.Error.Message != handlerErr.Error() {
		t.Errorf("expected the handler error on the drop record, got %+v", entry.Error)
	}

	group := eventGroupOf(t, entry)
	if group["reason"] != dropRetriesExhausted {
		t.Errorf("reason = %v, want %q", group["reason"], dropRetriesExhausted)
	}
	if group["attempt"] != maxRetries {
		t.Errorf("attempt = %v, want the true final attempt %d (never attempt+1)", group["attempt"], maxRetries)
	}
	if group["outcome"] != outcomeFailure {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
	// Retries were scheduled for every attempt but the last.
	if got := len(recordsOf(sink, "events.broker", "message retry scheduled")); got != maxRetries-1 {
		t.Errorf("retry records = %d, want %d%s", got, maxRetries-1, allRecords(sink))
	}
}

// A dead-letter reports the original delivery's true final attempt plus the DLQ
// topic it was routed to — and the record is emitted exactly once, whatever the
// number of .dlq subscribers, ONLY once the dead-letter has actually been
// accepted (see TestEventDeadLetterRefusedEnqueueReportsDropOnly for the other
// half of that ordering).
func TestEventDeadLetterRecordReportsTrueFinalAttempt(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "terminal-outcome", "dead-letter-reported-only-when-a-subscriber-accepts")
	const maxRetries = 2
	topic := NewTopic[string]("logging.terminal.dlq")
	broker, sink := brokerWithSink()

	handlerErr := stderrors.New("conformance handler failure")
	signal := newFailingHandlerSignal(maxRetries)
	if err := broker.Subscribe(Handle(topic, func(context.Context, *Message[string]) error {
		signal.record()
		return handlerErr
	}, WithMaxRetries(maxRetries), WithDLQ(true),
		WithBaseBackoff(time.Millisecond), WithMaxBackoff(2*time.Millisecond))); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	deadLettered := make(chan *Envelope, 1)
	if err := broker.Subscribe(&HandlerDefinition{
		Topic:   topic.Name + ".dlq",
		Options: DefaultHandlerOptions(),
		Handler: func(_ context.Context, env *Envelope) error {
			deadLettered <- env
			return nil
		},
	}); err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := Publish(context.Background(), broker, topic, "payload", WithMessageID("m-dlq")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	signal.wait(t)
	select {
	case env := <-deadLettered:
		// The record's attempt and the envelope attribute must agree: they are the
		// two reports of the same failure that used to disagree.
		if env.Attributes["dlq.original_attempt"] != "2" {
			t.Errorf("dlq.original_attempt = %q, want 2", env.Attributes["dlq.original_attempt"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the dead-letter delivery")
	}
	stopBroker(t, broker)

	entry := oneRecordOf(t, sink, "events.broker", "message dead-lettered")
	if entry.Level != logger.LevelError {
		t.Errorf("level = %v, want ERROR", entry.Level)
	}
	if entry.Error == nil || entry.Error.Message != handlerErr.Error() {
		t.Errorf("expected the handler error on the dead-letter record, got %+v", entry.Error)
	}

	group := eventGroupOf(t, entry)
	if group["topic"] != topic.Name {
		t.Errorf("topic = %v, want the original topic %q", group["topic"], topic.Name)
	}
	if group["messageId"] != "m-dlq" {
		t.Errorf("messageId = %v, want m-dlq", group["messageId"])
	}
	if group["attempt"] != maxRetries {
		t.Errorf("attempt = %v, want the true final attempt %d", group["attempt"], maxRetries)
	}
	if group["dlqTopic"] != topic.Name+".dlq" {
		t.Errorf("dlqTopic = %v, want %q", group["dlqTopic"], topic.Name+".dlq")
	}
	if group["outcome"] != outcomeFailure {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
	if got := len(recordsOf(sink, "events.broker", "message dropped")); got != 0 {
		t.Errorf("a dead-lettered message must not also report a drop, got %d%s", got, allRecords(sink))
	}
	// Ordering: the dead-letter record is emitted AFTER the enqueue that made the
	// dead-letter real, so every "message dead-lettered" is preceded by the record
	// of the failed final delivery, and nothing else claims this message.
	if got := len(recordsOf(sink, "events.handler", "message handling failed")); got != maxRetries {
		t.Errorf("terminal failure records = %d, want one per delivery (%d)%s", got, maxRetries, allRecords(sink))
	}
}

// The other half of the dead-letter ordering: when NO .dlq subscriber
// accepts the enqueue, the message is lost — so the broker reports a single
// message-level "message dropped" (reason dlq_enqueue_failed) and NO
// "message dead-lettered". Before the fix the dead-letter record was emitted at
// the decision point, so one message claimed both records and an operator
// alerting on "message dead-lettered" was pointed at a DLQ it never reached.
func TestEventDeadLetterRefusedEnqueueReportsDropOnly(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "terminal-outcome", "refused-dead-letter-enqueue-reports-drop-only")
	topic := NewTopic[string]("logging.terminal.dlq.refused")
	broker, sink := brokerWithSink()

	// A .dlq subscriber whose queue is already full and whose overflow policy
	// throws: its enqueue refuses the dead-letter.
	if err := broker.Subscribe(&HandlerDefinition{
		Topic: topic.Name + ".dlq",
		Options: HandlerOptions{
			Concurrency: 1,
			QueueLimit:  1,
			Overflow:    OverflowThrow,
		},
		Handler: func(context.Context, *Envelope) error { return nil },
	}); err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}
	worker := broker.subscriptions[topic.Name+".dlq"][0].worker
	worker.mu.Lock()
	worker.active = 1                              // the concurrency slot is taken…
	worker.queue = []Envelope{{Topic: topic.Name}} // …and the queue is at its limit
	worker.mu.Unlock()

	handlerErr := stderrors.New("conformance handler failure")
	env := Envelope{Topic: topic.Name, ID: "m-dlq-refused", Attempt: 2}
	broker.sendToDLQ(context.Background(), env, handlerErr)

	if got := len(recordsOf(sink, "events.broker", "message dead-lettered")); got != 0 {
		t.Errorf("a refused dead-letter must not claim a dead-letter, got %d%s", got, allRecords(sink))
	}
	entry := oneRecordOf(t, sink, "events.broker", "message dropped")
	if entry.Level != logger.LevelError {
		t.Errorf("level = %v, want ERROR (the message is lost)", entry.Level)
	}
	group := eventGroupOf(t, entry)
	if group["reason"] != dropDLQEnqueueFailed {
		t.Errorf("reason = %v, want %q", group["reason"], dropDLQEnqueueFailed)
	}
	if group["dlqTopic"] != topic.Name+".dlq" {
		t.Errorf("dlqTopic = %v, want %q", group["dlqTopic"], topic.Name+".dlq")
	}
	if group["attempt"] != 2 {
		t.Errorf("attempt = %v, want the original delivery's true final attempt 2", group["attempt"])
	}
	if group["outcome"] != outcomeFailure {
		t.Errorf("outcome = %v, want failure", group["outcome"])
	}
	if entry.Error == nil || entry.Error.Message == "" {
		t.Errorf("the drop record must carry the enqueue failure, got %+v", entry.Error)
	}
}

// A dead-letter that at least one subscriber accepts reports exactly ONE
// message-level record — the dead-letter — even when another subscriber refuses
// it: the message is not lost, so a drop record would be false. Both runtimes
// pick this behavior (Go sendToDLQ, TypeScript server/failure-router.ts).
func TestEventDeadLetterPartialAcceptanceReportsDeadLetterOnly(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "terminal-outcome", "partial-dead-letter-acceptance-reports-dead-letter-only")
	topic := NewTopic[string]("logging.terminal.dlq.partial")
	broker, sink := brokerWithSink()

	// Refusing subscriber first, accepting one second.
	if err := broker.Subscribe(&HandlerDefinition{
		Topic:   topic.Name + ".dlq",
		Options: HandlerOptions{Concurrency: 1, QueueLimit: 1, Overflow: OverflowThrow},
		Handler: func(context.Context, *Envelope) error { return nil },
	}); err != nil {
		t.Fatalf("subscribe refusing dlq: %v", err)
	}
	if err := broker.Subscribe(&HandlerDefinition{
		Topic:   topic.Name + ".dlq",
		Options: DefaultHandlerOptions(),
		Handler: func(context.Context, *Envelope) error { return nil },
	}); err != nil {
		t.Fatalf("subscribe accepting dlq: %v", err)
	}
	refusing := broker.subscriptions[topic.Name+".dlq"][0].worker
	refusing.mu.Lock()
	refusing.active = 1
	refusing.queue = []Envelope{{Topic: topic.Name}}
	refusing.mu.Unlock()

	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	broker.sendToDLQ(context.Background(), Envelope{Topic: topic.Name, ID: "m-dlq-partial", Attempt: 2}, stderrors.New("conformance handler failure"))
	stopBroker(t, broker)

	if got := len(recordsOf(sink, "events.broker", "message dropped")); got != 0 {
		t.Errorf("a message the DLQ accepted must not report a drop, got %d%s", got, allRecords(sink))
	}
	if got := len(recordsOf(sink, "events.broker", "message dead-lettered")); got != 1 {
		t.Errorf("dead-letter records = %d, want exactly 1%s", got, allRecords(sink))
	}
}

// Publishing accumulates onto the CALLER's boundary: repeated publishes inside
// one request append to event.publishes and increment event.publishCount instead
// of last-write-losing, so the enclosing terminal record carries the summary.
func TestPublishAccumulatesOntoCallerBoundary(t *testing.T) {
	topic := NewTopic[string]("logging.publish.accumulate")
	transport := &recordingTransport{}
	sink := logger.NewMemorySink()
	publisher := NewPublisher(topic, transport)
	publisher.log = logger.New("", logger.LevelDebug, sink).Named("events.publish")

	bag := logger.NewFieldBag()
	ctx := logger.ContextWithFieldBag(context.Background(), bag)
	if err := publisher.Publish(ctx, "first", WithMessageID("m-1")); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if err := publisher.Publish(ctx, "second", WithMessageID("m-2")); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	group, ok := bag.Snapshot()["event"].(map[string]any)
	if !ok {
		t.Fatalf("caller boundary carries no event group: %v", bag.Snapshot())
	}
	if group["publishCount"] != int64(2) {
		t.Errorf("publishCount = %v, want 2", group["publishCount"])
	}
	publishes, ok := group["publishes"].([]any)
	if !ok {
		t.Fatalf("publishes is not a list: %T %v", group["publishes"], group["publishes"])
	}
	if len(publishes) != 2 {
		t.Fatalf("publishes = %v, want both publishes (no last-write loss)", publishes)
	}
	for i, want := range []string{"m-1", "m-2"} {
		entry, ok := publishes[i].(map[string]any)
		if !ok {
			t.Fatalf("publishes[%d] is not a map: %T", i, publishes[i])
		}
		if entry["messageId"] != want || entry["topic"] != topic.Name {
			t.Errorf("publishes[%d] = %v, want topic %q messageId %q", i, entry, topic.Name, want)
		}
	}

	// Each publish also leaves its own debug record naming just that publish, so a
	// detached publish (no caller bag) is still observable.
	published := recordsOf(sink, "events.publish", "message published")
	if len(published) != 2 {
		t.Fatalf("expected 2 %q records, got %d%s", "message published", len(published), allRecords(sink))
	}
	first := eventGroupOf(t, published[0])
	if first["messageId"] != "m-1" || first["topic"] != topic.Name {
		t.Errorf("first publish record group = %v", first)
	}
}
