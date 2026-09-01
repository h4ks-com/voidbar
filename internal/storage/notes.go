package storage

import (
	"bytes"
	"encoding/json"

	badger "github.com/dgraph-io/badger/v4"
)

// User notes ("Add Note" in the client's profile sheet): one short
// private note per (owner, target) pair. Notes target any user-shaped id
// the client shows - bouncer members and hashed IRC author ids alike -
// so they are stored as opaque strings.

func userNoteKey(userID, targetID string) []byte {
	return []byte("note/" + userID + "/" + targetID)
}

// PutUserNote stores a note (an empty note deletes it, per the
// Modify User Note contract).
func (s *Storage) PutUserNote(userID, targetID, note string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		if note == "" {
			return txn.Delete(userNoteKey(userID, targetID))
		}
		b, err := json.Marshal(note)
		if err != nil {
			return err
		}
		return txn.Set(userNoteKey(userID, targetID), b)
	})
}

// GetUserNote returns the note for a target ("" when there is none).
func (s *Storage) GetUserNote(userID, targetID string) (string, error) {
	var note string
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(userNoteKey(userID, targetID))
		if err != nil {
			return err
		}
		return item.Value(func(b []byte) error {
			return json.Unmarshal(b, &note)
		})
	})
	if err == badger.ErrKeyNotFound {
		return "", nil
	}
	return note, err
}

// ListUserNotes returns all of a user's notes as {target id: note}.
func (s *Storage) ListUserNotes(userID string) (map[string]string, error) {
	prefix := []byte("note/" + userID + "/")
	notes := map[string]string{}
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			target := string(bytes.TrimPrefix(item.KeyCopy(nil), prefix))
			var note string
			if err := item.Value(func(b []byte) error {
				return json.Unmarshal(b, &note)
			}); err != nil {
				return err
			}
			notes[target] = note
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}
