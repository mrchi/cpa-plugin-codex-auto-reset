package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func lifecyclePayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML), SchemaVersion: 6})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}
	return raw
}

func TestParseConfigDefaults(t *testing.T) {
	cases := map[string][]byte{
		"no payload":   nil,
		"no yaml":      lifecyclePayload(t, ""),
		"host keys":    lifecyclePayload(t, "enabled: true\npriority: 0\n"),
		"unknown keys": lifecyclePayload(t, "priority: 0\nstore:\n  version: 1.0.0\n"),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeHost()
			cfg := parseConfig(fake, payload)
			want := defaultConfig()
			if !reflect.DeepEqual(cfg, want) {
				t.Fatalf("config = %+v, want %+v", cfg, want)
			}
			if logs := fake.logged(); len(logs) != 0 {
				t.Fatalf("logged %+v, want no warnings", logs)
			}
		})
	}
}

func TestParseConfigValues(t *testing.T) {
	fake := newFakeHost()
	cfg := parseConfig(fake, lifecyclePayload(t, strings.TrimSpace(`
enabled: false
priority: 0
exclude_credentials:
  - codex-a@example.com.json
  - c1f0a9index
management_key: secret-key
management_base_url: http://127.0.0.1:9000/
`)))

	want := pluginConfig{
		Enabled:            false,
		ExcludeCredentials: []string{"codex-a@example.com.json", "c1f0a9index"},
		ManagementKey:      "secret-key",
		ManagementBaseURL:  "http://127.0.0.1:9000/",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

func TestParseConfigBlankBaseURLFallsBackToDefault(t *testing.T) {
	fake := newFakeHost()
	cfg := parseConfig(fake, lifecyclePayload(t, "enabled: true\nmanagement_base_url: \"  \"\n"))
	if cfg.ManagementBaseURL != defaultManagementBaseURL {
		t.Fatalf("ManagementBaseURL = %q, want %q", cfg.ManagementBaseURL, defaultManagementBaseURL)
	}
}

func TestParseConfigInvalidYAMLUsesDefaults(t *testing.T) {
	fake := newFakeHost()
	cfg := parseConfig(fake, lifecyclePayload(t, "enabled: false\nmanagement_key: [unterminated\n"))

	if !reflect.DeepEqual(cfg, defaultConfig()) {
		t.Fatalf("config = %+v, want defaults %+v", cfg, defaultConfig())
	}
	logs := fake.logged()
	if len(logs) != 1 || logs[0].Level != "warn" {
		t.Fatalf("logs = %+v, want one warning", logs)
	}
}

func TestParseConfigInvalidRequestUsesDefaults(t *testing.T) {
	fake := newFakeHost()
	cfg := parseConfig(fake, []byte("not a lifecycle request"))

	if !reflect.DeepEqual(cfg, defaultConfig()) {
		t.Fatalf("config = %+v, want defaults %+v", cfg, defaultConfig())
	}
	if logs := fake.logged(); len(logs) != 1 || logs[0].Level != "warn" {
		t.Fatalf("logs = %+v, want one warning", logs)
	}
}
