package webui

import (
	"context"
	"errors"
	"net/http"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// errProjectNotInTenant is the single answer for a project ID that is absent,
// archived, or owned by another tenant, so console users cannot probe the
// project ID space of other organisations.
var errProjectNotInTenant = errors.New("project not found")

// listProjectsHandler returns the repository projects the caller's tenant owns.
// These are the same canonical identities the Pi CLI resolves against, so a
// conversation scoped to one shares memory with that repository's CLI sessions.
func listProjectsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign-in required")
			return
		}
		if deps.Projects == nil {
			writeJSON(w, http.StatusOK, map[string]any{"projects": []any{}})
			return
		}

		projects, err := deps.Projects.ListForTenant(r.Context(), authCtx.TenantID)
		if err != nil {
			deps.Logger.Error().Err(err).Str("tenant_id", authCtx.TenantID).Msg("web projects list failed")
			writeError(w, http.StatusInternalServerError, "list_projects", "could not load projects")
			return
		}

		items := make([]map[string]string, 0, len(projects))
		for _, p := range projects {
			items = append(items, map[string]string{
				"id":           p.ID,
				"slug":         p.Slug,
				"display_name": p.DisplayName,
				"repo":         p.CanonicalRepoKey,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": items})
	}
}

// resolveTenantProject verifies that a project belongs to the caller's tenant.
// Every route that touches project memory funnels through here, so tenant
// scoping is enforced in exactly one place.
func (deps *Dependencies) resolveTenantProject(ctx context.Context, tenantID, projectID string) (*pgstore.MemoryProject, error) {
	if projectID == "" {
		return nil, errProjectNotInTenant
	}
	if deps.Projects == nil {
		return nil, errProjectNotInTenant
	}
	project, err := deps.Projects.GetForTenant(ctx, tenantID, projectID)
	if err != nil || project == nil {
		return nil, errProjectNotInTenant
	}
	return project, nil
}
