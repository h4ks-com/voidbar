package storage

import (
	badger "github.com/dgraph-io/badger/v4"
)

// settingsProtoKey namespaces the protobuf settings store per kind: the
// client addresses it by type (1 = preloaded, 2 = frecency) in the URL.
func settingsProtoKey(userID, kind string) []byte {
	return []byte("settingsproto:" + kind + ":" + userID)
}

// UserSettingsProto returns the persisted serialized settings protobuf for
// the kind, or nil when never written.
func (s *Storage) UserSettingsProto(userID, kind string) []byte {
	var out []byte
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(settingsProtoKey(userID, kind))
		if err != nil {
			return nil
		}
		return item.Value(func(val []byte) error {
			out = append([]byte(nil), val...)
			return nil
		})
	})
	return out
}

// SetUserSettingsProto stores the merged serialized settings protobuf for
// the kind.
func (s *Storage) SetUserSettingsProto(userID, kind string, blob []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(settingsProtoKey(userID, kind), blob)
	})
}
