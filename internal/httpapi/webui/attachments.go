package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/id"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// Attachment kinds, which decide how a file is rendered into a model payload.
const (
	kindImage = "image"
	kindText  = "text"
	kindPDF   = "pdf"
)

// sniffBytes is how much of an uploaded object is read back to determine its
// real type. http.DetectContentType never looks at more than 512 bytes.
const sniffBytes = 512

// allowedImageTypes are the raster formats every vision-capable model in the
// catalog accepts. Anything else is refused rather than forwarded and rejected
// upstream with an opaque provider error.
var allowedImageTypes = map[string]string{
	"image/png":  kindImage,
	"image/jpeg": kindImage,
	"image/gif":  kindImage,
	"image/webp": kindImage,
}

// createAttachmentHandler reserves an attachment and returns a signed URL the
// browser PUTs to directly. Bytes never pass through the proxy, so a 25 MB
// upload costs the data plane one small INSERT.
func createAttachmentHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		if deps.Blobs == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "attachments are not configured")
			return
		}
		if !deps.checkWriteBudget(r.Context(), authCtx) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many uploads; wait a moment and try again")
			return
		}

		var body struct {
			Filename       string `json:"filename"`
			ContentType    string `json:"content_type"`
			SizeBytes      int64  `json:"size_bytes"`
			ConversationID string `json:"conversation_id"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}

		filename := sanitizeFilename(body.Filename)
		if filename == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "filename is required")
			return
		}
		if body.SizeBytes <= 0 {
			writeError(w, http.StatusBadRequest, "validation_error", "size_bytes is required")
			return
		}
		if body.SizeBytes > deps.Config.AttachmentMaxBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("file exceeds the %d MB limit", deps.Config.AttachmentMaxBytes/(1024*1024)))
			return
		}
		// The declared type only binds the upload signature; the authoritative
		// type is sniffed from the stored bytes at finalize time.
		declared := normalizeContentType(body.ContentType)
		if declared == "" {
			declared = "application/octet-stream"
		}

		attachment := &pgstore.WebAttachment{
			TenantID:       authCtx.TenantID,
			UserID:         authCtx.UserID,
			ConversationID: body.ConversationID,
			Filename:       filename,
			MimeType:       declared,
			Kind:           kindText,
			SizeBytes:      body.SizeBytes,
			Bucket:         deps.Blobs.Bucket(),
		}
		attachment.ID = newAttachmentID()
		attachment.ObjectKey = objectKey(authCtx.TenantID, attachment.ID)
		ttl := deps.Config.AttachmentURLTTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		if ttl > time.Hour {
			ttl = time.Hour
		}
		attachment.UploadExpiresAt = time.Now().Add(ttl)
		if days := deps.Config.AttachmentRetention; days > 0 {
			expiry := time.Now().AddDate(0, 0, days)
			attachment.ExpiresAt = &expiry
		}

		if body.ConversationID != "" {
			if _, err := deps.Conversations.Get(r.Context(), authCtx.TenantID, authCtx.UserID, body.ConversationID); err != nil {
				writeError(w, http.StatusNotFound, "not_found", "conversation not found")
				return
			}
		}

		created, err := deps.Attachments.CreatePending(r.Context(), attachment, pgstore.AttachmentLimits{
			UserBytes:      deps.Config.AttachmentUserBytes,
			TenantBytes:    deps.Config.AttachmentTenantBytes,
			PendingPerUser: deps.Config.AttachmentPendingPerUser,
		})
		if err != nil {
			if errors.Is(err, pgstore.ErrAttachmentBudgetExceeded) {
				writeError(w, http.StatusTooManyRequests, "storage_limit", "upload limit reached; wait for pending uploads to expire or contact your administrator")
				return
			}
			if errors.Is(err, pgstore.ErrAttachmentOwnerInactive) {
				writeError(w, http.StatusForbidden, "forbidden", "your account is not active")
				return
			}
			deps.Logger.Error().Err(err).Msg("web attachment reserve failed")
			writeError(w, http.StatusInternalServerError, "reserve_failed", "could not reserve upload")
			return
		}

		uploadURL, err := deps.Blobs.SignedUpload(r.Context(), created.ObjectKey, declared, created.SizeBytes, created.UploadExpiresAt)
		if err != nil {
			// No upload authority was returned, so releasing this reservation is safe.
			if releaseErr := deps.Attachments.Delete(r.Context(), []string{created.ID}); releaseErr != nil {
				deps.Logger.Warn().Err(releaseErr).Msg("web attachment signing failure reservation release failed")
			}
			deps.Logger.Error().Err(err).Msg("web attachment signing failed")
			writeError(w, http.StatusBadGateway, "signing_failed", "could not prepare upload")
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"id":         created.ID,
			"upload_url": uploadURL,
			"method":     http.MethodPut,
			"headers":    blob.UploadHeaders(declared, created.SizeBytes),
			"expires_in": int(time.Until(created.UploadExpiresAt).Seconds()),
		})
	}
}

// finalizeAttachmentHandler verifies a completed upload and marks it usable.
// Size and content type come from the stored object; the values the client
// declared when reserving are not trusted here.
func finalizeAttachmentHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		if deps.Blobs == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "attachments are not configured")
			return
		}

		attachmentID := chi.URLParam(r, "attachmentID")
		attachment, err := deps.Attachments.Get(r.Context(), authCtx.TenantID, authCtx.UserID, attachmentID)
		if errors.Is(err, pgstore.ErrAttachmentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "attachment not found")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment lookup failed")
			writeError(w, http.StatusInternalServerError, "lookup_failed", "could not load attachment")
			return
		}
		if attachment.Status != pgstore.AttachmentPending {
			writeError(w, http.StatusConflict, "already_finalized", "attachment is not pending")
			return
		}

		info, err := deps.Blobs.Stat(r.Context(), attachment.ObjectKey)
		if errors.Is(err, blob.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "upload_incomplete", "upload has not completed")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment stat failed")
			writeError(w, http.StatusBadGateway, "stat_failed", "could not verify upload")
			return
		}
		if info.Size > deps.Config.AttachmentMaxBytes {
			deps.rejectAttachment(r.Context(), authCtx.TenantID, authCtx.UserID, attachment.ID)
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "uploaded file exceeds the size limit")
			return
		}

		head, err := deps.Blobs.ReadAtMost(r.Context(), attachment.ObjectKey, deps.Config.AttachmentMaxBytes+1)
		if errors.Is(err, blob.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "upload_incomplete", "upload has not completed")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment read failed")
			writeError(w, http.StatusBadGateway, "read_failed", "could not inspect upload")
			return
		}
		// Read one byte past the cap so an object that grew after Stat is caught
		// without buffering it. The same bytes are sniffed and hashed, so the
		// digest describes exactly what was verified.
		if int64(len(head)) > deps.Config.AttachmentMaxBytes {
			deps.rejectAttachment(r.Context(), authCtx.TenantID, authCtx.UserID, attachment.ID)
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "uploaded file exceeds the size limit")
			return
		}
		if int64(len(head)) != attachment.SizeBytes {
			deps.rejectAttachment(r.Context(), authCtx.TenantID, authCtx.UserID, attachment.ID)
			writeError(w, http.StatusBadRequest, "size_mismatch", "uploaded file does not match its reserved size")
			return
		}
		sniffHead := head
		if len(sniffHead) > sniffBytes {
			sniffHead = sniffHead[:sniffBytes]
		}
		sniffed := normalizeContentType(http.DetectContentType(sniffHead))
		kind, supported := classify(sniffed)
		if !supported {
			deps.rejectAttachment(r.Context(), authCtx.TenantID, authCtx.UserID, attachment.ID)
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_type",
				"unsupported file type "+sniffed+"; images, text and PDF are accepted")
			return
		}
		digest := sha256.Sum256(head)

		finalized, err := deps.Attachments.Finalize(r.Context(), authCtx.TenantID, authCtx.UserID,
			attachment.ID, sniffed, kind, hex.EncodeToString(digest[:]), int64(len(head)))
		if errors.Is(err, pgstore.ErrAttachmentNotFound) {
			writeError(w, http.StatusConflict, "already_finalized", "attachment is not pending")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment finalize failed")
			writeError(w, http.StatusInternalServerError, "finalize_failed", "could not finalize attachment")
			return
		}
		writeJSON(w, http.StatusOK, finalized)
	}
}

func (deps *Dependencies) rejectAttachment(ctx context.Context, tenantID, userID, attachmentID string) {
	if err := deps.Attachments.Reject(ctx, tenantID, userID, attachmentID); err != nil {
		deps.Logger.Warn().Err(err).Msg("web attachment rejection persistence failed")
	}
}

// downloadAttachmentHandler redirects to a short-lived signed URL. The
// redirect, rather than a proxied stream, keeps large downloads off the data
// plane while still enforcing ownership on every request.
func downloadAttachmentHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		if deps.Blobs == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "attachments are not configured")
			return
		}

		attachment, err := deps.Attachments.Get(r.Context(), authCtx.TenantID, authCtx.UserID, chi.URLParam(r, "attachmentID"))
		if errors.Is(err, pgstore.ErrAttachmentNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "attachment not found")
			return
		}
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment lookup failed")
			writeError(w, http.StatusInternalServerError, "lookup_failed", "could not load attachment")
			return
		}
		if attachment.Status != pgstore.AttachmentReady {
			writeError(w, http.StatusConflict, "not_ready", "attachment upload has not completed")
			return
		}

		url, err := deps.Blobs.SignedDownload(r.Context(), attachment.ObjectKey, attachment.Filename, deps.Config.AttachmentURLTTL)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web attachment download signing failed")
			writeError(w, http.StatusBadGateway, "signing_failed", "could not prepare download")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, url, http.StatusFound)
	}
}

// classify maps a sniffed MIME type to the rendering kind, and reports whether
// the console supports it at all.
func classify(mimeType string) (string, bool) {
	if kind, ok := allowedImageTypes[mimeType]; ok {
		return kind, true
	}
	if mimeType == "application/pdf" {
		return kindPDF, true
	}
	if strings.HasPrefix(mimeType, "text/") {
		return kindText, true
	}
	// http.DetectContentType reports most structured text as text/plain, but
	// these arrive labelled when a client sets them explicitly.
	switch mimeType {
	case "application/json", "application/xml", "application/x-yaml", "application/toml":
		return kindText, true
	}
	return "", false
}

// normalizeContentType strips parameters and lowercases, so "text/plain;
// charset=utf-8" and "Text/Plain" compare equal.
func normalizeContentType(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(raw, ";")[0]))
	}
	return strings.ToLower(parsed)
}

// sanitizeFilename reduces a client-supplied name to a single path element.
// The object key never contains it, so this only guards what is echoed back in
// Content-Disposition and rendered in the UI.
func sanitizeFilename(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")
	raw = path.Base(raw)
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	if raw == "." || raw == ".." || raw == "/" {
		return ""
	}
	return truncate(raw, 255)
}

// objectKey builds the storage key. It is derived entirely from server-side
// values — tenant and generated ID — so no client input reaches the path.
func objectKey(tenantID, attachmentID string) string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s/%04d/%02d/%s", tenantID, now.Year(), int(now.Month()), attachmentID)
}

// loadReadyAttachments fetches the attachments named by a turn, refusing the
// turn if any is missing. Silently dropping one would send the model a message
// the user believes contained a file.
func (deps *Dependencies) loadReadyAttachments(ctx context.Context, tenantID, userID string, ids []string) ([]*pgstore.WebAttachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := deps.Attachments.GetReadyBatch(ctx, tenantID, userID, ids)
	if err != nil {
		return nil, err
	}
	if len(found) != len(ids) {
		return nil, fmt.Errorf("%d of %d attachments are missing or not finished uploading", len(ids)-len(found), len(ids))
	}
	return found, nil
}

// newAttachmentID mints the identifier that doubles as the object name.
func newAttachmentID() string {
	return id.New("watt")
}
