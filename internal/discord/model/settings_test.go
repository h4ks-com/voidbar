package model

import "testing"

func TestSettingsWithDefaultsGuildFolders(t *testing.T) {
	persisted := map[string]any{
		"theme": "light",
		"guild_folders": []any{
			map[string]any{"id": float64(1), "guild_ids": []any{float64(1542540545585315840), "1542560217756073984"}, "name": "kept"},
			map[string]any{"id": float64(2), "guild_ids": []any{float64(999)}, "name": "dropped"},
		},
	}
	out := SettingsWithDefaults(persisted)
	if out["theme"] != "light" {
		t.Fatalf("overlay lost patch: theme=%v", out["theme"])
	}
	folders := out["guild_folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(folders))
	}
	ids := folders[0].(map[string]any)["guild_ids"].([]any)
	if len(ids) != 1 || ids[0] != "1542560217756073984" {
		t.Fatalf("numeric snowflake not dropped: %v", ids)
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
