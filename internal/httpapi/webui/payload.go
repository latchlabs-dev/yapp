package webui

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/earlgrey/piproxy/internal/pdfextract"
	"github.com/earlgrey/piproxy/internal/providers"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// payloadAttachmentBudget caps the attachment bytes materialized into one
// provider payload. It counts payload bytes, not file bytes: an image or a
// PDF travels base64-encoded at four thirds of its size, so charging the file
// size would let a full budget produce a body a third larger than the cap.
//
// History is rendered oldest-first, so in a long conversation it would be the
// older attachments that survive and the newest that degrade to a placeholder
// once the budget is spent. That is the wrong way round for a conversation
// about a just-uploaded file, and is why reserveCurrentTurn renders the newest
// user message's attachments first, before history is walked.
const payloadAttachmentBudget = 16 << 20

// storedBlock is one element of web_messages.content. It is the normalized
// shape the console persists and replays; provider-specific structures are
// built from it at request time and never stored.
type storedBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
	Filename     string `json:"filename,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

const (
	blockText       = "text"
	blockAttachment = "attachment"
)

// attachmentLoader is the slice of the attachment store the payload builder
// needs. Narrowing it here keeps payload assembly — which is where the byte
// budget is spent — testable without a database.
type attachmentLoader interface {
	GetReadyBatch(ctx context.Context, tenantID, userID string, ids []string) ([]*pgstore.WebAttachment, error)
}

// blobReader is the slice of blob.Store the payload builder needs. Only the
// bounded read is reachable: attachment objects are client-written and their
// signed upload URLs stay replayable until they expire, so an object can
// outgrow the size finalize verified. blob.Store's unbounded Read is
// deliberately left off this interface.
type blobReader interface {
	ReadAtMost(ctx context.Context, key string, limit int64) ([]byte, error)
}

// payloadBuilder renders conversation history into a provider request body.
type payloadBuilder struct {
	attachments       attachmentLoader
	blobs             blobReader
	tenantID          string
	userID            string
	remaining         int64
	extractPDF        func(context.Context, []byte, int64) (string, error)
	reservedMessage   *pgstore.WebMessage
	reservedIDs       map[string]bool
	reserving         bool
	replayingReserved bool
	renderedBytes     map[string]int64
	// rendered caches one attachment's provider content by attachment id, so a
	// file reserved for the current turn is not read, charged to the budget, or
	// noted about twice when history reaches it.
	rendered map[string][]map[string]any
	// notes collects user-visible warnings — a dropped attachment, an
	// unextractable PDF — so the UI can say what the model did not receive
	// instead of silently sending less than the user attached.
	notes []string
}

func newPayloadBuilder(deps *Dependencies, tenantID, userID string) *payloadBuilder {
	b := &payloadBuilder{
		tenantID:      tenantID,
		userID:        userID,
		remaining:     payloadAttachmentBudget,
		rendered:      map[string][]map[string]any{},
		renderedBytes: map[string]int64{},
		reservedIDs:   map[string]bool{},
		extractPDF:    pdfextract.Extract,
	}
	// Assigned only when present: a typed nil pointer in an interface field is
	// not nil, and the renderer's "storage unavailable" check tests for nil.
	if deps != nil && deps.Attachments != nil {
		b.attachments = deps.Attachments
	}
	if deps != nil && deps.Blobs != nil {
		b.blobs = deps.Blobs
	}
	return b
}

// reserveCurrentTurn renders the attachments of the newest user message before
// any history is walked.
//
// The budget is shared across the whole payload and history is emitted
// oldest-first, so without this pass a long conversation's older files spend the
// budget and the file the user just uploaded — the one the turn is almost
// certainly about — degrades to a placeholder. Rendering it first only decides
// which attachment wins the budget; the finished payload still carries every
// message in chronological order, because the rendered content is cached and
// replayed when the walk reaches that message.
func (b *payloadBuilder) reserveCurrentTurn(ctx context.Context, history []*pgstore.WebMessage, supportsImages, anthropic bool) error {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "user" {
			continue
		}
		b.reservedMessage = history[i]
		b.reserving = true
		defer func() { b.reserving = false }()
		for _, block := range decodeBlocks(history[i].Content) {
			if block.Type != blockAttachment {
				continue
			}
			if _, err := b.renderAttachment(ctx, block, supportsImages, anthropic); err != nil {
				return err
			}
			b.reservedIDs[block.AttachmentID] = true
		}
		return nil
	}
	return nil
}

// BuildOpenAI renders an OpenAI/DeepSeek chat-completions body.
func (b *payloadBuilder) BuildOpenAI(ctx context.Context, model, system string, history []*pgstore.WebMessage, supportsImages bool) ([]byte, error) {
	if err := b.reserveCurrentTurn(ctx, history, supportsImages, false); err != nil {
		return nil, err
	}
	messages := make([]map[string]any, 0, len(history)+1)
	if system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, m := range history {
		parts, err := b.openAIParts(ctx, m, supportsImages)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": parts})
	}
	return json.Marshal(map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
		"stream_options": map[string]any{
			// Without this, OpenAI-protocol streams carry no usage block and
			// the turn's cost has to be estimated rather than metered.
			"include_usage": true,
		},
	})
}

// BuildAnthropic renders an Anthropic messages body. The system prompt is a
// top-level field there, not a message.
func (b *payloadBuilder) BuildAnthropic(ctx context.Context, model, system string, history []*pgstore.WebMessage, supportsImages bool) ([]byte, error) {
	if err := b.reserveCurrentTurn(ctx, history, supportsImages, true); err != nil {
		return nil, err
	}
	messages := make([]map[string]any, 0, len(history))
	for _, m := range history {
		parts, err := b.anthropicParts(ctx, m, supportsImages)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": parts})
	}
	body := map[string]any{
		"model":      model,
		"messages":   messages,
		"stream":     true,
		"max_tokens": 8192,
	}
	if system != "" {
		body["system"] = system
	}
	return json.Marshal(body)
}

func (b *payloadBuilder) openAIParts(ctx context.Context, m *pgstore.WebMessage, supportsImages bool) ([]map[string]any, error) {
	b.replayingReserved = m == b.reservedMessage
	defer func() { b.replayingReserved = false }()
	blocks := decodeBlocks(m.Content)
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case blockText:
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, map[string]any{"type": "text", "text": block.Text})
			}
		case blockAttachment:
			rendered, err := b.renderAttachment(ctx, block, supportsImages, false)
			if err != nil {
				return nil, err
			}
			parts = append(parts, rendered...)
		}
	}
	return parts, nil
}

func (b *payloadBuilder) anthropicParts(ctx context.Context, m *pgstore.WebMessage, supportsImages bool) ([]map[string]any, error) {
	b.replayingReserved = m == b.reservedMessage
	defer func() { b.replayingReserved = false }()
	blocks := decodeBlocks(m.Content)
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case blockText:
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, map[string]any{"type": "text", "text": block.Text})
			}
		case blockAttachment:
			rendered, err := b.renderAttachment(ctx, block, supportsImages, true)
			if err != nil {
				return nil, err
			}
			parts = append(parts, rendered...)
		}
	}
	return parts, nil
}

// renderAttachment materializes one stored attachment reference into provider
// content. Anything it cannot send — an image on a text-only model, a file past
// the byte budget, a PDF it cannot read — becomes a labelled text placeholder
// plus a note, so the model and the user both know a file was present.
func (b *payloadBuilder) renderAttachment(ctx context.Context, block storedBlock, supportsImages, anthropic bool) ([]map[string]any, error) {
	if cached, ok := b.rendered[block.AttachmentID]; ok {
		if b.reserving {
			return cached, nil
		}
		if b.replayingReserved && b.reservedIDs[block.AttachmentID] {
			delete(b.reservedIDs, block.AttachmentID)
			return cached, nil
		}
		// Cached bytes still occupy space for each historical occurrence. Only
		// the current turn consumes the credit reserved for it in advance.
		cost := b.renderedBytes[block.AttachmentID]
		if cost > b.remaining {
			b.note(block.Filename + " was not sent again: it exceeds this conversation's attachment budget")
			return []map[string]any{textPart(placeholder(block, "too large to include in this turn"))}, nil
		}
		b.remaining -= cost
		return cached, nil
	}
	if b.blobs == nil || b.attachments == nil {
		return []map[string]any{textPart(placeholder(block, "attachment storage unavailable"))}, nil
	}

	records, err := b.attachments.GetReadyBatch(ctx, b.tenantID, b.userID, []string{block.AttachmentID})
	if err != nil {
		return nil, fmt.Errorf("load attachment %s: %w", block.AttachmentID, err)
	}
	if len(records) == 0 {
		return b.cache(block, textPart(placeholder(block, "no longer available"))), nil
	}
	record := records[0]
	if record.Kind == kindPDF && !anthropic && (record.SizeBytes < 0 || record.SizeBytes > pdfextract.MaxInputBytes) {
		b.note(record.Filename + " was not sent: the PDF is too large to read safely")
		return b.cache(block, textPart(placeholder(block, "PDF exceeds the text extraction size limit"))), nil
	}

	if b.remaining <= 0 || payloadSize(record, anthropic) > b.remaining {
		b.note(record.Filename + " was not sent: it exceeds this conversation's attachment budget")
		return b.cache(block, textPart(placeholder(block, "too large to include in this turn"))), nil
	}
	if record.Kind == kindImage && !supportsImages {
		b.note(record.Filename + " was not sent: the selected model does not accept images")
		return b.cache(block, textPart(placeholder(block, "not sent — this model does not accept images"))), nil
	}

	// One verified read per attachment, before any bytes are encoded or
	// charged. The signed upload URL remains replayable until it expires, so
	// what is stored now can differ from what finalize verified; bytes that no
	// longer match degrade to a placeholder like any other unusable file
	// rather than reaching the model.
	data, err := b.read(ctx, record)
	if err != nil {
		if errors.Is(err, errAttachmentChanged) {
			b.note(record.Filename + " was not sent: it no longer matches its verified upload; attach the file again")
			return b.cache(block, textPart(placeholder(block, "could not be verified and was not sent"))), nil
		}
		return nil, err
	}

	switch record.Kind {
	case kindImage:
		encoded := base64.StdEncoding.EncodeToString(data)
		if anthropic {
			return b.include(block, record.Filename, map[string]any{
				"type":   "image",
				"source": map[string]any{"type": "base64", "media_type": record.MimeType, "data": encoded},
			}), nil
		}
		return b.include(block, record.Filename, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + record.MimeType + ";base64," + encoded},
		}), nil

	case kindPDF:
		if anthropic {
			encoded := base64.StdEncoding.EncodeToString(data)
			return b.include(block, record.Filename, map[string]any{
				"type":   "document",
				"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": encoded},
			}), nil
		}
		text, err := b.extractPDF(ctx, data, b.remaining)
		if errors.Is(err, pdfextract.ErrOutputLimit) {
			b.note(record.Filename + " was not sent: its extracted text exceeds this turn's extraction or attachment budget")
			return b.cache(block, textPart(placeholder(block, "extracted text is too large to include in this turn"))), nil
		}
		if err != nil || strings.TrimSpace(text) == "" {
			// A scanned PDF has no text layer. Say so rather than sending an
			// empty block the model will answer from nothing.
			b.note(record.Filename + " text could not be read safely (or the PDF may be a scan); send it to an Anthropic model to include the document itself")
			return b.cache(block, textPart(placeholder(block, "text extraction unavailable or no extractable text"))), nil
		}
		return b.include(block, record.Filename, textPart(fencedFile(record.Filename, text))), nil

	default:
		return b.include(block, record.Filename, textPart(fencedFile(record.Filename, string(data)))), nil
	}
}

// include checks the final JSON representation, including fences, escaping,
// data-URL prefixes and provider metadata. Compressed PDF size and plain text
// byte length are not safe estimates of what the materialized payload costs.
func (b *payloadBuilder) include(block storedBlock, filename string, parts ...map[string]any) []map[string]any {
	encoded, err := json.Marshal(parts)
	if err != nil || int64(len(encoded)) > b.remaining {
		b.note(filename + " was not sent: it exceeds this conversation's attachment budget")
		return b.cache(block, textPart(placeholder(block, "too large to include in this turn")))
	}
	cost := int64(len(encoded))
	b.remaining -= cost
	b.renderedBytes[block.AttachmentID] = cost
	return b.cache(block, parts...)
}

// cache avoids repeated blob reads and extraction. Each historical occurrence
// still consumes payload space; the reserved current turn is charged only once.
func (b *payloadBuilder) cache(block storedBlock, parts ...map[string]any) []map[string]any {
	if block.AttachmentID != "" {
		b.rendered[block.AttachmentID] = parts
	}
	return parts
}

// errAttachmentChanged reports that the stored object no longer matches the
// size and digest finalize recorded for it.
var errAttachmentChanged = errors.New("stored attachment no longer matches its verified upload")

// read returns the attachment's bytes, bounded by the size finalize verified
// and checked against the digest finalize recorded.
//
// A signed upload URL is a bearer URL, valid until it expires rather than
// single-use, so within the upload TTL an object can be overwritten after it
// was verified — with something larger (an unbounded read, and a payload the
// budget projection never saw) or with something else entirely. Reading
// sizeBytes+1 bounds what can be buffered and catches growth; the digest
// catches a swap. Rows finalized before digests were recorded fall back to
// the size check alone.
func (b *payloadBuilder) read(ctx context.Context, record *pgstore.WebAttachment) ([]byte, error) {
	data, err := b.blobs.ReadAtMost(ctx, record.ObjectKey, record.SizeBytes+1)
	if err != nil {
		return nil, fmt.Errorf("read attachment %s: %w", record.ID, err)
	}
	if int64(len(data)) != record.SizeBytes {
		return nil, errAttachmentChanged
	}
	if record.SHA256 != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != record.SHA256 {
			return nil, errAttachmentChanged
		}
	}
	return data, nil
}

// payloadSize projects how many payload bytes an attachment will cost before it
// is read, so an oversized file is refused without fetching it. Base64 is what
// images and Anthropic documents travel as; everything else is inlined as text
// and needs a final JSON-size check after rendering. PDF text size is unknown
// until extraction; the worker bounds the compressed input independently.
func payloadSize(record *pgstore.WebAttachment, anthropic bool) int64 {
	switch record.Kind {
	case kindImage:
		return base64Size(record.SizeBytes)
	case kindPDF:
		if anthropic {
			return base64Size(record.SizeBytes)
		}
		return 0
	default:
		return record.SizeBytes
	}
}

func base64Size(raw int64) int64 {
	if raw < 0 {
		return 0
	}
	return int64(base64.StdEncoding.EncodedLen(int(raw)))
}

func (b *payloadBuilder) note(msg string) {
	b.notes = append(b.notes, msg)
}

// Notes returns the user-visible warnings gathered while building the payload.
func (b *payloadBuilder) Notes() []string { return b.notes }

func textPart(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func placeholder(block storedBlock, reason string) string {
	name := block.Filename
	if name == "" {
		name = "attachment"
	}
	return fmt.Sprintf("[attachment: %s (%s) — %s]", name, block.MimeType, reason)
}

// fencedFile wraps file content in a labelled fence so the model can tell where
// an attached file starts and ends, and so injected instructions inside it read
// as file content rather than as part of the user's request.
func fencedFile(filename, content string) string {
	lang := strings.TrimPrefix(filepath.Ext(filename), ".")
	// A linear scan avoids quadratic work for a malicious long backtick run.
	longest, run := 2, 0
	for _, ch := range content {
		if ch == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	return fmt.Sprintf("Attached file %s:\n%s%s\n%s\n%s", filename, fence, lang, content, fence)
}

func decodeBlocks(raw json.RawMessage) []storedBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []storedBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

func encodeBlocks(blocks []storedBlock) json.RawMessage {
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return encoded
}

// modelAcceptsImages reports whether a routed model declares image input. The
// catalog already carries this in Model.Input, so the console never has to
// guess or discover it by failing a request upstream.
func modelAcceptsImages(provider *providers.Provider, publicModel string) bool {
	if provider == nil {
		return false
	}
	for _, m := range provider.Models {
		if m.PublicID != publicModel && m.UpstreamID != publicModel {
			continue
		}
		for _, in := range m.Input {
			if strings.EqualFold(in, "image") {
				return true
			}
		}
		return false
	}
	return false
}
