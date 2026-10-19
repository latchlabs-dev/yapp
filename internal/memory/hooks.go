package memory

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// InjectionRequest describes one incoming context request, as seen by hooks.
type InjectionRequest struct {
	Project  string // sanitized project name
	Hint     string // optional task hint from the client
	TenantID string
	UserID   string
	KeyID    string
}

// Decision is the accumulated outcome of the hook chain. Hooks mutate it in
// order; later hooks see (and may override) earlier hooks' changes.
type Decision struct {
	Inject       bool   // whether to assemble and return a context bundle
	Reason       string // human-readable explanation of the final decision
	BudgetTokens int    // bundle token budget (hooks may raise/lower it)
}

// Hook is a single programmatic injection policy. Implementations decide
// whether (and how) context should be injected for a request. Hooks must be
// fast and must never block a session: any hook error is logged and skipped,
// leaving the decision unchanged.
type Hook interface {
	Name() string
	Evaluate(ctx context.Context, req *InjectionRequest, d *Decision) error
}

// HookFunc adapts a function to the Hook interface.
type HookFunc struct {
	HookName string
	Fn       func(ctx context.Context, req *InjectionRequest, d *Decision) error
}

// Name implements Hook.
func (h HookFunc) Name() string { return h.HookName }

// Evaluate implements Hook.
func (h HookFunc) Evaluate(ctx context.Context, req *InjectionRequest, d *Decision) error {
	return h.Fn(ctx, req, d)
}

// HookChain evaluates hooks in registration order against a base decision.
type HookChain struct {
	hooks   []Hook
	timeout time.Duration
	logger  zerolog.Logger
}

// NewHookChain creates a hook chain. Each hook gets at most timeout (default
// 500ms) to evaluate; slow or failing hooks are skipped, not fatal.
func NewHookChain(timeout time.Duration, logger zerolog.Logger, hooks ...Hook) *HookChain {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	return &HookChain{hooks: hooks, timeout: timeout, logger: logger}
}

// Append adds hooks to the end of the chain.
func (c *HookChain) Append(hooks ...Hook) {
	c.hooks = append(c.hooks, hooks...)
}

// Decide runs the chain. The base decision injects with defaultBudget; hooks
// then narrow or adjust it.
func (c *HookChain) Decide(ctx context.Context, req *InjectionRequest, defaultBudget int) Decision {
	d := Decision{Inject: true, Reason: "default", BudgetTokens: defaultBudget}
	if c == nil {
		return d
	}
	for _, hook := range c.hooks {
		hookCtx, cancel := context.WithTimeout(ctx, c.timeout)
		err := hook.Evaluate(hookCtx, req, &d)
		cancel()
		if err != nil {
			c.logger.Warn().Err(err).
				Str("hook", hook.Name()).
				Str("project", req.Project).
				Msg("injection hook failed; skipping")
		}
	}
	return d
}

// ruleMatcher is the store subset the DB rule hook needs.
type ruleMatcher interface {
	Match(ctx context.Context, project, tenantID, userID string) (*pgstore.MemoryInjectionRule, error)
}

// NewRuleHook returns a hook that applies operator-managed injection rules
// from the memory_injection_rules table (managed via the admin API). The most
// specific matching rule decides enabled/budget; no matching rule leaves the
// decision unchanged, so injection stays on by default.
func NewRuleHook(store ruleMatcher) Hook {
	return HookFunc{
		HookName: "db-rules",
		Fn: func(ctx context.Context, req *InjectionRequest, d *Decision) error {
			rule, err := store.Match(ctx, req.Project, req.TenantID, req.UserID)
			if err != nil {
				return err
			}
			if rule == nil {
				return nil
			}
			d.Inject = rule.Enabled
			if rule.Enabled {
				d.Reason = "rule " + rule.ID + " enabled injection"
			} else {
				d.Reason = "rule " + rule.ID + " disabled injection"
			}
			if rule.BudgetTokens != nil && *rule.BudgetTokens > 0 {
				d.BudgetTokens = *rule.BudgetTokens
			}
			return nil
		},
	}
}

// NewGlobalToggleHook returns a hook backed by a simple bool supplier (e.g. a
// config flag or an atomic switched at runtime). When the supplier returns
// false, injection is disabled regardless of earlier hooks.
func NewGlobalToggleHook(enabled func() bool) Hook {
	return HookFunc{
		HookName: "global-toggle",
		Fn: func(ctx context.Context, req *InjectionRequest, d *Decision) error {
			if !enabled() {
				d.Inject = false
				d.Reason = "globally disabled"
			}
			return nil
		},
	}
}
