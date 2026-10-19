package postgres

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/usage"
)

// UsageStore persists usage events to Postgres.
type UsageStore struct {
	pool *Pool
}

// NewUsageStore creates a new UsageStore.
func NewUsageStore(pool *Pool) *UsageStore {
	return &UsageStore{pool: pool}
}

// InsertUsageEvent writes a single usage event to the database.
// Empty tenant/user/key references are stored as NULL so usage is still
// persisted when auth is disabled or unavailable.
func (s *UsageStore) InsertUsageEvent(ctx context.Context, event *usage.Event) error {
	if event.ID == "" {
		event.ID = id.New("uev_")
	}

	now := time.Now()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO usage_events
			(id, request_id, tenant_id, user_id, key_id, project_id, extension_version,
			 provider, model, upstream_provider, upstream_model, provider_account_id,
			 status, error_class,
			 input_tokens, output_tokens, reasoning_tokens, cache_read_tokens, cache_write_tokens, total_tokens,
			 estimated_usage, cost_total_cents, subscription_rate_card_id, cost_basis,
			 started_at, completed_at, created_at, client_ip)
		VALUES
			($1, $2, $3, $4, $5,
			 CASE WHEN $6::text IS NULL THEN NULL ELSE (
				 SELECT mp.id FROM memory_projects mp
				  WHERE mp.tenant_id = $3 AND mp.id = $6 AND mp.status = 'active'
			 ) END,
			 $7,
			 $8, $9, $10, $11, $12,
			 $13, $14,
			 $15, $16, $17, $18, $19, $20,
			 $21, $22, $23, $24,
			 $25, $26, $27, $28)
		ON CONFLICT (request_id) DO UPDATE SET
				status        = EXCLUDED.status,
				error_class   = EXCLUDED.error_class,
				tenant_id     = COALESCE(EXCLUDED.tenant_id, usage_events.tenant_id),
				user_id       = COALESCE(EXCLUDED.user_id, usage_events.user_id),
				key_id        = COALESCE(EXCLUDED.key_id, usage_events.key_id),
				project_id    = COALESCE(EXCLUDED.project_id, usage_events.project_id),
				extension_version = COALESCE(EXCLUDED.extension_version, usage_events.extension_version),
				client_ip     = COALESCE(EXCLUDED.client_ip, usage_events.client_ip),
				provider      = EXCLUDED.provider,
				model         = EXCLUDED.model,
				upstream_provider = EXCLUDED.upstream_provider,
				upstream_model = EXCLUDED.upstream_model,
				provider_account_id = COALESCE(EXCLUDED.provider_account_id, usage_events.provider_account_id),
				input_tokens  = EXCLUDED.input_tokens,
				output_tokens = EXCLUDED.output_tokens,
				reasoning_tokens = EXCLUDED.reasoning_tokens,
				cache_read_tokens = EXCLUDED.cache_read_tokens,
				cache_write_tokens = EXCLUDED.cache_write_tokens,
				total_tokens  = EXCLUDED.total_tokens,
				estimated_usage = EXCLUDED.estimated_usage,
				completed_at  = EXCLUDED.completed_at,
				cost_total_cents = EXCLUDED.cost_total_cents,
				subscription_rate_card_id = COALESCE(EXCLUDED.subscription_rate_card_id, usage_events.subscription_rate_card_id),
				cost_basis = EXCLUDED.cost_basis
		`,
		usageEventInsertArgs(event, now)...,
	)
	if err != nil {
		return fmt.Errorf("insert usage event: %w", err)
	}
	return nil
}

func usageEventInsertArgs(event *usage.Event, createdAt time.Time) []interface{} {
	return []interface{}{
		event.ID, event.RequestID, nullableText(event.TenantID), nullableText(event.UserID), nullableText(event.KeyID), nullableText(event.ProjectID),
		nullableText(event.ExtensionVersion),
		event.Provider, event.Model, event.UpstreamProvider, event.UpstreamModel, nullableText(event.ProviderAccountID),
		event.Status, event.ErrorClass,
		event.InputTokens, event.OutputTokens, event.ReasoningTokens, event.CacheReadTokens, event.CacheWriteTokens, event.TotalTokens,
		event.EstimatedUsage, event.CostTotalCents, nullableInt64(event.SubscriptionRateCardID), costBasis(event),
		event.StartedAt, event.CompletedAt, createdAt, nullableInet(event.ClientIP),
	}
}

func nullableInt64(value int64) interface{} {
	if value <= 0 {
		return nil
	}
	return value
}

func costBasis(event *usage.Event) string {
	if event.CostBasis != "" {
		return event.CostBasis
	}
	if event.CostCalculated {
		return "metered"
	}
	return "default_model_rate"
}

func nullableText(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

// nullableInet converts a string IP into a netip.Addr that pgx encodes natively
// to the inet column. Unparseable or empty values become NULL rather than
// failing the whole insert.
func nullableInet(value string) interface{} {
	if value == "" {
		return nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return nil
	}
	return addr
}

// QueryCacheTrend returns daily cache usage for the authenticated person's
// user scope (preferred) or current API key when no user is attached. Tenant
// filtering is always applied so caller-controlled identifiers can never cross
// tenant boundaries.
func (s *UsageStore) QueryCacheTrend(
	ctx context.Context,
	tenantID, userID, keyID string,
	from, to time.Time,
) ([]usage.CacheTrendPoint, error) {
	where := `ue.started_at >= $1 AND ue.started_at < $2 AND ue.tenant_id = $3`
	args := []interface{}{from, to, tenantID}
	if userID != "" {
		where += ` AND ue.user_id = $4`
		args = append(args, userID)
	} else {
		where += ` AND ue.key_id = $4`
		args = append(args, keyID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc('day', ue.started_at) AS day,
		       COUNT(*) AS request_count,
		       COUNT(*) FILTER (WHERE ue.cache_read_tokens > 0) AS cached_requests,
		       COALESCE(SUM(ue.input_tokens), 0) AS input_tokens,
		       COALESCE(SUM(ue.cache_read_tokens), 0) AS cache_read_tokens,
		       COALESCE(SUM(ue.cache_write_tokens), 0) AS cache_write_tokens
		FROM usage_events ue
		WHERE `+where+`
		GROUP BY day
		ORDER BY day
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query cache trend: %w", err)
	}
	defer rows.Close()

	points := make([]usage.CacheTrendPoint, 0, 7)
	for rows.Next() {
		var point usage.CacheTrendPoint
		if err := rows.Scan(
			&point.Day,
			&point.RequestCount,
			&point.CachedRequests,
			&point.InputTokens,
			&point.CacheReadTokens,
			&point.CacheWriteTokens,
		); err != nil {
			return nil, fmt.Errorf("scan cache trend: %w", err)
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache trend: %w", err)
	}
	return points, nil
}

// Bounds on the number of aggregated rows QueryUsageReports returns. Without a
// cap an unbounded time range (or a high-cardinality tenant/user/model mix) can
// produce an arbitrarily large result set and exhaust memory. defaultUsageReportLimit
// applies when the caller supplies no positive limit; maxUsageReportLimit is the
// hard ceiling we clamp any caller-supplied value to.
const (
	defaultUsageReportLimit = 5000
	maxUsageReportLimit     = 10000
)

// clampUsageReportLimit normalizes a caller-supplied row cap: non-positive
// values fall back to the default and anything above the hard max is clamped
// down to it.
func clampUsageReportLimit(limit int) int {
	if limit <= 0 {
		return defaultUsageReportLimit
	}
	if limit > maxUsageReportLimit {
		return maxUsageReportLimit
	}
	return limit
}

// UsageReport holds aggregated usage data for a time period.
type UsageReport struct {
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	Granularity     string    `json:"granularity"` // hourly, daily, monthly
	TenantID        string    `json:"tenant_id,omitempty"`
	UserID          string    `json:"user_id,omitempty"`
	UserDisplayName string    `json:"user_display_name,omitempty"`
	UserEmail       string    `json:"user_email,omitempty"`
	ProjectID       string    `json:"project_id,omitempty"`
	ProjectName     string    `json:"project_name,omitempty"`
	Model           string    `json:"model,omitempty"`
	RequestCount    int64     `json:"request_count"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	CostCents       float64   `json:"cost_total_cents"`
}

// QueryUsageReports returns aggregated usage data.
// Granularity options: "hourly", "daily", "monthly".
// The from/to parameters define the time range; defaults to last 7 days.
// limit caps the number of aggregated rows returned; non-positive values fall
// back to defaultUsageReportLimit and anything above maxUsageReportLimit is
// clamped down to it so a large range can never exhaust memory.
func (s *UsageStore) QueryUsageReports(ctx context.Context, tenantID string, from, to time.Time, granularity string, userID, model string, limit int) ([]*UsageReport, error) {
	var dateTrunc string
	switch granularity {
	case "hourly":
		dateTrunc = "date_trunc('hour', ue.started_at)"
	case "daily", "":
		dateTrunc = "date_trunc('day', ue.started_at)"
		granularity = "daily"
	case "monthly":
		dateTrunc = "date_trunc('month', ue.started_at)"
	default:
		return nil, fmt.Errorf("invalid granularity: %s (expected hourly, daily, monthly)", granularity)
	}

	limit = clampUsageReportLimit(limit)

	// Build dynamic WHERE clause. dateTrunc is a fixed, switch-controlled SQL
	// expression inlined via %s below (never user input), so it is NOT a bind
	// parameter — only from/to and the optional filters are.
	where := `WHERE ue.started_at >= $1 AND ue.started_at < $2`
	args := []interface{}{from, to}
	argIdx := 3

	if tenantID != "" {
		where += fmt.Sprintf(" AND ue.tenant_id = $%d", argIdx)
		args = append(args, tenantID)
		argIdx++
	}
	if userID != "" {
		where += fmt.Sprintf(" AND ue.user_id = $%d", argIdx)
		args = append(args, userID)
		argIdx++
	}
	if model != "" {
		where += fmt.Sprintf(" AND ue.model = $%d", argIdx)
		args = append(args, model)
		argIdx++
	}

	// Bind the clamped row cap as the next positional parameter so it stays
	// parameterized rather than inlined into the query text.
	args = append(args, limit)
	limitIdx := argIdx

	query := fmt.Sprintf(`
		SELECT %s AS period_start,
		       %s AS period_end,
		       COALESCE(ue.tenant_id, '') AS tenant_id,
		       COALESCE(ue.user_id, '') AS user_id,
		       COALESCE(MAX(u.display_name), '') AS user_display_name,
		       COALESCE(MAX(u.email), '') AS user_email,
		       COALESCE(ue.project_id, '') AS project_id,
		       COALESCE(MAX(mp.display_name), '') AS project_name,
		       COALESCE(ue.model, '') AS model,
		       COUNT(*) AS request_count,
		       COALESCE(SUM(ue.input_tokens), 0) AS input_tokens,
		       COALESCE(SUM(ue.output_tokens), 0) AS output_tokens,
		       COALESCE(SUM(ue.reasoning_tokens), 0) AS reasoning_tokens,
		       COALESCE(SUM(ue.total_tokens), 0) AS total_tokens,
		       COALESCE(SUM(ue.cost_total_cents), 0) AS cost_total_cents
		FROM usage_events ue
		LEFT JOIN users u ON u.id = ue.user_id AND u.tenant_id = ue.tenant_id
		LEFT JOIN memory_projects mp ON mp.id = ue.project_id AND mp.tenant_id = ue.tenant_id
		%s
		GROUP BY period_start, ue.tenant_id, ue.user_id, ue.project_id, ue.model
		ORDER BY period_start DESC, ue.model
		LIMIT $%d
	`, dateTrunc, dateTrunc, where, limitIdx)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage reports: %w", err)
	}
	defer rows.Close()

	var results []*UsageReport
	for rows.Next() {
		r := &UsageReport{Granularity: granularity}
		if err := rows.Scan(&r.PeriodStart, &r.PeriodEnd, &r.TenantID, &r.UserID, &r.UserDisplayName, &r.UserEmail, &r.ProjectID, &r.ProjectName, &r.Model,
			&r.RequestCount, &r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.TotalTokens, &r.CostCents); err != nil {
			return nil, fmt.Errorf("scan usage report: %w", err)
		}
		results = append(results, r)
	}
	if results == nil {
		results = []*UsageReport{}
	}
	return results, rows.Err()
}
