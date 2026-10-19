package dataplane

import (
	"net/http"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/modelguard"
)

// CheckModelGuard applies static model guard rules (modelguard.Rules) to the
// requested model and returns the violation when the model is blocked. A nil
// services, missing rules, or empty model matches nothing.
//
// Guards run before provider routing and quota reservation so a denied
// request never reserves spend. Tenant-scoped rules are matched against the
// authenticated tenant; rules without a tenant apply to everyone.
func CheckModelGuard(r *http.Request, services *Services, model string) *modelguard.Violation {
	if services == nil {
		return nil
	}
	tenantID := ""
	if authCtx := middleware.GetAuthContext(r.Context()); authCtx != nil {
		tenantID = authCtx.TenantID
	}
	return services.ModelGuards.Check(model, tenantID)
}
