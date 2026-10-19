package hooks

import (
	"sort"
	"time"
)

// Delivery modes for a subscription.
const (
	// ModeBlocking runs the hook inside the request, so its decision can change
	// or refuse the turn. It costs latency on every matching request.
	ModeBlocking = "blocking"
	// ModeAsync delivers after the fact and cannot influence the turn.
	ModeAsync = "async"
)

// Behaviour when a blocking hook errors or times out.
const (
	// FailOpen lets the turn proceed. The default: a broken notification
	// endpoint must not take model access down for a whole tenant.
	FailOpen = "open"
	// FailClosed refuses the turn. For guardrails whose whole purpose is to
	// stop something leaving, where proceeding unchecked is the worse outcome.
	FailClosed = "closed"
)

// Subscription is one registered hook endpoint.
type Subscription struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	TenantID  string        `json:"tenant_id,omitempty"`  // "" matches every tenant
	ProjectID string        `json:"project_id,omitempty"` // "" matches every project
	EventType EventType     `json:"event_type"`
	URL       string        `json:"url"`
	Secret    string        `json:"-"`
	Timeout   time.Duration `json:"timeout"`
	Mode      string        `json:"mode"`
	// IncludeContent ships the prompt (and, on post-send, the reply) to the
	// endpoint. It is the difference between a metrics webhook and a
	// content-egress channel, which is why it is admin-only to set.
	IncludeContent bool `json:"include_content"`
	// AllowMutation makes the endpoint's rewrite fields authoritative. Without
	// it, a hook can only permit or block.
	AllowMutation bool   `json:"allow_mutation"`
	FailMode      string `json:"fail_mode"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
}

// Blocking reports whether this subscription runs inside the request.
func (s *Subscription) Blocking() bool { return s != nil && s.Mode == ModeBlocking }

// FailsClosed reports whether a delivery failure should refuse the turn.
func (s *Subscription) FailsClosed() bool { return s != nil && s.FailMode == FailClosed }

// Matches reports whether a subscription applies to an event. A blank tenant or
// project on the subscription is a wildcard, so one global subscription can
// cover every tenant without a row per tenant.
func (s *Subscription) Matches(ev *Event) bool {
	if s == nil || ev == nil || !s.Enabled {
		return false
	}
	if s.EventType != ev.Type {
		return false
	}
	if s.TenantID != "" && s.TenantID != ev.TenantID {
		return false
	}
	if s.ProjectID != "" && s.ProjectID != ev.ProjectID {
		return false
	}
	return true
}

// specificity ranks how narrowly a subscription is scoped. Project-scoped hooks
// run before tenant-scoped ones, which run before global ones, so a narrow
// guardrail sees the turn before a broad observer does.
func (s *Subscription) specificity() int {
	switch {
	case s.ProjectID != "":
		return 2
	case s.TenantID != "":
		return 1
	default:
		return 0
	}
}

// SubscriptionSource supplies the current subscription set. The concrete
// implementation refreshes from Postgres on an interval, so adding a hook does
// not require a redeploy.
type SubscriptionSource interface {
	Subscriptions() []*Subscription
}

// SubscriptionFunc adapts a function to SubscriptionSource, for tests and for
// statically configured sets.
type SubscriptionFunc func() []*Subscription

// Subscriptions implements SubscriptionSource.
func (f SubscriptionFunc) Subscriptions() []*Subscription { return f() }

// matching returns the subscriptions that apply to an event, ordered
// most-specific first and then by ascending priority.
func matching(source SubscriptionSource, ev *Event) []*Subscription {
	if source == nil || ev == nil {
		return nil
	}
	all := source.Subscriptions()
	out := make([]*Subscription, 0, len(all))
	for _, sub := range all {
		if sub.Matches(ev) {
			out = append(out, sub)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].specificity() != out[j].specificity() {
			return out[i].specificity() > out[j].specificity()
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DeliveryRecord is one attempt against one subscription, written to the
// delivery log so an operator can see whether a hook is healthy without
// instrumenting the endpoint itself.
type DeliveryRecord struct {
	SubscriptionID string
	EventID        string
	EventType      EventType
	TenantID       string
	Status         string // "ok" | "error" | "timeout" | "blocked" | "skipped"
	HTTPStatus     int
	Latency        time.Duration
	Error          string
	Decision       *Decision
}

// DeliveryRecorder persists delivery attempts. It is optional; hooks work
// without one, they are just harder to debug.
type DeliveryRecorder interface {
	RecordDelivery(rec DeliveryRecord)
}
