package storage

import (
	"encoding/json"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
)

func settingsKey(userID string) []byte {
	return []byte("settings:" + userID)
}

// UserSettings returns the persisted legacy client settings (the
// /users/@me/settings store). Result is never nil.
func (s *Storage) UserSettings(userID string) map[string]any {
	out := map[string]any{}
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(settingsKey(userID))
		if err != nil {
			return nil
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &out)
		})
	})
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// MergeUserSettings applies a PATCH body to the persisted settings.
// Top-level keys from patch replace existing ones; everything else is kept
// (the client sends only the changed top-level fields per request).
func (s *Storage) MergeUserSettings(userID string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	return s.db.Update(func(txn *badger.Txn) error {
		current := map[string]any{}
		if item, err := txn.Get(settingsKey(userID)); err == nil {
			_ = item.Value(func(val []byte) error {
				return json.Unmarshal(val, &current)
			})
		}
		for k, v := range patch {
			current[k] = v
		}
		val, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("marshal settings: %w", err)
		}
		return txn.Set(settingsKey(userID), val)
	})
}
