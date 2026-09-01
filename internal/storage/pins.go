package storage

import (
	"bytes"
	"sort"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// Channel pins are bouncer-local (IRC has no pin counterpart): the ids
// of pinned replay-buffer messages per channel, with the pin time as
// the value so lists can sort oldest-pin-first like Discord.

// MaxChannelPins matches Discord's documented per-channel pin limit.
const MaxChannelPins = 50

// PinnedMessage is one pin: the message id and when it was pinned.
type PinnedMessage struct {
	ID        string
	PinnedAt time.Time
}

func pinKey(channelID, messageID string) []byte {
	return []byte("pin/" + channelID + "/" + messageID)
}

// PutPin pins a message (idempotent; the original pin time stands).
func (s *Storage) PutPin(channelID, messageID string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		if _, err := txn.Get(pinKey(channelID, messageID)); err == nil {
			return nil
		} else if err != badger.ErrKeyNotFound {
			return err
		}
		b, err := time.Now().UTC().MarshalBinary()
		if err != nil {
			return err
		}
		return txn.Set(pinKey(channelID, messageID), b)
	})
}

// DeletePin unpins a message (idempotent).
func (s *Storage) DeletePin(channelID, messageID string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(pinKey(channelID, messageID))
	})
}

// ListPins returns the channel's pins oldest-pin-first.
func (s *Storage) ListPins(channelID string) ([]PinnedMessage, error) {
	prefix := []byte("pin/" + channelID + "/")
	var pins []PinnedMessage
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			id := string(bytes.TrimPrefix(item.KeyCopy(nil), prefix))
			var at time.Time
			if err := item.Value(func(b []byte) error {
				return at.UnmarshalBinary(b)
			}); err != nil {
				return err
			}
			pins = append(pins, PinnedMessage{ID: id, PinnedAt: at})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].PinnedAt.Before(pins[j].PinnedAt) })
	return pins, nil
}
