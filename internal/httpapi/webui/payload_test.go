package webui

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/pdfextract"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// fakeAttachments serves attachment metadata from memory.
type fakeAttachments map[string]*pgstore.WebAttachment

func (f fakeAttachments) GetReadyBatch(_ context.Context, _, _ string, ids []string) ([]*pgstore.WebAttachment, error) {
	out := make([]*pgstore.WebAttachment, 0, len(ids))
	for _, id := range ids {
		if record, ok := f[id]; ok {
			out = append(out, record)
		}
	}
	return out, nil
}

// fakeBlobs serves attachment bytes from memory and counts reads, so a test can
// assert that a file reserved for the current turn is not fetched twice. It
// records the limit of every read, so a test can also prove the builder never
// asks for more than the finalized size plus one byte.
type fakeBlobs struct {
	objects map[string][]byte
	reads   map[string]int
	limits  map[string]int64
}

// ReadAtMost honors the limit the way the real stores do: it hands back at
// most limit bytes, leaving size-growth detection to the caller.
func (f *fakeBlobs) ReadAtMost(_ context.Context, key string, limit int64) ([]byte, error) {
	if f.reads == nil {
		f.reads = map[string]int{}
	}
	if f.limits == nil {
		f.limits = map[string]int64{}
	}
	f.reads[key]++
	f.limits[key] = limit
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("no object %s", key)
	}
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// textAttachment registers one text attachment of the given size in both fakes.
func textAttachment(store fakeAttachments, blobs *fakeBlobs, id, filename string, size int64) storedBlock {
	store[id] = &pgstore.WebAttachment{
		ID:        id,
		Filename:  filename,
		MimeType:  "text/plain",
		Kind:      kindText,
		SizeBytes: size,
		ObjectKey: "obj/" + id,
		SHA256:    sha256Hex([]byte(strings.Repeat("x", int(size)))),
	}
	blobs.objects["obj/"+id] = []byte(strings.Repeat("x", int(size)))
	return storedBlock{Type: blockAttachment, AttachmentID: id, Filename: filename, MimeType: "text/plain", Kind: kindText}
}

// imageAttachment registers one image attachment, which travels base64-encoded.
func imageAttachment(store fakeAttachments, blobs *fakeBlobs, id, filename string, size int64) storedBlock {
	store[id] = &pgstore.WebAttachment{
		ID:        id,
		Filename:  filename,
		MimeType:  "image/png",
		Kind:      kindImage,
		SizeBytes: size,
		ObjectKey: "obj/" + id,
		SHA256:    sha256Hex([]byte(strings.Repeat("x", int(size)))),
	}
	blobs.objects["obj/"+id] = []byte(strings.Repeat("x", int(size)))
	return storedBlock{Type: blockAttachment, AttachmentID: id, Filename: filename, MimeType: "image/png", Kind: kindImage}
}

func userMessage(id string, blocks ...storedBlock) *pgstore.WebMessage {
	return &pgstore.WebMessage{ID: id, Role: "user", Content: encodeBlocks(blocks)}
}

func testBuilder(store fakeAttachments, blobs *fakeBlobs, budget int64) *payloadBuilder {
	b := newPayloadBuilder(nil, "tnt_1", "usr_1")
	b.attachments = store
	b.blobs = blobs
	b.remaining = budget
	return b
}

// The attachment budget is shared by the whole payload and history is rendered
// oldest-first, so without an up-front reservation the file the user just
// attached is the one that degrades to a placeholder. It is also the one the
// turn is about.
func TestNewestAttachmentKeepsTheBudget(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	old := textAttachment(store, blobs, "watt_old", "old-notes.txt", 900)
	fresh := textAttachment(store, blobs, "watt_new", "just-uploaded.txt", 200)

	builder := testBuilder(store, blobs, 1000)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", storedBlock{Type: blockText, Text: "here is an old file"}, old),
		userMessage("wmsg_2", storedBlock{Type: blockText, Text: "and here is the new one"}, fresh),
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}

	body := string(payload)
	if !strings.Contains(body, "Attached file just-uploaded.txt") {
		t.Errorf("the newest attachment was not included in full:\n%s", body)
	}
	if !strings.Contains(body, "old-notes.txt") {
		t.Error("the older attachment left no trace at all; it should degrade to a placeholder")
	}
	if !strings.Contains(body, "[attachment: old-notes.txt") {
		t.Errorf("expected the older file to be the one that degraded to a placeholder:\n%s", body)
	}
	notes := strings.Join(builder.Notes(), "\n")
	if !strings.Contains(notes, "old-notes.txt was not sent") {
		t.Errorf("the dropped file was not reported to the user: %q", notes)
	}
	if strings.Contains(notes, "just-uploaded.txt") {
		t.Errorf("the current turn's file was dropped: %q", notes)
	}
}

// Reserving the current turn decides only which file wins the budget. The
// payload itself must still read oldest-first, or the model sees the
// conversation out of order.
func TestReservationPreservesMessageOrder(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	old := textAttachment(store, blobs, "watt_old", "old.txt", 10)
	fresh := textAttachment(store, blobs, "watt_new", "new.txt", 10)

	builder := testBuilder(store, blobs, payloadAttachmentBudget)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "system prompt", []*pgstore.WebMessage{
		userMessage("wmsg_1", storedBlock{Type: blockText, Text: "first"}, old),
		{ID: "wmsg_2", Role: "assistant", Content: encodeBlocks([]storedBlock{{Type: blockText, Text: "reply"}})},
		userMessage("wmsg_3", storedBlock{Type: blockText, Text: "second"}, fresh),
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}

	var decoded struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if len(decoded.Messages) != 4 {
		t.Fatalf("messages = %d, want system + three turns", len(decoded.Messages))
	}
	if decoded.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", decoded.Messages[0].Role)
	}
	if !strings.Contains(string(decoded.Messages[1].Content), "old.txt") {
		t.Errorf("the first turn lost its attachment: %s", decoded.Messages[1].Content)
	}
	if !strings.Contains(string(decoded.Messages[3].Content), "new.txt") {
		t.Errorf("the current turn lost its attachment: %s", decoded.Messages[3].Content)
	}

	// Reserved then replayed: one read, one charge against the budget.
	if got := blobs.reads["obj/watt_new"]; got != 1 {
		t.Errorf("the reserved attachment was read %d times, want 1", got)
	}
	oldJSON, _ := json.Marshal(builder.rendered[old.AttachmentID])
	freshJSON, _ := json.Marshal(builder.rendered[fresh.AttachmentID])
	if want := int64(payloadAttachmentBudget - len(oldJSON) - len(freshJSON)); builder.remaining != want {
		t.Errorf("remaining budget = %d, want %d — a reserved file was charged twice", builder.remaining, want)
	}
}

// Anthropic payloads share the builder, so they share the reservation.
func TestReservationAppliesToAnthropicPayloads(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	old := textAttachment(store, blobs, "watt_old", "old.txt", 900)
	fresh := textAttachment(store, blobs, "watt_new", "new.txt", 200)

	builder := testBuilder(store, blobs, 1000)
	payload, err := builder.BuildAnthropic(context.Background(), "m", "system", []*pgstore.WebMessage{
		userMessage("wmsg_1", old),
		userMessage("wmsg_2", fresh),
	}, false)
	if err != nil {
		t.Fatalf("BuildAnthropic: %v", err)
	}
	if !strings.Contains(string(payload), "Attached file new.txt") {
		t.Errorf("the newest attachment was not included in full:\n%s", payload)
	}
}

// An assistant message is never the current turn, so the walk backwards must
// skip it and reserve the newest *user* message's files.
func TestReservationSkipsTrailingAssistantMessage(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	old := textAttachment(store, blobs, "watt_old", "old.txt", 900)
	fresh := textAttachment(store, blobs, "watt_new", "new.txt", 200)

	builder := testBuilder(store, blobs, 1000)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", old),
		userMessage("wmsg_2", fresh),
		{ID: "wmsg_3", Role: "assistant", Content: encodeBlocks([]storedBlock{{Type: blockText, Text: "reply"}})},
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}
	if !strings.Contains(string(payload), "Attached file new.txt") {
		t.Errorf("the newest user attachment lost the budget to an older file:\n%s", payload)
	}
}

// With no attachment storage configured every attachment degrades to a
// placeholder rather than failing the turn.
func TestPayloadWithoutAttachmentStorage(t *testing.T) {
	builder := newPayloadBuilder(&Dependencies{}, "tnt_1", "usr_1")
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", storedBlock{Type: blockAttachment, AttachmentID: "watt_1", Filename: "f.txt", MimeType: "text/plain"}),
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}
	if !strings.Contains(string(payload), "attachment storage unavailable") {
		t.Errorf("expected a placeholder:\n%s", payload)
	}
}

// The budget bounds the request body, and an image travels base64-encoded at
// four thirds of its size. Charging the file size instead would let a full
// budget produce a body a third larger than the cap.
func TestImageBudgetCountsBase64Expansion(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	image := imageAttachment(store, blobs, "watt_img", "diagram.png", 900)

	// 900 raw bytes fit in a 1000-byte budget; 1200 encoded bytes do not.
	builder := testBuilder(store, blobs, 1000)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", image),
	}, true)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}

	if !strings.Contains(string(payload), "too large to include in this turn") {
		t.Errorf("an image past the encoded budget was still inlined:\n%s", payload[:min(len(payload), 200)])
	}
	if blobs.reads["obj/watt_img"] != 0 {
		t.Error("the image was fetched even though it could not fit; the projection exists to avoid that")
	}
}

func TestImageChargedAtEncodedSize(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	image := imageAttachment(store, blobs, "watt_img", "diagram.png", 900)

	builder := testBuilder(store, blobs, payloadAttachmentBudget)
	if _, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", image),
	}, true); err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}

	encoded, _ := json.Marshal(builder.rendered[image.AttachmentID])
	want := int64(payloadAttachmentBudget) - int64(len(encoded))
	if builder.remaining != want {
		t.Errorf("remaining = %d, want %d (the encoded size, not the file size)", builder.remaining, want)
	}
}

// A PDF is inlined as extracted text for OpenAI-protocol providers and as a
// base64 document for Anthropic, so the projection differs by protocol.
func TestPayloadSizeProjectsByKindAndProtocol(t *testing.T) {
	cases := []struct {
		kind      string
		anthropic bool
		want      int64
	}{
		{kindImage, false, base64Size(3000)},
		{kindImage, true, base64Size(3000)},
		{kindPDF, true, base64Size(3000)},
		{kindPDF, false, 0},
		{kindText, false, 3000},
	}
	for _, tc := range cases {
		record := &pgstore.WebAttachment{Kind: tc.kind, SizeBytes: 3000}
		if got := payloadSize(record, tc.anthropic); got != tc.want {
			t.Errorf("payloadSize(%s, anthropic=%v) = %d, want %d", tc.kind, tc.anthropic, got, tc.want)
		}
	}
}

// The point of charging encoded bytes is an invariant: whatever the mix of
// attachments, what they contribute to the body stays inside the budget.
func TestPayloadStaysWithinBudget(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	history := []*pgstore.WebMessage{}
	for i := range 3 {
		block := imageAttachment(store, blobs, fmt.Sprintf("watt_%d", i), fmt.Sprintf("shot-%d.png", i), 400)
		history = append(history, userMessage(fmt.Sprintf("wmsg_%d", i), block))
	}

	builder := testBuilder(store, blobs, 1000)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", history, true)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}

	if builder.remaining < 0 {
		t.Errorf("the budget was overspent by %d bytes", -builder.remaining)
	}
	if !strings.Contains(string(payload), "too large to include in this turn") {
		t.Error("three 400-byte images cannot all fit a 1000-byte encoded budget")
	}
}

// A signed upload URL is a bearer URL, replayable until it expires, so an
// object can be overwritten after finalize verified it. A grown object must
// degrade to a placeholder: never an unbounded read, never a payload the
// budget projection never saw.
func TestGrownAttachmentDegradesInsteadOfBuffering(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	block := textAttachment(store, blobs, "watt_grown", "notes.txt", 16)
	// The upload URL is replayed with something far larger than what finalize
	// saw; the store hands back at most the requested window.
	blobs.objects["obj/watt_grown"] = []byte(strings.Repeat("y", 1<<20))

	builder := testBuilder(store, blobs, payloadAttachmentBudget)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", block),
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}
	if !strings.Contains(string(payload), "could not be verified") {
		t.Errorf("a swapped attachment was not refused:\n%s", payload[:min(len(payload), 300)])
	}
	if notes := strings.Join(builder.Notes(), "\n"); !strings.Contains(notes, "no longer matches its verified upload") {
		t.Errorf("the swap was not reported to the user: %q", notes)
	}
	if got := blobs.limits["obj/watt_grown"]; got != 17 {
		t.Errorf("read limit = %d, want the finalized size + 1 (17); the builder must bound its reads", got)
	}
}

// Same size, different bytes: only the digest catches this swap. Without it a
// replayed upload could substitute content after verification.
func TestDigestMismatchDegrades(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	block := textAttachment(store, blobs, "watt_tampered", "notes.txt", 16)
	blobs.objects["obj/watt_tampered"] = []byte(strings.Repeat("z", 16))

	builder := testBuilder(store, blobs, payloadAttachmentBudget)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("wmsg_1", block),
	}, false)
	if err != nil {
		t.Fatalf("BuildOpenAI: %v", err)
	}
	if !strings.Contains(string(payload), "could not be verified") {
		t.Errorf("same-size swapped bytes were sent unverified:\n%s", payload)
	}
	if strings.Contains(string(payload), "zzzz") {
		t.Error("the substituted bytes reached the payload")
	}
}

func TestPDFExpandedTextMustFitFinalJSONBudget(t *testing.T) {
	// This models a highly compressible PDF stream. Actual parsing and OS limits
	// are covered by internal/pdfextract's worker integration tests.
	var compressed bytes.Buffer
	zip := zlib.NewWriter(&compressed)
	_, _ = zip.Write([]byte(strings.Repeat("A", 65536)))
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	data := compressed.Bytes()
	store := fakeAttachments{
		"pdf": {ID: "pdf", Filename: "compressed.pdf", MimeType: "application/pdf", Kind: kindPDF, SizeBytes: int64(len(data)), ObjectKey: "pdf", SHA256: sha256Hex(data)},
	}
	blobs := &fakeBlobs{objects: map[string][]byte{"pdf": data}}
	builder := testBuilder(store, blobs, 4096)
	builder.extractPDF = func(_ context.Context, _ []byte, outputLimit int64) (string, error) {
		if outputLimit != 4096 {
			t.Fatalf("worker output cap = %d, want remaining budget", outputLimit)
		}
		// Even a worker returning too much must not overspend the builder.
		return strings.Repeat("A", 65536), nil
	}
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("new", storedBlock{Type: blockAttachment, AttachmentID: "pdf", Filename: "compressed.pdf"}),
	}, false)
	if err != nil || builder.remaining < 0 || strings.Contains(string(payload), strings.Repeat("A", 1024)) || !strings.Contains(string(payload), "too large to include") {
		t.Fatalf("expanded text escaped final budget: remaining=%d payload=%d err=%v", builder.remaining, len(payload), err)
	}
}

func TestPDFWorkerFailureDegradesAndAnthropicBypassesExtraction(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, pdfextract.ErrOutputLimit, pdfextract.ErrBusy} {
		store := fakeAttachments{}
		blobs := &fakeBlobs{objects: map[string][]byte{}}
		block := textAttachment(store, blobs, "pdf", "sample.pdf", 64)
		store["pdf"].Kind, store["pdf"].MimeType = kindPDF, "application/pdf"
		builder := testBuilder(store, blobs, 4096)
		builder.extractPDF = func(context.Context, []byte, int64) (string, error) { return "", failure }
		payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{userMessage("new", block)}, false)
		if err != nil || strings.Contains(string(payload), "Attached file") || len(builder.Notes()) == 0 {
			t.Fatalf("worker failure did not degrade safely: failure=%v payload=%s err=%v", failure, payload, err)
		}
		builder = testBuilder(store, blobs, 4096)
		builder.extractPDF = func(context.Context, []byte, int64) (string, error) {
			t.Fatal("Anthropic called the PDF worker")
			return "", nil
		}
		payload, err = builder.BuildAnthropic(context.Background(), "m", "", []*pgstore.WebMessage{userMessage("new", block)}, false)
		if err != nil || !strings.Contains(string(payload), `"type":"document"`) {
			t.Fatalf("native PDF path failed: payload=%s err=%v", payload, err)
		}
	}
}

func TestEscapedAndFencedTextMustFitJSONBudget(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	block := textAttachment(store, blobs, "quotes", "quote.txt", 64)
	data := []byte(strings.Repeat(`"`, 64))
	blobs.objects["obj/quotes"] = data
	store["quotes"].SHA256 = sha256Hex(data)
	builder := testBuilder(store, blobs, 100)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{userMessage("new", block)}, false)
	if err != nil || builder.remaining < 0 || strings.Contains(string(payload), "Attached file") {
		t.Fatalf("JSON escaping overspent budget: remaining=%d payload=%s err=%v", builder.remaining, payload, err)
	}
}

func TestRepeatedCachedAttachmentChargesEachOccurrence(t *testing.T) {
	store := fakeAttachments{}
	blobs := &fakeBlobs{objects: map[string][]byte{}}
	block := textAttachment(store, blobs, "repeat", "repeat.txt", 100)
	builder := testBuilder(store, blobs, 220)
	payload, err := builder.BuildOpenAI(context.Background(), "m", "", []*pgstore.WebMessage{
		userMessage("old", block), userMessage("new", block),
	}, false)
	if err != nil || strings.Count(string(payload), "Attached file repeat.txt") != 1 || !strings.Contains(string(payload), "too large to include") {
		t.Fatalf("cached replay bypassed budget/current-turn priority: payload=%s err=%v", payload, err)
	}
	if blobs.reads["obj/repeat"] != 1 || builder.remaining < 0 {
		t.Fatalf("cached attachment read/charge invalid: reads=%d remaining=%d", blobs.reads["obj/repeat"], builder.remaining)
	}
}
