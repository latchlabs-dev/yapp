package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

// ErrConversationNotFound is returned for a missing, archived, or
// wrong-tenant conversation. The three are deliberately indistinguishable so a
// caller cannot probe another tenant's ID space.
var ErrConversationNotFound = errors.New("conversation not found")

var ErrConversationBusy = errors.New("conversation already has a turn in progress")
var ErrTurnLeaseLost = errors.New("conversation turn lease expired or was replaced")

// WebTurnLease is a renewable, fenced turn reservation. It survives process
// crashes only until ExpiresAt; no SQL connection or transaction is retained
// while waiting for upstream output.
type WebTurnLease struct {
	ConversationID string
	Token          string
	ExpiresAt      time.Time
}

// WebConversation is one browser chat thread.
type WebConversation struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"-"`
	UserID     string    `json:"-"`
	ProjectID  string    `json:"project_id,omitempty"`
	Title      string    `json:"title"`
	Model      string    `json:"model"`
	ProviderID string    `json:"provider_id"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// WebMessage is one turn in a conversation. Content is the normalized block
// list the web layer renders and replays into provider payloads, never raw
// provider JSON.
type WebMessage struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	Seq            int             `json:"seq"`
	Role           string          `json:"role"`
	Content        json.RawMessage `json:"content"`
	Model          string          `json:"model,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CostCents      float64         `json:"cost_total_cents"`
	Annotations    json.RawMessage `json:"annotations,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// WebConversationStore persists browser conversations and their messages.
type WebConversationStore struct {
	pool *Pool
}

// NewWebConversationStore creates a new WebConversationStore.
func NewWebConversationStore(pool *Pool) *WebConversationStore {
	return &WebConversationStore{pool: pool}
}

// Create inserts a new conversation owned by the given user.
func (s *WebConversationStore) Create(ctx context.Context, c *WebConversation) (*WebConversation, error) {
	if c.ID == "" {
		c.ID = id.New("wcv")
	}
	out := &WebConversation{}
	var projectID *string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO web_conversations (id, tenant_id, user_id, project_id, title, model, provider_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
		 RETURNING id, tenant_id, user_id, project_id, title, model, provider_id, created_at, updated_at`,
		c.ID, c.TenantID, c.UserID, nullableText(c.ProjectID), c.Title, c.Model, c.ProviderID,
	).Scan(&out.ID, &out.TenantID, &out.UserID, &projectID, &out.Title, &out.Model, &out.ProviderID,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}
	if projectID != nil {
		out.ProjectID = *projectID
	}
	return out, nil
}

// List returns a user's active conversations, most recently updated first.
func (s *WebConversationStore) List(ctx context.Context, tenantID, userID string, limit int) ([]*WebConversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, user_id, project_id, title, model, provider_id, created_at, updated_at
		   FROM web_conversations
		  WHERE tenant_id = $1 AND user_id = $2 AND archived_at IS NULL
		  ORDER BY updated_at DESC
		  LIMIT $3`,
		tenantID, userID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var result []*WebConversation
	for rows.Next() {
		c := &WebConversation{}
		var projectID *string
		if err := rows.Scan(&c.ID, &c.TenantID, &c.UserID, &projectID, &c.Title, &c.Model,
			&c.ProviderID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		if projectID != nil {
			c.ProjectID = *projectID
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// Get returns one active conversation owned by the given user.
func (s *WebConversationStore) Get(ctx context.Context, tenantID, userID, conversationID string) (*WebConversation, error) {
	c := &WebConversation{}
	var projectID *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, user_id, project_id, title, model, provider_id, created_at, updated_at
		   FROM web_conversations
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND archived_at IS NULL`,
		conversationID, tenantID, userID,
	).Scan(&c.ID, &c.TenantID, &c.UserID, &projectID, &c.Title, &c.Model, &c.ProviderID,
		&c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	if projectID != nil {
		c.ProjectID = *projectID
	}
	return c, nil
}

// ConversationUpdate carries the mutable conversation fields. A nil field is
// left unchanged, which lets the UI patch the title without resending the
// project or model.
type ConversationUpdate struct {
	Title     *string
	Model     *string
	Provider  *string
	ProjectID *string // pointer to empty string clears the project
}

// Update applies a partial update and returns the updated conversation.
func (s *WebConversationStore) Update(ctx context.Context, tenantID, userID, conversationID string, up ConversationUpdate) (*WebConversation, error) {
	c := &WebConversation{}
	var projectID *string
	var newProject interface{}
	if up.ProjectID != nil {
		newProject = nullableText(*up.ProjectID)
	}
	err := s.pool.QueryRow(ctx,
		`UPDATE web_conversations
		    SET title       = COALESCE($4, title),
		        model       = COALESCE($5, model),
		        provider_id = COALESCE($6, provider_id),
		        project_id  = CASE WHEN $7::bool THEN $8 ELSE project_id END,
		        updated_at  = now()
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND archived_at IS NULL
		  RETURNING id, tenant_id, user_id, project_id, title, model, provider_id, created_at, updated_at`,
		conversationID, tenantID, userID,
		up.Title, up.Model, up.Provider,
		up.ProjectID != nil, newProject,
	).Scan(&c.ID, &c.TenantID, &c.UserID, &projectID, &c.Title, &c.Model, &c.ProviderID,
		&c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update conversation: %w", err)
	}
	if projectID != nil {
		c.ProjectID = *projectID
	}
	return c, nil
}

// Archive soft-deletes a conversation, matching the tenant/user convention:
// the row stays for accounting, but reads no longer return it.
func (s *WebConversationStore) Archive(ctx context.Context, tenantID, userID, conversationID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE web_conversations SET archived_at = now(), updated_at = now()
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND archived_at IS NULL`,
		conversationID, tenantID, userID,
	)
	if err != nil {
		return fmt.Errorf("archive conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConversationNotFound
	}
	return nil
}

// AcquireTurn serializes model turns across replicas. Callers reject a busy
// conversation before persisting another prompt, and renew the short lease
// while generating. Expired reservations can be reclaimed after a crash.
func (s *WebConversationStore) AcquireTurn(ctx context.Context, tenantID, userID, conversationID string, ttl time.Duration) (*WebTurnLease, error) {
	if ttl <= 0 {
		return nil, errors.New("turn lease duration must be positive")
	}
	lease := &WebTurnLease{ConversationID: conversationID, Token: id.New("wturn")}
	err := s.pool.QueryRow(ctx,
		`UPDATE web_conversations
		    SET turn_token = $4, turn_expires_at = clock_timestamp() + $5 * interval '1 microsecond'
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND archived_at IS NULL
		    AND (turn_token IS NULL OR turn_expires_at <= clock_timestamp())
		 RETURNING turn_expires_at`, conversationID, tenantID, userID, lease.Token, ttl.Microseconds(),
	).Scan(&lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.Get(ctx, tenantID, userID, conversationID); getErr != nil {
			return nil, getErr
		}
		return nil, ErrConversationBusy
	}
	if err != nil {
		return nil, fmt.Errorf("acquire turn: %w", err)
	}
	return lease, nil
}

// RenewTurn cannot resurrect an expired lease. A delayed old worker must
// cancel, rather than become active alongside a replacement worker.
func (s *WebConversationStore) RenewTurn(ctx context.Context, lease *WebTurnLease, ttl time.Duration) (time.Time, error) {
	if lease == nil || lease.Token == "" {
		return time.Time{}, ErrTurnLeaseLost
	}
	if ttl <= 0 {
		return time.Time{}, errors.New("turn lease duration must be positive")
	}
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`UPDATE web_conversations
		    SET turn_expires_at = clock_timestamp() + $3 * interval '1 microsecond'
		  WHERE id = $1 AND turn_token = $2 AND turn_expires_at > clock_timestamp() AND archived_at IS NULL
		 RETURNING turn_expires_at`, lease.ConversationID, lease.Token, ttl.Microseconds(),
	).Scan(&expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrTurnLeaseLost
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("renew turn: %w", err)
	}
	return expiresAt, nil
}

// ReleaseTurn is token-scoped, so cleanup from an expired worker cannot
// release the replacement worker's reservation.
func (s *WebConversationStore) ReleaseTurn(ctx context.Context, lease *WebTurnLease) error {
	if lease == nil || lease.Token == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE web_conversations SET turn_token = NULL, turn_expires_at = NULL
		  WHERE id = $1 AND turn_token = $2`, lease.ConversationID, lease.Token)
	if err != nil {
		return fmt.Errorf("release turn: %w", err)
	}
	return nil
}

// AppendMessage atomically allocates the next sequence number by updating the
// conversation row, instead of reading MAX(seq) from an MVCC snapshot. Calls
// outside a model turn must not write through another worker's active lease.
func (s *WebConversationStore) AppendMessage(ctx context.Context, m *WebMessage) (*WebMessage, error) {
	return s.appendMessage(ctx, m, "")
}

// AppendMessageForTurn fences persistence against the current lease. Even a
// stalled worker whose cancellation has not run cannot write a late reply
// after another replica has recovered its expired turn.
func (s *WebConversationStore) AppendMessageForTurn(ctx context.Context, m *WebMessage, lease *WebTurnLease) (*WebMessage, error) {
	if lease == nil || lease.Token == "" || lease.ConversationID != m.ConversationID {
		return nil, ErrTurnLeaseLost
	}
	return s.appendMessage(ctx, m, lease.Token)
}

func (s *WebConversationStore) appendMessage(ctx context.Context, m *WebMessage, leaseToken string) (*WebMessage, error) {
	if m.ID == "" {
		m.ID = id.New("wmsg")
	}
	if len(m.Content) == 0 {
		m.Content = json.RawMessage(`[]`)
	}
	out := &WebMessage{}
	var model, requestID *string
	var annotations []byte
	err := s.pool.QueryRow(ctx,
		`WITH allocated AS (
		   UPDATE web_conversations SET next_message_seq = next_message_seq + 1
		    WHERE id = $2 AND archived_at IS NULL
		      AND (($11::text IS NULL AND (turn_token IS NULL OR turn_expires_at <= clock_timestamp()))
		        OR (turn_token = $11 AND turn_expires_at > clock_timestamp()))
		    RETURNING next_message_seq - 1 AS seq
		 )
		 INSERT INTO web_messages
		     (id, conversation_id, seq, role, content, model, request_id,
		      input_tokens, output_tokens, cost_total_cents, annotations, created_at)
		 SELECT $1, $2, allocated.seq,
		        $3, $4, $5, $6, $7, $8, $9, $10, now()
		   FROM allocated
		 RETURNING id, conversation_id, seq, role, content, model, request_id,
		           input_tokens, output_tokens, cost_total_cents, annotations, created_at`,
		m.ID, m.ConversationID, m.Role, m.Content, nullableText(m.Model), nullableText(m.RequestID),
		m.InputTokens, m.OutputTokens, m.CostCents, nullableJSON(m.Annotations), nullableText(leaseToken),
	).Scan(&out.ID, &out.ConversationID, &out.Seq, &out.Role, &out.Content, &model, &requestID,
		&out.InputTokens, &out.OutputTokens, &out.CostCents, &annotations, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if leaseToken != "" {
			return nil, ErrTurnLeaseLost
		}
		return nil, ErrConversationBusy
	}
	if err != nil {
		return nil, fmt.Errorf("append message: %w", err)
	}
	if model != nil {
		out.Model = *model
	}
	if requestID != nil {
		out.RequestID = *requestID
	}
	if len(annotations) > 0 {
		out.Annotations = annotations
	}
	return out, nil
}

// ListMessages returns a conversation's messages in order. limit caps how many
// of the most recent messages are returned, which is what bounds the history
// replayed into a provider payload.
func (s *WebConversationStore) ListMessages(ctx context.Context, conversationID string, limit int) ([]*WebMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.conversation_id, m.seq, m.role, m.content, m.model, m.request_id,
		        m.input_tokens, m.output_tokens,
		        COALESCE(ue.cost_total_cents, m.cost_total_cents) AS cost_total_cents,
		        m.annotations, m.created_at
		   FROM (
		     SELECT * FROM web_messages WHERE conversation_id = $1 ORDER BY seq DESC LIMIT $2
		   ) m
		   LEFT JOIN usage_events ue ON ue.request_id = m.request_id
		  ORDER BY m.seq ASC`,
		conversationID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var result []*WebMessage
	for rows.Next() {
		m := &WebMessage{}
		var model, requestID *string
		var annotations []byte
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Content, &model, &requestID,
			&m.InputTokens, &m.OutputTokens, &m.CostCents, &annotations, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if model != nil {
			m.Model = *model
		}
		if requestID != nil {
			m.RequestID = *requestID
		}
		if len(annotations) > 0 {
			m.Annotations = annotations
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// AnnotateMessage merges annotations into an already-stored message.
//
// It is how a prompt that was accepted and then failed stops looking like a
// message nobody answered: the reason is attached to the prompt itself, so it
// survives a reload and is visible on another device. Merging rather than
// replacing keeps any annotations a hook already recorded.
func (s *WebConversationStore) AnnotateMessage(ctx context.Context, messageID string, annotations map[string]string) error {
	if messageID == "" || len(annotations) == 0 {
		return nil
	}
	encoded, err := json.Marshal(annotations)
	if err != nil {
		return fmt.Errorf("encode annotations: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE web_messages
		    SET annotations = COALESCE(annotations, '{}'::jsonb) || $2::jsonb
		  WHERE id = $1`,
		messageID, encoded,
	)
	if err != nil {
		return fmt.Errorf("annotate message: %w", err)
	}
	return nil
}

// Touch bumps a conversation's updated_at so it sorts to the top of the list.
func (s *WebConversationStore) Touch(ctx context.Context, conversationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE web_conversations SET updated_at = now() WHERE id = $1`, conversationID)
	if err != nil {
		return fmt.Errorf("touch conversation: %w", err)
	}
	return nil
}

// nullableJSON stores NULL rather than an empty JSONB value for absent data.
func nullableJSON(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
