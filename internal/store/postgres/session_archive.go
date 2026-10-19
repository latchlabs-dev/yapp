package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

// DefaultSessionArchiveLockTimeout bounds how long a capture worker waits for
// a conversation's transcript lock before giving up and flagging a rebuild.
const DefaultSessionArchiveLockTimeout = 5 * time.Second

// SessionArchiveStore persists transcript state (session_transcripts) and turn
// summaries (session_turns). It implements sessionarchive.StateStore.
type SessionArchiveStore struct {
	pool             *Pool
	lockTimeout      time.Duration
	repairLeaseToken string
}

// WithRepairLease fences the generation swap to a live repair claim.
func (s *SessionArchiveStore) WithRepairLease(token string) *SessionArchiveStore {
	copy := *s
	copy.repairLeaseToken = token
	return &copy
}

// NewSessionArchiveStore returns a Postgres transcript state store.
func NewSessionArchiveStore(pool *Pool) *SessionArchiveStore {
	return &SessionArchiveStore{pool: pool, lockTimeout: DefaultSessionArchiveLockTimeout}
}

// WithLockTimeout overrides the conversation lock wait (tests).
func (s *SessionArchiveStore) WithLockTimeout(timeout time.Duration) *SessionArchiveStore {
	s.lockTimeout = timeout
	return s
}

var _ sessionarchive.StateStore = (*SessionArchiveStore)(nil)

// WithConversationLock runs fn inside one transaction holding a transaction
// scoped advisory lock for the conversation.
func (s *SessionArchiveStore) WithConversationLock(ctx context.Context, tenantID, conversationID string, fn func(tx sessionarchive.StateTx) error) error {
	// Proxy and admin each reserve at most two archive backends. Archive
	// callbacks can read payloads or Postgres objects through another connection;
	// leave headroom in the shared ten-backend PgBouncer pool for those reads.
	// Bound cloud I/O while the transaction is pinned below the pooler's idle
	// transaction timeout. Waiting for admission also respects caller cancellation.
	ctx, cancel := context.WithTimeout(ctx, sessionarchive.TransactionTimeout)
	defer cancel()
	select {
	case s.pool.archiveSlots <- struct{}{}:
		defer func() { <-s.pool.archiveSlots }()
	case <-ctx.Done():
		return fmt.Errorf("wait for archive transaction slot: %w", ctx.Err())
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transcript transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", s.lockTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set transcript lock timeout: %w", err)
	}
	// Length-prefixing the tenant keeps (tenant, conversation) pairs distinct
	// without a separator character the ids might contain.
	lockKey := fmt.Sprintf("session_archive:%d:%s:%s", len(tenantID), tenantID, conversationID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return fmt.Errorf("lock conversation transcript: %w", err)
	}
	if s.repairLeaseToken != "" {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT true FROM session_transcripts
		 WHERE COALESCE(tenant_id,'')=$1 AND conversation_id=$2
		 AND repair_lease_token=$3 AND repair_lease_expires_at>clock_timestamp() FOR UPDATE`,
			tenantID, conversationID, s.repairLeaseToken).Scan(&live); errors.Is(err, pgx.ErrNoRows) {
			return ErrRepairLeaseLost
		} else if err != nil {
			return err
		}
	}
	if err := fn(&archiveTx{tx: tx, tenantID: tenantID, conversationID: conversationID}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transcript transaction: %w", err)
	}
	return nil
}

// Get returns a conversation's transcript state, or nil when none exists.
func (s *SessionArchiveStore) Get(ctx context.Context, tenantID, conversationID string) (*sessionarchive.State, error) {
	return loadTranscriptState(ctx, s.pool, tenantID, conversationID)
}

// ListTurns returns one generation's turns in seq order.
func (s *SessionArchiveStore) ListTurns(ctx context.Context, transcriptID string, generation int) ([]sessionarchive.Turn, error) {
	return listTranscriptTurns(ctx, s.pool, transcriptID, generation)
}

// FindByConversation returns every tenant's transcript for a conversation id,
// most recently active first. Conversation ids are caller-supplied, so the
// same id can exist under more than one tenant.
func (s *SessionArchiveStore) FindByConversation(ctx context.Context, conversationID string) ([]*sessionarchive.State, error) {
	rows, err := s.pool.Query(ctx, selectTranscriptSQL+` WHERE conversation_id = $1 ORDER BY last_activity_at DESC`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("find transcripts for conversation: %w", err)
	}
	defer rows.Close()
	var states []*sessionarchive.State
	for rows.Next() {
		st, err := scanTranscriptState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, st)
	}
	return states, rows.Err()
}

// ConversationTenant is one tenant holding captures for a conversation id.
type ConversationTenant struct {
	TenantID      string
	TenantName    string
	LastPayloadAt time.Time
	RequestCount  int
}

// ConversationTenants lists the tenants with captured requests for a
// conversation id, most recently active first.
func (s *SessionArchiveStore) ConversationTenants(ctx context.Context, conversationID string) ([]ConversationTenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(cp.tenant_id, ''), COALESCE(t.name, ''), MAX(cp.created_at), COUNT(*)
		  FROM conversation_payloads cp
		  LEFT JOIN tenants t ON t.id = cp.tenant_id
		 WHERE cp.conversation_id = $1
		 GROUP BY 1, 2
		 ORDER BY 3 DESC
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list conversation tenants: %w", err)
	}
	defer rows.Close()
	var tenants []ConversationTenant
	for rows.Next() {
		var ct ConversationTenant
		if err := rows.Scan(&ct.TenantID, &ct.TenantName, &ct.LastPayloadAt, &ct.RequestCount); err != nil {
			return nil, fmt.Errorf("scan conversation tenant: %w", err)
		}
		tenants = append(tenants, ct)
	}
	return tenants, rows.Err()
}

// TurnCosts returns recorded cost per request id for one transcript
// generation. Requests without a usage event yet are absent.
func (s *SessionArchiveStore) TurnCosts(ctx context.Context, transcriptID string, generation int) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT st.request_id, COALESCE(SUM(ue.cost_total_cents), 0)
		  FROM session_turns st
		  JOIN usage_events ue ON ue.request_id = st.request_id
		 WHERE st.transcript_id = $1 AND st.generation = $2
		 GROUP BY st.request_id
	`, transcriptID, generation)
	if err != nil {
		return nil, fmt.Errorf("sum transcript turn costs: %w", err)
	}
	defer rows.Close()
	costs := make(map[string]float64)
	for rows.Next() {
		var requestID string
		var cents float64
		if err := rows.Scan(&requestID, &cents); err != nil {
			return nil, fmt.Errorf("scan transcript turn cost: %w", err)
		}
		costs[requestID] = cents
	}
	return costs, rows.Err()
}

// SessionIP is one distinct client IP observed among a transcript's
// requests, with how many requests carried it and when it was first and
// last seen. Used to trace which devices/servers drove a session.
type SessionIP struct {
	IP        string
	Requests  int
	FirstSeen time.Time
	LastSeen  time.Time
}

// TurnIPs aggregates the client IPs recorded for one transcript generation's
// requests (usage_events.client_ip), most-used first. Requests without a
// usage event or a recorded IP are absent.
func (s *SessionArchiveStore) TurnIPs(ctx context.Context, transcriptID string, generation int) ([]SessionIP, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host(ue.client_ip) AS ip, COUNT(*) AS requests,
		       MIN(ue.started_at) AS first_seen,
		       MAX(COALESCE(ue.completed_at, ue.started_at)) AS last_seen
		  FROM session_turns st
		  JOIN usage_events ue ON ue.request_id = st.request_id
		 WHERE st.transcript_id = $1 AND st.generation = $2 AND ue.client_ip IS NOT NULL
		 GROUP BY 1
		 ORDER BY requests DESC, ip
	`, transcriptID, generation)
	if err != nil {
		return nil, fmt.Errorf("aggregate transcript client ips: %w", err)
	}
	defer rows.Close()
	ips := []SessionIP{}
	for rows.Next() {
		var ip SessionIP
		if err := rows.Scan(&ip.IP, &ip.Requests, &ip.FirstSeen, &ip.LastSeen); err != nil {
			return nil, fmt.Errorf("scan transcript client ip: %w", err)
		}
		ips = append(ips, ip)
	}
	return ips, rows.Err()
}

// OwnerNames are display names for a transcript's tenant and user.
type OwnerNames struct {
	TenantName      string
	UserDisplayName string
	UserEmail       string
}

// OwnerNames resolves display names; unknown ids resolve to empty strings.
func (s *SessionArchiveStore) OwnerNames(ctx context.Context, tenantID, userID string) (OwnerNames, error) {
	var names OwnerNames
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT name FROM tenants WHERE id = $1), ''),
		       COALESCE((SELECT display_name FROM users WHERE id = $2), ''),
		       COALESCE((SELECT email FROM users WHERE id = $2), '')
	`, tenantID, userID).Scan(&names.TenantName, &names.UserDisplayName, &names.UserEmail)
	if err != nil {
		return OwnerNames{}, fmt.Errorf("resolve transcript owner names: %w", err)
	}
	return names, nil
}

// MarkNeedsRebuild flags a conversation whose capture could not be applied.
// It deliberately takes no advisory lock: a lock timeout is one of its causes.
func (s *SessionArchiveStore) MarkNeedsRebuild(ctx context.Context, p *capture.Payload, cause string) error {
	requestedAt := time.Now().UTC()
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = requestedAt
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO session_transcripts
			(id, tenant_id, user_id, key_id, conversation_id, provider, model,
			 started_at, last_activity_at, needs_rebuild, rebuild_requested_at, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, true, $9, $10)
		ON CONFLICT ((COALESCE(tenant_id, '')), conversation_id) DO UPDATE SET
			needs_rebuild = true,
			rebuild_requested_at = EXCLUDED.rebuild_requested_at,
			last_error = EXCLUDED.last_error,
			updated_at = now()
	`, id.New("strn"), nullableText(p.TenantID), nullableText(p.UserID), nullableText(p.KeyID), p.ConversationID,
		p.Provider, p.Model, createdAt, requestedAt, cause)
	if err != nil {
		return fmt.Errorf("flag transcript for rebuild: %w", err)
	}
	return nil
}

type archiveTx struct {
	tx             pgx.Tx
	tenantID       string
	conversationID string
}

func (t *archiveTx) Load(ctx context.Context) (*sessionarchive.State, error) {
	return loadTranscriptState(ctx, t.tx, t.tenantID, t.conversationID)
}

func (t *archiveTx) Save(ctx context.Context, st *sessionarchive.State, exact bool) error {
	var storedID string
	err := t.tx.QueryRow(ctx, `
		INSERT INTO session_transcripts
			(id, tenant_id, user_id, key_id, conversation_id, provider, model, api,
			 opening_prompt, last_prompt, request_count, user_turn_count, tool_call_count,
			 error_count, reset_count, started_at, last_activity_at, generation, line_count,
			 line_hashes, system_sha, last_seq, merged_through_seq, merged_key, merged_bytes,
			 segment_bytes, last_output_count, last_payload_at, last_request_id,
			 objects_location, needs_rebuild, rebuild_requested_at, last_error, updated_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8,
			 $9, $10, $11, $12, $13,
			 $14, $15, $16, $17, $18, $19,
			 $20, $21, $22, $23, $24, $25,
			 $26, $27, $28, $29,
			 $30, $31, $32, $33, now())
		ON CONFLICT ((COALESCE(tenant_id, '')), conversation_id) DO UPDATE SET
			user_id = EXCLUDED.user_id,
			key_id = EXCLUDED.key_id,
			provider = EXCLUDED.provider,
			model = EXCLUDED.model,
			api = EXCLUDED.api,
			opening_prompt = EXCLUDED.opening_prompt,
			last_prompt = EXCLUDED.last_prompt,
			request_count = EXCLUDED.request_count,
			user_turn_count = EXCLUDED.user_turn_count,
			tool_call_count = EXCLUDED.tool_call_count,
			error_count = EXCLUDED.error_count,
			reset_count = EXCLUDED.reset_count,
			started_at = EXCLUDED.started_at,
			last_activity_at = EXCLUDED.last_activity_at,
			generation = EXCLUDED.generation,
			line_count = EXCLUDED.line_count,
			line_hashes = EXCLUDED.line_hashes,
			system_sha = EXCLUDED.system_sha,
			last_seq = EXCLUDED.last_seq,
			merged_through_seq = EXCLUDED.merged_through_seq,
			merged_key = EXCLUDED.merged_key,
			merged_bytes = EXCLUDED.merged_bytes,
			segment_bytes = EXCLUDED.segment_bytes,
			last_output_count = EXCLUDED.last_output_count,
			last_payload_at = EXCLUDED.last_payload_at,
			last_request_id = EXCLUDED.last_request_id,
			objects_location = EXCLUDED.objects_location,
			needs_rebuild = CASE WHEN $34 THEN EXCLUDED.needs_rebuild
			                     ELSE session_transcripts.needs_rebuild OR EXCLUDED.needs_rebuild END,
			rebuild_requested_at = CASE WHEN $34 THEN EXCLUDED.rebuild_requested_at
			                     ELSE GREATEST(session_transcripts.rebuild_requested_at, EXCLUDED.rebuild_requested_at) END,
			last_error = CASE WHEN $34 THEN EXCLUDED.last_error
			                  ELSE COALESCE(EXCLUDED.last_error, session_transcripts.last_error) END,
			updated_at = now()
		RETURNING id
	`,
		st.TranscriptID, nullableText(st.TenantID), nullableText(st.UserID), nullableText(st.KeyID), st.ConversationID,
		st.Provider, st.Model, string(st.API),
		nullableText(st.OpeningPrompt), nullableText(st.LastPrompt), st.RequestCount, st.UserTurnCount, st.ToolCallCount,
		st.ErrorCount, st.ResetCount, st.StartedAt, st.LastActivityAt, st.Generation, st.LineCount(),
		nonNilBytes(st.LineHashes), st.SystemSHA, st.LastSeq, st.MergedThroughSeq, nullableText(st.MergedKey), st.MergedBytes,
		st.SegmentBytes, st.LastOutputCount, st.LastPayloadAt, nullableText(st.LastRequestID),
		st.ObjectsLocation, st.NeedsRebuild, st.RebuildRequestedAt, nullableText(st.LastError),
		exact,
	).Scan(&storedID)
	if err != nil {
		return fmt.Errorf("save transcript state: %w", err)
	}
	st.TranscriptID = storedID
	return nil
}

func (t *archiveTx) InsertTurns(ctx context.Context, transcriptID string, turns []sessionarchive.Turn) error {
	if len(turns) == 0 {
		return nil
	}
	rows := make([][]any, len(turns))
	for i, turn := range turns {
		toolNames := turn.ToolNames
		if toolNames == nil {
			toolNames = []string{}
		}
		rows[i] = []any{
			transcriptID, turn.Generation, turn.Seq, turn.RequestID, turn.CreatedAt, turn.Provider, turn.Model,
			turn.Status, string(turn.Kind), turn.Base, turn.InputCount, turn.DeltaCount, turn.OutputCount,
			turn.SystemChanged, nullableText(turn.PromptPreview), toolNames, turn.SegmentKey, turn.SegmentBytes,
		}
	}
	_, err := t.tx.CopyFrom(ctx, pgx.Identifier{"session_turns"}, []string{
		"transcript_id", "generation", "seq", "request_id", "created_at", "provider", "model",
		"status", "kind", "base", "input_count", "delta_count", "output_count",
		"system_changed", "prompt_preview", "tool_names", "segment_key", "segment_bytes",
	}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("insert transcript turns: %w", err)
	}
	return nil
}

func (t *archiveTx) ListTurns(ctx context.Context, transcriptID string, generation int) ([]sessionarchive.Turn, error) {
	return listTranscriptTurns(ctx, t.tx, transcriptID, generation)
}

func (t *archiveTx) DeleteGeneration(ctx context.Context, transcriptID string, generation int) error {
	if _, err := t.tx.Exec(ctx, `DELETE FROM session_turns WHERE transcript_id = $1 AND generation = $2`, transcriptID, generation); err != nil {
		return fmt.Errorf("delete transcript generation: %w", err)
	}
	return nil
}

type archiveQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const selectTranscriptSQL = `
	SELECT id, COALESCE(tenant_id, ''), COALESCE(user_id, ''), COALESCE(key_id, ''), conversation_id,
	       provider, model, api, COALESCE(opening_prompt, ''), COALESCE(last_prompt, ''),
	       request_count, user_turn_count, tool_call_count, error_count, reset_count,
	       started_at, last_activity_at, generation, line_hashes, system_sha,
	       last_seq, merged_through_seq, COALESCE(merged_key, ''), merged_bytes, segment_bytes,
	       last_output_count, last_payload_at, COALESCE(last_request_id, ''), objects_location,
	       needs_rebuild, rebuild_requested_at, COALESCE(last_error, ''), created_at, updated_at
	  FROM session_transcripts`

func loadTranscriptState(ctx context.Context, q archiveQueryer, tenantID, conversationID string) (*sessionarchive.State, error) {
	st, err := scanTranscriptState(q.QueryRow(ctx, selectTranscriptSQL+`
		 WHERE COALESCE(tenant_id, '') = $1 AND conversation_id = $2`, tenantID, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

func scanTranscriptState(row pgx.Row) (*sessionarchive.State, error) {
	st := &sessionarchive.State{}
	var api string
	err := row.Scan(
		&st.TranscriptID, &st.TenantID, &st.UserID, &st.KeyID, &st.ConversationID,
		&st.Provider, &st.Model, &api, &st.OpeningPrompt, &st.LastPrompt,
		&st.RequestCount, &st.UserTurnCount, &st.ToolCallCount, &st.ErrorCount, &st.ResetCount,
		&st.StartedAt, &st.LastActivityAt, &st.Generation, &st.LineHashes, &st.SystemSHA,
		&st.LastSeq, &st.MergedThroughSeq, &st.MergedKey, &st.MergedBytes, &st.SegmentBytes,
		&st.LastOutputCount, &st.LastPayloadAt, &st.LastRequestID, &st.ObjectsLocation,
		&st.NeedsRebuild, &st.RebuildRequestedAt, &st.LastError, &st.CreatedAt, &st.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan transcript state: %w", err)
	}
	st.API = sessionarchive.API(api)
	return st, nil
}

func listTranscriptTurns(ctx context.Context, q archiveQueryer, transcriptID string, generation int) ([]sessionarchive.Turn, error) {
	rows, err := q.Query(ctx, `
		SELECT generation, seq, request_id, created_at, provider, model, status, kind,
		       base, input_count, delta_count, output_count, system_changed,
		       COALESCE(prompt_preview, ''), tool_names, segment_key, segment_bytes
		  FROM session_turns
		 WHERE transcript_id = $1 AND generation = $2
		 ORDER BY seq
	`, transcriptID, generation)
	if err != nil {
		return nil, fmt.Errorf("list transcript turns: %w", err)
	}
	defer rows.Close()
	turns := []sessionarchive.Turn{}
	for rows.Next() {
		var turn sessionarchive.Turn
		var kind string
		if err := rows.Scan(
			&turn.Generation, &turn.Seq, &turn.RequestID, &turn.CreatedAt, &turn.Provider, &turn.Model, &turn.Status, &kind,
			&turn.Base, &turn.InputCount, &turn.DeltaCount, &turn.OutputCount, &turn.SystemChanged,
			&turn.PromptPreview, &turn.ToolNames, &turn.SegmentKey, &turn.SegmentBytes,
		); err != nil {
			return nil, fmt.Errorf("scan transcript turn: %w", err)
		}
		turn.Kind = sessionarchive.TurnKind(kind)
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
