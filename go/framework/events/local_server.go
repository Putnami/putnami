package events

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

const (
	// DefaultLocalServerPort is the default loopback port used by the local event server.
	DefaultLocalServerPort = 4222

	// CodeEventsHTTP identifies local event server/client HTTP failures.
	CodeEventsHTTP errors.Code = "events.http"
	// CodeEventsRemoteAck identifies negative acknowledgements from remote subscribers.
	CodeEventsRemoteAck errors.Code = "events.remote_ack"
	// CodeEventsSubscriber identifies remote subscriber lifecycle failures.
	CodeEventsSubscriber errors.Code = "events.subscriber"
)

// LocalServer exposes a MemoryBroker through the TypeScript-compatible local
// event server protocol used during multi-process development.
type LocalServer struct {
	broker      *MemoryBroker
	addr        string
	port        int
	token       string
	server      *http.Server
	listener    net.Listener
	subscribers map[string]*remoteSubscriber
	mu          sync.Mutex
	log         *logger.Logger
}

// LocalServerConfig configures a local event server instance.
type LocalServerConfig struct {
	Port         int
	DrainTimeout time.Duration
	Token        string
}

// NewLocalServer creates a local HTTP event server backed by a MemoryBroker.
func NewLocalServer(config ...LocalServerConfig) *LocalServer {
	cfg := LocalServerConfig{Port: DefaultLocalServerPort}
	if len(config) > 0 {
		cfg = config[0]
	} else if cfg.Port == 0 {
		cfg.Port = DefaultLocalServerPort
	}
	token := cfg.Token
	if token == "" {
		token = generateID()
	}
	return &LocalServer{
		broker:      NewMemoryBroker(MemoryBrokerConfig{DrainTimeout: cfg.DrainTimeout}),
		addr:        fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		port:        cfg.Port,
		token:       token,
		subscribers: make(map[string]*remoteSubscriber),
		log:         logger.Default().Named("events.server"),
	}
}

// Broker returns the in-process broker used by the local server.
func (s *LocalServer) Broker() *MemoryBroker { return s.broker }

// Token returns the bearer token expected by local server HTTP endpoints.
func (s *LocalServer) Token() string { return s.token }

// Port returns the bound local event server port.
func (s *LocalServer) Port() int {
	if s.listener != nil {
		if addr, ok := s.listener.Addr().(*net.TCPAddr); ok {
			return addr.Port
		}
	}
	return s.port
}

// Start begins serving the local event protocol over loopback HTTP.
func (s *LocalServer) Start(ctx context.Context) error {
	if err := s.broker.Start(ctx); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/publish", s.auth(s.handlePublish))
	mux.HandleFunc("/subscribe", s.auth(s.handleSubscribe))
	mux.HandleFunc("/pull", s.auth(s.handlePull))
	mux.HandleFunc("/ack", s.auth(s.handleAck))
	mux.HandleFunc("/unsubscribe", s.auth(s.handleUnsubscribe))

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return errors.Wrap(err, CodeEventsHTTP, errors.String("phase", "listen"))
	}
	s.listener = ln
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := writeLocalTokenFile(s.Port(), s.token); err != nil {
		// Discovery is best-effort. Same-process users still have the token.
		s.log.Debug("failed writing local events token file", slog.String("error", err.Error()))
	}
	go func() {
		if err := s.server.Serve(ln); err != nil && !stderrors.Is(err, http.ErrServerClosed) {
			s.log.Error("local event server stopped unexpectedly", nil, slog.String("error", err.Error()))
		}
	}()
	return nil
}

// Stop gracefully stops the local event server and drains its broker.
func (s *LocalServer) Stop(ctx context.Context) error {
	if err := removeLocalTokenFile(s.Port()); err != nil {
		s.log.Debug("failed removing local events token file", slog.String("error", err.Error()))
	}
	s.mu.Lock()
	for id, sub := range s.subscribers {
		sub.close(errors.New(CodeEventsStopped, "server stopping"))
		delete(s.subscribers, id)
	}
	s.mu.Unlock()
	var shutdownErr error
	if s.server != nil {
		shutdownErr = s.server.Shutdown(ctx)
	}
	if err := s.broker.Stop(ctx); err != nil {
		return err
	}
	if shutdownErr != nil {
		return errors.Wrap(shutdownErr, CodeEventsHTTP, errors.String("phase", "shutdown"))
	}
	return nil
}

func (s *LocalServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "putnami-events"})
}

func (s *LocalServer) handlePublish(w http.ResponseWriter, r *http.Request) {
	var env Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Strip caller-supplied auth.* on ingress (anti-spoofing), matching push.
	stripAuthAttributes(env.Attributes)
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	if err := s.broker.Publish(r.Context(), env); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *LocalServer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var body remoteSubscribeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid subscriber definition"})
		return
	}
	sub := newRemoteSubscriber(body.Topic)
	def := &HandlerDefinition{
		Topic:   body.Topic,
		Options: body.Options.toHandlerOptions(),
		Filter:  body.Filter.Attributes,
		Handler: func(ctx context.Context, env *Envelope) error {
			return sub.enqueue(ctx, *env)
		},
	}
	if err := s.broker.Subscribe(def); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.subscribers[sub.ID] = sub
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"subscriberId": sub.ID})
}

func (s *LocalServer) handlePull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubscriberID string `json:"subscriberId"`
		TimeoutMS    int    `json:"timeoutMs,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SubscriberID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid subscriber id"})
		return
	}
	sub := s.getSubscriber(body.SubscriberID)
	if sub == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "subscriber not found"})
		return
	}
	timeout := time.Duration(body.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > 25*time.Second {
		timeout = 25 * time.Second
	}
	delivery, ok := sub.pull(r.Context(), timeout)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, delivery)
}

func (s *LocalServer) handleAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubscriberID string `json:"subscriberId"`
		DeliveryID   string `json:"deliveryId"`
		Status       string `json:"status"`
		Reason       string `json:"reason,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid ack"})
		return
	}
	sub := s.getSubscriber(body.SubscriberID)
	if sub == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "subscriber not found"})
		return
	}
	if !sub.ack(body.DeliveryID, body.Status, body.Reason) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "delivery not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *LocalServer) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubscriberID string `json:"subscriberId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid subscriber id"})
		return
	}
	s.mu.Lock()
	if sub := s.subscribers[body.SubscriberID]; sub != nil {
		sub.close(errors.New(CodeEventsStopped, "subscriber removed"))
		delete(s.subscribers, body.SubscriberID)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *LocalServer) getSubscriber(id string) *remoteSubscriber {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscribers[id]
}

func (s *LocalServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
			return
		}
		next(w, r)
	}
}

type remoteSubscribeBody struct {
	Topic   string               `json:"topic"`
	Options remoteHandlerOptions `json:"options"`
	Filter  remoteFilter         `json:"filter,omitempty"`
}

type remoteFilter struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

type remoteHandlerOptions struct {
	Group        string `json:"group,omitempty"`
	Distribution string `json:"distribution"`
	MaxRetries   int    `json:"maxRetries"`
	MaxBackoff   int    `json:"maxBackoff"`
	Timeout      int    `json:"timeout"`
	Concurrency  int    `json:"concurrency"`
	QueueLimit   int    `json:"queueLimit"`
	Overflow     string `json:"overflow"`
	DLQ          bool   `json:"dlq"`
	Ack          string `json:"ack"`
}

func (o remoteHandlerOptions) toHandlerOptions() HandlerOptions {
	opts := DefaultHandlerOptions()
	opts.Group = o.Group
	opts.Distribution = Distribution(o.Distribution)
	opts.MaxRetries = o.MaxRetries
	opts.MaxBackoff = time.Duration(o.MaxBackoff) * time.Millisecond
	opts.Timeout = time.Duration(o.Timeout) * time.Millisecond
	opts.Concurrency = o.Concurrency
	opts.QueueLimit = o.QueueLimit
	opts.Overflow = Overflow(o.Overflow)
	opts.DLQ = o.DLQ
	opts.Ack = AckMode(o.Ack)
	if opts.Distribution == "" {
		opts.Distribution = Competing
	}
	if opts.Overflow == "" {
		opts.Overflow = OverflowThrow
	}
	if opts.Ack == "" {
		opts.Ack = AutoAck
	}
	return opts
}

func fromHandlerOptions(opts HandlerOptions) remoteHandlerOptions {
	if opts.Distribution == "" {
		opts.Distribution = Competing
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 10
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 60 * time.Second
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Overflow == "" {
		opts.Overflow = OverflowThrow
	}
	if opts.Ack == "" {
		opts.Ack = AutoAck
	}
	return remoteHandlerOptions{
		Group:        opts.Group,
		Distribution: string(opts.Distribution),
		MaxRetries:   opts.MaxRetries,
		MaxBackoff:   int(opts.MaxBackoff / time.Millisecond),
		Timeout:      int(opts.Timeout / time.Millisecond),
		Concurrency:  opts.Concurrency,
		QueueLimit:   opts.QueueLimit,
		Overflow:     string(opts.Overflow),
		DLQ:          opts.DLQ,
		Ack:          string(opts.Ack),
	}
}

type remoteSubscriber struct {
	ID      string
	Topic   string
	mu      sync.Mutex
	waiter  chan struct{}
	queue   []*remoteDelivery
	pending map[string]*remoteDelivery
	closed  bool
}

type remoteDelivery struct {
	DeliveryID string        `json:"deliveryId"`
	Message    remoteMessage `json:"message"`
	done       chan error
}

type remoteMessage struct {
	ID           string            `json:"id"`
	Topic        string            `json:"topic"`
	Channel      string            `json:"channel,omitempty"`
	Payload      any               `json:"payload"`
	Key          string            `json:"key,omitempty"`
	DedupeKey    string            `json:"dedupeKey,omitempty"`
	TopicVersion string            `json:"topicVersion,omitempty"`
	Timestamp    string            `json:"timestamp"`
	Attributes   map[string]string `json:"attributes"`
	Attempt      int               `json:"attempt"`
	TraceID      string            `json:"traceId,omitempty"`
}

func newRemoteSubscriber(topic string) *remoteSubscriber {
	return &remoteSubscriber{
		ID:      generateID(),
		Topic:   topic,
		pending: make(map[string]*remoteDelivery),
	}
}

func (s *remoteSubscriber) enqueue(ctx context.Context, env Envelope) error {
	d := &remoteDelivery{
		DeliveryID: generateID(),
		Message: remoteMessage{
			ID:           env.ID,
			Topic:        env.Topic,
			Channel:      env.Channel,
			Payload:      env.Payload,
			Key:          env.Key,
			DedupeKey:    env.DedupeKey,
			TopicVersion: env.TopicVersion,
			Timestamp:    env.Timestamp.Format(time.RFC3339Nano),
			Attributes:   env.Attributes,
			Attempt:      env.Attempt,
			TraceID:      env.TraceID,
		},
		done: make(chan error, 1),
	}
	if d.Message.Attributes == nil {
		d.Message.Attributes = map[string]string{}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New(CodeEventsStopped, "subscriber closed")
	}
	s.queue = append(s.queue, d)
	if s.waiter != nil {
		close(s.waiter)
		s.waiter = nil
	}
	s.mu.Unlock()

	select {
	case err := <-d.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *remoteSubscriber) pull(ctx context.Context, timeout time.Duration) (*remoteDelivery, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			d := s.queue[0]
			copy(s.queue, s.queue[1:])
			s.queue = s.queue[:len(s.queue)-1]
			s.pending[d.DeliveryID] = d
			s.mu.Unlock()
			return d, true
		}
		if s.closed {
			s.mu.Unlock()
			return nil, false
		}
		waiter := s.waiter
		if waiter == nil {
			waiter = make(chan struct{})
			s.waiter = waiter
		}
		s.mu.Unlock()

		select {
		case <-waiter:
		case <-deadline.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (s *remoteSubscriber) ack(deliveryID, status, reason string) bool {
	s.mu.Lock()
	d := s.pending[deliveryID]
	delete(s.pending, deliveryID)
	s.mu.Unlock()
	if d == nil {
		return false
	}
	if status == "nack" {
		if reason == "" {
			reason = "remote subscriber negatively acknowledged delivery"
		}
		d.done <- errors.New(CodeEventsRemoteAck, reason)
		return true
	}
	d.done <- nil
	return true
}

func (s *remoteSubscriber) close(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, d := range s.queue {
		d.done <- err
	}
	for _, d := range s.pending {
		d.done <- err
	}
	s.queue = nil
	s.pending = map[string]*remoteDelivery{}
	if s.waiter != nil {
		close(s.waiter)
		s.waiter = nil
	}
}

// LocalServerTransport connects to a TypeScript-compatible LocalServer.
type LocalServerTransport struct {
	Endpoint string
	Token    string

	client        *http.Client
	mu            sync.Mutex
	subs          []*localSubscription
	running       bool
	runContext    context.Context
	cancelContext context.CancelFunc
	log           *logger.Logger
	logs          eventLoggers
}

// NewLocalServerTransport creates a transport that talks to a local event server.
func NewLocalServerTransport(endpoint, token string) *LocalServerTransport {
	return &LocalServerTransport{
		Endpoint: endpoint,
		Token:    token,
		client:   &http.Client{Timeout: 30 * time.Second},
		log:      logger.Default().Named("events.server"),
		logs:     newEventLoggers(),
	}
}

// Publish sends an envelope to the configured local event server.
func (t *LocalServerTransport) Publish(ctx context.Context, env Envelope) error {
	return t.request(ctx, "/publish", env, nil)
}

// Subscribe registers a handler with the configured local event server.
func (t *LocalServerTransport) Subscribe(def *HandlerDefinition) error {
	body := remoteSubscribeBody{
		Topic:   def.Topic,
		Options: fromHandlerOptions(def.Options),
	}
	if len(def.Filter) > 0 {
		body.Filter.Attributes = def.Filter
	}
	var response struct {
		SubscriberID string `json:"subscriberId"`
	}
	if err := t.request(context.Background(), "/subscribe", body, &response); err != nil {
		return err
	}
	t.mu.Lock()
	sub := &localSubscription{id: response.SubscriberID, def: def}
	t.subs = append(t.subs, sub)
	runCtx := t.runContext
	if t.running && runCtx != nil {
		go t.pump(runCtx, sub)
	}
	t.mu.Unlock()
	return nil
}

// Start begins pulling deliveries for all registered remote subscriptions.
func (t *LocalServerTransport) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.running = true
	t.runContext = runCtx
	t.cancelContext = cancel
	subs := append([]*localSubscription(nil), t.subs...)
	t.mu.Unlock()
	for _, sub := range subs {
		go t.pump(runCtx, sub)
	}
	return nil
}

// Stop cancels remote delivery pumps and unsubscribes from the local server.
func (t *LocalServerTransport) Stop(ctx context.Context) error {
	t.mu.Lock()
	t.running = false
	if t.cancelContext != nil {
		t.cancelContext()
	}
	t.runContext = nil
	t.cancelContext = nil
	subs := append([]*localSubscription(nil), t.subs...)
	t.subs = nil
	t.mu.Unlock()
	var first error
	for _, sub := range subs {
		if err := t.request(ctx, "/unsubscribe", map[string]string{"subscriberId": sub.id}, nil); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type localSubscription struct {
	id  string
	def *HandlerDefinition
}

func (t *LocalServerTransport) pump(ctx context.Context, sub *localSubscription) {
	for {
		t.mu.Lock()
		running := t.running
		t.mu.Unlock()
		if !running {
			return
		}

		var delivery struct {
			DeliveryID string        `json:"deliveryId"`
			Message    remoteMessage `json:"message"`
		}
		err := t.request(ctx, "/pull", map[string]any{"subscriberId": sub.id, "timeoutMs": 25000}, &delivery)
		if err != nil || delivery.DeliveryID == "" {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		status, reason := "ack", ""
		env := Envelope{
			Protocol:     ProtocolVersion,
			ID:           delivery.Message.ID,
			Topic:        delivery.Message.Topic,
			Channel:      delivery.Message.Channel,
			Payload:      delivery.Message.Payload,
			Key:          delivery.Message.Key,
			DedupeKey:    delivery.Message.DedupeKey,
			TopicVersion: delivery.Message.TopicVersion,
			Attributes:   delivery.Message.Attributes,
			Attempt:      delivery.Message.Attempt,
			TraceID:      delivery.Message.TraceID,
		}
		if ts, err := time.Parse(time.RFC3339Nano, delivery.Message.Timestamp); err == nil {
			env.Timestamp = ts
		}
		// A pulled delivery is a full delivery boundary: it gets the same single
		// terminal record as every other transport. The handler is invoked directly
		// (no added timeout) to preserve this path's existing semantics — the local
		// server owns the delivery deadline.
		deliveryCtx, _, err := dispatchDelivery(ctx, t.logs, &env, func(deliveryCtx context.Context, msg *Envelope) error {
			return sub.def.Handler(deliveryCtx, msg)
		})
		if err != nil {
			status, reason = "nack", err.Error()
		}
		if err := t.request(ctx, "/ack", map[string]string{
			"subscriberId": sub.id,
			"deliveryId":   delivery.DeliveryID,
			"status":       status,
			"reason":       reason,
		}, nil); err != nil && ctx.Err() == nil {
			t.log.WarnCtx(deliveryCtx, "failed to acknowledge remote event delivery",
				slog.String("subscriberId", sub.id),
				slog.String("deliveryId", delivery.DeliveryID),
				logger.ErrorAttr(err),
			)
		}
	}
}

func (t *LocalServerTransport) request(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.Wrap(err, CodeEventsHTTP, errors.String("phase", "marshal"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint+path, bytes.NewReader(data))
	if err != nil {
		return errors.Wrap(err, CodeEventsHTTP, errors.String("phase", "request"))
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return errors.Wrap(err, CodeEventsHTTP, errors.String("phase", "do"), errors.String("path", path))
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.log.Debug("failed closing events HTTP response body", slog.String("error", err.Error()))
		}
	}()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
			errBody = nil
		}
		msg := errBody["error"]
		if msg == "" {
			msg = resp.Status
		}
		return errors.New(CodeEventsHTTP, msg, errors.Int("status", resp.StatusCode), errors.String("path", path))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return errors.Wrap(err, CodeEventsHTTP, errors.String("phase", "decode"), errors.String("path", path))
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

// localTokenFilePath returns the local discovery token file path for a port.
func localTokenFilePath(port int) string {
	if port == 0 {
		port = DefaultLocalServerPort
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("putnami-events-%d.token", port))
}

func writeLocalTokenFile(port int, token string) error {
	return os.WriteFile(localTokenFilePath(port), []byte(token), 0o600)
}

func removeLocalTokenFile(port int) error {
	err := os.Remove(localTokenFilePath(port))
	if stderrors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
