package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type insightProcessorInput struct {
	Name          string `json:"name"`
	GRPCTarget    string `json:"grpc_target"`
	Plaintext     bool   `json:"plaintext"`
	TLSServerName string `json:"tls_server_name"`
	TimeoutMS     int    `json:"timeout_ms"`
	Status        string `json:"status"`
}

type insightConfigurationInput struct {
	ProcessorID             string `json:"processor_id"`
	Name                    string `json:"name"`
	TenantID                string `json:"tenant_id"`
	ProjectID               string `json:"project_id"`
	Instructions            string `json:"instructions"`
	AllowNewTags            *bool  `json:"allow_new_tags"`
	IncludeSessionSummary   *bool  `json:"include_session_summary"`
	IncludeInsightSummaries *bool  `json:"include_insight_summaries"`
	MaxInputItems           int    `json:"max_input_items"`
	MaxInputChars           int    `json:"max_input_chars"`
	Status                  string `json:"status"`
}

func listInsightProcessorsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		records, err := deps.InsightStore.ListProcessors(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_insight_processors", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"insight_processors": records})
	}
}

func createInsightProcessorHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		var input insightProcessorInput
		if !decodeInsightJSON(w, r, &input) || !validateInsightProcessor(w, &input) {
			return
		}
		record := processorFromInput("", input)
		if err := deps.InsightStore.CreateProcessor(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_insight_processor", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, record)
	}
}

func updateInsightProcessorHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		var input insightProcessorInput
		if !decodeInsightJSON(w, r, &input) || !validateInsightProcessor(w, &input) {
			return
		}
		record := processorFromInput(chi.URLParam(r, "processorID"), input)
		if err := deps.InsightStore.UpdateProcessor(r.Context(), record); err != nil {
			writeInsightStoreError(w, "update_insight_processor", err)
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func disableInsightProcessorHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		if err := deps.InsightStore.DisableProcessor(r.Context(), chi.URLParam(r, "processorID")); err != nil {
			writeError(w, http.StatusInternalServerError, "disable_insight_processor", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
	}
}

func testInsightProcessorHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		if deps.InsightClients == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "insight gRPC client is not configured")
			return
		}
		processor, err := deps.InsightStore.GetProcessor(r.Context(), chi.URLParam(r, "processorID"))
		if err != nil {
			writeInsightStoreError(w, "get_insight_processor", err)
			return
		}
		health, err := deps.InsightClients.Health(r.Context(), processor)
		if err != nil {
			writeError(w, http.StatusBadGateway, "insight_processor_unavailable", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, health)
	}
}

func processorFromInput(id string, input insightProcessorInput) *pgstore.InsightProcessorRecord {
	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = "active"
	}
	timeout := input.TimeoutMS
	if timeout == 0 {
		timeout = 120000
	}
	return &pgstore.InsightProcessorRecord{ID: id, Name: strings.TrimSpace(input.Name), GRPCTarget: strings.TrimSpace(input.GRPCTarget), Plaintext: input.Plaintext, TLSServerName: strings.TrimSpace(input.TLSServerName), TimeoutMS: timeout, Status: status}
}

func validateInsightProcessor(w http.ResponseWriter, input *insightProcessorInput) bool {
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.GRPCTarget) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "name and grpc_target are required")
		return false
	}
	if strings.Contains(input.GRPCTarget, "://") && !strings.HasPrefix(input.GRPCTarget, "dns:///") && !strings.HasPrefix(input.GRPCTarget, "unix://") {
		writeError(w, http.StatusBadRequest, "validation_error", "grpc_target must be a gRPC target, not an HTTP URL")
		return false
	}
	if input.Status != "" && input.Status != "active" && input.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "validation_error", "status must be active or disabled")
		return false
	}
	return true
}

func listInsightConfigurationsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		records, err := deps.InsightStore.ListConfigurations(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_insight_configurations", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"insight_configurations": records})
	}
}

func createInsightConfigurationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		var input insightConfigurationInput
		if !decodeInsightJSON(w, r, &input) || !validateInsightConfiguration(w, input) {
			return
		}
		record := configurationFromInput("", input)
		if err := deps.InsightStore.CreateConfiguration(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_insight_configuration", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, record)
	}
}

func updateInsightConfigurationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		var input insightConfigurationInput
		if !decodeInsightJSON(w, r, &input) || !validateInsightConfiguration(w, input) {
			return
		}
		record := configurationFromInput(chi.URLParam(r, "configurationID"), input)
		if err := deps.InsightStore.UpdateConfiguration(r.Context(), record); err != nil {
			writeInsightStoreError(w, "update_insight_configuration", err)
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func disableInsightConfigurationHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		if err := deps.InsightStore.DisableConfiguration(r.Context(), chi.URLParam(r, "configurationID")); err != nil {
			writeError(w, http.StatusInternalServerError, "disable_insight_configuration", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
	}
}

func validateInsightConfiguration(w http.ResponseWriter, input insightConfigurationInput) bool {
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.ProcessorID) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "name and processor_id are required")
		return false
	}
	if input.Status != "" && input.Status != "active" && input.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "validation_error", "status must be active or disabled")
		return false
	}
	if strings.TrimSpace(input.ProjectID) != "" && strings.TrimSpace(input.TenantID) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "tenant_id is required for a project-scoped configuration")
		return false
	}
	return true
}

func configurationFromInput(id string, input insightConfigurationInput) *pgstore.InsightConfigurationRecord {
	boolValue := func(value *bool, fallback bool) bool {
		if value == nil {
			return fallback
		}
		return *value
	}
	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = "active"
	}
	maxItems := input.MaxInputItems
	if maxItems == 0 {
		maxItems = 1000
	}
	maxChars := input.MaxInputChars
	if maxChars == 0 {
		maxChars = 500000
	}
	return &pgstore.InsightConfigurationRecord{
		ID: id, ProcessorID: strings.TrimSpace(input.ProcessorID), Name: strings.TrimSpace(input.Name),
		TenantID: strings.TrimSpace(input.TenantID), ProjectID: strings.TrimSpace(input.ProjectID),
		Instructions: strings.TrimSpace(input.Instructions), AllowNewTags: boolValue(input.AllowNewTags, true),
		IncludeSessionSummary:   boolValue(input.IncludeSessionSummary, true),
		IncludeInsightSummaries: boolValue(input.IncludeInsightSummaries, true),
		MaxInputItems:           maxItems, MaxInputChars: maxChars, Status: status,
	}
}

func listInsightTagsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		records, err := deps.InsightStore.ListTags(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_insight_tags", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"insight_tags": records})
	}
}

func upsertInsightTagHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		var record pgstore.InsightTagRecord
		if !decodeInsightJSON(w, r, &record) {
			return
		}
		if strings.TrimSpace(record.Name) == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "tag name is required")
			return
		}
		if err := deps.InsightStore.UpsertTag(r.Context(), &record); err != nil {
			writeError(w, http.StatusInternalServerError, "upsert_insight_tag", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, &record)
	}
}

func disableInsightTagHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		if err := deps.InsightStore.DisableTag(r.Context(), chi.URLParam(r, "tagName")); err != nil {
			writeError(w, http.StatusInternalServerError, "disable_insight_tag", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
	}
}

func listInsightRunsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireInsightStore(w, deps) {
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		records, err := deps.InsightStore.ListRuns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_insight_runs", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"insight_runs": records})
	}
}

func requireInsightStore(w http.ResponseWriter, deps *Dependencies) bool {
	if deps.InsightStore == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "insight configuration store is not configured")
		return false
	}
	return true
}

func decodeInsightJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func writeInsightStoreError(w http.ResponseWriter, kind string, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "insight resource not found")
		return
	}
	writeError(w, http.StatusInternalServerError, kind, err.Error())
}
