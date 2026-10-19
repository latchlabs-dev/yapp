package sessionarchive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// SegmentTurn is one turn record of a transcript object: the request's
// metadata, its input delta, and its output.
type SegmentTurn struct {
	Seq               int
	RequestID         string
	CreatedAt         time.Time
	Provider          string
	Model             string
	Status            string
	API               API
	Kind              TurnKind
	Base              int
	InputCount        int
	RequestTruncated  bool
	ResponseTruncated bool
	// System is set when the system text changed at this turn.
	System  *string
	Inputs  []IndexedItem
	Outputs []json.RawMessage
	Error   string
}

// IndexedItem is an input item with its position in the conversation line.
type IndexedItem struct {
	Index int
	Raw   json.RawMessage
}

// segmentLine is the JSONL record union. "t" discriminates: turn, system,
// input, output, error. A turn line starts each record.
type segmentLine struct {
	T                 string          `json:"t"`
	V                 int             `json:"v,omitempty"`
	Seq               int             `json:"seq,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	CreatedAt         *time.Time      `json:"created_at,omitempty"`
	Provider          string          `json:"provider,omitempty"`
	Model             string          `json:"model,omitempty"`
	Status            string          `json:"status,omitempty"`
	API               API             `json:"api,omitempty"`
	Kind              TurnKind        `json:"kind,omitempty"`
	Base              int             `json:"base,omitempty"`
	InputCount        int             `json:"input_count,omitempty"`
	RequestTruncated  bool            `json:"request_truncated,omitempty"`
	ResponseTruncated bool            `json:"response_truncated,omitempty"`
	I                 *int            `json:"i,omitempty"`
	Raw               json.RawMessage `json:"raw,omitempty"`
	Text              string          `json:"text,omitempty"`
}

// encodeSegment serializes one turn as a gzip JSONL member. Merged transcripts
// are byte concatenations of such members, which gzip readers decode as one
// stream, so merging never recompresses.
func encodeSegment(turn *SegmentTurn) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)

	createdAt := turn.CreatedAt.UTC()
	lines := []segmentLine{{
		T: "turn", V: segmentFormatVersion, Seq: turn.Seq, RequestID: turn.RequestID, CreatedAt: &createdAt,
		Provider: turn.Provider, Model: turn.Model, Status: turn.Status, API: turn.API, Kind: turn.Kind,
		Base: turn.Base, InputCount: turn.InputCount,
		RequestTruncated: turn.RequestTruncated, ResponseTruncated: turn.ResponseTruncated,
	}}
	if turn.System != nil {
		lines = append(lines, segmentLine{T: "system", Text: *turn.System})
	}
	for _, input := range turn.Inputs {
		index := input.Index
		lines = append(lines, segmentLine{T: "input", I: &index, Raw: input.Raw})
	}
	for _, output := range turn.Outputs {
		lines = append(lines, segmentLine{T: "output", Raw: output})
	}
	if turn.Error != "" {
		lines = append(lines, segmentLine{T: "error", Text: turn.Error})
	}
	for i := range lines {
		if err := enc.Encode(&lines[i]); err != nil {
			_ = zw.Close()
			return nil, fmt.Errorf("encode segment line: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close segment gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeTranscript reads a segment or merged transcript object.
func decodeTranscript(data []byte) ([]SegmentTurn, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open transcript gzip: %w", err)
	}
	defer zr.Close()
	return decodeRecords(zr, false)
}

func decodeRecords(reader io.Reader, strict bool) ([]SegmentTurn, error) {
	dec := json.NewDecoder(reader)

	var turns []SegmentTurn
	for {
		var line segmentLine
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return turns, nil
			}
			return nil, fmt.Errorf("decode transcript line: %w", err)
		}
		if line.T == "turn" {
			if strict && line.V != 0 && line.V != segmentFormatVersion {
				return nil, fmt.Errorf("unsupported transcript format %d", line.V)
			}
			turn := SegmentTurn{
				Seq: line.Seq, RequestID: line.RequestID, Provider: line.Provider, Model: line.Model,
				Status: line.Status, API: line.API, Kind: line.Kind, Base: line.Base, InputCount: line.InputCount,
				RequestTruncated: line.RequestTruncated, ResponseTruncated: line.ResponseTruncated,
			}
			if line.CreatedAt != nil {
				turn.CreatedAt = *line.CreatedAt
			}
			turns = append(turns, turn)
			continue
		}
		if len(turns) == 0 {
			return nil, fmt.Errorf("transcript %q line before any turn", line.T)
		}
		current := &turns[len(turns)-1]
		switch line.T {
		case "system":
			text := line.Text
			current.System = &text
		case "input":
			index := 0
			if line.I != nil {
				index = *line.I
			}
			current.Inputs = append(current.Inputs, IndexedItem{Index: index, Raw: line.Raw})
		case "output":
			current.Outputs = append(current.Outputs, line.Raw)
		case "error":
			current.Error = line.Text
		default:
			if strict {
				return nil, fmt.Errorf("unknown transcript record %q", line.T)
			}
		}
	}
}
