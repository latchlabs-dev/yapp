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
	"strconv"
	"time"
)

// defaultBlockingBudget bounds the total time every blocking subscription may
// add to one request. It is a budget across all hooks, not per hook, because
// three two-second hooks are just as fatal to a turn as one six-second hook.
const defaultBlockingBudget = 3 * time.Second

// maxContentBytes caps the conversation shipped to a content-aware hook. A long
// thread with inlined attachments can be megabytes; the cap keeps hook delivery
// from becoming the slowest part of a turn.
const maxContentBytes = 128 << 10

// ErrHookRefused is returned when a fail-closed subscription could not be
// reached. It is distinct from an abort: nobody decided to block the turn, we
// just could not ask.
var ErrHookRefused = errors.New("hook could not be consulted and is configured to fail closed")

// SetSubscriptions installs the source of DB-managed hook subscriptions.
// Passing nil leaves only the in-process handlers and the legacy global webhook.
func (m *Manager) SetSubscriptions(source SubscriptionSource) {
	if m == nil {
		return
	}
	m.subscriptions = source
}

// SetDeliveryRecorder installs the delivery log sink.
func (m *Manager) SetDeliveryRecorder(recorder DeliveryRecorder) {
	if m == nil {
		return
	}
	m.deliveries = recorder
}

// SetBlockingBudget overrides the total in-request hook budget.
func (m *Manager) SetBlockingBudget(budget time.Duration) {
	if m == nil || budget <= 0 {
		return
	}
	m.blockingBudget = budget
}

// SetAllowPrivateHookEgress permits hook URLs on private networks. Development
// and the live-test harness only.
func (m *Manager) SetAllowPrivateHookEgress(allow bool) {
	if m == nil {
		return
	}
	m.allowPrivateEgress = allow
}

// WillMutateOutput reports whether any subscription is registered that could
// rewrite the reply for this event. A caller that streams has to know this
// before it starts, because it must buffer instead: a token already flushed to
// the client cannot be taken back.
func (m *Manager) WillMutateOutput(ev *Event) bool {
	if m == nil || m.subscriptions == nil {
		return false
	}
	for _, sub := range matching(m.subscriptions, ev) {
		if sub.Blocking() && sub.AllowMutation {
			return true
		}
	}
	return false
}

// WillReviewOutput reports whether output must be withheld while a blocking
// decision is made. A blocking hook can abort even without mutation rights.
// Trusted in-process handlers can also refuse or rewrite output. Async-only
// observers do not require buffering.
func (m *Manager) WillReviewOutput(ev *Event) bool {
	if m == nil || ev == nil {
		return false
	}
	for _, handler := range m.handlers {
		if handlerHandles(handler, ev.Type) {
			return true
		}
	}
	for _, sub := range matching(m.subscriptions, ev) {
		if sub.Blocking() {
			return true
		}
	}
	return false
}

// runSubscriptions delivers an event to every matching blocking subscription,
// in order, under one shared budget, and merges their decisions.
//
// Ordering matters and is deliberate: an earlier hook's rewrite is what the
// next hook sees, so a redaction hook placed before a classification hook means
// the classifier never sees the raw text.
func (m *Manager) runSubscriptions(ctx context.Context, ev *Event) (*Decision, error) {
	subs := matching(m.subscriptions, ev)
	if len(subs) == 0 {
		return nil, nil
	}

	budget := m.blockingBudget
	if budget <= 0 {
		budget = defaultBlockingBudget
	}
	deadline := time.Now().Add(budget)

	merged := &Decision{}
	touched := false
	for _, sub := range subs {
		if !sub.Blocking() {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.recordDelivery(DeliveryRecord{
				SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
				Status: "timeout", Error: "blocking hook budget exhausted before this hook ran",
			})
			if sub.FailsClosed() {
				return nil, fmt.Errorf("%w: %s", ErrHookRefused, sub.Name)
			}
			continue
		}

		decision, err := m.deliverSubscription(ctx, sub, ev, remaining)
		if err != nil {
			if sub.FailsClosed() {
				return nil, fmt.Errorf("%w: %s: %v", ErrHookRefused, sub.Name, err)
			}
			continue
		}
		if decision == nil {
			continue
		}
		touched = true
		if decision.Abort {
			return decision, nil
		}
		mergeDecision(merged, decision, sub, ev)
	}
	if !touched {
		return nil, nil
	}
	return merged, nil
}

// mergeDecision folds one endpoint's response into the running decision.
// Mutation fields are honoured only for a subscription that was granted
// allow_mutation, so a compromised or over-eager endpoint cannot start
// rewriting prompts by simply returning the field.
func mergeDecision(merged, decision *Decision, sub *Subscription, ev *Event) {
	for k, v := range decision.Metadata {
		if merged.Metadata == nil {
			merged.Metadata = map[string]string{}
		}
		merged.Metadata[k] = v
	}
	for k, v := range decision.Annotations {
		if merged.Annotations == nil {
			merged.Annotations = map[string]string{}
		}
		merged.Annotations[k] = v
	}
	if !sub.AllowMutation {
		return
	}
	if decision.SystemPrepend != "" {
		merged.SystemPrepend += decision.SystemPrepend
	}
	if len(decision.RewrittenMessages) > 0 {
		merged.RewrittenMessages = decision.RewrittenMessages
		// Later hooks must see what the earlier one produced, not the original.
		if ev.Content != nil {
			ev.Content.Messages = decision.RewrittenMessages
		}
	}
	if decision.RewrittenOutput != "" {
		merged.RewrittenOutput = decision.RewrittenOutput
		if ev.Content != nil {
			ev.Content.Output = decision.RewrittenOutput
		}
	}
}

// deliverSubscription posts one event to one endpoint and decodes its decision.
func (m *Manager) deliverSubscription(ctx context.Context, sub *Subscription, ev *Event, budget time.Duration) (*Decision, error) {
	timeout := sub.Timeout
	if timeout <= 0 || timeout > budget {
		timeout = budget
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := ValidateSubscriptionURL(callCtx, sub.URL, m.allowPrivateEgress); err != nil {
		m.recordDelivery(DeliveryRecord{
			SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
			Status: "error", Error: err.Error(),
		})
		return nil, err
	}

	body, err := json.Marshal(payloadFor(sub, ev))
	if err != nil {
		return nil, fmt.Errorf("marshal hook payload: %w", err)
	}

	start := time.Now()
	resp, err := m.postSigned(callCtx, sub, ev, body)
	if err != nil {
		status := "error"
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timeout"
		}
		m.recordDelivery(DeliveryRecord{
			SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
			Status: status, Latency: time.Since(start), Error: err.Error(),
		})
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("hook %s returned HTTP %d", sub.Name, resp.StatusCode)
		m.recordDelivery(DeliveryRecord{
			SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
			Status: "error", HTTPStatus: resp.StatusCode, Latency: time.Since(start), Error: err.Error(),
		})
		return nil, err
	}

	// A 204, or any empty body, means "no opinion" — the common case for a
	// notification endpoint that happens to be registered as blocking.
	decision := &Decision{}
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(decision); err != nil {
			m.recordDelivery(DeliveryRecord{
				SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
				Status: "error", HTTPStatus: resp.StatusCode, Latency: time.Since(start),
				Error: "undecodable hook response: " + err.Error(),
			})
			return nil, err
		}
	}

	status := "ok"
	if decision.Abort {
		status = "blocked"
	}
	m.recordDelivery(DeliveryRecord{
		SubscriptionID: sub.ID, EventID: ev.ID, EventType: ev.Type, TenantID: ev.TenantID,
		Status: status, HTTPStatus: resp.StatusCode, Latency: time.Since(start), Decision: decision,
	})
	return decision, nil
}

func (m *Manager) postSigned(ctx context.Context, sub *Subscription, ev *Event, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build hook request: %w", err)
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Piproxy-Event-ID", ev.ID)
	req.Header.Set("X-Piproxy-Event-Type", string(ev.Type))
	req.Header.Set("X-Piproxy-Subscription-ID", sub.ID)
	req.Header.Set("X-Piproxy-Timestamp", timestamp)
	if sub.Secret != "" {
		// The timestamp is inside the signed material so a captured delivery
		// cannot be replayed later against the same endpoint.
		mac := hmac.New(sha256.New, []byte(sub.Secret))
		_, _ = mac.Write([]byte(timestamp))
		_, _ = mac.Write([]byte{'.'})
		_, _ = mac.Write(body)
		req.Header.Set("X-Piproxy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return m.client.Do(req)
}

// payloadFor produces the event a subscription is entitled to see. A
// subscription without include_content gets a copy with the content stripped,
// never the original with content attached.
func payloadFor(sub *Subscription, ev *Event) *Event {
	if sub.IncludeContent && ev.Content != nil {
		trimmed := *ev
		trimmed.Content = capContent(ev.Content)
		return &trimmed
	}
	stripped := *ev
	stripped.Content = nil
	return &stripped
}

// capContent bounds the content shipped to an endpoint, dropping the oldest
// turns first and flagging that it did so. A hook that must not judge a partial
// conversation can check Truncated and decline.
func capContent(content *EventContent) *EventContent {
	out := &EventContent{
		System:    content.System,
		Output:    content.Output,
		Truncated: content.Truncated,
	}
	budget := maxContentBytes - len(content.System) - len(content.Output)
	if budget < 0 {
		budget = 0
	}
	kept := make([]ContentMessage, 0, len(content.Messages))
	for i := len(content.Messages) - 1; i >= 0; i-- {
		msg := content.Messages[i]
		if len(msg.Text) > budget {
			out.Truncated = true
			break
		}
		budget -= len(msg.Text)
		kept = append(kept, msg)
	}
	// kept was built newest-first; restore chronological order.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if len(kept) != len(content.Messages) {
		out.Truncated = true
	}
	out.Messages = kept
	return out
}

func (m *Manager) recordDelivery(rec DeliveryRecord) {
	if m == nil || m.deliveries == nil {
		return
	}
	m.deliveries.RecordDelivery(rec)
}

// deliverAsyncSubscriptions fans an event out to the non-blocking
// subscriptions. It runs on the manager's async worker, never in a request.
func (m *Manager) deliverAsyncSubscriptions(ctx context.Context, ev *Event) error {
	var errs []error
	for _, sub := range matching(m.subscriptions, ev) {
		if sub.Blocking() {
			continue
		}
		timeout := sub.Timeout
		if timeout <= 0 {
			timeout = m.cfg.Timeout
		}
		if _, err := m.deliverSubscription(ctx, sub, ev, timeout); err != nil {
			errs = append(errs, fmt.Errorf("subscription %s: %w", sub.Name, err))
		}
	}
	return errors.Join(errs...)
}
