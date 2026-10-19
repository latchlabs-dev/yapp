package modelguard_test

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/modelguard"
)

func TestCheckMatchesExactAndCorpPrefixedForms(t *testing.T) {
	rules := modelguard.Rules{
		{Model: "deepseek-v4-pro", Message: "use deepseek-v4-flash instead"},
	}
	for _, model := range []string{"deepseek-v4-pro", "corp-deepseek-v4-pro", " deepseek-v4-pro "} {
		if v := rules.Check(model, ""); v == nil {
			t.Errorf("expected guard to match %q", model)
		} else if v.Message != "use deepseek-v4-flash instead" {
			t.Errorf("unexpected message: %q", v.Message)
		}
	}
}

func TestCheckRuleWithCorpPrefixMatchesBareRequest(t *testing.T) {
	rules := modelguard.Rules{
		{Model: "corp-deepseek-v4-pro", Message: "nope"},
	}
	if v := rules.Check("deepseek-v4-pro", ""); v == nil {
		t.Error("corp-prefixed rule must match the bare upstream id")
	}
}

func TestCheckDoesNotMatchOtherModels(t *testing.T) {
	rules := modelguard.Rules{
		{Model: "deepseek-v4-pro", Message: "nope"},
	}
	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro-max-evil", "", "gpt-4"} {
		if v := rules.Check(model, ""); v != nil {
			t.Errorf("guard must not match %q", model)
		}
	}
}

func TestCheckPrefixGlob(t *testing.T) {
	rules := modelguard.Rules{
		{Model: "old-model-*", Message: "retired"},
	}
	if rules.Check("old-model-x", "") == nil {
		t.Error("glob rule must match prefixed model")
	}
	if rules.Check("corp-old-model-x", "") == nil {
		t.Error("glob rule must match corp-prefixed form")
	}
	if rules.Check("old-model", "") != nil {
		t.Error("glob rule must not match bare prefix without suffix")
	}
	if rules.Check("other-old-model-x", "") != nil {
		t.Error("glob rule must not match models merely containing the prefix")
	}
}

func TestCheckTenantScoping(t *testing.T) {
	rules := modelguard.Rules{
		{Model: "m", Message: "scoped", TenantID: "tnA"},
		{Model: "n", Message: "global"},
	}
	if v := rules.Check("m", "tnA"); v == nil || v.Message != "scoped" {
		t.Error("tenant rule must fire for its tenant")
	}
	if v := rules.Check("m", "tnB"); v != nil {
		t.Error("tenant rule must not fire for other tenants")
	}
	if v := rules.Check("n", "tnB"); v == nil || v.Message != "global" {
		t.Error("global rule must fire for any tenant")
	}
}

func TestNilRulesMatchNothing(t *testing.T) {
	var rules modelguard.Rules
	if v := rules.Check("deepseek-v4-pro", ""); v != nil {
		t.Error("nil rules must match nothing")
	}
}

func TestParseValid(t *testing.T) {
	rules, err := modelguard.Parse(`[
		{"model":"deepseek-v4-pro","message":"use deepseek-v4-flash instead"},
		{"model":"old*","message":"retired","error_type":"not_found"}
	]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if v := rules.Check("deepseek-v4-pro", ""); v == nil || v.ErrorType != errors.ErrorModelBlocked {
		t.Error("default error type must be model_blocked")
	}
	if v := rules.Check("old-1", ""); v == nil || v.ErrorType != errors.ErrorNotFound {
		t.Error("explicit error_type must be honored")
	}
}

func TestParseRejectsInvalidRules(t *testing.T) {
	cases := map[string]string{
		"malformed json":  `[{"model":`,
		"missing model":   `[{"message":"m"}]`,
		"missing message": `[{"model":"m"}]`,
		"unknown type":    `[{"model":"m","message":"x","error_type":"nope"}]`,
		"not an array":    `{"model":"m"}`,
	}
	for name, raw := range cases {
		if _, err := modelguard.Parse(raw); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseEmptyDisables(t *testing.T) {
	rules, err := modelguard.Parse("[]")
	if err != nil {
		t.Fatalf("parse []: %v", err)
	}
	if len(rules) != 0 {
		t.Error("empty array must parse to no rules")
	}
	if v := rules.Check("deepseek-v4-pro", ""); v != nil {
		t.Error("empty rules must match nothing")
	}
}

func TestDefaultRulesDoNotImposeDeploymentPolicy(t *testing.T) {
	if rules := modelguard.DefaultRules(); len(rules) != 0 {
		t.Fatalf("default guards = %v, want none", rules)
	}
}
