package main

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Window constants carried by the codex 429 error body (ADR-0002).
const (
	weeklyWindowMinutes   = 10080
	weeklyFallbackSeconds = 18000 // longer than the 5-hour window, so it must be weekly
	oneDaySeconds         = 86400
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
	ResetsInSeconds  int64
	WindowMinutes    int64
	HasWindowMinutes bool
	Weekly           bool
}

// classify is pure: no host calls, no I/O. It takes the record and the active
// configuration and decides whether this exhaustion is worth spending a reset credit
// on (ADR-0002).
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
	result.ResetsInSeconds = limit.resetsInSeconds
	result.WindowMinutes, result.HasWindowMinutes = limit.windowMinutes, limit.hasWindowMinutes
	result.Weekly = limit.weekly()

	if !cfg.Enabled {
		result.Reason = reasonDisabled
		return result
	}
	if isExcluded(cfg.ExcludeCredentials, record) {
		result.Reason = reasonExcluded
		return result
	}
	switch {
	case !result.Weekly && (limit.hasWindowMinutes || limit.resetsInSeconds > 0):
		result.Reason = reasonNotWeekly
	case !result.Weekly:
		result.Reason = reasonUnknownWindow
	case limit.resetsInSeconds > oneDaySeconds:
		result.Hit, result.Reason = true, reasonHit
	case limit.resetsInSeconds > 0:
		result.Reason = reasonWithinDay
	default:
		result.Reason = reasonUnknownWindow
	}
	return result
}

// usageLimit is the part of the upstream 429 body this plugin cares about.
type usageLimit struct {
	resetsInSeconds  int64
	windowMinutes    int64
	hasWindowMinutes bool
}

// weekly classifies the window: an explicit limit_window_minutes is authoritative,
// otherwise a reset further out than the 5-hour window can only be the weekly one.
func (l usageLimit) weekly() bool {
	if l.hasWindowMinutes {
		return l.windowMinutes == weeklyWindowMinutes
	}
	return l.resetsInSeconds > weeklyFallbackSeconds
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
