package model

import (
	"encoding/json"
	"testing"
)

func TestSettingsWithDefaultsGuildFolders(t *testing.T) {
	persisted := map[string]any{
		"theme": "light",
		"guild_folders": []any{
			// json.Number keeps the client's exact snowflake digits.
			map[string]any{"id": json.Number("1"), "guild_ids": []any{json.Number("1542540545585315840"), "1542560217756073984"}, "name": "exact"},
			// float64 below 2^53 recovers exactly.
			map[string]any{"id": json.Number("2"), "guild_ids": []any{float64(999)}, "name": "small"},
			// float64 snowflake is mangled past recovery: dropped folder.
			map[string]any{"id": json.Number("3"), "guild_ids": []any{float64(2e18)}, "name": "dropped"},
		},
	}
	out := SettingsWithDefaults(persisted)
	if out["theme"] != "light" {
		t.Fatalf("overlay lost patch: theme=%v", out["theme"])
	}
	folders := out["guild_folders"].([]any)
	if len(folders) != 2 {
		t.Fatalf("want 2 folders, got %d: %#v", len(folders), folders)
	}
	ids := folders[0].(map[string]any)["guild_ids"].([]any)
	if len(ids) != 2 || ids[0] != "1542540545585315840" || ids[1] != "1542560217756073984" {
		t.Fatalf("json.Number snowflake not kept exactly: %v", ids)
	}
	small := folders[1].(map[string]any)["guild_ids"].([]any)
	if len(small) != 1 || small[0] != "999" {
		t.Fatalf("small float64 id not recovered: %v", small)
	}
	if id := folders[0].(map[string]any)["id"]; id != "1" {
		t.Fatalf("folder id not stringified: %v", id)
	}
}

func TestSettingsWithDefaultsEmpty(t *testing.T) {
	out := SettingsWithDefaults(nil)
	if out["theme"] != "dark" {
		t.Fatalf("defaults missing: %v", out["theme"])
	}
	if _, ok := out["guild_folders"].([]any); !ok {
		t.Fatal("guild_folders not an array")
	}
}
