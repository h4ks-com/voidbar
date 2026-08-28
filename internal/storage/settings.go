package storage

import (
	"bytes"
	"encoding/json"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
)

func settingsKey(userID string) []byte {
	return []byte("settings:" + userID)
}

// decodeSettings parses stored settings with UseNumber: snowflake ids the
// Android client PATCHes as JSON numbers must keep their exact digits, or
// every id above 2^53 is silently rounded and folders stop matching the
// client's local state (its settings store then re-syncs in a loop).
func decodeSettings(val []byte, out *map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(val))
	dec.UseNumber()
	return dec.Decode(out)
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
			return decodeSettings(val, &out)
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
				return decodeSettings(val, &current)
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
