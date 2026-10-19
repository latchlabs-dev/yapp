// Package sessionlogs accepts canonical Pi session JSONL uploads from the
// corp-proxy extension.
package sessionlogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

const (
	maxStatusSessions    = 500
	maxCompressedBytes   = 20 << 20
	maxUncompressedBytes = 64 << 20
)

var (
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
	sha256Pattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store interface {
	ExistingHashes(ctx context.Context, tenantID, ownerID string, sessionIDs []string) (map[string]string, error)
	Put(ctx context.Context, record *pgstore.PiSessionLogRecord) error
}

type Handler struct {
	store    Store
	projects projectResolver
	logger   zerolog.Logger
}

type projectResolver interface {
	Resolve(ctx context.Context, tenantID, slug string, remotes []string) (*pgstore.MemoryProject, error)
	GetForTenant(ctx context.Context, tenantID, projectID string) (*pgstore.MemoryProject, error)
}

func NewHandler(store Store, projects projectResolver, logger zerolog.Logger) *Handler {
	return &Handler{store: store, projects: projects, logger: logger}
}

type StatusItem struct {
	SessionID string `json:"session_id"`
	SHA256    string `json:"sha256"`
}

type StatusRequest struct {
	Sessions []StatusItem `json:"sessions"`
}

type StatusResponse struct {
	RequiredSessionIDs []string `json:"required_session_ids"`
}

// Status returns the session ids whose supplied hashes are not already stored.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	authCtx, ownerID, ok := requestOwner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated session owner required")
		return
	}

	var req StatusRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid status request: "+err.Error())
		return
	}
	if len(req.Sessions) == 0 || len(req.Sessions) > maxStatusSessions {
		writeError(w, http.StatusBadRequest, "validation_error", fmt.Sprintf("sessions must contain 1 to %d items", maxStatusSessions))
		return
	}

	ids := make([]string, 0, len(req.Sessions))
	seen := make(map[string]struct{}, len(req.Sessions))
	for i := range req.Sessions {
		item := &req.Sessions[i]
		item.SHA256 = strings.ToLower(strings.TrimSpace(item.SHA256))
		if !validSessionID(item.SessionID) || !sha256Pattern.MatchString(item.SHA256) {
			writeError(w, http.StatusBadRequest, "validation_error", "invalid session_id or sha256")
			return
		}
		if _, exists := seen[item.SessionID]; exists {
			writeError(w, http.StatusBadRequest, "validation_error", "duplicate session_id")
			return
		}
		seen[item.SessionID] = struct{}{}
		ids = append(ids, item.SessionID)
	}

	existing, err := h.store.ExistingHashes(r.Context(), authCtx.TenantID, ownerID, ids)
	if err != nil {
		h.logger.Error().Err(err).Msg("failed to compare Pi session hashes")
		writeError(w, http.StatusInternalServerError, "storage_error", "failed to compare session hashes")
		return
	}

	required := make([]string, 0, len(req.Sessions))
	for _, item := range req.Sessions {
		if existing[item.SessionID] != item.SHA256 {
			required = append(required, item.SessionID)
		}
	}
	writeJSON(w, http.StatusOK, StatusResponse{RequiredSessionIDs: required})
}

type sessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
}

// Upload verifies and stores one gzip-compressed canonical Pi JSONL file.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	authCtx, ownerID, ok := requestOwner(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated session owner required")
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "sessionID"))
	expectedHash := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Session-SHA256")))
	if !validSessionID(sessionID) || !sha256Pattern.MatchString(expectedHash) {
		writeError(w, http.StatusBadRequest, "validation_error", "invalid session id or X-Session-SHA256")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_encoding", "Content-Encoding: gzip is required")
		return
	}

	compressed, err := io.ReadAll(io.LimitReader(r.Body, maxCompressedBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "failed to read compressed session")
		return
	}
	if len(compressed) == 0 || len(compressed) > maxCompressedBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "compressed session exceeds 20 MiB")
		return
	}

	raw, err := decompressLimited(compressed, maxUncompressedBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_gzip", err.Error())
		return
	}
	sum := sha256.Sum256(raw)
	actualHash := hex.EncodeToString(sum[:])
	if actualHash != expectedHash {
		writeError(w, http.StatusUnprocessableEntity, "hash_mismatch", "session SHA-256 does not match X-Session-SHA256")
		return
	}

	header, entryCount, err := parseSessionHeader(raw)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_session", err.Error())
		return
	}
	if header.ID != sessionID {
		writeError(w, http.StatusUnprocessableEntity, "session_id_mismatch", "URL session id does not match JSONL header")
		return
	}

	createdAt := parseTime(header.Timestamp)
	modifiedAt := parseUnixMillis(r.Header.Get("X-Session-Modified-Ms"))
	fileName := sanitizeFileName(r.Header.Get("X-Session-File-Name"))
	record := &pgstore.PiSessionLogRecord{
		TenantID:          authCtx.TenantID,
		UserID:            authCtx.UserID,
		KeyID:             authCtx.KeyID,
		OwnerID:           ownerID,
		SessionID:         sessionID,
		SessionFileName:   fileName,
		Project:           projectFromCWD(header.CWD),
		CWD:               header.CWD,
		FormatVersion:     header.Version,
		ParentSession:     header.ParentSession,
		ContentSHA256:     actualHash,
		UncompressedBytes: int64(len(raw)),
		EntryCount:        entryCount,
		ContentGzip:       compressed,
		SessionCreatedAt:  createdAt,
		SessionModifiedAt: modifiedAt,
	}
	if h.projects != nil {
		projectID := strings.TrimSpace(r.Header.Get("X-Project-ID"))
		projectLabel := strings.TrimSpace(r.Header.Get("X-Project"))
		if projectLabel == "" {
			projectLabel = record.Project
		}
		var resolved *pgstore.MemoryProject
		if projectID != "" {
			resolved, err = h.projects.GetForTenant(r.Context(), authCtx.TenantID, projectID)
		} else {
			resolved, err = h.projects.Resolve(r.Context(), authCtx.TenantID, projectLabel, []string{r.Header.Get("X-Repository-Remote")})
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "invalid project identity")
			return
		}
		if resolved != nil {
			record.ProjectID = resolved.ID
			record.Project = resolved.Slug
		}
	}
	if err := h.store.Put(r.Context(), record); err != nil {
		h.logger.Error().Err(err).Str("session_id", sessionID).Msg("failed to store Pi session log")
		writeError(w, http.StatusInternalServerError, "storage_error", "failed to store session log")
		return
	}

	w.Header().Set("ETag", `"`+actualHash+`"`)
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":             "stored",
		"session_id":         sessionID,
		"sha256":             actualHash,
		"entry_count":        entryCount,
		"uncompressed_bytes": len(raw),
	})
}

func requestOwner(r *http.Request) (*auth.Context, string, bool) {
	authCtx := middleware.GetAuthContext(r.Context())
	if authCtx == nil || authCtx.TenantID == "" {
		return nil, "", false
	}
	ownerID := "key:" + authCtx.KeyID
	if authCtx.UserID != "" {
		ownerID = "user:" + authCtx.UserID
	}
	return authCtx, ownerID, true
}

func validSessionID(value string) bool {
	return sessionIDPattern.MatchString(strings.TrimSpace(value))
}

func decompressLimited(compressed []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("invalid gzip session: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("decompress session: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("uncompressed session exceeds 64 MiB")
	}
	return raw, nil
}

func parseSessionHeader(raw []byte) (sessionHeader, int, error) {
	firstLine, _, _ := bytes.Cut(raw, []byte("\n"))
	var header sessionHeader
	if err := json.Unmarshal(bytes.TrimSpace(firstLine), &header); err != nil {
		return header, 0, fmt.Errorf("invalid Pi session header: %w", err)
	}
	if header.Type != "session" || !validSessionID(header.ID) {
		return header, 0, fmt.Errorf("first JSONL entry is not a valid Pi session header")
	}
	lines := bytes.Count(raw, []byte("\n"))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		lines++
	}
	return header, max(0, lines-1), nil
}

func parseTime(value string) *time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	return &t
}

func parseUnixMillis(value string) *time.Time {
	ms, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

func sanitizeFileName(value string) string {
	value = filepath.Base(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/"))
	if len(value) > 255 {
		value = value[len(value)-255:]
	}
	return value
}

func projectFromCWD(cwd string) string {
	normalized := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(cwd), `\`, "/"), "/")
	if normalized == "" {
		return ""
	}
	project := normalized[strings.LastIndex(normalized, "/")+1:]
	project = strings.ToLower(strings.TrimSpace(project))
	project = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(project, "-")
	return strings.Trim(project, "-")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, private")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]map[string]string{
		"error": {"type": errType, "message": message},
	})
}
