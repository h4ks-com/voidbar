package storage

import (
	"bytes"
	"encoding/json"

	badger "github.com/dgraph-io/badger/v4"
)

// Read state: one channel's read position per user - the last message
// acknowledged (the marker the client jumps to) plus the pending-mention
// badge count. Rows are created lazily: an absent row reads as "everything
// read", so only channels someone actually marked (or was mentioned in)
// carry state. This is what survives client restarts - the client tracks
// read state live itself, but its store is born empty on every launch and
// only READY read_state rehydrates it.

// ReadState is the stored row; the discord id lives in the key.
type ReadState struct {
	ChannelID     string `json:"channel_id"`
	LastMessageID string `json:"last_message_id"`
	MentionCount  int    `json:"mention_count"`
}

func readStateKey(userID, channelID string) []byte {
	return []byte("rs/" + userID + "/" + channelID)
}

// PutReadState upserts one row.
func (s *Storage) PutReadState(userID string, rs ReadState) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(readStateKey(userID, rs.ChannelID), b)
	})
}

// GetReadState returns one row (ok=false when absent).
func (s *Storage) GetReadState(userID, channelID string) (ReadState, bool) {
	var rs ReadState
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(readStateKey(userID, channelID))
		if err != nil {
			return err
		}
		return item.Value(func(b []byte) error {
			return json.Unmarshal(b, &rs)
		})
	})
	return rs, err == nil
}

// ReadStates lists every row the user has.
func (s *Storage) ReadStates(userID string) ([]ReadState, error) {
	prefix := []byte("rs/" + userID + "/")
	var out []ReadState
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var rs ReadState
			if err := item.Value(func(b []byte) error {
				return json.Unmarshal(b, &rs)
			}); err != nil {
				return err
			}
			rs.ChannelID = string(bytes.TrimPrefix(item.KeyCopy(nil), prefix))
			out = append(out, rs)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
