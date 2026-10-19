// Package hooks emits metadata-only activity events to internal and external
// receivers. It intentionally never carries request, response, transcript, or
// tool payload bodies.
package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/id"
)

type EventType string

const (
	EventUserMessagePreSend  EventType = "user_message.pre_send"
	EventUserMessagePostSend EventType = "user_message.post_send"
	EventSessionUploaded     EventType = "session.uploaded"

	// Web console lifecycle events. These carry no prompt content.
	EventWebConversationCreated EventType = "web.conversation_created"
	EventWebAttachmentUploaded  EventType = "web.attachment_uploaded"
	EventWebMemoryEdited        EventType = "web.memory_edited"

	EventToolCallStarted   EventType = "tool_call.started"
	EventToolCallCompleted EventType = "tool_call.completed"
	EventToolCallFailed    EventType = "tool_call.failed"
)

type ActorKind string

const (
	ActorUser ActorKind = "user"
	ActorTool ActorKind = "tool"
)

type Event struct {
	ID        string    `json:"id"`
	Type      EventType `json:"type"`
	EmittedAt time.Time `json:"emitted_at"`

	RequestID      string `json:"request_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	SessionFile    string `json:"session_file,omitempty"`

	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	KeyID    string `json:"key_id,omitempty"`

	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Route     string `json:"route,omitempty"`

	Actor  EventActor  `json:"actor"`
	Stats  EventStats  `json:"stats"`
	Result EventResult `json:"result"`

	// Content is present only for subscriptions that explicitly asked for it.
	// Every other consumer — the JSONL sink, the legacy global webhook, and any
	// subscription without include_content — continues to receive metadata
	// only, so turning on a content-aware guardrail for one tenant does not
	// start shipping everyone's prompts to existing endpoints.
	Content *EventContent `json:"content,omitempty"`
}

// ContentMessage is one normalized turn. The console and the CLI speak
// different provider protocols; hooks see this single shape rather than raw
// OpenAI or Anthropic JSON, so one hook implementation covers both.
type ContentMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// EventContent carries the conversation a hook is allowed to inspect.
type EventContent struct {
	System   string           `json:"system,omitempty"`
	Messages []ContentMessage `json:"messages,omitempty"`
	// Output is the model's reply, present on post-send events only.
	Output string `json:"output,omitempty"`
	// Truncated reports that content was elided to fit the size cap, so a hook
	// can refuse to make a judgement on a partial view rather than assume it
	// saw everything.
	Truncated bool `json:"truncated,omitempty"`
}

type EventActor struct {
	Kind ActorKind `json:"kind"`
}

type EventStats struct {
	MessageCount int        `json:"message_count,omitempty"`
	InputTokens  int64      `json:"input_tokens,omitempty"`
	OutputTokens int64      `json:"output_tokens,omitempty"`
	TotalTokens  int64      `json:"total_tokens,omitempty"`
	CostCents    float64    `json:"cost_cents,omitempty"`
	Estimated    bool       `json:"estimated,omitempty"`
	Streaming    bool       `json:"streaming,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	DurationMs   int64      `json:"duration_ms,omitempty"`
}

type EventResult struct {
	Status     string `json:"status,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

var ErrDeliveryFailed = errors.New("hook delivery failed")

type Decision struct {
	Abort      bool              `json:"abort,omitempty"`
	StatusCode int               `json:"status_code,omitempty"`
	ErrorType  string            `json:"error_type,omitempty"`
	Message    string            `json:"message,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`

	// SystemPrepend is prefixed to the system prompt before the request is
	// forwarded. Honoured on pre-send only, and only for a subscription that
	// was granted allow_mutation.
	SystemPrepend string `json:"system_prepend,omitempty"`

	// RewrittenMessages replaces the conversation the model receives — the
	// redaction path. A hook returning this must return the whole list, not a
	// patch: a partial list would silently drop turns.
	RewrittenMessages []ContentMessage `json:"rewritten_messages,omitempty"`

	// RewrittenOutput replaces the model's reply on post-send. Honouring it
	// forces the turn to buffer instead of stream, because a token already
	// flushed to the browser cannot be recalled.
	RewrittenOutput string `json:"rewritten_output,omitempty"`

	// Annotations are surfaced to the caller and shown in the console, so a
	// user can see that a guardrail acted rather than silently receiving
	// altered content.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Mutates reports whether a decision changes what the model sees or what the
// user receives, as opposed to merely permitting or blocking the turn.
func (d *Decision) Mutates() bool {
	if d == nil {
		return false
	}
	return d.SystemPrepend != "" || len(d.RewrittenMessages) > 0 || d.RewrittenOutput != ""
}

type Config struct {
	WebhookURL    string
	WebhookSecret string
	QueueSize     int
	Timeout       time.Duration
	Sinks         []Sink
}

// Sink persists or forwards normalized metadata events. Sink writes run only
// on the manager's bounded async worker, never on the request hot path.
type Sink interface {
	Name() string
	Write(ctx context.Context, ev *Event) error
	Close() error
}

type Manager struct {
	cfg      Config
	client   *http.Client
	queue    chan queuedEvent
	handlers []Handler
	logger   zerolog.Logger

	// DB-managed subscriptions, installed after construction so hooks can be
	// added or changed without a redeploy. All four are read-only after
	// startup wiring.
	subscriptions      SubscriptionSource
	deliveries         DeliveryRecorder
	blockingBudget     time.Duration
	allowPrivateEgress bool

	// stopped is set by Close before the queue is closed so EmitAsync stops
	// enqueuing and never sends on a closed channel. closeOnce guards the
	// single close(queue); wg tracks the run() goroutine so Close can wait for
	// the queue to drain before returning.
	sendMu    sync.RWMutex
	stopped   atomic.Bool
	closeOnce sync.Once
	sinksOnce sync.Once
	wg        sync.WaitGroup
}

type queuedEvent struct {
	event       *Event
	runHandlers bool
}

type Handler interface {
	Name() string
	Events() []EventType
	Handle(ctx context.Context, ev *Event) (*Decision, error)
}

type HandlerFunc struct {
	HandlerName string
	EventTypes  []EventType
	Fn          func(ctx context.Context, ev *Event) (*Decision, error)
}

func (h HandlerFunc) Name() string { return h.HandlerName }

func (h HandlerFunc) Events() []EventType { return h.EventTypes }

func (h HandlerFunc) Handle(ctx context.Context, ev *Event) (*Decision, error) {
	return h.Fn(ctx, ev)
}

func NewManager(cfg Config, logger zerolog.Logger) *Manager {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 1500 * time.Millisecond
	}
	m := &Manager{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		queue:  make(chan queuedEvent, cfg.QueueSize),
		logger: logger,
	}
	m.wg.Add(1)
	go m.run()
	return m
}

func (m *Manager) Append(handlers ...Handler) {
	if m == nil {
		return
	}
	m.handlers = append(m.handlers, handlers...)
}

func (m *Manager) HasWebhook() bool {
	return m != nil && m.cfg.WebhookURL != ""
}

func (m *Manager) HasSinks() bool {
	return m != nil && len(m.cfg.Sinks) > 0
}

// EmitPreSend runs the blocking path for a pre-send event.
func (m *Manager) EmitPreSend(ctx context.Context, ev *Event) (*Decision, error) {
	return m.EmitBlocking(ctx, ev)
}

// EmitBlocking runs in-process handlers and every matching blocking
// subscription inside the caller's request, returning their merged decision. It
// also queues the event for the async destinations, so an event is never
// delivered twice to the same endpoint.
func (m *Manager) EmitBlocking(ctx context.Context, ev *Event) (*Decision, error) {
	return m.emitBlocking(ctx, ev, true)
}

func (m *Manager) emitBlocking(ctx context.Context, ev *Event, enqueue bool) (*Decision, error) {
	if m == nil || ev == nil {
		return nil, nil
	}
	normalize(ev)
	start := time.Now()
	decision, err := m.runHandlers(ctx, ev)
	if err == nil && (decision == nil || !decision.Abort) {
		// In-process handlers run first: they are trusted code with no network
		// cost, so a policy they can settle never pays for a round trip.
		var subDecision *Decision
		subDecision, err = m.runSubscriptions(ctx, ev)
		if subDecision != nil {
			if decision == nil || subDecision.Abort {
				decision = subDecision
			} else {
				// runSubscriptions has already checked mutation grants. An
				// annotations-only subscription must not erase a trusted
				// handler's earlier redaction.
				mergeDecision(decision, subDecision, &Subscription{AllowMutation: true}, ev)
			}
		}
	}
	if enqueue && m.hasAsyncDestinations() {
		m.enqueue(ev, false)
	}
	m.logger.Info().
		Str("event_id", ev.ID).
		Str("event_type", string(ev.Type)).
		Dur("hook_handler_duration", time.Since(start)).
		Err(err).
		Msg("hook pre-send handlers completed")
	if err != nil {
		// Both errors are wrapped: callers match ErrDeliveryFailed to classify
		// the failure, and ErrHookRefused to tell "a fail-closed hook could not
		// be consulted" apart from "a hook decided to block".
		return nil, fmt.Errorf("%w: %w", ErrDeliveryFailed, err)
	}
	return decision, nil
}

func (m *Manager) EmitAsync(ev *Event) {
	if m == nil || ev == nil {
		return
	}
	normalize(ev)
	m.enqueue(ev, true)
}

func (m *Manager) enqueue(ev *Event, runHandlers bool) {
	// Reject events once shutdown has begun so we never send on a closed queue.
	if m.stopped.Load() {
		return
	}
	m.sendMu.RLock()
	defer m.sendMu.RUnlock()
	if m.stopped.Load() {
		return
	}
	select {
	case m.queue <- queuedEvent{event: ev, runHandlers: runHandlers}:
	default:
		m.logger.Warn().
			Str("event_id", ev.ID).
			Str("event_type", string(ev.Type)).
			Msg("hook event dropped: queue full")
	}
}

// Close stops accepting new async events and waits for run() to drain every
// event already buffered in the queue before returning. It is safe to call
// more than once. run() builds its own per-event timeout context from
// context.Background(), so the drain is not tied to any caller-cancelled
// context; callers should still invoke Close before cancelling the root
// context so in-flight webhook delivery has network time.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.sendMu.Lock()
		defer m.sendMu.Unlock()
		m.stopped.Store(true)
		close(m.queue)
	})
	m.wg.Wait()
	m.sinksOnce.Do(func() {
		for _, sink := range m.cfg.Sinks {
			if sink == nil {
				continue
			}
			if err := sink.Close(); err != nil {
				m.logger.Error().Err(err).Str("hook_sink", sink.Name()).Msg("close hook sink")
			}
		}
	})
}

func (m *Manager) run() {
	defer m.wg.Done()
	for queued := range m.queue {
		ev := queued.event
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
		start := time.Now()
		var err error
		if queued.runHandlers {
			_, err = m.runHandlers(ctx, ev)
		}
		if err == nil {
			err = m.deliverAsync(ctx, ev)
		}
		cancel()
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str("event_id", ev.ID).
				Str("event_type", string(ev.Type)).
				Dur("hook_delivery_duration", time.Since(start)).
				Msg("hook async delivery failed")
			continue
		}
		m.logger.Info().
			Str("event_id", ev.ID).
			Str("event_type", string(ev.Type)).
			Dur("hook_delivery_duration", time.Since(start)).
			Msg("hook async delivered")
	}
}

func (m *Manager) hasAsyncDestinations() bool {
	if len(m.cfg.Sinks) > 0 || m.cfg.WebhookURL != "" {
		return true
	}
	if m.subscriptions == nil {
		return false
	}
	for _, sub := range m.subscriptions.Subscriptions() {
		if sub.Enabled && !sub.Blocking() {
			return true
		}
	}
	return false
}

func (m *Manager) deliverAsync(ctx context.Context, ev *Event) error {
	var errs []error
	for _, sink := range m.cfg.Sinks {
		if sink == nil {
			continue
		}
		if err := sink.Write(ctx, ev); err != nil {
			errs = append(errs, fmt.Errorf("%s sink: %w", sink.Name(), err))
		}
	}
	if m.cfg.WebhookURL != "" {
		if err := m.deliverWebhook(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.deliverAsyncSubscriptions(ctx, ev); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) runHandlers(ctx context.Context, ev *Event) (*Decision, error) {
	if len(m.handlers) == 0 {
		return nil, nil
	}
	final := &Decision{}
	touched := false
	for _, handler := range m.handlers {
		if !handlerHandles(handler, ev.Type) {
			continue
		}
		decision, err := handler.Handle(ctx, ev)
		if err != nil {
			m.logger.Warn().
				Err(err).
				Str("hook_handler", handler.Name()).
				Str("event_id", ev.ID).
				Str("event_type", string(ev.Type)).
				Msg("internal hook handler failed")
			continue
		}
		if decision != nil {
			touched = true
			if decision.Abort {
				return decision, nil
			}
			// Trusted handlers have mutation rights. Apply their rewrite to
			// the event before the next reviewer runs, and retain it when a
			// later handler merely supplies annotations.
			mergeDecision(final, decision, &Subscription{AllowMutation: true}, ev)
		}
	}
	if !touched {
		return nil, nil
	}
	return final, nil
}

func handlerHandles(handler Handler, typ EventType) bool {
	for _, candidate := range handler.Events() {
		if candidate == typ {
			return true
		}
	}
	return false
}

func (m *Manager) deliverWebhook(ctx context.Context, ev *Event) error {
	// The legacy global webhook predates content-aware hooks and never opted
	// in, so it keeps receiving metadata only.
	metadataOnly := *ev
	metadataOnly.Content = nil
	body, err := json.Marshal(&metadataOnly)
	if err != nil {
		return fmt.Errorf("marshal hook event: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build hook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Piproxy-Event-ID", ev.ID)
	req.Header.Set("X-Piproxy-Event-Type", string(ev.Type))
	req.Header.Set("X-Piproxy-Timestamp", ev.EmittedAt.UTC().Format(time.RFC3339Nano))
	if m.cfg.WebhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(m.cfg.WebhookSecret))
		_, _ = mac.Write(body)
		req.Header.Set("X-Piproxy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("send hook event: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hook endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func normalize(ev *Event) {
	if ev.ID == "" {
		ev.ID = id.New("hook")
	}
	if ev.EmittedAt.IsZero() {
		ev.EmittedAt = time.Now().UTC()
	}
	if ev.Actor.Kind == "" {
		ev.Actor.Kind = ActorUser
	}
}
