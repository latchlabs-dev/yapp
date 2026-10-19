package postgres

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/usage"
)

func TestNullableText(t *testing.T) {
	if got := nullableText(""); got != nil {
		t.Fatalf("expected nil for empty string, got %#v", got)
	}
	if got := nullableText("tenant_123"); got != "tenant_123" {
		t.Fatalf("expected original string, got %#v", got)
	}
}

func TestUsageEventInsertArgsUsesNullAuthRefs(t *testing.T) {
	startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(time.Second)
	createdAt := completedAt.Add(time.Second)
	event := &usage.Event{
		ID:               "use_123",
		RequestID:        "req_123",
		Provider:         "openai",
		Model:            "gpt-4o",
		UpstreamProvider: "openai",
		UpstreamModel:    "gpt-4o",
		Status:           "success",
		InputTokens:      10,
		OutputTokens:     20,
		ReasoningTokens:  7,
		TotalTokens:      30,
		StartedAt:        startedAt,
		CompletedAt:      &completedAt,
	}

	args := usageEventInsertArgs(event, createdAt)
	if len(args) != 28 {
		t.Fatalf("expected 28 args, got %d", len(args))
	}
	if args[2] != nil || args[3] != nil || args[4] != nil {
		t.Fatalf("expected nil auth refs, got tenant=%#v user=%#v key=%#v", args[2], args[3], args[4])
	}
	if args[15] != event.OutputTokens || args[16] != event.ReasoningTokens {
		t.Fatalf("expected output/reasoning args at 15/16, got %#v/%#v", args[15], args[16])
	}
	// client_ip is the last arg; empty IP must be NULL so the inet insert never fails.
	if args[27] != nil {
		t.Fatalf("expected nil client_ip for empty IP, got %#v", args[27])
	}

	event.TenantID = "tenant_123"
	event.UserID = "user_123"
	event.KeyID = "key_123"
	event.ProjectID = "mprj_123"
	event.ExtensionVersion = "1.3.24"
	event.ProviderAccountID = "provider_123"
	event.ClientIP = "203.0.113.7"
	args = usageEventInsertArgs(event, createdAt)
	if args[2] != event.TenantID || args[3] != event.UserID || args[4] != event.KeyID {
		t.Fatalf("expected populated auth refs, got tenant=%#v user=%#v key=%#v", args[2], args[3], args[4])
	}
	if args[5] != event.ProjectID {
		t.Fatalf("expected populated project ref, got %#v", args[5])
	}
	if args[6] != event.ExtensionVersion {
		t.Fatalf("expected populated extension version, got %#v", args[6])
	}
	if args[11] != event.ProviderAccountID {
		t.Fatalf("expected provider account ref, got %#v", args[11])
	}
	if got, ok := args[27].(netip.Addr); !ok || got.String() != "203.0.113.7" {
		t.Fatalf("expected client_ip netip.Addr 203.0.113.7, got %#v", args[27])
	}

	// An unparseable IP must degrade to NULL rather than poison the insert.
	event.ClientIP = "not-an-ip"
	args = usageEventInsertArgs(event, createdAt)
	if args[27] != nil {
		t.Fatalf("expected nil client_ip for invalid IP, got %#v", args[27])
	}
}

func TestQueryUsageReportsRejectsInvalidGranularity(t *testing.T) {
	store := &UsageStore{}
	_, err := store.QueryUsageReports(context.Background(), "", time.Now().Add(-time.Hour), time.Now(), "weekly", "", "", 0)
	if err == nil {
		t.Fatal("expected invalid granularity error")
	}
}

func TestClampUsageReportLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to default", 0, defaultUsageReportLimit},
		{"negative falls back to default", -5, defaultUsageReportLimit},
		{"in-range passes through", 1234, 1234},
		{"at max passes through", maxUsageReportLimit, maxUsageReportLimit},
		{"above max clamps down", maxUsageReportLimit + 1, maxUsageReportLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampUsageReportLimit(tc.in); got != tc.want {
				t.Fatalf("clampUsageReportLimit(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
