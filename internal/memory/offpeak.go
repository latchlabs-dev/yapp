package memory

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const DefaultCompactionPeakWindowsUTC = "01:00-04:00,06:00-10:00"

type dailyWindow struct {
	start time.Duration
	end   time.Duration
}

// OffPeakSchedule describes UTC periods when expensive LLM jobs should not run.
type OffPeakSchedule struct {
	peak []dailyWindow
}

// NewOffPeakSchedule parses comma-separated UTC windows such as
// "01:00-04:00,06:00-10:00". The special value "none" disables deferral.
func NewOffPeakSchedule(spec string) (OffPeakSchedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "none") {
		return OffPeakSchedule{}, nil
	}

	parts := strings.Split(spec, ",")
	windows := make([]dailyWindow, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bounds := strings.Split(part, "-")
		if len(bounds) != 2 {
			return OffPeakSchedule{}, fmt.Errorf("invalid UTC peak window %q", part)
		}
		start, err := parseClockUTC(bounds[0], false)
		if err != nil {
			return OffPeakSchedule{}, fmt.Errorf("invalid UTC peak window %q: %w", part, err)
		}
		end, err := parseClockUTC(bounds[1], true)
		if err != nil {
			return OffPeakSchedule{}, fmt.Errorf("invalid UTC peak window %q: %w", part, err)
		}
		if start == end {
			return OffPeakSchedule{}, fmt.Errorf("invalid UTC peak window %q: start and end are equal", part)
		}
		windows = append(windows, dailyWindow{start: start, end: end})
	}
	return OffPeakSchedule{peak: windows}, nil
}

func parseClockUTC(value string, allowEndOfDay bool) (time.Duration, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid hour")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("invalid minute")
	}
	if hour == 24 && minute == 0 && allowEndOfDay {
		return 24 * time.Hour, nil
	}
	if hour < 0 || hour > 23 {
		return 0, fmt.Errorf("hour out of range")
	}
	if minute < 0 || minute > 59 {
		return 0, fmt.Errorf("minute out of range")
	}
	return time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, nil
}

// NextAllowedAt returns now when now is outside peak windows, or the end of the
// active peak window when now falls inside one.
func (s OffPeakSchedule) NextAllowedAt(now time.Time) time.Time {
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	timeOfDay := now.Sub(dayStart)

	var wakeAt time.Time
	for _, window := range s.peak {
		var candidate time.Time
		switch {
		case window.start < window.end:
			if timeOfDay >= window.start && timeOfDay < window.end {
				candidate = dayStart.Add(window.end)
			}
		default: // Crosses midnight.
			if timeOfDay >= window.start {
				candidate = dayStart.Add(24 * time.Hour).Add(window.end)
			} else if timeOfDay < window.end {
				candidate = dayStart.Add(window.end)
			}
		}
		if !candidate.IsZero() && candidate.After(wakeAt) {
			wakeAt = candidate
		}
	}
	if !wakeAt.IsZero() {
		return wakeAt
	}
	return now
}

func (s OffPeakSchedule) ExecutionBucket(now time.Time, interval time.Duration) time.Time {
	now = now.UTC()
	allowedAt := s.NextAllowedAt(now)
	if allowedAt.After(now) {
		return allowedAt
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return now.Truncate(interval)
}
