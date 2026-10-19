package sessionarchive

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
)

const (
	// DefaultPageTurns is the transcript page size when none is requested.
	DefaultPageTurns = 50
	// MaxPageTurns caps one transcript page.
	MaxPageTurns = 200

	segmentReadConcurrency = 8
)

// Reader renders stored transcripts.
type Reader struct {
	objects blob.Objects
}

// NewReader returns a transcript reader over objects.
func NewReader(objects blob.Objects) *Reader {
	return &Reader{objects: objects}
}

// TranscriptTurn is one rendered turn.
type TranscriptTurn struct {
	Seq               int           `json:"seq"`
	RequestID         string        `json:"request_id"`
	CreatedAt         time.Time     `json:"created_at"`
	Provider          string        `json:"provider"`
	Model             string        `json:"model"`
	Status            string        `json:"status"`
	API               API           `json:"api"`
	Kind              TurnKind      `json:"kind"`
	Base              int           `json:"base"`
	InputCount        int           `json:"input_count"`
	RequestTruncated  bool          `json:"request_truncated,omitempty"`
	ResponseTruncated bool          `json:"response_truncated,omitempty"`
	System            *string       `json:"system,omitempty"`
	Items             []DisplayItem `json:"items"`
	Output            []DisplayItem `json:"output"`
	Error             string        `json:"error,omitempty"`
}

// TranscriptPage is a window of rendered turns.
type TranscriptPage struct {
	Turns   []TranscriptTurn `json:"turns"`
	NextSeq *int             `json:"next_seq"`
	LastSeq int              `json:"last_seq"`
}

// Load returns every turn record of the state's generation in seq order:
// the merged object followed by the unmerged tail segments.
func (r *Reader) Load(ctx context.Context, state *State, turns []Turn) ([]SegmentTurn, error) {
	var all []SegmentTurn
	if state.MergedKey != "" {
		data, err := r.objects.Read(ctx, state.MergedKey)
		if err != nil {
			return nil, fmt.Errorf("read merged transcript: %w", err)
		}
		if all, err = decodeTranscript(data); err != nil {
			return nil, err
		}
	}

	var tail []Turn
	for _, t := range turns {
		if t.Generation == state.Generation && t.Seq > state.MergedThroughSeq {
			tail = append(tail, t)
		}
	}
	sort.Slice(tail, func(i, j int) bool { return tail[i].Seq < tail[j].Seq })

	decoded := make([][]SegmentTurn, len(tail))
	errs := make([]error, len(tail))
	sem := make(chan struct{}, segmentReadConcurrency)
	var wg sync.WaitGroup
	for i, t := range tail {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := r.objects.Read(ctx, key)
			if err != nil {
				errs[i] = fmt.Errorf("read transcript segment: %w", err)
				return
			}
			decoded[i], errs[i] = decodeTranscript(data)
		}(i, t.SegmentKey)
	}
	wg.Wait()
	for i := range tail {
		if errs[i] != nil {
			return nil, errs[i]
		}
		all = append(all, decoded[i]...)
	}
	return all, nil
}

// Page renders up to limit turns starting at the first turn with seq ≥ fromSeq.
func (r *Reader) Page(ctx context.Context, state *State, turns []Turn, fromSeq, limit int) (*TranscriptPage, error) {
	all, err := r.Load(ctx, state, turns)
	if err != nil {
		return nil, err
	}
	return renderPage(all, state.LastSeq, fromSeq, limit), nil
}

// RenderJSONLPage renders a selective, verified window from the Parquet service.
// It requires every requested turn plus the preceding turn, preserving echo
// markers at page boundaries. Incomplete windows must fall back to the source.
func RenderJSONLPage(data []byte, lastSeq, fromSeq, limit int) (*TranscriptPage, error) {
	fromSeq = max(1, fromSeq)
	limit = pageLimit(limit)
	all, err := decodeRecords(bytes.NewReader(data), true)
	if err != nil {
		return nil, err
	}
	if fromSeq > lastSeq {
		if len(all) != 0 {
			return nil, fmt.Errorf("unexpected records beyond last sequence")
		}
		return renderPage(nil, lastSeq, fromSeq, limit), nil
	}
	lo, hi := max(1, fromSeq-1), fromSeq+min(limit, lastSeq-fromSeq+1)-1
	if len(all) != hi-lo+1 {
		return nil, fmt.Errorf("incomplete Parquet page coverage")
	}
	for i := range all {
		if all[i].Seq != lo+i {
			return nil, fmt.Errorf("Parquet page sequence mismatch")
		}
	}
	page := renderPage(all, lastSeq, fromSeq, limit)
	if hi < lastSeq {
		next := hi + 1
		page.NextSeq = &next
	}
	return page, nil
}

func pageLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageTurns
	case limit > MaxPageTurns:
		return MaxPageTurns
	}
	return limit
}

func renderPage(all []SegmentTurn, lastSeq, fromSeq, limit int) *TranscriptPage {
	limit = pageLimit(limit)
	start := sort.Search(len(all), func(i int) bool { return all[i].Seq >= fromSeq })
	end := min(start+limit, len(all))

	page := &TranscriptPage{Turns: make([]TranscriptTurn, 0, end-start), LastSeq: lastSeq}
	for i := start; i < end; i++ {
		var prev *SegmentTurn
		if i > 0 {
			prev = &all[i-1]
		}
		page.Turns = append(page.Turns, renderTurn(&all[i], prev))
	}
	if end < len(all) {
		next := all[end].Seq
		page.NextSeq = &next
	}
	return page
}

// renderTurn normalizes a turn record. When the turn extends exactly the
// previous turn's line, its leading assistant-side items repeat the previous
// turn's output and are marked as echoes.
func renderTurn(turn, prev *SegmentTurn) TranscriptTurn {
	out := TranscriptTurn{
		Seq: turn.Seq, RequestID: turn.RequestID, CreatedAt: turn.CreatedAt, Provider: turn.Provider,
		Model: turn.Model, Status: turn.Status, API: turn.API, Kind: turn.Kind, Base: turn.Base,
		InputCount: turn.InputCount, RequestTruncated: turn.RequestTruncated,
		ResponseTruncated: turn.ResponseTruncated, System: turn.System, Error: turn.Error,
		Items: []DisplayItem{}, Output: []DisplayItem{},
	}

	echoBudget := 0
	if prev != nil && turn.Kind == KindExtend && turn.Base == prev.InputCount {
		echoBudget = len(prev.Outputs)
	}
	echoing := echoBudget > 0
	for j, input := range turn.Inputs {
		displays := NormalizeItem(input.Raw)
		echo := echoing && j < echoBudget && assistantSide(displays)
		if !echo {
			echoing = false
		}
		for _, d := range displays {
			index := input.Index
			d.Index = &index
			d.Echo = echo
			out.Items = append(out.Items, d)
		}
	}
	for _, raw := range turn.Outputs {
		out.Output = append(out.Output, NormalizeItem(raw)...)
	}
	return out
}

func assistantSide(items []DisplayItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		switch item.Kind {
		case DisplayAssistant, DisplayReasoning, DisplayToolCall:
		default:
			return false
		}
	}
	return true
}
