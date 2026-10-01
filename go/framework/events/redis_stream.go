package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

// codeEventsRedisStream identifies Redis Streams transport failures.
const codeEventsRedisStream errors.Code = "events.redis_stream"

// RedisCommandClient is the minimal command client surface needed by RedisStreamTransport.
type RedisCommandClient interface {
	Do(ctx context.Context, command string, args ...string) (any, error)
}

// RedisStreamTransportConfig configures a reliable Redis Streams event transport.
type RedisStreamTransportConfig struct {
	Client       RedisCommandClient
	KeyPrefix    string
	GroupPrefix  string
	ConsumerName string
	MaxLen       int
	BlockTimeout time.Duration
	Count        int
	OnError      func(error)
}

// RedisStreamTransport maps event topics to Redis Streams with consumer groups.
type RedisStreamTransport struct {
	client       RedisCommandClient
	keyPrefix    string
	groupPrefix  string
	consumerName string
	maxLen       int
	blockTimeout time.Duration
	count        int
	onError      func(error)
	logs         eventLoggers

	mu      sync.Mutex
	subs    []*redisStreamSubscription
	ctx     context.Context
	cancel  context.CancelFunc
	running bool
	wg      sync.WaitGroup
}

type redisStreamSubscription struct {
	def   *HandlerDefinition
	group string
	id    string
	// retryID is the independent read cursor for this subscription's
	// per-group retry stream. Retries are re-published to a stream keyed by
	// BOTH topic and group so only THIS group re-consumes them, never the
	// other broadcast groups reading the shared topic stream.
	retryID string
}

type redisStreamRecord struct {
	id      string
	message string
}

// NewRedisStreamTransport creates a Redis Streams event transport.
func NewRedisStreamTransport(config RedisStreamTransportConfig) *RedisStreamTransport {
	blockTimeout := config.BlockTimeout
	if blockTimeout == 0 {
		blockTimeout = 5 * time.Second
	}
	count := config.Count
	if count == 0 {
		count = 100
	}
	consumerName := config.ConsumerName
	if consumerName == "" {
		consumerName = "putnami-" + generateID()
	}
	return &RedisStreamTransport{
		client:       config.Client,
		keyPrefix:    config.KeyPrefix,
		groupPrefix:  config.GroupPrefix,
		consumerName: consumerName,
		maxLen:       config.MaxLen,
		blockTimeout: blockTimeout,
		count:        count,
		onError:      config.OnError,
		logs:         newEventLoggers(),
	}
}

// Publish appends an envelope to the Redis stream for its topic.
func (t *RedisStreamTransport) Publish(ctx context.Context, env Envelope) error {
	return t.xadd(ctx, t.streamFor(env.Topic), env)
}

// Subscribe registers a handler in a Redis consumer group for the topic.
func (t *RedisStreamTransport) Subscribe(def *HandlerDefinition) error {
	if t.client == nil {
		return errors.New(codeEventsRedisStream, "redis command client is required")
	}
	t.mu.Lock()
	sub := &redisStreamSubscription{
		def:     def,
		group:   t.groupFor(def, len(t.subs)),
		id:      "0",
		retryID: "0",
	}
	t.subs = append(t.subs, sub)
	running := t.running
	ctx := t.ctx
	t.mu.Unlock()
	if running {
		if err := t.ensureGroup(ctx, sub); err != nil {
			return err
		}
		t.startSubscription(ctx, sub)
	}
	return nil
}

// Start ensures consumer groups and begins consuming registered streams.
func (t *RedisStreamTransport) Start(ctx context.Context) error {
	if t.client == nil {
		return errors.New(codeEventsRedisStream, "redis command client is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.ctx = runCtx
	t.cancel = cancel
	t.running = true
	subs := append([]*redisStreamSubscription(nil), t.subs...)
	t.mu.Unlock()
	for _, sub := range subs {
		if err := t.ensureGroup(runCtx, sub); err != nil {
			cancel()
			return err
		}
		t.startSubscription(runCtx, sub)
	}
	return nil
}

// Stop cancels consumers and closes the Redis client when supported.
func (t *RedisStreamTransport) Stop(_ context.Context) error {
	t.mu.Lock()
	t.running = false
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Unlock()
	t.wg.Wait()
	if closer, ok := t.client.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func (t *RedisStreamTransport) startSubscription(ctx context.Context, sub *redisStreamSubscription) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.consume(ctx, sub)
	}()
}

func (t *RedisStreamTransport) consume(ctx context.Context, sub *redisStreamSubscription) {
	for {
		t.mu.Lock()
		running := t.running
		t.mu.Unlock()
		if !running || ctx.Err() != nil {
			return
		}
		// Poll the shared topic stream (blocking, the primary source), then this
		// group's own retry stream (non-blocking). The retry stream is scoped to
		// (topic, group) so a retry re-published here is re-consumed ONLY by this
		// subscription's group — other broadcast groups reading the shared topic
		// stream never see it. The retry read is non-blocking on purpose: blocking
		// on the (usually idle) retry stream would stall every iteration for the
		// full blockTimeout before returning to the topic, throttling topic
		// throughput to COUNT events per blockTimeout window.
		if fatal := t.consumeStream(ctx, sub, t.streamFor(sub.def.Topic), &sub.id, t.blockTimeout); fatal {
			return
		}
		if fatal := t.consumeStream(ctx, sub, t.retryStreamFor(sub.def.Topic, sub.group), &sub.retryID, 0); fatal {
			return
		}
	}
}

// consumeStream reads and dispatches one batch from a single stream using the
// supplied cursor. It preserves the pending-drain cursor semantics: cursor "0"
// (or a concrete ID) drains this group's PEL from that point, and an empty
// batch advances the cursor to ">" for live delivery. A positive block adds a
// BLOCK wait (for the primary topic stream); block <= 0 issues a non-blocking
// read (for the secondary retry stream, so it never stalls the loop). It
// returns true when the caller should stop consuming (context canceled or
// transport stopped).
func (t *RedisStreamTransport) consumeStream(ctx context.Context, sub *redisStreamSubscription, streamKey string, cursor *string, block time.Duration) bool {
	args := []string{"GROUP", sub.group, t.consumerName}
	if block > 0 {
		args = append(args, "BLOCK", fmt.Sprintf("%d", block.Milliseconds()))
	}
	args = append(args, "COUNT", fmt.Sprintf("%d", t.count), "STREAMS", streamKey, *cursor)
	response, err := t.client.Do(ctx, "XREADGROUP", args...)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		t.reportError(err)
		time.Sleep(50 * time.Millisecond)
		return false
	}
	records := parseRedisStreamResponse(response, streamKey)
	if len(records) == 0 {
		*cursor = ">"
		return false
	}
	drainingPending := *cursor != ">"
	for _, record := range records {
		if err := t.handleRecord(ctx, sub, streamKey, record); err != nil {
			t.reportError(err)
		}
	}
	if drainingPending {
		*cursor = records[len(records)-1].id
	}
	return false
}

func (t *RedisStreamTransport) handleRecord(ctx context.Context, sub *redisStreamSubscription, streamKey string, record redisStreamRecord) error {
	var env Envelope
	if err := json.Unmarshal([]byte(record.message), &env); err != nil {
		if _, ackErr := t.client.Do(ctx, "XACK", streamKey, sub.group, record.id); ackErr != nil {
			return errors.Wrap(ackErr, codeEventsRedisStream, errors.String("phase", "xack_decode_failure"))
		}
		return errors.Wrap(err, codeEventsRedisStream, errors.String("phase", "decode"))
	}
	// Strip caller-supplied auth.* on ingress (anti-spoofing), matching push.
	stripAuthAttributes(env.Attributes)
	if env.Attempt < 1 {
		env.Attempt = 1
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	// The boundary context carries the delivery's trace id and event group, so the
	// retry/dead-letter records below correlate with the terminal record. It only
	// adds values to ctx: cancellation and deadlines are unchanged.
	deliveryCtx, err := invokeTransportHandler(ctx, t.logs, sub.def, env)
	if err != nil {
		return t.handleFailure(deliveryCtx, err, env, sub, streamKey, record.id)
	}
	_, ackErr := t.client.Do(ctx, "XACK", streamKey, sub.group, record.id)
	return ackErr
}

func (t *RedisStreamTransport) handleFailure(ctx context.Context, originalErr error, env Envelope, sub *redisStreamSubscription, streamKey, recordID string) error {
	opts := normalizeHandlerOptions(sub.def.Options)
	if env.Attempt < opts.MaxRetries {
		delay := retryBackoff(env.Attempt, opts.BaseBackoff, opts.MaxBackoff)
		// WARNING: recoverable failure, so the structured error travels as an attr
		// instead of promoting the record to ERROR.
		t.logs.broker.WarnCtx(ctx, "message retry scheduled",
			retryEventAttr(env, delay, opts),
			logger.ErrorAttr(originalErr),
		)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		retryEnv := env
		retryEnv.Attempt++
		// Re-publish to THIS group's retry stream, not the shared topic stream.
		// XADDing back to streamFor(topic) would make the retry visible to every
		// other broadcast group's ">" cursor, re-running handlers that already
		// succeeded (cross-group fan-out amplification). The (topic, group)-keyed
		// retry stream is read only by this subscription's group loop.
		if err := t.xadd(ctx, t.retryStreamFor(retryEnv.Topic, sub.group), retryEnv); err != nil {
			return err
		}
		_, err := t.client.Do(ctx, "XACK", streamKey, sub.group, recordID)
		return err
	}
	if !opts.DLQ {
		// Retries are exhausted with no DLQ: the record is acknowledged and the
		// message is lost. attempt is the true final delivery count, not attempt+1.
		t.logs.broker.ErrorCtx(ctx, "message dropped", originalErr, eventAttr(env, map[string]any{
			"reason":  dropRetriesExhausted,
			"outcome": outcomeFailure,
		}))
		_, err := t.client.Do(ctx, "XACK", streamKey, sub.group, recordID)
		return err
	}
	dlqEnv := env
	dlqEnv.Topic = env.Topic + ".dlq"
	dlqEnv.Attempt = 1
	if dlqEnv.Attributes == nil {
		dlqEnv.Attributes = map[string]string{}
	}
	dlqEnv.Attributes["dlq.original_topic"] = env.Topic
	dlqEnv.Attributes["dlq.original_attempt"] = fmt.Sprintf("%d", env.Attempt)
	dlqEnv.Attributes["dlq.error"] = originalErr.Error()
	if err := t.xadd(ctx, t.streamFor(dlqEnv.Topic), dlqEnv); err != nil {
		return err
	}
	// Emitted only once the dead-letter is actually written: a failed XADD leaves
	// the record pending in the group's PEL for redelivery, so claiming a
	// dead-letter there would be both premature and duplicated on the retry. The
	// group describes the ORIGINAL delivery (topic, true final attempt) plus the
	// dlqTopic it was routed to.
	t.logs.broker.ErrorCtx(ctx, "message dead-lettered", originalErr, eventAttr(env, map[string]any{
		"dlqTopic": dlqEnv.Topic,
		"outcome":  outcomeFailure,
	}))
	_, err := t.client.Do(ctx, "XACK", streamKey, sub.group, recordID)
	return err
}

func (t *RedisStreamTransport) ensureGroup(ctx context.Context, sub *redisStreamSubscription) error {
	if err := t.createGroup(ctx, t.streamFor(sub.def.Topic), sub.group); err != nil {
		return err
	}
	// The per-group retry stream needs its own consumer group so this loop can
	// XREADGROUP its retries. MKSTREAM creates it empty, so "$" == "0" here and
	// retries (only ever XADD'd after this group exists) are always delivered.
	return t.createGroup(ctx, t.retryStreamFor(sub.def.Topic, sub.group), sub.group)
}

func (t *RedisStreamTransport) createGroup(ctx context.Context, streamKey, group string) error {
	_, err := t.client.Do(ctx, "XGROUP", "CREATE", streamKey, group, "$", "MKSTREAM")
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return errors.Wrap(err, codeEventsRedisStream, errors.String("phase", "xgroup"))
	}
	return nil
}

func (t *RedisStreamTransport) xadd(ctx context.Context, streamKey string, env Envelope) error {
	if t.client == nil {
		return errors.New(codeEventsRedisStream, "redis command client is required")
	}
	data, err := json.Marshal(env)
	if err != nil {
		return errors.Wrap(err, codeEventsRedisStream, errors.String("phase", "marshal"))
	}
	args := []string{streamKey}
	if t.maxLen > 0 {
		args = append(args, "MAXLEN", "~", fmt.Sprintf("%d", t.maxLen))
	}
	args = append(args, "*", "message", string(data))
	_, err = t.client.Do(ctx, "XADD", args...)
	return err
}

func (t *RedisStreamTransport) streamFor(topic string) string {
	return redisKey(t.keyPrefix, topic)
}

// retryStreamFor returns the retry stream key scoped to BOTH the topic and the
// consuming group, so retries are isolated to the group that failed and never
// redelivered to the other broadcast groups reading the shared topic stream.
func (t *RedisStreamTransport) retryStreamFor(topic, group string) string {
	return t.streamFor(topic) + ":" + group + ":retry"
}

func (t *RedisStreamTransport) groupFor(def *HandlerDefinition, index int) string {
	if def.Options.Group != "" {
		return redisKey(t.groupPrefix, def.Options.Group)
	}
	name := strings.ReplaceAll(def.Topic, ".", "-")
	if def.Options.Distribution == Broadcast {
		return redisKey(t.groupPrefix, fmt.Sprintf("broadcast:%s:%d", name, index))
	}
	return redisKey(t.groupPrefix, "competing:"+name)
}

func (t *RedisStreamTransport) reportError(err error) {
	if t.onError != nil {
		t.onError(err)
	}
}

func redisKey(prefix, topic string) string {
	if prefix == "" {
		return topic
	}
	return prefix + ":" + topic
}

func parseRedisStreamResponse(response any, expectedStream string) []redisStreamRecord {
	streams := asSlice(response)
	records := []redisStreamRecord{}
	for _, stream := range streams {
		parts := asSlice(stream)
		if len(parts) < 2 || valueString(parts[0]) != expectedStream {
			continue
		}
		for _, entry := range asSlice(parts[1]) {
			entryParts := asSlice(entry)
			if len(entryParts) < 2 {
				continue
			}
			id := valueString(entryParts[0])
			fields := asSlice(entryParts[1])
			for i := 0; i+1 < len(fields); i += 2 {
				if valueString(fields[i]) == "message" {
					records = append(records, redisStreamRecord{id: id, message: valueString(fields[i+1])})
				}
			}
		}
	}
	return records
}

func asSlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	default:
		return nil
	}
}

func valueString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
