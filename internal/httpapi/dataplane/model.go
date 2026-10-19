package dataplane

import (
	"encoding/json"
	"strings"
)

// CorpModelPrefix namespaces proxy-served model ids so they never collide with
// a client's built-in providers. Pi (and similar clients) identify models by
// id and silently dedupe duplicates — when the proxy advertised bare
// "deepseek-v4-flash" it was shadowed by the client's built-in DeepSeek
// provider, so traffic bypassed the proxy entirely. Advertising
// "corp-deepseek-v4-flash" keeps the proxy models distinct and selectable; the
// proxy strips the prefix below before routing and forwarding upstream.
const CorpModelPrefix = "corp-"

// NormalizeModel strips CorpModelPrefix from the model name and, when present,
// rewrites the "model" field in the request body so the upstream provider
// receives its real model id. Returns the (possibly unchanged) body and the
// effective model name used for routing, quota, usage, and capture.
//
// The body is re-serialized only when a rewrite is needed; all other fields
// are preserved verbatim via json.RawMessage.
func NormalizeModel(body []byte, model string) ([]byte, string) {
	if !strings.HasPrefix(model, CorpModelPrefix) {
		return body, model
	}
	stripped := strings.TrimPrefix(model, CorpModelPrefix)
	return RewriteModel(body, stripped), stripped
}

func RewriteModel(body []byte, model string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body
	}
	obj["model"] = encoded
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}
