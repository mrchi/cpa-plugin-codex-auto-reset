package main

import (
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Window constants carried by the codex 429 error body (ADR-0002).
const (
	weeklyWindowMinutes   = 10080
	weeklyFallbackSeconds = 18000 // longer than the 5-hour window, so it must be weekly
	oneDaySeconds         = 86400
)

// Window labels, as they appear in the decision log (spec story 17).
const (
	windowWeekly   = "weekly"
	windowFiveHour = "5h"
	windowUnknown  = "unknown"
)

// Decision reasons double as the log message. An empty reason means the record was
// ignored outright and nothing is logged (spec story 22).
const (
	reasonHit           = "codex weekly usage limit reached: reset eligible"
	reasonDisabled      = "auto-reset disabled: ignoring exhaustion signal"
	reasonExcluded      = "credential excluded from auto-reset: ignoring exhaustion signal"
	reasonNotWeekly     = "window is not the weekly window: no reset"
	reasonWithinDay     = "weekly window recovers within a day: no reset"
	reasonUnknownWindow = "window or reset timing missing from error body: no reset"
)

// classification is the verdict for one usage record. Reason is empty for records
// that are not a codex 429 usage_limit_reached at all; every other outcome carries a
// reason so negative decisions stay auditable (spec story 17).
type classification struct {
	Hit              bool
	Reason           string
	Window           string
	ResetsInSeconds  int64
	WindowMinutes    int64
	HasWindowMinutes bool
	Weekly           bool
}

// classify makes no host calls and no I/O; it reads the process clock to resolve the
// reset timing. It takes the record and the active configuration and decides whether
// this exhaustion is worth spending a reset credit on (ADR-0002).
func classify(record pluginapi.UsageRecord, cfg pluginConfig) classification {
	var result classification
	// Codex subscription credentials only; everything else is ignored outright.
	if record.Provider != "codex" || record.AuthType != "oauth" {
		return result
	}
	if !record.Failed || record.Failure.StatusCode != 429 {
		return result
	}
	limit, okLimit := parseUsageLimit(record.Failure.Body)
	if !okLimit {
		return result
	}
	resetsIn, okResets := limit.resetsIn(now())
	if okResets {
		result.ResetsInSeconds = resetsIn
	}
	result.WindowMinutes, result.HasWindowMinutes = limit.windowMinutes, limit.hasWindowMinutes
	result.Weekly = limit.isWeekly(resetsIn)
	switch {
	case result.Weekly:
		result.Window = windowWeekly
	case limit.hasWindowMinutes || okResets:
		result.Window = windowFiveHour
	default:
		result.Window = windowUnknown
	}

	if !cfg.Enabled {
		result.Reason = reasonDisabled
		return result
	}
	if isExcluded(cfg.ExcludeCredentials, record) {
		result.Reason = reasonExcluded
		return result
	}
	switch {
	case !result.Weekly && (limit.hasWindowMinutes || okResets):
		result.Reason = reasonNotWeekly
	case !result.Weekly:
		result.Reason = reasonUnknownWindow
	case okResets && resetsIn > oneDaySeconds:
		result.Hit, result.Reason = true, reasonHit
	case okResets && resetsIn > 0:
		result.Reason = reasonWithinDay
	default:
		result.Reason = reasonUnknownWindow
	}
	return result
}

// usageLimit is the part of the upstream 429 body this plugin cares about.
type usageLimit struct {
	resetsInSeconds  int64
	resetsAt         time.Time
	hasResetsAt      bool
	windowMinutes    int64
	hasWindowMinutes bool
}

// resetsIn reports how long the window still has to run: the explicit
// resets_in_seconds when it is positive, otherwise a resets_at timestamp measured
// against at. The body carries both fields, and a body carrying only resets_at must
// still classify — otherwise the whole feature fails silently on that shape. ok=false
// means the body gave no usable timing at all.
func (l usageLimit) resetsIn(at time.Time) (int64, bool) {
	if l.resetsInSeconds > 0 {
		return l.resetsInSeconds, true
	}
	if !l.hasResetsAt {
		return 0, false
	}
	seconds := int64(l.resetsAt.Sub(at).Seconds())
	if seconds <= 0 {
		return 0, false
	}
	return seconds, true
}

// isWeekly classifies the window: an explicit limit_window_minutes is authoritative,
// otherwise a reset further out than the 5-hour window can only be the weekly one.
func (l usageLimit) isWeekly(resetsInSeconds int64) bool {
	if l.hasWindowMinutes {
		return l.windowMinutes == weeklyWindowMinutes
	}
	return resetsInSeconds > weeklyFallbackSeconds
}

// parseUsageLimit extracts the quota fields from a 429 error body. CPA parses
// resets_at/resets_in_seconds for its own cooldown but never limit_window_minutes,
// so the raw body is the only source for the window. The body carries the error
// either at the top level or nested under "error" (both shapes occur upstream), each
// field may be absent, and unknown fields are ignored; a body that cannot be
// understood yields ok=false and the record is treated as a non-match rather than an
// error.
func parseUsageLimit(body string) (usageLimit, bool) {
	if strings.TrimSpace(body) == "" {
		return usageLimit{}, false
	}
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal != nil {
		return usageLimit{}, false
	}
	// Nested object first, matching CPA's own parser ordering.
	candidates := []map[string]json.RawMessage{nil, payload}
	if rawError, okError := payload["error"]; okError {
		var nested map[string]json.RawMessage
		if json.Unmarshal(rawError, &nested) == nil {
			candidates[0] = nested
		}
	}

	matched := false
	for _, candidate := range candidates {
		if candidate != nil && isUsageLimitType(candidate) {
			matched = true
		}
	}
	if !matched {
		return usageLimit{}, false
	}

	var limit usageLimit
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if !limit.hasWindowMinutes {
			if value, okValue := bodyInt64(candidate["limit_window_minutes"]); okValue && value > 0 {
				limit.windowMinutes, limit.hasWindowMinutes = value, true
			}
		}
		if limit.resetsInSeconds == 0 {
			if value, okValue := bodyInt64(candidate["resets_in_seconds"]); okValue {
				limit.resetsInSeconds = value
			}
		}
		if !limit.hasResetsAt {
			if value, okValue := bodyTime(candidate["resets_at"]); okValue {
				limit.resetsAt, limit.hasResetsAt = value, true
			}
		}
	}
	return limit, true
}

func isUsageLimitType(object map[string]json.RawMessage) bool {
	var errorType string
	if errUnmarshal := json.Unmarshal(object["type"], &errorType); errUnmarshal != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(errorType), "usage_limit_reached")
}

func bodyInt64(raw json.RawMessage) (int64, bool) {
	var number float64
	if errUnmarshal := json.Unmarshal(raw, &number); errUnmarshal != nil {
		return 0, false
	}
	return int64(number), true
}

// bodyTime reads an RFC3339 string field such as resets_at.
func bodyTime(raw json.RawMessage) (time.Time, bool) {
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal != nil {
		return time.Time{}, false
	}
	parsed, errParse := time.Parse(time.RFC3339, strings.TrimSpace(text))
	if errParse != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// isExcluded matches exclude_credentials entries against the credential's auth id,
// its auth file name and its runtime auth index (D8). A blank entry never matches, so
// an empty allow-list cannot accidentally exclude everything.
func isExcluded(entries []string, record pluginapi.UsageRecord) bool {
	fileName := path.Base(record.AuthID)
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == record.AuthID || entry == fileName || entry == record.AuthIndex {
			return true
		}
	}
	return false
}
