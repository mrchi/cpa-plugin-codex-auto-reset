package main

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	pluginID = "cpa-auto-reset"

	defaultManagementBaseURL = "http://127.0.0.1:8317"
)

// lifecycleRequest is the register/reconfigure payload; ConfigYAML arrives base64
// encoded because it is a []byte (D8).
type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// pluginConfig mirrors plugins.configs.cpa-auto-reset. The host injects `enabled`
// and `priority`; anything else here is plugin-private.
type pluginConfig struct {
	Enabled            bool     `yaml:"enabled"`
	ExcludeCredentials []string `yaml:"exclude_credentials"`
	ManagementKey      string   `yaml:"management_key"`
	ManagementBaseURL  string   `yaml:"management_base_url"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		// The host always writes `enabled`, so a missing key means defaults apply,
		// not that the plugin is off.
		Enabled:           true,
		ManagementBaseURL: defaultManagementBaseURL,
	}
}

// parseConfig decodes the lifecycle request. Bad input falls back to defaults with a
// warning: returning an error rejects the registration, and panicking gets the
// plugin fused (D9).
func parseConfig(h host, request []byte) pluginConfig {
	if len(request) == 0 {
		return defaultConfig()
	}
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		h.log("warn", "invalid lifecycle request, using default config: "+errUnmarshal.Error(), logFields(nil))
		return defaultConfig()
	}
	if len(req.ConfigYAML) == 0 {
		return defaultConfig()
	}
	cfg := defaultConfig()
	if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
		h.log("warn", "invalid config yaml, using default config: "+errUnmarshal.Error(), logFields(nil))
		return defaultConfig()
	}
	cfg.ManagementBaseURL = strings.TrimSpace(cfg.ManagementBaseURL)
	if cfg.ManagementBaseURL == "" {
		cfg.ManagementBaseURL = defaultManagementBaseURL
	}
	return cfg
}

func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultConfig()
}
