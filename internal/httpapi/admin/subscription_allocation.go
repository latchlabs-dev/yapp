package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

func providerBillingPeriodsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SubscriptionRateStore == nil {
			writeError(w, 503, "unavailable", "subscription accounting unavailable")
			return
		}
		periods, err := deps.SubscriptionRateStore.ListBillingPeriods(r.Context(), chi.URLParam(r, "accountID"))
		if err != nil {
			writeError(w, 500, "billing_periods", err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"billing_periods": periods})
	}
}

func createProviderBillingPeriodHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SubscriptionRateStore == nil {
			writeError(w, 503, "unavailable", "subscription accounting unavailable")
			return
		}
		var req struct {
			Start  time.Time `json:"starts_at"`
			End    time.Time `json:"ends_at"`
			Amount int64     `json:"amount_millicents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid_request", err.Error())
			return
		}
		p := subscriptioncost.Period{ProviderAccountID: chi.URLParam(r, "accountID"), Start: req.Start, End: req.End, Amount: req.Amount}
		if err := deps.SubscriptionRateStore.CreateBillingPeriod(r.Context(), p); err != nil {
			writeError(w, 400, "invalid_billing_period", err.Error())
			return
		}
		if deps.AuditLogger != nil {
			deps.AuditLogger.Logf("admin", "admin", audit.ActionProviderUpdated, "provider_account", p.ProviderAccountID, "success", "recorded paid subscription period")
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
	}
}
