package memory

import (
	"testing"
	"time"
)

func mustUTC(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func TestOffPeakScheduleDefaultDeepSeekWindows(t *testing.T) {
	schedule, err := NewOffPeakSchedule(DefaultCompactionPeakWindowsUTC)
	if err != nil {
		t.Fatalf("NewOffPeakSchedule: %v", err)
	}

	cases := map[string]string{
		"2026-07-08T00:59:00Z": "2026-07-08T00:59:00Z",
		"2026-07-08T01:00:00Z": "2026-07-08T04:00:00Z",
		"2026-07-08T03:59:59Z": "2026-07-08T04:00:00Z",
		"2026-07-08T04:00:00Z": "2026-07-08T04:00:00Z",
		"2026-07-08T06:00:00Z": "2026-07-08T10:00:00Z",
		"2026-07-08T09:59:59Z": "2026-07-08T10:00:00Z",
		"2026-07-08T10:00:00Z": "2026-07-08T10:00:00Z",
	}
	for input, want := range cases {
		got := schedule.NextAllowedAt(mustUTC(t, input))
		if !got.Equal(mustUTC(t, want)) {
			t.Errorf("NextAllowedAt(%s) = %s, want %s", input, got.Format(time.RFC3339), want)
		}
	}
}

func TestOffPeakScheduleCrossMidnightWindow(t *testing.T) {
	schedule, err := NewOffPeakSchedule("22:00-02:00")
	if err != nil {
		t.Fatalf("NewOffPeakSchedule: %v", err)
	}

	cases := map[string]string{
		"2026-07-08T21:59:00Z": "2026-07-08T21:59:00Z",
		"2026-07-08T22:00:00Z": "2026-07-09T02:00:00Z",
		"2026-07-09T01:30:00Z": "2026-07-09T02:00:00Z",
		"2026-07-09T02:00:00Z": "2026-07-09T02:00:00Z",
	}
	for input, want := range cases {
		got := schedule.NextAllowedAt(mustUTC(t, input))
		if !got.Equal(mustUTC(t, want)) {
			t.Errorf("NextAllowedAt(%s) = %s, want %s", input, got.Format(time.RFC3339), want)
		}
	}
}

func TestOffPeakScheduleExecutionBucketUsesNextAllowedTime(t *testing.T) {
	schedule, err := NewOffPeakSchedule(DefaultCompactionPeakWindowsUTC)
	if err != nil {
		t.Fatalf("NewOffPeakSchedule: %v", err)
	}
	got := schedule.ExecutionBucket(mustUTC(t, "2026-07-08T02:30:00Z"), 6*time.Hour)
	want := mustUTC(t, "2026-07-08T04:00:00Z")
	if !got.Equal(want) {
		t.Fatalf("ExecutionBucket = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestOffPeakScheduleRejectsBadWindow(t *testing.T) {
	if _, err := NewOffPeakSchedule("01:00/04:00"); err == nil {
		t.Fatal("expected invalid window error")
	}
}
