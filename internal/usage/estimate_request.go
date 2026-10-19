package usage

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
)

// Vision-aware request token estimation.
//
// The pre-flight quota estimate (TPM windows, token totals, spend
// reservations) used to price the raw request body at len/4. That is
// ~accurate for text, but multimodal requests carry images as base64 inside
// the JSON: a 1 MB screenshot contributes ~350k phantom "tokens" of base64
// text while the model prices it at ~1-2k vision tokens — a 100-300x
// over-count that falsely exhausts TPM budgets for vision traffic.
//
// EstimateRequestTokens walks the request instead: text is estimated at
// chars/4, and images are estimated from their decoded pixel dimensions
// ((w*h)/750, the standard vision pricing formula, floored at min
// per-image tokens). Images referenced by URL have unknown dimensions and
// get a nominal screenshot-sized estimate. Anything unparseable falls back
// to the flat body estimate, so this can only improve accuracy.

const (
	// minImageTokens matches the floor most vision providers charge for a
	// tiny image tile.
	minImageTokens = 85
	// urlImageTokenEstimate is the nominal cost of an image whose dimensions
	// are unknown (referenced by URL): a typical 1080p screenshot.
	urlImageTokenEstimate = 1500
	// imageHeaderDecodeBytes is how much base64 to decode to read image
	// dimensions. JPEG EXIF blobs can push the SOF marker a few KB in.
	imageHeaderDecodeBytes = 8192
)

// EstimateRequestTokens estimates input tokens for an OpenAI- or
// Anthropic-style chat request body with multimodal content priced
// correctly. It never returns less than 0 and falls back to the flat
// len/4 body estimate when the body is not recognizable JSON.
func EstimateRequestTokens(body []byte) int64 {
	// Fast path: no image markers anywhere → the flat estimate is already
	// right, and we skip JSON parsing on the hot path.
	if !bytes.Contains(body, []byte(`"image`)) {
		return EstimateTextTokens(string(body))
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return EstimateTextTokens(string(body))
	}

	// Everything outside the content (model, tools, metadata, ...) keeps the
	// flat estimate.
	contentRaw := map[string]json.RawMessage{}
	for key, value := range envelope {
		switch key {
		case "messages", "system", "tools":
			contentRaw[key] = value
			delete(envelope, key)
		}
	}
	rest, err := json.Marshal(envelope)
	if err != nil {
		return EstimateTextTokens(string(body))
	}
	total := EstimateTextTokens(string(rest))

	for _, key := range []string{"system", "tools", "messages"} {
		raw, ok := contentRaw[key]
		if !ok || len(raw) == 0 {
			continue
		}
		if key == "messages" {
			var messages []json.RawMessage
			if err := json.Unmarshal(raw, &messages); err != nil {
				total += EstimateTextTokens(string(raw))
				continue
			}
			for _, message := range messages {
				total += estimateMessageTokens(message)
			}
			continue
		}
		// system / tools: string, part array, or tool definitions — text-ish.
		total += estimateContentValueTokens(raw)
	}
	return total
}

// estimateMessageTokens estimates one message object, pricing its content
// parts (text or image) correctly.
func estimateMessageTokens(message json.RawMessage) int64 {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(message, &obj); err != nil {
		return EstimateTextTokens(string(message))
	}
	content, ok := obj["content"]
	if !ok {
		// tool_calls and other non-content messages are text-ish JSON.
		return EstimateTextTokens(string(message))
	}
	// role etc. add a handful of tokens; the flat estimate of the message
	// skeleton without content covers them.
	delete(obj, "content")
	skeleton, err := json.Marshal(obj)
	if err != nil {
		skeleton = message
	}
	return estimateContentValueTokens(content) + EstimateTextTokens(string(skeleton))
}

// estimateContentValueTokens estimates a content value that may be a plain
// string, an array of typed parts, or any other JSON.
func estimateContentValueTokens(raw json.RawMessage) int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return EstimateTextTokens(s)
		}
	}
	if trimmed[0] == '[' {
		var parts []json.RawMessage
		if err := json.Unmarshal(trimmed, &parts); err == nil {
			total := int64(0)
			for _, part := range parts {
				total += estimatePartTokens(part)
			}
			return total
		}
	}
	return EstimateTextTokens(string(raw))
}

// estimatePartTokens estimates one content part. Recognized shapes:
//   - {"type":"text","text":"..."}                       (OpenAI, Anthropic)
//   - {"type":"image_url","image_url":{"url":...}}       (OpenAI chat)
//   - {"type":"input_image","image_url":...}             (OpenAI responses)
//   - {"type":"image","source":{"type":"base64","data":...,"media_type":...}}
//     (Anthropic)
//
// Anything else (tool_use, tool_result with text, ...) is text-ish JSON.
func estimatePartTokens(part json.RawMessage) int64 {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(part, &obj); err != nil {
		return EstimateTextTokens(string(part))
	}

	partType := jsonString(obj["type"])

	// Text-bearing parts: estimate the text, not the JSON wrapper.
	if text, ok := obj["text"]; ok && partType != "image_url" && partType != "input_image" {
		var s string
		if err := json.Unmarshal(text, &s); err == nil {
			return EstimateTextTokens(s)
		}
	}

	// OpenAI chat: image_url.url (data URI or http URL).
	if partType == "image_url" || partType == "input_image" {
		if urlRaw, ok := obj["image_url"]; ok {
			return estimateImageRefTokens(urlRaw)
		}
	}

	// Anthropic: source{type:"base64",data} or source{type:"url",url}.
	if partType == "image" {
		if sourceRaw, ok := obj["source"]; ok {
			return estimateAnthropicSourceTokens(sourceRaw)
		}
	}

	// tool_result and friends may nest content arrays (with images).
	if nested, ok := obj["content"]; ok && (partType == "tool_result" || partType == "tool_use") {
		return estimateContentValueTokens(nested)
	}

	return EstimateTextTokens(string(part))
}

// estimateImageRefTokens handles image_url values that are either a plain
// string URL/data-URI or {"url": "..."}.
func estimateImageRefTokens(raw json.RawMessage) int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var url string
		if err := json.Unmarshal(trimmed, &url); err == nil {
			return estimateImageURLTokens(url)
		}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if url := jsonString(obj["url"]); url != "" {
			return estimateImageURLTokens(url)
		}
	}
	return EstimateTextTokens(string(raw))
}

// estimateAnthropicSourceTokens handles Anthropic image sources.
func estimateAnthropicSourceTokens(raw json.RawMessage) int64 {
	var source struct {
		Type      string `json:"type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return EstimateTextTokens(string(raw))
	}
	switch strings.ToLower(source.Type) {
	case "base64":
		return estimateBase64ImageTokens(source.Data)
	case "url":
		return estimateImageURLTokens(source.URL)
	}
	return EstimateTextTokens(string(raw))
}

// estimateImageURLTokens estimates a data-URI or http(s) image reference.
func estimateImageURLTokens(url string) int64 {
	if strings.HasPrefix(url, "data:") {
		return estimateDataURLTokens(url)
	}
	// Unknown dimensions (remote URL or unsupported scheme).
	return urlImageTokenEstimate
}

// estimateDataURLTokens decodes just enough of a data URI's base64 payload
// to sniff pixel dimensions.
func estimateDataURLTokens(dataURI string) int64 {
	comma := strings.Index(dataURI, ",")
	if comma < 0 || comma+1 >= len(dataURI) {
		return urlImageTokenEstimate
	}
	payload := dataURI[comma+1:]
	// data: URIs sometimes URL-encode padding; base64.RawStdEncoding misses
	// it, so strip whitespace and fall back gracefully.
	payload = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, payload)
	if strings.Contains(dataURI[:comma], ";base64") {
		return estimateBase64ImageTokens(payload)
	}
	// Non-base64 data URIs are rare; sniff the raw bytes if decodable.
	return urlImageTokenEstimate
}

// estimateBase64ImageTokens decodes the image header from base64 and prices
// the image from its dimensions.
func estimateBase64ImageTokens(b64 string) int64 {
	if len(b64) < 8 {
		return minImageTokens
	}
	decoded := make([]byte, imageHeaderDecodeBytes)
	// Stream-decode only the header prefix. (base64.Encoding.Decode panics
	// in go1.27 when dst is smaller than the full src, so never use it here.)
	n, _ := io.ReadFull(base64.NewDecoder(base64.StdEncoding, strings.NewReader(b64)), decoded)
	if n == 0 {
		return urlImageTokenEstimate
	}
	width, height, ok := imageDimensions(decoded[:n])
	if !ok || width <= 0 || height <= 0 {
		return urlImageTokenEstimate
	}
	tokens := (int64(width) * int64(height)) / 750
	if tokens < minImageTokens {
		tokens = minImageTokens
	}
	return tokens
}

// imageDimensions sniffs PNG, GIF, JPEG, and WebP dimensions from a header
// prefix. It returns ok=false for anything else.
func imageDimensions(b []byte) (width, height int, ok bool) {
	// PNG: signature then IHDR width@16 height@20 (big endian).
	if len(b) >= 24 && bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24])), true
	}
	// GIF: logical screen size little endian at 6.
	if len(b) >= 10 && (bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))) {
		return int(binary.LittleEndian.Uint16(b[6:8])), int(binary.LittleEndian.Uint16(b[8:10])), true
	}
	// JPEG: walk the segment stream to an SOFn marker.
	if len(b) >= 4 && b[0] == 0xFF && b[1] == 0xD8 {
		return jpegDimensions(b)
	}
	// WebP: RIFF container.
	if len(b) >= 30 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.HasPrefix(b[8:12], []byte("WEBP")) {
		return webpDimensions(b)
	}
	return 0, 0, false
}

// jpegDimensions walks JPEG markers until an SOFn frame header, whose
// dimensions follow big endian.
func jpegDimensions(b []byte) (int, int, bool) {
	i := 2
	for i+9 < len(b) {
		if b[i] != 0xFF {
			i++
			continue
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF || marker == 0x00 || (marker >= 0xD0 && marker <= 0xD9):
			i++
			continue
		}
		segmentLength := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		isSOF := (marker >= 0xC0 && marker <= 0xC3) || (marker >= 0xC5 && marker <= 0xC7) ||
			(marker >= 0xC9 && marker <= 0xCB) || (marker >= 0xCD && marker <= 0xCF)
		if isSOF {
			if i+9 >= len(b) {
				break
			}
			height := int(binary.BigEndian.Uint16(b[i+5 : i+7]))
			width := int(binary.BigEndian.Uint16(b[i+7 : i+9]))
			return width, height, true
		}
		if segmentLength < 2 {
			break
		}
		i += 2 + segmentLength
	}
	return 0, 0, false
}

// webpDimensions reads VP8X (canvas), VP8 (lossy), or VP8L (lossless)
// chunk dimensions.
func webpDimensions(b []byte) (int, int, bool) {
	chunk := b[12:16]
	switch {
	case bytes.HasPrefix(chunk, []byte("VP8X")) && len(b) >= 30:
		width := 1 + int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		height := 1 + int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return width, height, true
	case bytes.HasPrefix(chunk, []byte("VP8 ")) && len(b) >= 30:
		// Lossy: 3-byte frame tag, then sync code 0x9d012a, then 14-bit dims.
		width := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF)
		height := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF)
		return width, height, true
	case bytes.HasPrefix(chunk, []byte("VP8L")) && len(b) >= 25:
		bits := binary.LittleEndian.Uint32(b[21:25])
		width := int(bits&0x3FFF) + 1
		height := int((bits>>14)&0x3FFF) + 1
		return width, height, true
	}
	return 0, 0, false
}

func jsonString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
