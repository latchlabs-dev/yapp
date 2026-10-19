package admin

import (
	"net/http"
	"strconv"
	"strings"
)

func listExtensionErrorsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ExtensionErrorStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "extension error store not configured")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		total, records, err := deps.ExtensionErrorStore.List(
			r.Context(), strings.TrimSpace(r.URL.Query().Get("tenant_id")),
			strings.TrimSpace(r.URL.Query().Get("operation")), limit, offset,
		)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("extension errors list failed")
			writeError(w, http.StatusInternalServerError, "list_extension_errors", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"extension_errors": records,
			"total_count":      total,
		})
	}
}
