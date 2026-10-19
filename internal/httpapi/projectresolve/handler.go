// Package projectresolve maps caller-observed Git remotes to a canonical,
// tenant-scoped project ID without granting access to project memory.
package projectresolve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type Store interface {
	Resolve(context.Context, string, string, []string) (*pgstore.MemoryProject, error)
}

type Request struct {
	Project string   `json:"project"`
	Repo    string   `json:"repo,omitempty"`
	Remotes []string `json:"remotes,omitempty"`
}

func NewHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request Request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid project identity")
			return
		}
		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil || authCtx.TenantID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated tenant required")
			return
		}
		remotes := append([]string(nil), request.Remotes...)
		if strings.TrimSpace(request.Repo) != "" {
			remotes = append(remotes, request.Repo)
		}
		project, err := store.Resolve(r.Context(), authCtx.TenantID, request.Project, remotes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_project", "project identity could not be resolved")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if project == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{"project_id": ""})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"project_id": project.ID,
			"project":    project.Slug,
		})
	}
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {"type": errorType, "message": message},
	})
}
