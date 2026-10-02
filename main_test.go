package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func decodeResult(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw %s)", errUnmarshal, raw)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	return env.Result
}

func TestLifecycleReturnsRegistration(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		t.Run(method, func(t *testing.T) {
			fake := newFakeHost()
			raw, errHandle := handleMethod(fake, method, lifecyclePayload(t, "enabled: true\n"))
			if errHandle != nil {
				t.Fatalf("handleMethod: %v", errHandle)
			}

			var result struct {
				SchemaVersion uint32             `json:"schema_version"`
				Metadata      pluginapi.Metadata `json:"metadata"`
				Capabilities  map[string]any     `json:"capabilities"`
			}
			if errUnmarshal := json.Unmarshal(decodeResult(t, raw), &result); errUnmarshal != nil {
				t.Fatalf("decode result: %v", errUnmarshal)
			}
			if result.SchemaVersion != 1 {
				t.Errorf("schema_version = %d, want 1", result.SchemaVersion)
			}
			// Every one of these is required for the host to accept the plugin.
			for name, value := range map[string]string{
				"Name":             result.Metadata.Name,
				"Version":          result.Metadata.Version,
				"Author":           result.Metadata.Author,
				"GitHubRepository": result.Metadata.GitHubRepository,
			} {
				if strings.TrimSpace(value) == "" {
					t.Errorf("metadata %s is empty", name)
				}
			}
			if result.Metadata.Name != pluginID {
				t.Errorf("metadata Name = %q, want %q", result.Metadata.Name, pluginID)
			}
			// ConfigFields drive the management panel's config form (ADR-0005), the only
			// presentation mechanism the host offers. Pin presence, order and types so a
			// refactor of registration cannot silently drop the panel's config items.
			wantFields := []struct {
				name          string
				typ           pluginapi.ConfigFieldType
				wantSubstring string // a substring pinning the field's semantics
			}{
				{"management_key", pluginapi.ConfigFieldTypeString, "credit"},
				{"management_base_url", pluginapi.ConfigFieldTypeString, "8317"},
				{"include_credentials", pluginapi.ConfigFieldTypeArray, "auth_index"},
			}
			if len(result.Metadata.ConfigFields) != len(wantFields) {
				t.Fatalf("ConfigFields = %+v, want %d fields", result.Metadata.ConfigFields, len(wantFields))
			}
			for i, want := range wantFields {
				field := result.Metadata.ConfigFields[i]
				if field.Name != want.name {
					t.Errorf("ConfigFields[%d].Name = %q, want %q", i, field.Name, want.name)
				}
				if field.Type != want.typ {
					t.Errorf("ConfigFields[%d] (%s) Type = %q, want %q", i, field.Name, field.Type, want.typ)
				}
				if strings.TrimSpace(field.Description) == "" {
					t.Errorf("ConfigFields[%d] (%s) has no description", i, field.Name)
				}
				if !strings.Contains(field.Description, want.wantSubstring) {
					t.Errorf("ConfigFields[%d] (%s) description %q does not mention %q", i, field.Name, field.Description, want.wantSubstring)
				}
				// Descriptions are user-facing and the repo documents Chinese for
				// user-facing prose, so pin non-ASCII rather than trusting review.
				if !strings.ContainsFunc(field.Description, func(r rune) bool { return r > unicode.MaxASCII }) {
					t.Errorf("ConfigFields[%d] (%s) description is not Chinese: %q", i, field.Name, field.Description)
				}
				if len(field.EnumValues) != 0 {
					t.Errorf("ConfigFields[%d] (%s) has enum values, want none", i, field.Name)
				}
			}
			// enabled/priority are host-injected and carry their own panel switch, so
			// declaring them here would create a second, conflicting control.
			for _, field := range result.Metadata.ConfigFields {
				if field.Name == "enabled" || field.Name == "priority" {
					t.Errorf("ConfigFields must not declare host-injected key %q", field.Name)
				}
			}
			for capability, enabled := range result.Capabilities {
				if capability != "usage_plugin" {
					t.Errorf("unexpected capability %q", capability)
				}
				if !enabled.(bool) {
					t.Errorf("capability %q not enabled", capability)
				}
			}
			if len(result.Capabilities) != 1 {
				t.Errorf("capabilities = %v, want only usage_plugin", result.Capabilities)
			}
		})
	}
}

func TestRegisterSurvivesBrokenConfig(t *testing.T) {
	fake := newFakeHost()
	raw, errHandle := handleMethod(fake, pluginabi.MethodPluginRegister, lifecyclePayload(t, "enabled: [broken\n"))
	if errHandle != nil {
		t.Fatalf("handleMethod: %v", errHandle)
	}
	decodeResult(t, raw)
	// The broken config warns, then the defaults it falls back to warn again: enabled
	// with no included credentials.
	if logs := fake.logged(); len(logs) != 2 || logs[0].Level != "warn" || logs[1].Level != "warn" {
		t.Fatalf("logs = %+v, want two warnings", logs)
	}
}

func TestUnknownMethodIsAnErrorEnvelope(t *testing.T) {
	raw, errHandle := handleMethod(newFakeHost(), "plugin.quiesce", []byte("{}"))
	if errHandle != nil {
		t.Fatalf("handleMethod: %v", errHandle)
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	if env.OK {
		t.Fatalf("envelope ok = true, want false (%s)", raw)
	}
	if env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("error = %+v, want code unknown_method", env.Error)
	}
}

func TestShutdownSucceeds(t *testing.T) {
	raw, errHandle := handleMethod(newFakeHost(), pluginabi.MethodPluginShutdown, nil)
	if errHandle != nil {
		t.Fatalf("handleMethod: %v", errHandle)
	}
	decodeResult(t, raw)
}
