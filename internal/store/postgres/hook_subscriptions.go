package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/id"
)

// ErrHookSubscriptionNotFound is returned for a missing subscription.
var ErrHookSubscriptionNotFound = errors.New("hook subscription not found")

// HookSubscriptionRecord is the stored form of a hook registration. The secret
// is encrypted at rest with the same encryptor that protects provider
// credentials, and is never returned to the admin API after creation.
type HookSubscriptionRecord struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	TenantID       string    `json:"tenant_id,omitempty"`
	ProjectID      string    `json:"project_id,omitempty"`
	EventType      string    `json:"event_type"`
	URL            string    `json:"url"`
	Secret         string    `json:"-"`
	HasSecret      bool      `json:"has_secret"`
	TimeoutMS      int       `json:"timeout_ms"`
	Mode           string    `json:"mode"`
	IncludeContent bool      `json:"include_content"`
	AllowMutation  bool      `json:"allow_mutation"`
	FailMode       string    `json:"fail_mode"`
	Priority       int       `json:"priority"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// HookDeliveryRecord is one logged delivery attempt.
type HookDeliveryRecord struct {
	ID             int64           `json:"id"`
	SubscriptionID string          `json:"subscription_id"`
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	TenantID       string          `json:"tenant_id,omitempty"`
	Status         string          `json:"status"`
	HTTPStatus     int             `json:"http_status,omitempty"`
	LatencyMS      int             `json:"latency_ms,omitempty"`
	Error          string          `json:"error,omitempty"`
	Decision       json.RawMessage `json:"decision,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// secretCipher is the encryption seam, satisfied by *secrets.Encryptor.
type secretCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(blob []byte) ([]byte, error)
}

// HookSubscriptionStore persists hook registrations and their delivery log.
type HookSubscriptionStore struct {
	pool   *Pool
	cipher secretCipher
}

// NewHookSubscriptionStore creates the store. A nil cipher disables secrets:
// subscriptions can still be registered, but unsigned, which is only
// appropriate for endpoints on a trusted network.
func NewHookSubscriptionStore(pool *Pool, cipher secretCipher) *HookSubscriptionStore {
	return &HookSubscriptionStore{pool: pool, cipher: cipher}
}

const hookSubscriptionColumns = `id, name, tenant_id, project_id, event_type, url,
	secret_encrypted, timeout_ms, mode, include_content, allow_mutation,
	fail_mode, priority, enabled, created_at, updated_at`

// List returns every subscription, newest-priority order, for the admin UI.
// Secrets are decrypted only by LoadActive; List reports presence, not value.
func (s *HookSubscriptionStore) List(ctx context.Context) ([]*HookSubscriptionRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+hookSubscriptionColumns+`
		   FROM hook_subscriptions
		  ORDER BY event_type, priority, name`)
	if err != nil {
		return nil, fmt.Errorf("list hook subscriptions: %w", err)
	}
	defer rows.Close()
	return s.scanAll(rows, false)
}

// Get returns one subscription without its secret.
func (s *HookSubscriptionStore) Get(ctx context.Context, subscriptionID string) (*HookSubscriptionRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+hookSubscriptionColumns+` FROM hook_subscriptions WHERE id = $1`, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("get hook subscription: %w", err)
	}
	defer rows.Close()
	records, err := s.scanAll(rows, false)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, ErrHookSubscriptionNotFound
	}
	return records[0], nil
}

// LoadActive returns the enabled subscriptions with secrets decrypted, in the
// form the hook manager consumes. A row whose secret cannot be decrypted is
// skipped rather than delivered unsigned: an endpoint that expects a signature
// would reject it anyway, and sending it unsigned is the worse failure.
func (s *HookSubscriptionStore) LoadActive(ctx context.Context) ([]*hooks.Subscription, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+hookSubscriptionColumns+` FROM hook_subscriptions WHERE enabled ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("load hook subscriptions: %w", err)
	}
	defer rows.Close()
	records, err := s.scanAll(rows, true)
	if err != nil {
		return nil, err
	}

	out := make([]*hooks.Subscription, 0, len(records))
	for _, r := range records {
		if r.HasSecret && r.Secret == "" {
			// Decryption failed; skip rather than sign with nothing.
			continue
		}
		out = append(out, &hooks.Subscription{
			ID:             r.ID,
			Name:           r.Name,
			TenantID:       r.TenantID,
			ProjectID:      r.ProjectID,
			EventType:      hooks.EventType(r.EventType),
			URL:            r.URL,
			Secret:         r.Secret,
			Timeout:        time.Duration(r.TimeoutMS) * time.Millisecond,
			Mode:           r.Mode,
			IncludeContent: r.IncludeContent,
			AllowMutation:  r.AllowMutation,
			FailMode:       r.FailMode,
			Priority:       r.Priority,
			Enabled:        r.Enabled,
		})
	}
	return out, nil
}

// Upsert creates or replaces a subscription. An empty Secret on an update
// leaves the stored secret untouched, so editing a hook's timeout does not
// silently unsign it.
func (s *HookSubscriptionStore) Upsert(ctx context.Context, r *HookSubscriptionRecord) (*HookSubscriptionRecord, error) {
	if r.ID == "" {
		r.ID = id.New("hks")
	}
	var encrypted []byte
	if r.Secret != "" {
		if s.cipher == nil {
			return nil, errors.New("hook secret provided but credential encryption is not configured")
		}
		var err error
		if encrypted, err = s.cipher.Encrypt([]byte(r.Secret)); err != nil {
			return nil, fmt.Errorf("encrypt hook secret: %w", err)
		}
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO hook_subscriptions
		     (id, name, tenant_id, project_id, event_type, url, secret_encrypted,
		      timeout_ms, mode, include_content, allow_mutation, fail_mode,
		      priority, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now(), now())
		 ON CONFLICT (id) DO UPDATE
		    SET name             = EXCLUDED.name,
		        tenant_id        = EXCLUDED.tenant_id,
		        project_id       = EXCLUDED.project_id,
		        event_type       = EXCLUDED.event_type,
		        url              = EXCLUDED.url,
		        secret_encrypted = COALESCE(EXCLUDED.secret_encrypted, hook_subscriptions.secret_encrypted),
		        timeout_ms       = EXCLUDED.timeout_ms,
		        mode             = EXCLUDED.mode,
		        include_content  = EXCLUDED.include_content,
		        allow_mutation   = EXCLUDED.allow_mutation,
		        fail_mode        = EXCLUDED.fail_mode,
		        priority         = EXCLUDED.priority,
		        enabled          = EXCLUDED.enabled,
		        updated_at       = now()`,
		r.ID, r.Name, nullableText(r.TenantID), nullableText(r.ProjectID), r.EventType, r.URL,
		nullableBytes(encrypted), r.TimeoutMS, r.Mode, r.IncludeContent, r.AllowMutation,
		r.FailMode, r.Priority, r.Enabled,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert hook subscription: %w", err)
	}
	return s.Get(ctx, r.ID)
}

// Delete removes a subscription permanently.
func (s *HookSubscriptionStore) Delete(ctx context.Context, subscriptionID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM hook_subscriptions WHERE id = $1`, subscriptionID)
	if err != nil {
		return fmt.Errorf("delete hook subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHookSubscriptionNotFound
	}
	return nil
}

// InsertDelivery appends one delivery attempt to the log.
func (s *HookSubscriptionStore) InsertDelivery(ctx context.Context, rec *HookDeliveryRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO hook_deliveries
		     (subscription_id, event_id, event_type, tenant_id, status, http_status,
		      latency_ms, error, decision, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`,
		rec.SubscriptionID, rec.EventID, rec.EventType, nullableText(rec.TenantID), rec.Status,
		nullableInt(rec.HTTPStatus), nullableInt(rec.LatencyMS), nullableText(rec.Error),
		nullableJSON(rec.Decision),
	)
	if err != nil {
		return fmt.Errorf("insert hook delivery: %w", err)
	}
	return nil
}

// ListDeliveries returns recent attempts, optionally for one subscription.
func (s *HookSubscriptionStore) ListDeliveries(ctx context.Context, subscriptionID string, limit int) ([]*HookDeliveryRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, subscription_id, event_id, event_type, tenant_id, status,
		        http_status, latency_ms, error, decision, created_at
		   FROM hook_deliveries
		  WHERE ($1::text IS NULL OR subscription_id = $1)
		  ORDER BY created_at DESC
		  LIMIT $2`,
		nullableText(subscriptionID), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list hook deliveries: %w", err)
	}
	defer rows.Close()

	var result []*HookDeliveryRecord
	for rows.Next() {
		rec := &HookDeliveryRecord{}
		var tenantID, errText *string
		var httpStatus, latency *int
		var decision []byte
		if err := rows.Scan(&rec.ID, &rec.SubscriptionID, &rec.EventID, &rec.EventType, &tenantID,
			&rec.Status, &httpStatus, &latency, &errText, &decision, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan hook delivery: %w", err)
		}
		if tenantID != nil {
			rec.TenantID = *tenantID
		}
		if errText != nil {
			rec.Error = *errText
		}
		if httpStatus != nil {
			rec.HTTPStatus = *httpStatus
		}
		if latency != nil {
			rec.LatencyMS = *latency
		}
		rec.Decision = decision
		result = append(result, rec)
	}
	return result, rows.Err()
}

// DeleteExpiredDeliveries trims the delivery log. It is an operational trail,
// not an audit record, so a short retention is deliberate.
func (s *HookSubscriptionStore) DeleteExpiredDeliveries(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		retentionDays = 14
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM hook_deliveries WHERE created_at < now() - make_interval(days => $1::int)`,
		retentionDays)
	if err != nil {
		return 0, fmt.Errorf("delete expired hook deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *HookSubscriptionStore) scanAll(rows pgx.Rows, decryptSecrets bool) ([]*HookSubscriptionRecord, error) {
	var result []*HookSubscriptionRecord
	for rows.Next() {
		r := &HookSubscriptionRecord{}
		var tenantID, projectID *string
		var secret []byte
		if err := rows.Scan(&r.ID, &r.Name, &tenantID, &projectID, &r.EventType, &r.URL,
			&secret, &r.TimeoutMS, &r.Mode, &r.IncludeContent, &r.AllowMutation,
			&r.FailMode, &r.Priority, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan hook subscription: %w", err)
		}
		if tenantID != nil {
			r.TenantID = *tenantID
		}
		if projectID != nil {
			r.ProjectID = *projectID
		}
		r.HasSecret = len(secret) > 0
		if decryptSecrets && r.HasSecret && s.cipher != nil {
			if plain, err := s.cipher.Decrypt(secret); err == nil {
				r.Secret = string(plain)
			}
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func nullableBytes(value []byte) interface{} {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableInt(value int) interface{} {
	if value == 0 {
		return nil
	}
	return value
}
