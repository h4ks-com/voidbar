package storage

import "testing"

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
