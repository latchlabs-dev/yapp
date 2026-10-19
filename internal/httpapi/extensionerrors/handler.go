// Package extensionerrors accepts best-effort extension health reports.
package extensionerrors

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/logging"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

var codePattern = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
var secretTokenPattern = regexp.MustCompile(`(?i)(?:bearer\s+|cpk_live_|sk-ant-|sk-)[A-Za-z0-9._-]+`)

type Store interface {
	Upsert(context.Context, *pgstore.ExtensionErrorRecord) error
}

type Report struct {
	Operation        string `json:"operation"`
	ErrorCode        string `json:"error_code"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	Message          string `json:"message,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	ConversationID   string `json:"conversation_id,omitempty"`
	ExtensionVersion string `json:"extension_version,omitempty"`
}

func NewHandler(store Store, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var report Report
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid extension error report")
			return
		}
		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil || authCtx.TenantID == "" || authCtx.KeyID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated key required")
			return
		}

		report.Operation = strings.ToLower(strings.TrimSpace(report.Operation))
		report.ErrorCode = strings.ToLower(strings.TrimSpace(report.ErrorCode))
		if !codePattern.MatchString(report.Operation) || !codePattern.MatchString(report.ErrorCode) {
			writeError(w, http.StatusBadRequest, "validation_error", "invalid operation or error_code")
			return
		}
		if report.HTTPStatus < 0 || report.HTTPStatus > 599 {
			writeError(w, http.StatusBadRequest, "validation_error", "invalid http_status")
			return
		}
		if len(report.ProjectID) > 200 || len(report.ConversationID) > 200 || len(report.ExtensionVersion) > 64 {
			writeError(w, http.StatusBadRequest, "validation_error", "report field exceeds maximum length")
			return
		}
		message := secretTokenPattern.ReplaceAllString(strings.TrimSpace(report.Message), "[REDACTED]")
		message = logging.Redact(message)
		if len(message) > 512 {
			message = message[:512]
		}
		record := &pgstore.ExtensionErrorRecord{
			TenantID: authCtx.TenantID, UserID: authCtx.UserID, KeyID: authCtx.KeyID,
			Operation: report.Operation, ErrorCode: report.ErrorCode,
			HTTPStatus: report.HTTPStatus, Message: message,
			ProjectID:        strings.TrimSpace(report.ProjectID),
			ConversationID:   strings.TrimSpace(report.ConversationID),
			ExtensionVersion: strings.TrimSpace(report.ExtensionVersion),
		}
		if err := store.Upsert(r.Context(), record); err != nil {
			logger.Error().Err(err).Str("operation", report.Operation).Msg("failed to persist extension error")
			writeError(w, http.StatusServiceUnavailable, "unavailable", "extension error monitoring unavailable")
			return
		}
		logger.Warn().Str("operation", report.Operation).Str("error_code", report.ErrorCode).
			Int("http_status", report.HTTPStatus).Msg("extension error reported")
		w.WriteHeader(http.StatusAccepted)
	}
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {"type": errorType, "message": message},
	})
}
