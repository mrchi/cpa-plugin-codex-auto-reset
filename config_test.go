package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func lifecyclePayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string]any{"config_yaml": []byte(configYAML), "schema_version": 6})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}
	return raw
}

// TestRegisterWithoutConfigKeepsTheDefaults registers with every shape of "no plugin
// configuration" and shows the defaults are in effect: the plugin stays enabled, and a
// weekly exhaustion is still classified as a hit. The default configuration carries no
// management key, so the flow stops at the pre-flight and spends nothing — the
// operator has to configure the key before auto-reset can do anything.
func TestRegisterWithoutConfigKeepsTheDefaults(t *testing.T) {
	cases := map[string][]byte{
		"no payload":   nil,
		"no yaml":      lifecyclePayload(t, ""),
		"host keys":    lifecyclePayload(t, "enabled: true\npriority: 0\n"),
		"unknown keys": lifecyclePayload(t, "priority: 0\nstore:\n  version: 1.0.0\n"),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := newFakeHost()
			if _, errHandle := handleMethod(fake, pluginabi.MethodPluginRegister, payload); errHandle != nil {
				t.Fatalf("plugin.register: %v", errHandle)
			}
			if logs := fake.logged(); len(logs) != 0 {
				t.Fatalf("logs = %+v, want no warnings", logs)
			}
			sendUsage(t, fake, hitRecord())
			matched := logsWithMessage(fake.logged(), reasonHit)
			if len(matched) != 1 || matched[0].Level != "info" {
				t.Fatalf("hit logs = %+v, want one info line: the default is enabled", matched)
			}
			// The hit dispatches the reset flow, which stops at the pre-flight because
			// the default configuration carries no management key: waiting for its line
			// keeps this case deterministic and pins that no credit can be spent.
			waitForLog(t, fake, reasonManagementKeyEmpty)
			if got := fake.requestCount(); got != 0 {
				t.Errorf("http.do calls = %v, want none without a management key", fake.callTrace())
			}
		})
	}
}

// TestRegisterDisabledIgnoresExhaustion covers the global switch.
func TestRegisterDisabledIgnoresExhaustion(t *testing.T) {
	useFakeClock(t, testClock)
	fake := newFakeHost()
	registerConfig(t, fake, "enabled: false\nmanagement_key: secret-key\n")

	sendUsage(t, fake, hitRecord())

	matched := logsWithMessage(fake.logged(), reasonDisabled)
	if len(matched) != 1 || matched[0].Level != "debug" {
		t.Fatalf("disabled logs = %+v, want one debug line", matched)
	}
	if got := fake.authGetCalls(); len(got) != 0 {
		t.Errorf("auth.get calls = %v, want none", got)
	}
}

// TestExcludeCredentialsSkipsMatchingCredential covers the per-credential exclusion
// list, matched against the auth id, its file name and the runtime auth index.
func TestExcludeCredentialsSkipsMatchingCredential(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		authID     string
		wantReason string
	}{
		{
			name:       "excluded by auth id",
			configYAML: "enabled: true\nexclude_credentials:\n  - codex-a@example.com.json\n",
			authID:     "codex-a@example.com.json",
			wantReason: reasonExcluded,
		},
		{
			name:       "excluded by auth file name",
			configYAML: "enabled: true\nexclude_credentials:\n  - codex-a@example.com.json\n",
			authID:     "codex/codex-a@example.com.json",
			wantReason: reasonExcluded,
		},
		{
			name:       "excluded by auth index",
			configYAML: "enabled: true\nexclude_credentials:\n  - c1f0a9\n",
			authID:     "codex-a@example.com.json",
			wantReason: reasonExcluded,
		},
		{
			name:       "a blank entry never excludes",
			configYAML: "enabled: true\nmanagement_key: secret-key\nexclude_credentials:\n  - \"  \"\n",
			authID:     "codex-a@example.com.json",
			wantReason: reasonHit,
		},
		{
			name:       "an absent list never excludes",
			configYAML: "enabled: true\nmanagement_key: secret-key\n",
			authID:     "codex-a@example.com.json",
			wantReason: reasonHit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := newFakeHost()
			registerConfig(t, fake, test.configYAML)

			record := hitRecord()
			record.AuthID = test.authID
			sendUsage(t, fake, record)

			if got := logsWithMessage(fake.logged(), test.wantReason); len(got) != 1 {
				t.Fatalf("logs with %q = %+v (all logs %+v), want one", test.wantReason, got, fake.logged())
			}
			if test.wantReason == reasonExcluded {
				if got := fake.authGetCalls(); len(got) != 0 {
					t.Errorf("auth.get calls = %v, want none", got)
				}
				return
			}
			waitForLog(t, fake, reasonAuthUnreadable)
		})
	}
}

// TestConfiguredManagementEndpointIsUsed covers management_key and
// management_base_url: both must reach the cooldown-clear request.
func TestConfiguredManagementEndpointIsUsed(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	configuredURL := "http://127.0.0.1:9000"
	fake.script(http.MethodPost, configuredURL+managementResetQuotaPath, http.StatusOK, `{"status":"ok"}`)
	registerConfig(t, fake, "enabled: true\nmanagement_key: secret-key\nmanagement_base_url: "+configuredURL+"/\n")

	sendUsage(t, fake, hitRecord())
	waitForLog(t, fake, reasonCooldownCleared)

	quota := fake.requestsFor(http.MethodPost, configuredURL+managementResetQuotaPath)
	if len(quota) != 1 {
		t.Fatalf("cooldown-clear requests to the configured endpoint = %d, want one", len(quota))
	}
	assertHeader(t, quota[0], "Authorization", "Bearer secret-key")
}

// TestBlankManagementBaseURLFallsBackToDefault covers the documented default.
func TestBlankManagementBaseURLFallsBackToDefault(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	registerConfig(t, fake, "enabled: true\nmanagement_key: secret-key\nmanagement_base_url: \"  \"\n")

	sendUsage(t, fake, hitRecord())
	waitForLog(t, fake, reasonCooldownCleared)

	if got := fake.requestsFor(http.MethodPost, quotaURL); len(got) != 1 {
		t.Fatalf("cooldown-clear requests to the default endpoint = %d, want one", len(got))
	}
}

// TestInvalidConfigWarnsAndKeepsTheDefaults covers D9: a bad lifecycle payload or a
// bad YAML document must not fail the registration, and the plugin keeps working with
// the defaults.
func TestInvalidConfigWarnsAndKeepsTheDefaults(t *testing.T) {
	tests := []struct {
		name    string
		payload func(t *testing.T) []byte
	}{
		{
			name:    "an unparsable lifecycle request",
			payload: func(*testing.T) []byte { return []byte("not a lifecycle request") },
		},
		{
			name: "an unparsable config document",
			payload: func(t *testing.T) []byte {
				return lifecyclePayload(t, "enabled: false\nmanagement_key: [unterminated\n")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := newFakeHost()
			raw, errHandle := handleMethod(fake, pluginabi.MethodPluginRegister, test.payload(t))
			if errHandle != nil {
				t.Fatalf("plugin.register: %v", errHandle)
			}
			decodeResult(t, raw)
			logs := fake.logged()
			if len(logs) != 1 || logs[0].Level != "warn" {
				t.Fatalf("logs = %+v, want exactly one warning", logs)
			}

			// The defaults are in effect: the plugin is enabled, so the record is a hit
			// and the reset flow starts — then stops at the missing management key
			// without touching upstream.
			fake.clearLogs()
			sendUsage(t, fake, hitRecord())
			if got := logsWithMessage(fake.logged(), reasonHit); len(got) != 1 {
				t.Fatalf("hit logs = %+v, want the default enabled plugin to hit", got)
			}
			waitForLog(t, fake, reasonManagementKeyEmpty)
			if got := fake.requestCount(); got != 0 {
				t.Errorf("http.do calls = %v, want none without a management key", fake.callTrace())
			}
		})
	}
}
