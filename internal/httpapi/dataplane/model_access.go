package dataplane

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
)

// CheckModelAccess applies tenant model allow/deny settings. A nil store or
// unauthenticated request is treated as unrestricted.
func CheckModelAccess(r *http.Request, services *Services, model string) error {
	return CheckModelRouteAccess(r, services, model, model)
}

// CheckModelRouteAccess applies tenant policy to both a synthetic public name
// and its concrete target so renaming cannot bypass a target blacklist.
func CheckModelRouteAccess(r *http.Request, services *Services, publicModel, targetModel string) error {
	if strings.TrimSpace(publicModel) == "" {
		return nil
	}
	if services == nil || services.ModelAccess == nil {
		return nil
	}
	authCtx := middleware.GetAuthContext(r.Context())
	if authCtx == nil || authCtx.TenantID == "" {
		return nil
	}
	cfg, err := services.ModelAccess.Get(r.Context(), authCtx.TenantID)
	if err != nil {
		return fmt.Errorf("load tenant model settings: %w", err)
	}
	if !cfg.AllowsSynthetic(publicModel, targetModel) {
		return fmt.Errorf("model %s is not enabled for tenant %s", publicModel, authCtx.TenantID)
	}
	return nil
}
