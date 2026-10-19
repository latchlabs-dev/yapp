package admin

import (
	"strings"
	"testing"
	"time"
)

func TestValidateUsageReportRange(t *testing.T) {
	to := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		from        time.Time
		to          time.Time
		granularity string
		wantErr     string
	}{
		{
			name:        "default daily window allowed",
			from:        to.AddDate(0, 0, -7),
			to:          to,
			granularity: "daily",
		},
		{
			name:        "hourly max allowed",
			from:        to.Add(-31 * 24 * time.Hour),
			to:          to,
			granularity: "hourly",
		},
		{
			name:        "hourly too large",
			from:        to.Add(-31*24*time.Hour - time.Second),
			to:          to,
			granularity: "hourly",
			wantErr:     "limited",
		},
		{
			name:        "daily too large",
			from:        to.Add(-366*24*time.Hour - time.Second),
			to:          to,
			granularity: "daily",
			wantErr:     "limited",
		},
		{
			name:        "monthly too large",
			from:        to.Add(-5*366*24*time.Hour - time.Second),
			to:          to,
			granularity: "monthly",
			wantErr:     "limited",
		},
		{
			name:        "inverted rejected",
			from:        to,
			to:          to.Add(-time.Second),
			granularity: "daily",
			wantErr:     "before",
		},
		{
			name:        "invalid granularity ignored by range helper",
			from:        to.AddDate(0, 0, -1000),
			to:          to,
			granularity: "weekly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUsageReportRange(tt.from, tt.to, tt.granularity)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateUsageReportRange returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
