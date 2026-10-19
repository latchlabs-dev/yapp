package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

var scpRemotePattern = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)
var projectSlugInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)

// MemoryProject is the stable tenant-scoped identity for one Git repository.
type MemoryProject struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id"`
	Slug             string `json:"slug"`
	DisplayName      string `json:"display_name"`
	CanonicalRepoKey string `json:"canonical_repo_key,omitempty"`
}

// MemoryProjectMetrics is the project-level rollup shown to administrators.
// Session totals come from client submissions. RequestCount and last activity
// come from authenticated model traffic observed by the proxy itself.
type MemoryProjectMetrics struct {
	SessionCount         int        `json:"session_count"`
	UnattributedSessions int        `json:"unattributed_sessions"`
	DistilledSessions    int        `json:"distilled_sessions"`
	SkippedSessions      int        `json:"skipped_sessions"`
	PendingSessions      int        `json:"pending_sessions"`
	FailedSessions       int        `json:"failed_sessions"`
	MessageCount         int64      `json:"message_count"`
	TotalTokens          int64      `json:"total_tokens"`
	TotalCostCents       float64    `json:"total_cost_cents"`
	RequestCount         int64      `json:"request_count"`
	LastActivityAt       *time.Time `json:"last_activity_at,omitempty"`
	LastActivitySource   string     `json:"last_activity_source,omitempty"`
}

// ProjectAttributionCoverage makes historical blind spots explicit rather
// than presenting the canonical-project list as complete.
type ProjectAttributionCoverage struct {
	TotalSessions             int64 `json:"total_sessions"`
	CanonicalProjectSessions  int64 `json:"canonical_project_sessions"`
	LegacyProjectSessions     int64 `json:"legacy_project_sessions"`
	UnattributedSessions      int64 `json:"unattributed_sessions"`
	TotalModelRequests        int64 `json:"total_model_requests"`
	ProjectAttributedRequests int64 `json:"project_attributed_requests"`
}

// AdminProjectUser is the stable user identity attached to an insight when its
// journal provenance can be matched unambiguously to a submitted session.
type AdminProjectUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// AdminProjectParticipant summarizes one user's submitted work on a project.
type AdminProjectParticipant struct {
	UserID         string     `json:"user_id"`
	DisplayName    string     `json:"display_name"`
	Email          string     `json:"email"`
	SessionCount   int        `json:"session_count"`
	MessageCount   int64      `json:"message_count"`
	TotalCostCents float64    `json:"total_cost_cents"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

// AdminMemoryProject joins the stable repository identity with its tenant and
// project-level session metrics. Memory documents and generated insights live
// in Duffel and are joined by the admin HTTP layer using ID as the storage key.
type AdminMemoryProject struct {
	ID               string                       `json:"id"`
	TenantID         string                       `json:"tenant_id"`
	TenantName       string                       `json:"tenant_name"`
	Slug             string                       `json:"slug"`
	DisplayName      string                       `json:"display_name"`
	CanonicalRepoKey string                       `json:"canonical_repo_key"`
	Remotes          []string                     `json:"remotes"`
	Participants     []AdminProjectParticipant    `json:"participants"`
	Metrics          MemoryProjectMetrics         `json:"metrics"`
	CreatedAt        time.Time                    `json:"created_at"`
	UpdatedAt        time.Time                    `json:"updated_at"`
	InsightUsers     map[string]*AdminProjectUser `json:"-"`
}

// MemoryProjectStore resolves repository evidence to stable project IDs.
type MemoryProjectStore struct {
	pool *Pool
}

func NewMemoryProjectStore(pool *Pool) *MemoryProjectStore {
	return &MemoryProjectStore{pool: pool}
}

// NormalizeRepositoryRemote collapses common SSH/HTTPS Git remote forms to
// host/owner/repository. Credentials, query strings, fragments, and .git are
// deliberately excluded from the stored identity.
func NormalizeRepositoryRemote(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("repository remote is empty")
	}

	var host, path string
	if match := scpRemotePattern.FindStringSubmatch(raw); match != nil && !strings.Contains(raw, "://") {
		host, path = match[1], match[2]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("invalid repository remote")
		}
		host, path = u.Hostname(), u.EscapedPath()
		if decoded, err := url.PathUnescape(path); err == nil {
			path = decoded
		}
	}

	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) < 2 {
		return "", fmt.Errorf("repository remote must include host, owner, and repository")
	}
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
		if parts[i] == "" || parts[i] == "." || parts[i] == ".." {
			return "", fmt.Errorf("invalid repository path")
		}
	}
	if strings.ContainsAny(strings.Join(parts, "/"), " \t\r\n") {
		return "", fmt.Errorf("invalid repository path")
	}
	return host + "/" + strings.Join(parts, "/"), nil
}

// Resolve returns nil when no valid Git remote was supplied. Matching any
// known remote returns the existing project, so origin/upstream aliases from
// different users converge without relying on checkout directory names.
func (s *MemoryProjectStore) Resolve(ctx context.Context, tenantID, slug string, remotes []string) (*MemoryProject, error) {
	if len(remotes) > 16 {
		remotes = remotes[:16]
	}
	normalized := make([]string, 0, len(remotes))
	seen := make(map[string]struct{}, len(remotes))
	for _, remote := range remotes {
		key, err := NormalizeRepositoryRemote(remote)
		if err != nil {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	slug = projectSlugInvalid.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-.")
	if len(slug) > 64 {
		slug = slug[:64]
		slug = strings.TrimRight(slug, "-.")
	}
	if slug == "" {
		parts := strings.Split(normalized[0], "/")
		slug = parts[len(parts)-1]
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin memory project resolution: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	project := &MemoryProject{}
	err = tx.QueryRow(ctx, `
		SELECT mp.id, mp.tenant_id, mp.slug, mp.display_name,
		       COALESCE(mp.canonical_repo_key, '')
		  FROM memory_project_remotes mr
		  JOIN memory_projects mp ON mp.id = mr.project_id
		 WHERE mr.tenant_id = $1 AND mr.normalized_remote = ANY($2)
		 ORDER BY array_position($2::text[], mr.normalized_remote)
		 LIMIT 1
	`, tenantID, normalized).Scan(&project.ID, &project.TenantID, &project.Slug, &project.DisplayName, &project.CanonicalRepoKey)
	if err == nil {
		if err := touchProjectRemotes(ctx, tx, project.ID, tenantID, normalized); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit memory project resolution: %w", err)
		}
		return project, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("find memory project by remote: %w", err)
	}

	// Upgrade exactly one legacy label match in place. If the label is already
	// ambiguous, create a new repository-backed project instead of guessing.
	rows, queryErr := tx.Query(ctx, `
		SELECT id, tenant_id, slug, display_name
		  FROM memory_projects
		 WHERE tenant_id = $1 AND slug = $2 AND canonical_repo_key IS NULL
		 ORDER BY created_at
		 LIMIT 2 FOR UPDATE
	`, tenantID, slug)
	if queryErr != nil {
		return nil, fmt.Errorf("find legacy memory project: %w", queryErr)
	}
	var legacy []*MemoryProject
	for rows.Next() {
		candidate := &MemoryProject{}
		if err := rows.Scan(&candidate.ID, &candidate.TenantID, &candidate.Slug, &candidate.DisplayName); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan legacy memory project: %w", err)
		}
		legacy = append(legacy, candidate)
	}
	rows.Close()
	if len(legacy) == 1 {
		project = legacy[0]
		project.CanonicalRepoKey = normalized[0]
		if _, err := tx.Exec(ctx, `UPDATE memory_projects SET canonical_repo_key=$2, updated_at=now() WHERE id=$1`, project.ID, normalized[0]); err != nil {
			return nil, fmt.Errorf("upgrade legacy memory project: %w", err)
		}
	} else {
		project = &MemoryProject{ID: id.New("mprj"), TenantID: tenantID, Slug: slug, DisplayName: slug, CanonicalRepoKey: normalized[0]}
		err = tx.QueryRow(ctx, `
			INSERT INTO memory_projects (id, tenant_id, slug, display_name, canonical_repo_key)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (tenant_id, canonical_repo_key) WHERE canonical_repo_key IS NOT NULL
			DO UPDATE SET updated_at=now()
			RETURNING id, tenant_id, slug, display_name, canonical_repo_key
		`, project.ID, tenantID, slug, slug, normalized[0]).Scan(
			&project.ID, &project.TenantID, &project.Slug, &project.DisplayName, &project.CanonicalRepoKey,
		)
		if err != nil {
			return nil, fmt.Errorf("create memory project: %w", err)
		}
	}

	if err := touchProjectRemotes(ctx, tx, project.ID, tenantID, normalized); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit memory project resolution: %w", err)
	}
	return project, nil
}

func touchProjectRemotes(ctx context.Context, tx pgx.Tx, projectID, tenantID string, remotes []string) error {
	for i, remote := range remotes {
		kind := "observed"
		if i == 0 {
			kind = "canonical"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_project_remotes (project_id, tenant_id, normalized_remote, remote_kind)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (tenant_id, normalized_remote)
			DO UPDATE SET last_seen_at=now()
		`, projectID, tenantID, remote, kind); err != nil {
			return fmt.Errorf("record memory project remote: %w", err)
		}
	}
	return nil
}

// GetForTenant validates a client-supplied stable ID against its auth tenant.
func (s *MemoryProjectStore) GetForTenant(ctx context.Context, tenantID, projectID string) (*MemoryProject, error) {
	project := &MemoryProject{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, slug, display_name, COALESCE(canonical_repo_key, '')
		  FROM memory_projects WHERE tenant_id=$1 AND id=$2 AND status='active'
	`, tenantID, projectID).Scan(&project.ID, &project.TenantID, &project.Slug, &project.DisplayName, &project.CanonicalRepoKey)
	if err != nil {
		return nil, fmt.Errorf("get tenant memory project: %w", err)
	}
	return project, nil
}

// ListForTenant returns a tenant's active projects, most recently active
// first. This is the web console's project picker: unlike ListAdmin it is
// tenant-scoped and carries no cross-tenant rollups.
func (s *MemoryProjectStore) ListForTenant(ctx context.Context, tenantID string) ([]*MemoryProject, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mp.id, mp.tenant_id, mp.slug, mp.display_name, COALESCE(mp.canonical_repo_key, '')
		  FROM memory_projects mp
		 WHERE mp.tenant_id = $1 AND mp.status = 'active'
		 ORDER BY mp.updated_at DESC, mp.display_name
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list tenant memory projects: %w", err)
	}
	defer rows.Close()

	var result []*MemoryProject
	for rows.Next() {
		p := &MemoryProject{}
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Slug, &p.DisplayName, &p.CanonicalRepoKey); err != nil {
			return nil, fmt.Errorf("scan tenant memory project: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

const adminMemoryProjectSelect = `
	SELECT mp.id, mp.tenant_id, t.name, mp.slug, mp.display_name,
	       mp.canonical_repo_key,
	       COALESCE(repos.remotes, ARRAY[]::text[]),
	       COALESCE(people.items, '[]'::jsonb),
	       COALESCE(stats.session_count, 0),
	       COALESCE(stats.unattributed_sessions, 0),
	       COALESCE(stats.distilled_sessions, 0),
	       COALESCE(stats.skipped_sessions, 0),
	       COALESCE(stats.pending_sessions, 0),
	       COALESCE(stats.failed_sessions, 0),
	       COALESCE(stats.message_count, 0),
	       COALESCE(stats.total_tokens, 0),
	       COALESCE(stats.total_cost_cents, 0),
	       COALESCE(request_stats.request_count, 0),
	       COALESCE(request_stats.last_activity_at, stats.last_activity_at),
	       CASE WHEN request_stats.last_activity_at IS NOT NULL THEN 'proxy_request'
	            WHEN stats.last_activity_at IS NOT NULL THEN 'session_submission'
	            ELSE '' END,
	       mp.created_at, mp.updated_at
	  FROM memory_projects mp
	  JOIN tenants t ON t.id = mp.tenant_id
	  LEFT JOIN LATERAL (
		SELECT array_agg(mr.normalized_remote ORDER BY
		       CASE WHEN mr.remote_kind = 'canonical' THEN 0 ELSE 1 END,
		       mr.first_seen_at, mr.normalized_remote) AS remotes
		  FROM memory_project_remotes mr
		 WHERE mr.tenant_id = mp.tenant_id AND mr.project_id = mp.id
	  ) repos ON true
	  LEFT JOIN LATERAL (
		SELECT jsonb_agg(
		         jsonb_build_object(
		           'user_id', ps.user_id,
		           'display_name', COALESCE(u.display_name, ''),
		           'email', u.email,
		           'session_count', ps.session_count,
		           'message_count', ps.message_count,
		           'total_cost_cents', ps.total_cost_cents,
		           'last_activity_at', ps.last_activity_at,
		           'conversation_ids', ps.conversation_ids
		         )
		         ORDER BY ps.last_activity_at DESC NULLS LAST, u.email
		       ) AS items
		  FROM (
			SELECT ss.user_id,
			       COUNT(*)::int AS session_count,
			       COALESCE(SUM(ss.message_count), 0)::bigint AS message_count,
			       COALESCE(SUM(ss.total_cost_cents), 0)::double precision AS total_cost_cents,
			       MAX(GREATEST(COALESCE(ss.ended_at, ss.created_at), ss.created_at)) AS last_activity_at,
			       array_agg(DISTINCT ss.conversation_id) AS conversation_ids
			  FROM session_submissions ss
			 WHERE ss.tenant_id = mp.tenant_id
			   AND ss.project_id = mp.id
			   AND ss.user_id IS NOT NULL
			 GROUP BY ss.user_id
		  ) ps
		  JOIN users u ON u.tenant_id = mp.tenant_id AND u.id = ps.user_id
	  ) people ON true
	  LEFT JOIN LATERAL (
		SELECT COUNT(*)::int AS session_count,
		       COUNT(*) FILTER (WHERE ss.user_id IS NULL)::int AS unattributed_sessions,
		       COUNT(*) FILTER (WHERE ss.distilled_at IS NOT NULL
		                              AND COALESCE(ss.distill_error, '') = '')::int AS distilled_sessions,
		       COUNT(*) FILTER (WHERE ss.distilled_at IS NOT NULL
		                              AND COALESCE(ss.distill_error, '') <> '')::int AS skipped_sessions,
		       COUNT(*) FILTER (WHERE ss.distilled_at IS NULL
		                              AND COALESCE(ss.distill_error, '') = '')::int AS pending_sessions,
		       COUNT(*) FILTER (WHERE ss.distilled_at IS NULL
		                              AND COALESCE(ss.distill_error, '') <> '')::int AS failed_sessions,
		       COALESCE(SUM(ss.message_count), 0)::bigint AS message_count,
		       COALESCE(SUM(ss.total_tokens), 0)::bigint AS total_tokens,
		       COALESCE(SUM(ss.total_cost_cents), 0)::double precision AS total_cost_cents,
		       MAX(GREATEST(COALESCE(ss.ended_at, ss.created_at), ss.created_at)) AS last_activity_at
		  FROM session_submissions ss
		 WHERE ss.tenant_id = mp.tenant_id AND ss.project_id = mp.id
	  ) stats ON true
	  LEFT JOIN LATERAL (
		SELECT COUNT(*)::bigint AS request_count,
		       MAX(ue.started_at) AS last_activity_at
		  FROM usage_events ue
		 WHERE ue.tenant_id = mp.tenant_id AND ue.project_id = mp.id
	  ) request_stats ON true
`

type projectRowScanner interface {
	Scan(dest ...any) error
}

func scanAdminMemoryProject(row projectRowScanner) (*AdminMemoryProject, error) {
	project := &AdminMemoryProject{}
	var participantsJSON []byte
	err := row.Scan(
		&project.ID, &project.TenantID, &project.TenantName, &project.Slug,
		&project.DisplayName, &project.CanonicalRepoKey, &project.Remotes, &participantsJSON,
		&project.Metrics.SessionCount, &project.Metrics.UnattributedSessions, &project.Metrics.DistilledSessions,
		&project.Metrics.SkippedSessions, &project.Metrics.PendingSessions,
		&project.Metrics.FailedSessions, &project.Metrics.MessageCount,
		&project.Metrics.TotalTokens, &project.Metrics.TotalCostCents,
		&project.Metrics.RequestCount, &project.Metrics.LastActivityAt,
		&project.Metrics.LastActivitySource, &project.CreatedAt, &project.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if project.Remotes == nil {
		project.Remotes = []string{}
	}
	if err := decodeProjectParticipants(project, participantsJSON); err != nil {
		return nil, err
	}
	return project, nil
}

func decodeProjectParticipants(project *AdminMemoryProject, participantsJSON []byte) error {
	var participantRows []struct {
		AdminProjectParticipant
		ConversationIDs []string `json:"conversation_ids"`
	}
	if err := json.Unmarshal(participantsJSON, &participantRows); err != nil {
		return fmt.Errorf("decode project participants: %w", err)
	}
	project.Participants = make([]AdminProjectParticipant, 0, len(participantRows))
	project.InsightUsers = make(map[string]*AdminProjectUser)
	for _, row := range participantRows {
		project.Participants = append(project.Participants, row.AdminProjectParticipant)
		user := &AdminProjectUser{ID: row.UserID, DisplayName: row.DisplayName, Email: row.Email}
		project.InsightUsers["user:"+row.UserID] = user
		for _, conversationID := range row.ConversationIDs {
			key := "conversation:" + conversationID
			existing, exists := project.InsightUsers[key]
			if exists && (existing == nil || existing.ID != user.ID) {
				project.InsightUsers[key] = nil
				continue
			}
			project.InsightUsers[key] = user
		}
	}
	return nil
}

// InsightUser resolves journal provenance only when it maps to exactly one
// participant. Caller-controlled conversation IDs are never globally trusted.
func (p *AdminMemoryProject) InsightUser(kind, value string) *AdminProjectUser {
	if p == nil || value == "" {
		return nil
	}
	return p.InsightUsers[kind+":"+value]
}

// MatchDistillerInsight identifies the exact caller-supplied conversation ID
// that produced a journal entry. It compares complete markers from known
// project sessions and chooses the longest match so delimiter-containing IDs
// cannot be truncated into another participant's conversation.
func (p *AdminMemoryProject) MatchDistillerInsight(entry string) (string, *AdminProjectUser) {
	if p == nil || entry == "" {
		return "", nil
	}
	const prefix = "conversation:"
	bestID := ""
	var bestUser *AdminProjectUser
	for key, user := range p.InsightUsers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		conversationID := strings.TrimPrefix(key, prefix)
		marker := "(source: distiller, conversation: " + conversationID + ")"
		if !strings.HasPrefix(entry, marker) {
			continue
		}
		if len(entry) > len(marker) && entry[len(marker)] != '\n' && entry[len(marker)] != '\r' {
			continue
		}
		if len(conversationID) > len(bestID) {
			bestID = conversationID
			bestUser = user
		}
	}
	return bestID, bestUser
}

// ListAdmin returns active projects that have a validated Git repository
// identity. Directory labels never participate in deduplication; the unique
// tenant/repository key populated by Resolve is authoritative.
func (s *MemoryProjectStore) ListAdmin(ctx context.Context) ([]*AdminMemoryProject, error) {
	rows, err := s.pool.Query(ctx, adminMemoryProjectSelect+`
		 WHERE mp.status = 'active'
		   AND mp.canonical_repo_key IS NOT NULL
		   AND t.status <> 'deleted'
		 ORDER BY COALESCE(request_stats.last_activity_at, stats.last_activity_at) DESC NULLS LAST,
		          mp.updated_at DESC, mp.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list repository projects: %w", err)
	}
	defer rows.Close()

	projects := make([]*AdminMemoryProject, 0)
	for rows.Next() {
		project, scanErr := scanAdminMemoryProject(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan repository project: %w", scanErr)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list repository projects: %w", err)
	}
	return projects, nil
}

// AttributionCoverage reports both historical session mapping and the newer
// server-observed model-request mapping across active tenants.
func (s *MemoryProjectStore) AttributionCoverage(ctx context.Context) (*ProjectAttributionCoverage, error) {
	coverage := &ProjectAttributionCoverage{}
	err := s.pool.QueryRow(ctx, `
		WITH session_coverage AS (
			SELECT COUNT(*)::bigint AS total_sessions,
			       COUNT(*) FILTER (WHERE mp.canonical_repo_key IS NOT NULL)::bigint AS canonical_sessions,
			       COUNT(*) FILTER (WHERE mp.id IS NOT NULL AND mp.canonical_repo_key IS NULL)::bigint AS legacy_sessions,
			       COUNT(*) FILTER (WHERE mp.id IS NULL)::bigint AS unattributed_sessions
			  FROM session_submissions ss
			  JOIN tenants t ON t.id = ss.tenant_id AND t.status <> 'deleted'
			  LEFT JOIN memory_projects mp ON mp.tenant_id = ss.tenant_id AND mp.id = ss.project_id
		), request_coverage AS (
			SELECT COUNT(*)::bigint AS total_requests,
			       COUNT(*) FILTER (WHERE ue.project_id IS NOT NULL)::bigint AS attributed_requests
			  FROM usage_events ue
			  JOIN tenants t ON t.id = ue.tenant_id AND t.status <> 'deleted'
		)
		SELECT sc.total_sessions, sc.canonical_sessions, sc.legacy_sessions, sc.unattributed_sessions,
		       rc.total_requests, rc.attributed_requests
		  FROM session_coverage sc CROSS JOIN request_coverage rc
	`).Scan(
		&coverage.TotalSessions, &coverage.CanonicalProjectSessions,
		&coverage.LegacyProjectSessions, &coverage.UnattributedSessions,
		&coverage.TotalModelRequests, &coverage.ProjectAttributedRequests,
	)
	if err != nil {
		return nil, fmt.Errorf("project attribution coverage: %w", err)
	}
	return coverage, nil
}

// GetAdmin returns one active repository-backed project without requiring a
// tenant parameter. This is only exposed behind the authenticated admin API.
func (s *MemoryProjectStore) GetAdmin(ctx context.Context, projectID string) (*AdminMemoryProject, error) {
	project, err := scanAdminMemoryProject(s.pool.QueryRow(ctx, adminMemoryProjectSelect+`
		 WHERE mp.id = $1
		   AND mp.status = 'active'
		   AND mp.canonical_repo_key IS NOT NULL
		   AND t.status <> 'deleted'
	`, projectID))
	if err != nil {
		return nil, fmt.Errorf("get repository project: %w", err)
	}
	return project, nil
}
