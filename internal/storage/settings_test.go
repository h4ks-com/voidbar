package storage

import (
	"encoding/json"
	"testing"
)

func TestSettingsMergePersist(t *testing.T) {
	s := openTest(t)
	if got := s.UserSettings("u1"); len(got) != 0 {
		t.Fatalf("fresh settings not empty: %v", got)
	}
	if err := s.MergeUserSettings("u1", map[string]any{"theme": "dark", "locale": "ru"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MergeUserSettings("u1", map[string]any{"theme": "light", "dismissed": map[string]any{"promo": true}}); err != nil {
		t.Fatal(err)
	}
	got := s.UserSettings("u1")
	if got["theme"] != "light" || got["locale"] != "ru" {
		t.Fatalf("merge wrong: %v", got)
	}
	if d, ok := got["dismissed"].(map[string]any); !ok || d["promo"] != true {
		t.Fatalf("object value lost: %v", got["dismissed"])
	}
	// Users are isolated.
	if got := s.UserSettings("u2"); len(got) != 0 {
		t.Fatalf("isolation broken: %v", got)
	}
}

func TestSettingsSnowflakePrecision(t *testing.T) {
	s := openTest(t)
	// The Android client PATCHes guild ids as JSON numbers; UseNumber at
	// ingress yields json.Number, which must survive a merge/read cycle
	// with exact digits (float64 would round past 2^53).
	patch := map[string]any{
		"guild_folders": []any{
			map[string]any{"id": json.Number("1"), "guild_ids": []any{json.Number("1542540545585315840")}},
		},
	}
	if err := s.MergeUserSettings("u1", patch); err != nil {
		t.Fatal(err)
	}
	got := s.UserSettings("u1")
	folders, ok := got["guild_folders"].([]any)
	if !ok || len(folders) != 1 {
		t.Fatalf("folders lost: %#v", got["guild_folders"])
	}
	ids := folders[0].(map[string]any)["guild_ids"].([]any)
	if len(ids) != 1 || ids[0] != json.Number("1542540545585315840") {
		t.Fatalf("snowflake digits mangled: %#v", ids)
	}
}
