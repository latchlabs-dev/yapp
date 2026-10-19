package sessionarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
)

// DefaultMergeSegments is the number of unmerged segments that triggers a merge.
const DefaultMergeSegments = 32

// TransactionTimeout bounds object I/O inside the archive transaction, below
// PgBouncer's idle transaction timeout. Large rebuild preparation stays outside.
const TransactionTimeout = time.Minute

// BuilderOptions tunes a Builder.
type BuilderOptions struct {
	MergeSegments int
	Logger        zerolog.Logger
	Now           func() time.Time
}

// Builder turns captured requests into transcript segments and state.
type Builder struct {
	objects  blob.Objects
	states   StateStore
	payloads PayloadSource
	// mergeSegments is the unmerged segment count that triggers a merge.
	mergeSegments int
	logger        zerolog.Logger
	now           func() time.Time

	mu         sync.Mutex
	rebuilding map[string]*rebuildCall
}

type rebuildCall struct {
	done  chan struct{}
	state *State
	err   error
}

// NewBuilder returns a transcript builder. payloads may be nil, in which case
// Rebuild is unavailable and a new transcript is not checked for older
// captures.
func NewBuilder(objects blob.Objects, states StateStore, payloads PayloadSource, opts BuilderOptions) *Builder {
	if opts.MergeSegments < 2 {
		opts.MergeSegments = DefaultMergeSegments
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Builder{
		objects:       objects,
		states:        states,
		payloads:      payloads,
		mergeSegments: opts.MergeSegments,
		logger:        opts.Logger,
		now:           opts.Now,
		rebuilding:    make(map[string]*rebuildCall),
	}
}

// States exposes the builder's state store to readers.
func (b *Builder) States() StateStore { return b.states }

// Objects exposes the builder's object store to readers.
func (b *Builder) Objects() blob.Objects { return b.objects }

// Apply appends one captured request to its conversation's transcript. It is
// a no-op for captures without a conversation id. Callers on the capture path
// must treat errors as "needs rebuild", never as a failed capture.
func (b *Builder) Apply(ctx context.Context, p *capture.Payload) (*Turn, error) {
	if p == nil || strings.TrimSpace(p.ConversationID) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, TransactionTimeout)
	defer cancel()
	prep := prepare(p)

	var applied *Turn
	var superseded []string
	err := b.states.WithConversationLock(ctx, p.TenantID, p.ConversationID, func(tx StateTx) error {
		applied, superseded = nil, nil
		state, err := tx.Load(ctx)
		if err != nil {
			return err
		}
		if state == nil {
			state = b.newState(p.TenantID, p.ConversationID, 1)
			if b.payloads != nil {
				earlier, err := b.payloads.HasRequestsBefore(ctx, p.TenantID, p.ConversationID, p.CreatedAt, p.RequestID)
				if err != nil {
					return fmt.Errorf("check earlier captures: %w", err)
				}
				if earlier {
					// The conversation predates the archive (or earlier
					// captures failed to apply): this turn alone would hide
					// that history, so ask for a rebuild.
					now := b.now()
					state.NeedsRebuild = true
					state.RebuildRequestedAt = &now
					state.LastError = "transcript started after earlier captures; rebuild required"
				}
			}
		} else if state.LastRequestID == p.RequestID {
			return nil // duplicate delivery of the request applied last
		} else if !state.StoredIn(b.objects.Location()) {
			// Appending here would mix keys from two stores; the caller flags
			// a rebuild, which regenerates the whole transcript in this store.
			return fmt.Errorf("%w: %s, now %s", errForeignObjectStore, state.ObjectsLocation, b.objects.Location())
		}
		if state.ObjectsLocation == "" {
			state.ObjectsLocation = b.objects.Location()
		}

		turn, err := b.writeTurn(ctx, state, prep, GenerationTag(state.Generation, ""))
		if err != nil {
			return err
		}
		if err := tx.Save(ctx, state, false); err != nil {
			return err
		}
		if err := tx.InsertTurns(ctx, state.TranscriptID, []Turn{turn}); err != nil {
			return err
		}
		if state.LastSeq-state.MergedThroughSeq >= b.mergeSegments {
			turns, err := tx.ListTurns(ctx, state.TranscriptID, state.Generation)
			if err != nil {
				return err
			}
			superseded, err = b.merge(ctx, state, turns, GenerationTag(state.Generation, ""))
			if err != nil {
				return err
			}
			if err := tx.Save(ctx, state, false); err != nil {
				return err
			}
		}
		applied = &turn
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.deleteBestEffort(ctx, superseded)
	return applied, nil
}

// Rebuild regenerates a conversation's transcript from its raw captures as a
// new generation and swaps it in atomically. Concurrent rebuilds of the same
// conversation in this process share one run.
func (b *Builder) Rebuild(ctx context.Context, tenantID, conversationID string) (*State, error) {
	if b.payloads == nil {
		return nil, errors.New("transcript rebuild requires a payload source")
	}
	key := tenantID + "\x00" + conversationID
	b.mu.Lock()
	if call, ok := b.rebuilding[key]; ok {
		b.mu.Unlock()
		select {
		case <-call.done:
			return call.state, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &rebuildCall{done: make(chan struct{})}
	b.rebuilding[key] = call
	b.mu.Unlock()

	call.state, call.err = b.rebuild(ctx, tenantID, conversationID)

	b.mu.Lock()
	delete(b.rebuilding, key)
	b.mu.Unlock()
	close(call.done)
	return call.state, call.err
}

// Rebuilding reports whether this process is rebuilding the conversation.
func (b *Builder) Rebuilding(tenantID, conversationID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.rebuilding[tenantID+"\x00"+conversationID]
	return ok
}

func (b *Builder) rebuild(ctx context.Context, tenantID, conversationID string) (*State, error) {
	started := b.now()
	refs, err := b.payloads.ListConversationRequests(ctx, tenantID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list captures: %w", err)
	}
	if len(refs) == 0 {
		return nil, ErrNoPayloads
	}
	current, err := b.states.Get(ctx, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	oldGeneration := 0
	state := b.newState(tenantID, conversationID, 1)
	if current != nil {
		oldGeneration = current.Generation
		state.TranscriptID = current.TranscriptID
		state.Generation = current.Generation + 1
	}
	tag := GenerationTag(state.Generation, id.New(""))

	var (
		turns   []Turn
		written []string // every new-generation object, removed if the swap fails
		garbage []string // new-generation segments folded into a merged object
	)
	applied := make(map[string]struct{}, len(refs))
	applyRef := func(ref PayloadRef) error {
		p, err := b.payloads.LoadPayload(ctx, ref.RequestID)
		if errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("load capture %s: %w", ref.RequestID, err)
		}
		if err != nil {
			return fmt.Errorf("load capture %s: %w", ref.RequestID, err)
		}
		applied[ref.RequestID] = struct{}{}
		turn, err := b.writeTurn(ctx, state, prepare(p), tag)
		if err != nil {
			return err
		}
		written = append(written, turn.SegmentKey)
		turns = append(turns, turn)
		if state.LastSeq-state.MergedThroughSeq >= b.mergeSegments {
			superseded, err := b.merge(ctx, state, turns, tag)
			if err != nil {
				return err
			}
			written = append(written, state.MergedKey)
			garbage = append(garbage, superseded...)
		}
		return nil
	}
	discard := func() { b.deleteBestEffort(context.WithoutCancel(ctx), written) }

	for _, ref := range refs {
		if err := applyRef(ref); err != nil {
			discard()
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, TransactionTimeout)
	defer cancel()
	superseded := make([]string, 0, len(turns)+1)
	err = b.states.WithConversationLock(ctx, tenantID, conversationID, func(tx StateTx) error {
		superseded = nil
		cur, err := tx.Load(ctx)
		if err != nil {
			return err
		}
		curGeneration := 0
		if cur != nil {
			curGeneration = cur.Generation
		}
		if curGeneration != oldGeneration || (cur != nil && cur.TranscriptID != state.TranscriptID) {
			return errRebuildRaced
		}
		// Catch up on captures applied to the old generation while this
		// generation was being built.
		latest, err := b.payloads.ListConversationRequests(ctx, tenantID, conversationID)
		if err != nil {
			return fmt.Errorf("list captures for catch-up: %w", err)
		}
		for _, ref := range latest {
			if _, ok := applied[ref.RequestID]; !ok {
				if err := applyRef(ref); err != nil {
					return err
				}
			}
		}
		state.NeedsRebuild = false
		state.LastError = ""
		if cur != nil {
			oldTurns, err := tx.ListTurns(ctx, cur.TranscriptID, cur.Generation)
			if err != nil {
				return err
			}
			for _, t := range oldTurns {
				if t.Seq > cur.MergedThroughSeq {
					superseded = append(superseded, t.SegmentKey)
				}
			}
			if cur.MergedKey != "" {
				superseded = append(superseded, cur.MergedKey)
			}
			// A capture that failed after this rebuild listed its inputs is
			// not in this generation; keep the flag so it gets another pass.
			state.RebuildRequestedAt = cur.RebuildRequestedAt
			state.NeedsRebuild = cur.RebuildRequestedAt != nil && cur.RebuildRequestedAt.After(started)
			if err := tx.DeleteGeneration(ctx, cur.TranscriptID, cur.Generation); err != nil {
				return err
			}
		}
		if err := tx.Save(ctx, state, true); err != nil {
			return err
		}
		return tx.InsertTurns(ctx, state.TranscriptID, turns)
	})
	if err != nil {
		discard()
		if errors.Is(err, errRebuildRaced) {
			return b.states.Get(ctx, tenantID, conversationID)
		}
		return nil, err
	}
	b.deleteBestEffort(ctx, append(superseded, garbage...))
	return state, nil
}

func (b *Builder) newState(tenantID, conversationID string, generation int) *State {
	return &State{
		TranscriptID:    id.New("strn"),
		TenantID:        tenantID,
		ConversationID:  conversationID,
		Generation:      generation,
		ObjectsLocation: b.objects.Location(),
	}
}

// writeTurn advances state by one capture and stores the turn's segment.
func (b *Builder) writeTurn(ctx context.Context, state *State, prep prepared, generationTag string) (Turn, error) {
	seq := state.LastSeq + 1
	turn, segment := advance(state, prep, seq)
	data, err := encodeSegment(segment)
	if err != nil {
		return Turn{}, err
	}
	key := SegmentKey(state.TenantID, state.ConversationID, generationTag, seq)
	if err := b.objects.Put(ctx, key, data, objectContentType); err != nil {
		return Turn{}, fmt.Errorf("store transcript segment: %w", err)
	}
	turn.SegmentKey = key
	turn.SegmentBytes = len(data)
	state.SegmentBytes += int64(len(data))
	return turn, nil
}

// merge folds the unmerged segments into a new merged object. turns must hold
// every turn of the state's generation. It returns the objects the merged
// object supersedes; delete them only after the new state is committed.
func (b *Builder) merge(ctx context.Context, state *State, turns []Turn, generationTag string) ([]string, error) {
	var buf bytes.Buffer
	superseded := make([]string, 0, len(turns)+1)
	if state.MergedKey != "" {
		data, err := b.objects.Read(ctx, state.MergedKey)
		if err != nil {
			return nil, fmt.Errorf("read merged transcript: %w", err)
		}
		buf.Write(data)
		superseded = append(superseded, state.MergedKey)
	}
	merged := 0
	for _, t := range turns {
		if t.Seq <= state.MergedThroughSeq || t.Seq > state.LastSeq {
			continue
		}
		data, err := b.objects.Read(ctx, t.SegmentKey)
		if err != nil {
			return nil, fmt.Errorf("read transcript segment %d: %w", t.Seq, err)
		}
		buf.Write(data)
		superseded = append(superseded, t.SegmentKey)
		merged++
	}
	if want := state.LastSeq - state.MergedThroughSeq; merged != want {
		return nil, fmt.Errorf("merge found %d unmerged segments, want %d", merged, want)
	}
	key := MergedKey(state.TenantID, state.ConversationID, generationTag, state.LastSeq)
	if err := b.objects.Put(ctx, key, buf.Bytes(), objectContentType); err != nil {
		return nil, fmt.Errorf("store merged transcript: %w", err)
	}
	state.MergedKey = key
	state.MergedThroughSeq = state.LastSeq
	state.MergedBytes = int64(buf.Len())
	state.SegmentBytes = 0
	return superseded, nil
}

func (b *Builder) deleteBestEffort(ctx context.Context, keys []string) {
	// Cleanup must not hold the sole repair worker or delay shutdown through
	// hundreds of cloud deletes. These are disposable derived objects; raw
	// capture archives are never included here.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		if key == "" {
			continue
		}
		if err := b.objects.Delete(ctx, key); err != nil {
			b.logger.Warn().Err(err).Str("key", key).Msg("failed to delete superseded transcript object")
		}
	}
}

// prepared is the lock-free, CPU-bound part of applying a capture.
type prepared struct {
	payload   *capture.Payload
	parsed    *ParsedRequest
	hashes    []byte
	systemSHA string
	outputs   []json.RawMessage
	errText   string
}

func prepare(p *capture.Payload) prepared {
	prep := prepared{payload: p}
	api := APIUnknown
	if parsed, err := ParseRequest(p.Provider, p.RequestBody); err == nil {
		prep.parsed = parsed
		prep.hashes = extendChain(nil, parsed.Items)
		prep.systemSHA = systemDigest(parsed.System)
		api = parsed.API
	}
	prep.outputs = ParseResponseOutput(api, p.ResponseFormat, p.ResponseBody)
	if len(prep.outputs) == 0 && p.Status != "" && p.Status != statusSuccess {
		prep.errText = truncateBytes(strings.TrimSpace(string(p.ResponseBody)), errorTextMaxBytes)
		if prep.errText == "" {
			prep.errText = p.ErrorClass
		}
	}
	return prep
}

// advance applies the §7.3 rules to state and returns the turn row and the
// segment record for this capture.
func advance(state *State, prep prepared, seq int) (Turn, *SegmentTurn) {
	p := prep.payload
	n := state.LineCount()
	turn := Turn{
		Generation: state.Generation, Seq: seq, RequestID: p.RequestID, CreatedAt: p.CreatedAt,
		Provider: p.Provider, Model: p.Model, Status: p.Status, OutputCount: len(prep.outputs),
		ToolNames: []string{},
	}
	segment := &SegmentTurn{
		Seq: seq, RequestID: p.RequestID, CreatedAt: p.CreatedAt, Provider: p.Provider, Model: p.Model,
		Status: p.Status, API: APIUnknown, RequestTruncated: p.RequestTruncated,
		ResponseTruncated: p.ResponseTruncated, Outputs: prep.outputs, Error: prep.errText,
	}

	var delta []json.RawMessage
	switch {
	case prep.parsed == nil:
		turn.Kind, turn.Base, turn.InputCount = KindUnparsed, n, n
	case prep.parsed.Append:
		turn.Kind, turn.Base = KindAppend, n
		delta = prep.parsed.Items
		state.LineHashes = extendChain(state.LineHashes, delta)
		turn.InputCount = state.LineCount()
	default:
		items := prep.parsed.Items
		m := len(items)
		common := commonPrefixItems(state.LineHashes, prep.hashes)
		turn.InputCount = m
		switch {
		case common == n && m >= n:
			turn.Kind, turn.Base = KindExtend, n
			delta = items[n:]
			state.LineHashes = prep.hashes
		case common == m && m < n:
			turn.Kind, turn.Base = KindPrefix, m
		default:
			turn.Kind, turn.Base = KindReset, common
			delta = items[common:]
			state.LineHashes = prep.hashes
			state.ResetCount++
		}
	}
	if prep.parsed != nil {
		segment.API = prep.parsed.API
		state.API = prep.parsed.API
		if prep.systemSHA != state.SystemSHA {
			turn.SystemChanged = true
			system := prep.parsed.System
			segment.System = &system
			state.SystemSHA = prep.systemSHA
		}
	}
	segment.Kind, segment.Base, segment.InputCount = turn.Kind, turn.Base, turn.InputCount
	turn.DeltaCount = len(delta)
	segment.Inputs = make([]IndexedItem, len(delta))
	for i, raw := range delta {
		segment.Inputs[i] = IndexedItem{Index: turn.Base + i, Raw: raw}
	}

	userTexts := 0
	var lastUser string
	for _, raw := range delta {
		for _, d := range NormalizeItem(raw) {
			if d.Kind != DisplayUser || strings.TrimSpace(d.Text) == "" {
				continue
			}
			if state.OpeningPrompt == "" {
				state.OpeningPrompt, _ = truncateRunes(d.Text, openingPromptMaxRunes)
			}
			lastUser = d.Text
			userTexts++
		}
	}
	// A reset resends history that was already counted; only its newest user
	// message is new.
	if turn.Kind == KindReset && userTexts > 1 {
		userTexts = 1
	}
	state.UserTurnCount += userTexts
	if lastUser != "" {
		turn.PromptPreview, _ = truncateRunes(lastUser, promptPreviewMaxRunes)
		state.LastPrompt = turn.PromptPreview
	}
	for _, raw := range prep.outputs {
		for _, d := range NormalizeItem(raw) {
			if d.Kind == DisplayToolCall {
				turn.ToolNames = append(turn.ToolNames, d.Name)
			}
		}
	}
	state.ToolCallCount += len(turn.ToolNames)

	state.RequestCount++
	if p.Status != "" && p.Status != statusSuccess {
		state.ErrorCount++
	}
	if p.Provider != "" {
		state.Provider = p.Provider
	}
	if p.Model != "" {
		state.Model = p.Model
	}
	if p.UserID != "" {
		state.UserID = p.UserID
	}
	if p.KeyID != "" {
		state.KeyID = p.KeyID
	}
	if state.StartedAt.IsZero() || p.CreatedAt.Before(state.StartedAt) {
		state.StartedAt = p.CreatedAt
	}
	activity := p.CreatedAt
	if p.CompletedAt != nil && p.CompletedAt.After(activity) {
		activity = *p.CompletedAt
	}
	if activity.After(state.LastActivityAt) {
		state.LastActivityAt = activity
	}
	createdAt := p.CreatedAt
	state.LastPayloadAt = &createdAt
	state.LastRequestID = p.RequestID
	state.LastOutputCount = len(prep.outputs)
	state.LastSeq = seq
	return turn, segment
}
