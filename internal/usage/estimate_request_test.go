package usage_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/usage"
)

// pngWithDimensions builds a minimal PNG whose IHDR advertises w×h.
func pngWithDimensions(w, h int) string {
	header := make([]byte, 33)
	copy(header, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	copy(header[12:], "IHDR")
	putBE32 := func(off int, v int) {
		header[off] = byte(v >> 24)
		header[off+1] = byte(v >> 16)
		header[off+2] = byte(v >> 8)
		header[off+3] = byte(v)
	}
	putBE32(16, w)
	putBE32(20, h)
	return base64.StdEncoding.EncodeToString(header)
}

func TestEstimateRequestTokensTextOnlyMatchesFlatEstimate(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello world, how are you today?"}]}`)
	want := usage.EstimateTextTokens(string(body))
	if got := usage.EstimateRequestTokens(body); got != want {
		t.Errorf("text-only body: got %d, want flat estimate %d", got, want)
	}
}

func TestEstimateRequestTokensPricesDataURIByPixels(t *testing.T) {
	// 1920x1080 screenshot as a data URI: vision cost is w*h/750 = 2764
	// tokens, NOT the ~341k the base64 characters would suggest.
	img := pngWithDimensions(1920, 1080)
	body := []byte(fmt.Sprintf(
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`,
		img))

	got := usage.EstimateRequestTokens(body)
	if got > 5000 {
		t.Errorf("1920x1080 data URI estimated at %d tokens; base64 inflation not fixed (expect ~2800)", got)
	}
	if got < 2764 {
		t.Errorf("1920x1080 data URI estimated at %d tokens; below pixel formula 2764", got)
	}
}

func TestEstimateRequestTokensTinyImageHitsFloor(t *testing.T) {
	img := pngWithDimensions(1, 1)
	body := []byte(fmt.Sprintf(
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`,
		img))
	got := usage.EstimateRequestTokens(body)
	if got > 200 {
		t.Errorf("1x1 image estimated at %d tokens; floor is 85 plus envelope", got)
	}
}

func TestEstimateRequestTokensHugeBase64NoLongerExplodes(t *testing.T) {
	// The regression that exhausted 10M TPM budgets: a multi-MB base64 image
	// used to count as hundreds of thousands of text tokens.
	big := pngWithDimensions(1000, 1000) + strings.Repeat("A", 4*1024*1024) // 4MB of base64 payload
	body := []byte(fmt.Sprintf(
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`,
		big))
	got := usage.EstimateRequestTokens(body)
	if got > 100*1024 {
		t.Errorf("4MB base64 image estimated at %d tokens; must stay near the 1333 pixel cost, not len/4", got)
	}
}

func TestEstimateRequestTokensURLImageIsNominal(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/shot.png"}}]}]}`)
	got := usage.EstimateRequestTokens(body)
	if got < 1500 || got > 1600 {
		t.Errorf("URL image estimated at %d; want nominal 1500 plus a small envelope", got)
	}
}

func TestEstimateRequestTokensAnthropicImageSource(t *testing.T) {
	img := pngWithDimensions(800, 600) // 640 tokens
	body := []byte(fmt.Sprintf(
		`{"model":"m","system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}]}]}`,
		img))
	got := usage.EstimateRequestTokens(body)
	if got < 640 {
		t.Errorf("anthropic base64 image estimated at %d; below pixel formula 640", got)
	}
	if got > 1500 {
		t.Errorf("anthropic base64 image estimated at %d; base64 inflation not fixed", got)
	}
}

func TestEstimateRequestTokensAnthropicURLSource(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}]}`)
	if got := usage.EstimateRequestTokens(body); got < 1500 {
		t.Errorf("anthropic URL image estimated at %d; want nominal 1500", got)
	}
}

func TestEstimateRequestTokensStringContentAndMultiMessage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"you are terse"},{"role":"user","content":"hello there friend"},{"role":"assistant","content":"hi"},{"role":"user","content":"one more"}]}`)
	got := usage.EstimateRequestTokens(body)
	flat := usage.EstimateTextTokens(string(body))
	// Structured walk and flat estimate must be the same ballpark.
	if got < flat/2 || got > flat*2 {
		t.Errorf("all-text conversation: structured %d vs flat %d — drifted too far", got, flat)
	}
}

func TestEstimateRequestTokensGarbageFallsBack(t *testing.T) {
	body := []byte(`not json at all "image"`)
	want := usage.EstimateTextTokens(string(body))
	if got := usage.EstimateRequestTokens(body); got != want {
		t.Errorf("garbage body: got %d, want flat %d", got, want)
	}
}

func TestEstimateRequestTokensToolResultWithNestedImage(t *testing.T) {
	img := pngWithDimensions(640, 480) // 409 tokens
	body := []byte(fmt.Sprintf(
		`{"model":"m","messages":[{"role":"tool","content":[{"type":"tool_result","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}]}`,
		img))
	got := usage.EstimateRequestTokens(body)
	if got > 1200 {
		t.Errorf("nested tool_result image estimated at %d; base64 inflation not fixed", got)
	}
}

func TestEstimateRequestTokensResponsesAPIInputImage(t *testing.T) {
	img := pngWithDimensions(512, 512) // 349 tokens
	body := []byte(fmt.Sprintf(
		`{"model":"m","input":[{"type":"message","content":[{"type":"input_text","text":"see"},{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`,
		img))
	got := usage.EstimateRequestTokens(body)
	if got > 1000 {
		t.Errorf("responses-style input_image estimated at %d; base64 inflation not fixed", got)
	}
}
