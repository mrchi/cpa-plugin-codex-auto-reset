package main

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaBody renders the upstream 429 error body shape: the error object may sit at
// the top level or under "error".
func quotaBody(nested bool, fields string) string {
	if nested {
		return `{"error":{` + fields + `}}`
	}
	return `{` + fields + `}`
}

// usageRecord builds the codex-oauth 429 usage_limit_reached record every case
// starts from; cases vary one field at a time.
func usageRecord(body string) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider:  "codex",
		AuthType:  "oauth",
		AuthID:    authFile,
		AuthIndex: authIndex,
		Model:     "gpt-5.5",
		Failed:    true,
		Failure:   pluginapi.UsageFailure{StatusCode: 429, Body: body},
	}
}

func marshalRecord(t *testing.T, record pluginapi.UsageRecord) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		t.Fatalf("marshal record: %v", errMarshal)
	}
	return raw
}

func TestUsageHandleClassifiesExhaustion(t *testing.T) {
	weeklyBody := hitBody()
	weeklyFallbackBody := quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":200000`)
	// resetsAtBody renders a body whose only timing is the timestamp; the offset is
	// applied to the case's own clock.
	resetsAt := func(offset time.Duration) func(time.Time) string {
		return func(at time.Time) string { return resetsAtBody(at.Add(offset)) }
	}
	bothFields := func(offset time.Duration) func(time.Time) string {
		return func(at time.Time) string {
			return weeklyBodyWith(`"resets_in_seconds":200000,"resets_at":"` + at.Add(offset).Format(time.RFC3339) + `"`)
		}
	}

	tests := []struct {
		name       string
		configYAML string
		record     pluginapi.UsageRecord
		// bodyAt replaces the record body with one built from the case's clock, for
		// the resets_at cases whose timestamp must sit relative to "now".
		bodyAt func(at time.Time) string
		// wantLevel is "" when the record is ignored with no log at all.
		wantLevel        string
		wantReason       string
		wantWindow       string
		wantResetsInSecs int64 // 0 means the case does not pin the number
	}{
		{
			name:             "weekly window exhaustion is a hit",
			configYAML:       includedConfigYAML,
			record:           usageRecord(weeklyBody),
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:       "top-level error object is a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(false, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":200000`)),
			wantLevel:  "info",
			wantReason: reasonHit,
			wantWindow: windowWeekly,
		},
		{
			name:             "missing limit_window_minutes falls back to resets_in_seconds",
			configYAML:       includedConfigYAML,
			record:           usageRecord(weeklyFallbackBody),
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:             "resets_at alone still classifies as a hit",
			configYAML:       includedConfigYAML,
			record:           usageRecord(resetsAtBody(testClock.Add(200000 * time.Second))),
			bodyAt:           resetsAt(200000 * time.Second),
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:       "resets_at alone drives the weekly fallback window",
			configYAML: includedConfigYAML,
			record:     usageRecord(timingOnlyBody(`"resets_in_seconds":200000`)),
			bodyAt: func(at time.Time) string {
				return timingOnlyBody(`"resets_at":"` + at.Add(200000*time.Second).Format(time.RFC3339) + `"`)
			},
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:       "a non-positive resets_in_seconds falls back to resets_at",
			configYAML: includedConfigYAML,
			record:     usageRecord(weeklyBodyWith(`"resets_in_seconds":-1`)),
			bodyAt: func(at time.Time) string {
				return weeklyBodyWith(`"resets_in_seconds":-1,"resets_at":"` + at.Add(200000*time.Second).Format(time.RFC3339) + `"`)
			},
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:             "resets_in_seconds wins when both are present",
			configYAML:       includedConfigYAML,
			record:           usageRecord(weeklyBody),
			bodyAt:           bothFields(3600 * time.Second),
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:       "reset exactly one day out is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":86400`)),
			wantLevel:  "debug",
			wantReason: reasonWithinDay,
			wantWindow: windowWeekly,
		},
		{
			name:             "resets_at inside the one-day guard is not a hit",
			configYAML:       includedConfigYAML,
			record:           usageRecord(resetsAtBody(testClock.Add(time.Hour))),
			bodyAt:           resetsAt(time.Hour),
			wantLevel:        "debug",
			wantReason:       reasonWithinDay,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 3600,
		},
		{
			name:       "reset just past one day is a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":86401`)),
			wantLevel:  "info",
			wantReason: reasonHit,
			wantWindow: windowWeekly,
		},
		{
			name:       "a resets_at already in the past is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(resetsAtBody(testClock.Add(-time.Hour))),
			bodyAt:     resetsAt(-time.Hour),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
			wantWindow: windowWeekly,
		},
		{
			name:       "5-hour window is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":300,"resets_in_seconds":3600`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
			wantWindow: windowFiveHour,
		},
		{
			name:       "fallback range without limit_window_minutes is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":3600`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
			wantWindow: windowFiveHour,
		},
		{
			name:       "fallback boundary 18000 is not weekly",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":18000`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
			wantWindow: windowFiveHour,
		},
		{
			// A window the plugin does not act on must not be reported as the 5-hour one:
			// the label is what the audit log says was seen.
			name:       "an explicit window that is neither weekly nor 5-hour is unknown",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":43200,"resets_in_seconds":3600`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
			wantWindow: windowUnknown,
		},
		{
			name:       "weekly window without reset timing is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
			wantWindow: windowWeekly,
		},
		{
			name:       "no window and no reset timing is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached"`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
			wantWindow: windowUnknown,
		},
		{
			name:       "non-positive reset is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":-1`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
			wantWindow: windowWeekly,
		},
		{
			// The nested object is read first, so a non-positive timing inside it must not
			// claim the field and hide the usable one at the top level.
			name:       "a negative nested resets_in_seconds does not mask the top-level timing",
			configYAML: includedConfigYAML,
			record: usageRecord(`{"error":{"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":-1},` +
				`"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":200000}`),
			wantLevel:        "info",
			wantReason:       reasonHit,
			wantWindow:       windowWeekly,
			wantResetsInSecs: 200000,
		},
		{
			name:       "an unparsable resets_at is not a hit",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_at":"soon"`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
			wantWindow: windowWeekly,
		},
		{
			name:       "disabled plugin ignores exhaustion",
			configYAML: "enabled: false\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "debug",
			wantReason: reasonDisabled,
			wantWindow: windowWeekly,
		},
		{
			name:       "included by auth id",
			configYAML: "enabled: true\ninclude_credentials:\n  - codex-a@example.com.json\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "info",
			wantReason: reasonHit,
			wantWindow: windowWeekly,
		},
		{
			name:       "included by auth file name",
			configYAML: "enabled: true\ninclude_credentials:\n  - codex-a@example.com.json\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.AuthID = "codex/codex-a@example.com.json"
				return record
			}(),
			wantLevel:  "info",
			wantReason: reasonHit,
			wantWindow: windowWeekly,
		},
		{
			name:       "included by auth index",
			configYAML: "enabled: true\ninclude_credentials:\n  - c1f0a9\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "info",
			wantReason: reasonHit,
			wantWindow: windowWeekly,
		},
		{
			name:       "a blank include entry never matches",
			configYAML: "enabled: true\ninclude_credentials:\n  - \"\"\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "debug",
			wantReason: reasonNotIncluded,
			wantWindow: windowWeekly,
		},
		{
			name:       "non-codex provider is ignored",
			configYAML: includedConfigYAML,
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Provider = "openai"
				return record
			}(),
		},
		{
			name:       "api key credential is ignored",
			configYAML: includedConfigYAML,
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.AuthType = "api_key"
				return record
			}(),
		},
		{
			name:       "successful record is ignored",
			configYAML: includedConfigYAML,
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Failed = false
				return record
			}(),
		},
		{
			name:       "non-429 failure is ignored",
			configYAML: includedConfigYAML,
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Failure.StatusCode = 500
				return record
			}(),
		},
		{
			name:       "other error type is ignored",
			configYAML: includedConfigYAML,
			record:     usageRecord(quotaBody(true, `"type":"rate_limit_error","resets_in_seconds":200000`)),
		},
		{
			name:       "unparsable body is ignored",
			configYAML: includedConfigYAML,
			record:     usageRecord("upstream 429 without a json body"),
		},
		{
			name:       "empty body is ignored",
			configYAML: includedConfigYAML,
			record:     usageRecord(""),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := useFakeClock(t, testClock)
			fake := newFakeHost()
			registerConfig(t, fake, test.configYAML)
			fake.clearLogs()

			record := test.record
			if test.bodyAt != nil {
				record.Failure.Body = test.bodyAt(clock.now())
			}
			sendUsage(t, fake, record)

			logs := fake.logged()
			if test.wantLevel == "" {
				if len(logs) != 0 {
					t.Fatalf("logs = %+v, want the record ignored without logging", logs)
				}
				if got := fake.authGetCalls(); len(got) != 0 {
					t.Errorf("auth.get calls = %v, want none", got)
				}
				return
			}
			// A hit also produces reset-flow logs on the dispatched goroutine, so the
			// verdict is the record carrying the expected reason.
			matched := logsWithMessage(logs, test.wantReason)
			if len(matched) != 1 {
				t.Fatalf("logs with reason %q = %+v, want exactly one (all logs: %+v)", test.wantReason, matched, logs)
			}
			entry := matched[0]
			if entry.Level != test.wantLevel {
				t.Errorf("level = %q, want %q", entry.Level, test.wantLevel)
			}
			for _, field := range []string{"plugin", "auth_id", "auth_index", "model", "resets_in_seconds", "window"} {
				if _, okField := entry.Fields[field]; !okField {
					t.Errorf("field %q missing from %v", field, entry.Fields)
				}
			}
			if entry.Fields["plugin"] != pluginID {
				t.Errorf("field plugin = %v, want %q", entry.Fields["plugin"], pluginID)
			}
			if test.wantWindow != "" && entry.Fields["window"] != test.wantWindow {
				t.Errorf("field window = %v, want %q", entry.Fields["window"], test.wantWindow)
			}
			if test.wantResetsInSecs != 0 && entry.Fields["resets_in_seconds"] != test.wantResetsInSecs {
				t.Errorf("field resets_in_seconds = %v, want %d", entry.Fields["resets_in_seconds"], test.wantResetsInSecs)
			}

			if test.wantReason == reasonHit {
				// A hit hands the record to the reset flow, which stops at the pre-flight
				// because these cases configure no management key: waiting for its line
				// keeps the case deterministic and pins that nothing is spent.
				waitForLog(t, fake, reasonManagementKeyEmpty)
			}
			if got := fake.requestCount(); got != 0 {
				t.Errorf("http.do calls = %d, want none", got)
			}
			if got := fake.authGetCalls(); len(got) != 0 {
				t.Errorf("auth.get calls = %v, want none", got)
			}
		})
	}
}

// TestDecisionLogNamesItsSources pins the audit trail (spec story 17): the log says
// where the window came from and where the reset timing came from, so a resets_at
// derivation is never reported as an upstream resets_in_seconds.
func TestDecisionLogNamesItsSources(t *testing.T) {
	tests := []struct {
		name       string
		body       func(at time.Time) string
		wantWindow string
		wantResets string
	}{
		{
			name:       "explicit window and resets_in_seconds",
			body:       func(time.Time) string { return hitBody() },
			wantWindow: "limit_window_minutes",
			wantResets: "resets_in_seconds",
		},
		{
			name:       "explicit window and resets_at",
			body:       func(at time.Time) string { return resetsAtBody(at.Add(200000 * time.Second)) },
			wantWindow: "limit_window_minutes",
			wantResets: "resets_at",
		},
		{
			name: "fallback window from resets_at",
			body: func(at time.Time) string {
				return timingOnlyBody(`"resets_at":` + strconv.FormatInt(at.Add(200000*time.Second).Unix(), 10))
			},
			wantWindow: "reset_timing",
			wantResets: "resets_at",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := useFakeClock(t, testClock)
			fake := newFakeHost()
			registerConfig(t, fake, includedConfigYAML)
			sendUsage(t, fake, usageRecord(test.body(clock.now())))
			entry := waitForLog(t, fake, reasonHit)
			if entry.Fields["window_source"] != test.wantWindow {
				t.Errorf("window_source = %v, want %q", entry.Fields["window_source"], test.wantWindow)
			}
			if entry.Fields["resets_source"] != test.wantResets {
				t.Errorf("resets_source = %v, want %q", entry.Fields["resets_source"], test.wantResets)
			}
			waitForLog(t, fake, reasonManagementKeyEmpty)
		})
	}
}
