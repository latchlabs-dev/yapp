package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeRuleMatcher struct {
	rule *pgstore.MemoryInjectionRule
	err  error
}

func (f *fakeRuleMatcher) Match(ctx context.Context, project, tenantID, userID string) (*pgstore.MemoryInjectionRule, error) {
	return f.rule, f.err
}

func TestHookChainDefaultInjects(t *testing.T) {
	chain := NewHookChain(0, zerolog.Nop())
	d := chain.Decide(context.Background(), &InjectionRequest{Project: "demo"}, 4000)
	if !d.Inject || d.BudgetTokens != 4000 {
		t.Errorf("decision = %+v", d)
	}
}

func TestGlobalToggleHook(t *testing.T) {
	enabled := true
	chain := NewHookChain(0, zerolog.Nop(), NewGlobalToggleHook(func() bool { return enabled }))

	if d := chain.Decide(context.Background(), &InjectionRequest{}, 4000); !d.Inject {
		t.Error("should inject while enabled")
	}
	enabled = false
	if d := chain.Decide(context.Background(), &InjectionRequest{}, 4000); d.Inject {
		t.Error("should not inject while disabled")
	}
}

func TestRuleHookDisables(t *testing.T) {
	budget := 1234
	matcher := &fakeRuleMatcher{rule: &pgstore.MemoryInjectionRule{ID: "mir_1", Enabled: false, BudgetTokens: &budget}}
	chain := NewHookChain(0, zerolog.Nop(), NewRuleHook(matcher))

	d := chain.Decide(context.Background(), &InjectionRequest{Project: "demo"}, 4000)
	if d.Inject {
		t.Error("rule should disable injection")
	}
	if d.Reason == "default" {
		t.Error("reason should mention the rule")
	}
}

func TestRuleHookBudgetOverride(t *testing.T) {
	budget := 1234
	matcher := &fakeRuleMatcher{rule: &pgstore.MemoryInjectionRule{ID: "mir_2", Enabled: true, BudgetTokens: &budget}}
	chain := NewHookChain(0, zerolog.Nop(), NewRuleHook(matcher))

	d := chain.Decide(context.Background(), &InjectionRequest{Project: "demo"}, 4000)
	if !d.Inject || d.BudgetTokens != 1234 {
		t.Errorf("decision = %+v", d)
	}
}

func TestRuleHookNoMatchLeavesDefault(t *testing.T) {
	chain := NewHookChain(0, zerolog.Nop(), NewRuleHook(&fakeRuleMatcher{}))
	d := chain.Decide(context.Background(), &InjectionRequest{Project: "demo"}, 4000)
	if !d.Inject || d.BudgetTokens != 4000 {
		t.Errorf("decision = %+v", d)
	}
}

func TestHookErrorIsSkipped(t *testing.T) {
	failing := HookFunc{HookName: "boom", Fn: func(ctx context.Context, req *InjectionRequest, d *Decision) error {
		return errors.New("db down")
	}}
	chain := NewHookChain(0, zerolog.Nop(), failing)
	d := chain.Decide(context.Background(), &InjectionRequest{Project: "demo"}, 4000)
	if !d.Inject {
		t.Error("failing hook must not change the decision")
	}
}

func TestHooksRunInOrder(t *testing.T) {
	first := HookFunc{HookName: "first", Fn: func(ctx context.Context, req *InjectionRequest, d *Decision) error {
		d.Inject = false
		d.Reason = "first said no"
		return nil
	}}
	second := HookFunc{HookName: "second", Fn: func(ctx context.Context, req *InjectionRequest, d *Decision) error {
		d.Inject = true
		d.Reason = "second overrode"
		return nil
	}}
	chain := NewHookChain(0, zerolog.Nop(), first, second)
	d := chain.Decide(context.Background(), &InjectionRequest{}, 4000)
	if !d.Inject || d.Reason != "second overrode" {
		t.Errorf("decision = %+v", d)
	}
}
