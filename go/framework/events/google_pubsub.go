package events

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// codeEventsGooglePubSub identifies Google Pub/Sub transport failures.
const codeEventsGooglePubSub errors.Code = "events.google_pubsub"

// GooglePubSubClient is the minimal client surface needed by
// GooglePubSubTransport. Production code can wrap cloud.google.com/go/pubsub or
// another compatible Pub/Sub client without adding that SDK to this module.
type GooglePubSubClient interface {
	Topic(name string) GooglePubSubTopic
	Subscription(name string) GooglePubSubSubscription
}

// GooglePubSubTopic publishes messages to a Google Pub/Sub topic.
type GooglePubSubTopic interface {
	Publish(ctx context.Context, message GooglePubSubPublishMessage) error
}

// GooglePubSubSubscription receives messages from a Google Pub/Sub subscription.
type GooglePubSubSubscription interface {
	Receive(ctx context.Context, handler func(context.Context, GooglePubSubMessage)) error
}

// GooglePubSubPublishMessage is the transport-neutral publish message shape.
type GooglePubSubPublishMessage struct {
	Data        []byte
	Attributes  map[string]string
	OrderingKey string
}

// GooglePubSubMessage is the minimal received message surface used by the transport.
type GooglePubSubMessage interface {
	MessageID() string
	MessageData() []byte
	MessageAttributes() map[string]string
	Ack()
	Nack()
}

// GooglePubSubTransportConfig configures a Google Pub/Sub event transport.
type GooglePubSubTransportConfig struct {
	Client             GooglePubSubClient
	TopicName          func(topic string) string
	SubscriptionName   func(def *HandlerDefinition) string
	OnError            func(error, GooglePubSubErrorContext)
	CloseSubscriptions *bool
}

// GooglePubSubErrorContext describes the subscription where Receive failed.
type GooglePubSubErrorContext struct {
	Definition       *HandlerDefinition
	Subscription     GooglePubSubSubscription
	SubscriptionName string
}

// GooglePubSubTransport maps event topics to Google Pub/Sub topics/subscriptions.
type GooglePubSubTransport struct {
	client           GooglePubSubClient
	topicName        func(topic string) string
	subscriptionName func(def *HandlerDefinition) string
	onError          func(error, GooglePubSubErrorContext)
	closeSubs        bool
	logs             eventLoggers

	mu       sync.Mutex
	subs     []*googlePubSubSubscription
	ctx      context.Context
	cancel   context.CancelFunc
	running  bool
	receiveW sync.WaitGroup
}

type googlePubSubSubscription struct {
	def              *HandlerDefinition
	subscription     GooglePubSubSubscription
	subscriptionName string
}

// NewGooglePubSubTransport creates a Google Pub/Sub event transport.
func NewGooglePubSubTransport(config GooglePubSubTransportConfig) *GooglePubSubTransport {
	topicName := config.TopicName
	if topicName == nil {
		topicName = func(topic string) string { return topic }
	}
	subscriptionName := config.SubscriptionName
	if subscriptionName == nil {
		subscriptionName = func(def *HandlerDefinition) string {
			if def.Options.Group != "" {
				return def.Options.Group
			}
			return strings.ReplaceAll(def.Topic, ".", "-")
		}
	}
	closeSubs := true
	if config.CloseSubscriptions != nil {
		closeSubs = *config.CloseSubscriptions
	}
	return &GooglePubSubTransport{
		client:           config.Client,
		topicName:        topicName,
		subscriptionName: subscriptionName,
		onError:          config.OnError,
		closeSubs:        closeSubs,
		logs:             newEventLoggers(),
	}
}

// Publish sends an envelope to a Google Pub/Sub topic.
func (t *GooglePubSubTransport) Publish(ctx context.Context, env Envelope) error {
	if t.client == nil {
		return errors.New(codeEventsGooglePubSub, "google pubsub client is required")
	}
	data, err := json.Marshal(env)
	if err != nil {
		return errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "marshal"))
	}
	return t.client.Topic(t.topicName(env.Topic)).Publish(ctx, GooglePubSubPublishMessage{
		Data:        data,
		Attributes:  env.Attributes,
		OrderingKey: env.Key,
	})
}

// Subscribe registers a handler against a Google Pub/Sub subscription.
func (t *GooglePubSubTransport) Subscribe(def *HandlerDefinition) error {
	if t.client == nil {
		return errors.New(codeEventsGooglePubSub, "google pubsub client is required")
	}
	subscriptionName := t.subscriptionName(def)
	sub := &googlePubSubSubscription{
		def:              def,
		subscription:     t.client.Subscription(subscriptionName),
		subscriptionName: subscriptionName,
	}
	t.mu.Lock()
	t.subs = append(t.subs, sub)
	running := t.running
	ctx := t.ctx
	t.mu.Unlock()
	if running {
		t.startSubscription(ctx, sub)
	}
	return nil
}

// Start begins receiving from all registered Google Pub/Sub subscriptions.
func (t *GooglePubSubTransport) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.ctx = runCtx
	t.cancel = cancel
	t.running = true
	subs := append([]*googlePubSubSubscription(nil), t.subs...)
	t.mu.Unlock()
	for _, sub := range subs {
		t.startSubscription(runCtx, sub)
	}
	return nil
}

// Stop cancels receive loops and optionally closes subscriptions.
func (t *GooglePubSubTransport) Stop(_ context.Context) error {
	t.mu.Lock()
	t.running = false
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Unlock()
	t.receiveW.Wait()
	if t.closeSubs {
		var first error
		t.mu.Lock()
		subs := append([]*googlePubSubSubscription(nil), t.subs...)
		t.mu.Unlock()
		for _, sub := range subs {
			if closer, ok := sub.subscription.(interface{ Close() error }); ok {
				if err := closer.Close(); err != nil && first == nil {
					first = err
				}
			}
		}
		return first
	}
	return nil
}

func (t *GooglePubSubTransport) startSubscription(ctx context.Context, sub *googlePubSubSubscription) {
	t.receiveW.Add(1)
	go func() {
		defer t.receiveW.Done()
		err := sub.subscription.Receive(ctx, func(messageCtx context.Context, message GooglePubSubMessage) {
			t.handleMessage(messageCtx, sub, message)
		})
		if err != nil && ctx.Err() == nil {
			t.handleError(err, sub)
		}
	}()
}

func (t *GooglePubSubTransport) handleMessage(ctx context.Context, sub *googlePubSubSubscription, raw GooglePubSubMessage) {
	env, err := decodeGooglePubSubEnvelope(raw)
	if err != nil {
		raw.Nack()
		return
	}
	// The delivery's terminal record reports the outcome; the nack hands the retry
	// decision back to Pub/Sub, which owns redelivery and dead-lettering here, so
	// this transport emits no retry/dead-letter record of its own.
	if _, err := invokeTransportHandler(ctx, t.logs, sub.def, env); err != nil {
		raw.Nack()
		return
	}
	raw.Ack()
}

func (t *GooglePubSubTransport) handleError(err error, sub *googlePubSubSubscription) {
	if t.onError != nil {
		t.onError(err, GooglePubSubErrorContext{
			Definition:       sub.def,
			Subscription:     sub.subscription,
			SubscriptionName: sub.subscriptionName,
		})
	}
}

func decodeGooglePubSubEnvelope(message GooglePubSubMessage) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(message.MessageData(), &env); err != nil {
		return env, errors.Wrap(err, codeEventsGooglePubSub, errors.String("phase", "decode"))
	}
	if env.ID == "" {
		env.ID = message.MessageID()
	}
	if env.Attributes == nil {
		env.Attributes = message.MessageAttributes()
	}
	if env.Attributes == nil {
		env.Attributes = map[string]string{}
	}
	// Strip caller-supplied auth.* on every ingress path, not just push: an
	// external publisher must not be able to spoof a reserved auth.* attribute.
	stripAuthAttributes(env.Attributes)
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	if env.Attempt < 1 {
		env.Attempt = 1
	}
	return env, nil
}
