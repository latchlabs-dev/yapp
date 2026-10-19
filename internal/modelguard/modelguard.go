// Package modelguard implements request-time model guards: rules that stop a
// request for a specific model with a custom, operator-defined error message.
//
// Guards are the primitive behind model deprecations and sunsets ("X is
// retired, use Y instead"). Unlike tenant model-access policy
// (internal/modelaccess), guards are process-global by default (a rule may
// optionally scope to a single tenant), carry a human-facing message that is
// returned verbatim to the caller, and are evaluated before provider routing
// and quota reservation, so a denied request never reserves spend.
//
// Rules are configured as a JSON array via PROXY_MODEL_GUARDS; when the
// variable is unset the DefaultRules ship (see DefaultRules). Setting the
// variable to a valid JSON array (including "[]" to disable all guards)
// replaces the defaults.
package modelguard

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/earlgrey/piproxy/internal/errors"
)

// CorpModelPrefix mirrors dataplane/modelaccess: proxy-served model ids are
// advertised as "corp-<id>" so they do not collide with client built-ins, and
// guards must match both the public prefixed and the bare upstream form.
const CorpModelPrefix = "corp-"

// Violation is the resolved denial for a guarded model: the message and error
// type the caller receives.
type Violation struct {
	Message   string
	ErrorType errors.ErrorType
}

// Rule blocks one model pattern with a custom error message.
//
// Model is matched against both the requested public name ("corp-X") and the
// bare upstream id ("X"). A trailing "*" makes the pattern a prefix glob for
// blocking families of ids (e.g. "deepseek-v4-pro*").
//
// Message is returned verbatim to the caller; write it for the user, not the
// logs ("deepseek-v4-pro is no longer available — use deepseek-v4-flash
// instead").
//
// ErrorType is optional and must be a known proxy error type (internal/errors);
// it defaults to "model_blocked" (HTTP 400). TenantID is optional and scopes
// the rule to one tenant; empty applies to all tenants.
type Rule struct {
	Model     string `json:"model"`
	Message   string `json:"message"`
	ErrorType string `json:"error_type,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
}

// Rules is an immutable set of guard rules. A nil Rules matches nothing.
type Rules []Rule

// Parse decodes a JSON array of rules, validating every entry. It fails fast
// on malformed JSON or incomplete rules so operator typos surface at startup,
// not as silently-unguarded models at request time.
func Parse(raw string) (Rules, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var parsed []Rule
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse model guards: %w", err)
	}
	for i, rule := range parsed {
		if strings.TrimSpace(rule.Model) == "" {
			return nil, fmt.Errorf("model guard %d: model is required", i)
		}
		if strings.TrimSpace(rule.Message) == "" {
			return nil, fmt.Errorf("model guard %d (%s): message is required", i, rule.Model)
		}
		if rule.ErrorType == "" {
			rule.ErrorType = string(errors.ErrorModelBlocked)
			parsed[i] = rule
			continue
		}
		if _, ok := errors.ErrorCode[errors.ErrorType(rule.ErrorType)]; !ok {
			return nil, fmt.Errorf("model guard %d (%s): unknown error_type %q", i, rule.Model, rule.ErrorType)
		}
	}
	return parsed, nil
}

// DefaultRules leaves model selection to the operator. Configure
// PROXY_MODEL_GUARDS to enforce instance-specific model policy.
func DefaultRules() Rules {
	return nil
}

// Check returns the first matching rule's violation for model, or nil when the
// model is not guarded. tenantID scopes tenant-specific rules; rules without a
// TenantID apply to everyone.
func (rs Rules) Check(model, tenantID string) *Violation {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	for _, rule := range rs {
		if rule.TenantID != "" && rule.TenantID != tenantID {
			continue
		}
		if matchModel(rule.Model, model) {
			errorType := errors.ErrorType(rule.ErrorType)
			if errorType == "" {
				errorType = errors.ErrorModelBlocked
			}
			return &Violation{
				Message:   rule.Message,
				ErrorType: errorType,
			}
		}
	}
	return nil
}

// matchModel compares a rule pattern to a requested model, considering both
// corp-prefixed and bare equivalents of each.
func matchModel(pattern, model string) bool {
	for _, p := range equivalentIDs(pattern) {
		glob := strings.TrimSuffix(p, "*")
		hasGlob := len(glob) < len(p)
		if hasGlob && glob == "" {
			continue // a bare "*" would block everything; treat as no rule
		}
		for _, m := range equivalentIDs(model) {
			if p == m {
				return true
			}
			if hasGlob && strings.HasPrefix(m, glob) {
				return true
			}
		}
	}
	return false
}

// equivalentIDs returns the corp-prefixed and bare forms of a model id.
func equivalentIDs(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if strings.HasPrefix(model, CorpModelPrefix) {
		return []string{model, strings.TrimPrefix(model, CorpModelPrefix)}
	}
	return []string{model, CorpModelPrefix + model}
}
