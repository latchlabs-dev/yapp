package sessionarchive

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
)

// MemoryStateStore is an in-process StateStore for tests and read-only dry
// runs. Transactions work on copies and commit only when the callback
// succeeds, mirroring the Postgres store's semantics.
type MemoryStateStore struct {
	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	states map[string]*State
	turns  map[string][]Turn // by transcript id
}

// NewMemoryStateStore returns an empty in-memory state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{
		locks:  make(map[string]*sync.Mutex),
		states: make(map[string]*State),
		turns:  make(map[string][]Turn),
	}
}

func conversationKey(tenantID, conversationID string) string {
	return tenantID + "\x00" + conversationID
}

// WithConversationLock implements StateStore.
func (s *MemoryStateStore) WithConversationLock(ctx context.Context, tenantID, conversationID string, fn func(tx StateTx) error) error {
	key := conversationKey(tenantID, conversationID)
	s.mu.Lock()
	lock, ok := s.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	tx := &memoryTx{key: key, turns: make(map[string][]Turn)}
	if st := s.states[key]; st != nil {
		tx.state = cloneState(st)
		tx.turns[st.TranscriptID] = append([]Turn(nil), s.turns[st.TranscriptID]...)
	}
	s.mu.Unlock()

	if err := fn(tx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.saved {
		committed := cloneState(tx.state)
		if cur := s.states[key]; cur != nil && !tx.exact && cur.NeedsRebuild {
			committed.NeedsRebuild = true
			committed.RebuildRequestedAt = cur.RebuildRequestedAt
		}
		committed.UpdatedAt = time.Now()
		s.states[key] = committed
	}
	for transcriptID, turns := range tx.turns {
		if tx.touched[transcriptID] {
			s.turns[transcriptID] = turns
		}
	}
	return nil
}

// Get implements StateStore.
func (s *MemoryStateStore) Get(_ context.Context, tenantID, conversationID string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.states[conversationKey(tenantID, conversationID)]; st != nil {
		return cloneState(st), nil
	}
	return nil, nil
}

// ListTurns implements StateStore.
func (s *MemoryStateStore) ListTurns(_ context.Context, transcriptID string, generation int) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filterGeneration(s.turns[transcriptID], generation), nil
}

// MarkNeedsRebuild implements StateStore.
func (s *MemoryStateStore) MarkNeedsRebuild(_ context.Context, p *capture.Payload, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey(p.TenantID, p.ConversationID)
	now := time.Now()
	st := s.states[key]
	if st == nil {
		st = &State{
			TranscriptID: id.New("strn"), TenantID: p.TenantID, ConversationID: p.ConversationID,
			Generation: 1, StartedAt: p.CreatedAt, LastActivityAt: p.CreatedAt, CreatedAt: now,
		}
		s.states[key] = st
	}
	st.NeedsRebuild = true
	st.RebuildRequestedAt = &now
	st.LastError = cause
	st.UpdatedAt = now
	return nil
}

// SetGeneration forces a conversation's generation; tests use it to simulate a
// concurrent rebuild winning the swap.
func (s *MemoryStateStore) SetGeneration(tenantID, conversationID string, generation int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.states[conversationKey(tenantID, conversationID)]; st != nil {
		st.Generation = generation
	}
}

type memoryTx struct {
	key     string
	state   *State
	saved   bool
	exact   bool
	turns   map[string][]Turn
	touched map[string]bool
}

func (tx *memoryTx) Load(context.Context) (*State, error) {
	if tx.state == nil {
		return nil, nil
	}
	return cloneState(tx.state), nil
}

func (tx *memoryTx) Save(_ context.Context, state *State, exact bool) error {
	if tx.state != nil && tx.state.TranscriptID != "" && tx.state.TranscriptID != state.TranscriptID {
		// The unique (tenant, conversation) row keeps its id, as in Postgres.
		state.TranscriptID = tx.state.TranscriptID
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now()
	}
	tx.state = cloneState(state)
	tx.saved = true
	tx.exact = exact
	return nil
}

func (tx *memoryTx) InsertTurns(_ context.Context, transcriptID string, turns []Turn) error {
	tx.touch(transcriptID)
	tx.turns[transcriptID] = append(tx.turns[transcriptID], turns...)
	sort.SliceStable(tx.turns[transcriptID], func(i, j int) bool {
		a, b := tx.turns[transcriptID][i], tx.turns[transcriptID][j]
		if a.Generation != b.Generation {
			return a.Generation < b.Generation
		}
		return a.Seq < b.Seq
	})
	return nil
}

func (tx *memoryTx) ListTurns(_ context.Context, transcriptID string, generation int) ([]Turn, error) {
	return filterGeneration(tx.turns[transcriptID], generation), nil
}

func (tx *memoryTx) DeleteGeneration(_ context.Context, transcriptID string, generation int) error {
	tx.touch(transcriptID)
	kept := tx.turns[transcriptID][:0:0]
	for _, t := range tx.turns[transcriptID] {
		if t.Generation != generation {
			kept = append(kept, t)
		}
	}
	tx.turns[transcriptID] = kept
	return nil
}

func (tx *memoryTx) touch(transcriptID string) {
	if tx.touched == nil {
		tx.touched = make(map[string]bool)
	}
	tx.touched[transcriptID] = true
}

func filterGeneration(turns []Turn, generation int) []Turn {
	out := make([]Turn, 0, len(turns))
	for _, t := range turns {
		if t.Generation == generation {
			out = append(out, t)
		}
	}
	return out
}

func cloneState(st *State) *State {
	c := *st
	c.LineHashes = append([]byte(nil), st.LineHashes...)
	if st.LastPayloadAt != nil {
		v := *st.LastPayloadAt
		c.LastPayloadAt = &v
	}
	if st.RebuildRequestedAt != nil {
		v := *st.RebuildRequestedAt
		c.RebuildRequestedAt = &v
	}
	return &c
}
