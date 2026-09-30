package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// registryEntry models the plugin store entry fields this repo authors. The store takes
// the installed version from the release tag, so Version here is display-only, but the
// test below still holds it level with the plugin's own.
type registryEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Author      string `json:"author"`
	Version     string `json:"version"`
	Repository  string `json:"repository"`
}

// TestRegistryMatchesPlugin pins registry.json to the plugin's own identity. The store
// installs by id and fetches the release from repository, so a drifted id or repo
// breaks panel installs in a way nothing else in the repo would catch.
func TestRegistryMatchesPlugin(t *testing.T) {
	raw, errRead := os.ReadFile("registry.json")
	if errRead != nil {
		t.Fatalf("read registry.json: %v", errRead)
	}
	var registry struct {
		SchemaVersion int             `json:"schema_version"`
		Plugins       []registryEntry `json:"plugins"`
	}
	if errUnmarshal := json.Unmarshal(raw, &registry); errUnmarshal != nil {
		t.Fatalf("parse registry.json: %v", errUnmarshal)
	}
	if registry.SchemaVersion == 0 {
		t.Error("registry.json has no schema_version")
	}
	if len(registry.Plugins) != 1 {
		t.Fatalf("registry.json carries %d plugins, want 1", len(registry.Plugins))
	}
	entry := registry.Plugins[0]
	if entry.ID != pluginID {
		t.Errorf("registry id = %q, want plugin id %q", entry.ID, pluginID)
	}
	if entry.Repository != pluginRepo {
		t.Errorf("registry repository = %q, want %q", entry.Repository, pluginRepo)
	}
	// The listing's version is display-only, but it must not lag the plugin's own: a
	// bump that misses this file is a silent staleness the panel would show for good.
	if entry.Version != pluginVersion {
		t.Errorf("registry version = %q, want pluginVersion %q", entry.Version, pluginVersion)
	}
	required := map[string]string{
		"id":          entry.ID,
		"name":        entry.Name,
		"description": entry.Description,
		"author":      entry.Author,
		"repository":  entry.Repository,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			t.Errorf("registry field %s is empty; the plugin store rejects such an entry", field)
		}
	}
}
