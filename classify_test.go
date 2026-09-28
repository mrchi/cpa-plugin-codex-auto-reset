package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
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
		AuthID:    "codex-a@example.com.json",
		AuthIndex: "c1f0a9",
		Model:     "gpt-5.5",
		Failed:    true,
		Failure:   pluginapi.UsageFailure{StatusCode: 429, Body: body},
	}
}

func TestUsageHandleClassifiesExhaustion(t *testing.T) {
	weeklyBody := quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":200000`)
	weeklyFallbackBody := quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":200000`)

	tests := []struct {
		name       string
		configYAML string
		record     pluginapi.UsageRecord
		wantLevel  string // "" means the record is ignored with no log at all
		wantReason string
	}{
		{
			name:       "weekly window exhaustion is a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "info",
			wantReason: reasonHit,
		},
		{
			name:       "top-level error object is a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(false, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":200000`)),
			wantLevel:  "info",
			wantReason: reasonHit,
		},
		{
			name:       "missing limit_window_minutes falls back to resets_in_seconds",
			configYAML: "enabled: true\n",
			record:     usageRecord(weeklyFallbackBody),
			wantLevel:  "info",
			wantReason: reasonHit,
		},
		{
			name:       "reset exactly one day out is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":86400`)),
			wantLevel:  "debug",
			wantReason: reasonWithinDay,
		},
		{
			name:       "reset just past one day is a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":86401`)),
			wantLevel:  "info",
			wantReason: reasonHit,
		},
		{
			name:       "5-hour window is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":300,"resets_in_seconds":3600`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
		},
		{
			name:       "fallback range without limit_window_minutes is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":3600`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
		},
		{
			name:       "fallback boundary 18000 is not weekly",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","resets_in_seconds":18000`)),
			wantLevel:  "debug",
			wantReason: reasonNotWeekly,
		},
		{
			name:       "weekly window without reset timing is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
		},
		{
			name:       "no window and no reset timing is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached"`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
		},
		{
			name:       "non-positive reset is not a hit",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":-1`)),
			wantLevel:  "debug",
			wantReason: reasonUnknownWindow,
		},
		{
			name:       "disabled plugin ignores exhaustion",
			configYAML: "enabled: false\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "debug",
			wantReason: reasonDisabled,
		},
		{
			name:       "excluded by auth id",
			configYAML: "enabled: true\nexclude_credentials:\n  - codex-a@example.com.json\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "debug",
			wantReason: reasonExcluded,
		},
		{
			name:       "excluded by auth file name",
			configYAML: "enabled: true\nexclude_credentials:\n  - codex-a@example.com.json\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.AuthID = "codex/codex-a@example.com.json"
				return record
			}(),
			wantLevel:  "debug",
			wantReason: reasonExcluded,
		},
		{
			name:       "excluded by auth index",
			configYAML: "enabled: true\nexclude_credentials:\n  - c1f0a9\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "debug",
			wantReason: reasonExcluded,
		},
		{
			name:       "blank exclude entry never matches",
			configYAML: "enabled: true\nexclude_credentials:\n  - \"\"\n",
			record:     usageRecord(weeklyBody),
			wantLevel:  "info",
			wantReason: reasonHit,
		},
		{
			name:       "non-codex provider is ignored",
			configYAML: "enabled: true\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Provider = "openai"
				return record
			}(),
		},
		{
			name:       "api key credential is ignored",
			configYAML: "enabled: true\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.AuthType = "api_key"
				return record
			}(),
		},
		{
			name:       "successful record is ignored",
			configYAML: "enabled: true\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Failed = false
				return record
			}(),
		},
		{
			name:       "non-429 failure is ignored",
			configYAML: "enabled: true\n",
			record: func() pluginapi.UsageRecord {
				record := usageRecord(weeklyBody)
				record.Failure.StatusCode = 500
				return record
			}(),
		},
		{
			name:       "other error type is ignored",
			configYAML: "enabled: true\n",
			record:     usageRecord(quotaBody(true, `"type":"rate_limit_error","resets_in_seconds":200000`)),
		},
		{
			name:       "unparseable body is ignored",
			configYAML: "enabled: true\n",
			record:     usageRecord("upstream 429 without a json body"),
		},
		{
			name:       "empty body is ignored",
			configYAML: "enabled: true\n",
			record:     usageRecord(""),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeHost()
			if _, errRegister := handleMethod(fake, pluginabi.MethodPluginRegister, lifecyclePayload(t, test.configYAML)); errRegister != nil {
				t.Fatalf("register: %v", errRegister)
			}
			fake.logs = nil

			raw, errHandle := handleMethod(fake, pluginabi.MethodUsageHandle, marshalRecord(t, test.record))
			if errHandle != nil {
				t.Fatalf("usage.handle: %v", errHandle)
			}
			if result := string(decodeResult(t, raw)); result != "{}" {
				t.Errorf("result = %s, want {}", result)
			}

			logs := fake.logged()
			if test.wantLevel == "" {
				if len(logs) != 0 {
					t.Fatalf("logs = %+v, want the record ignored without logging", logs)
				}
			} else {
				if len(logs) != 1 {
					t.Fatalf("logs = %+v, want exactly one", logs)
				}
				if logs[0].Level != test.wantLevel {
					t.Errorf("level = %q, want %q", logs[0].Level, test.wantLevel)
				}
				if logs[0].Message != test.wantReason {
					t.Errorf("message = %q, want %q", logs[0].Message, test.wantReason)
				}
				for _, field := range []string{"plugin", "auth_id", "auth_index", "model", "resets_in_seconds", "window"} {
					if _, okField := logs[0].Fields[field]; !okField {
						t.Errorf("field %q missing from %v", field, logs[0].Fields)
					}
				}
				if logs[0].Fields["plugin"] != pluginID {
					t.Errorf("field plugin = %v, want %q", logs[0].Fields["plugin"], pluginID)
				}
			}

			// Classification is observation only: no reset work happens here (ticket 03).
			if calls := fake.authGetCalls(); len(calls) != 0 {
				t.Errorf("auth.get calls = %v, want none", calls)
			}
			if calls := fake.requests(); len(calls) != 0 {
				t.Errorf("http.do calls = %+v, want none", calls)
			}
		})
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
