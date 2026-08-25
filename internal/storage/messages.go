package storage

import (
	"encoding/json"
	"fmt"
	"strconv"

	badger "github.com/dgraph-io/badger/v4"
)

// MsgBufferCap is the replay ring size kept per channel.
const MsgBufferCap = 500

// BufferedMessage is the persisted form of a relayed message. It stores the
// fields the Discord payload shape needs; everything else is rebuilt at
// read time so the wire format can evolve without migrations.
type BufferedMessage struct {
	ID         string `json:"id"`
	ChannelID  string `json:"channel_id"`
	AuthorID   string `json:"author_id"` // user id for own sends, "irc:<nick>" for relays
	AuthorName string `json:"author_name"`
	Content    string `json:"content"`
	Nonce      any    `json:"nonce,omitempty"`
	Timestamp  string `json:"timestamp"` // RFC3339
	Type       int    `json:"type"`
}

// padID renders a message id so that lexicographic order matches id order:
// snowflake ids are decimal strings of varying length, so numeric ones are
// zero-padded to 20 chars (uint64 max has 20 digits); anything non-numeric
// sorts after all of them.
func padID(id string) string {
	if n, err := strconv.ParseUint(id, 10, 64); err == nil {
		return fmt.Sprintf("%020d", n)
	}
	return "z" + id
}

func msgKey(channelID, id string) []byte {
	return []byte("msg:" + channelID + ":" + padID(id))
}

func msgPrefix(channelID string) []byte {
	return []byte("msg:" + channelID + ":")
}

// AppendMessage stores a message and trims the channel ring to
// MsgBufferCap. Errors are non-fatal (the live relay still works); callers
// log them.
func (s *Storage) AppendMessage(m BufferedMessage) error {
	val, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(msgKey(m.ChannelID, m.ID), val); err != nil {
			return err
		}
		// Trim: keys-only scan over at most cap+1 entries.
		prefix := msgPrefix(m.ChannelID)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		var keys [][]byte
		for it.Rewind(); it.ValidForPrefix(prefix); it.Next() {
			keys = append(keys, append([]byte(nil), it.Item().Key()...))
		}
		it.Close()
		for i := 0; i < len(keys)-MsgBufferCap; i++ {
			if err := txn.Delete(keys[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChannelMessages returns buffered messages of a channel. Semantics follow
// Discord REST: with after set, ids strictly greater than after in
// ascending order; otherwise ids strictly less than before (or all) in
// descending order.
func (s *Storage) ChannelMessages(channelID, before, after string, limit int) []BufferedMessage {
	out := make([]BufferedMessage, 0, limit)
	_ = s.db.View(func(txn *badger.Txn) error {
		prefix := msgPrefix(channelID)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		if after == "" {
			opts.Reverse = true
		}
		it := txn.NewIterator(opts)
		defer it.Close()

		if after == "" {
			// Descending from before (exclusive) or the newest message.
			// In reverse mode Seek positions at the largest key <= start.
			start := append([]byte(nil), prefix...)
			if before != "" {
				start = append(start, padID(before)...)
			} else {
				start = append(start, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
					0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
			}
			exclude := string(msgKey(channelID, before))
			for it.Seek(start); it.ValidForPrefix(prefix); it.Next() {
				if before != "" && string(it.Item().Key()) == exclude {
					continue
				}
				if m, ok := decodeItem(it.Item()); ok {
					out = append(out, m)
				}
				if len(out) >= limit {
					break
				}
			}
		} else {
			// Ascending after (exclusive).
			exclude := msgKey(channelID, after)
			for it.Seek(exclude); it.ValidForPrefix(prefix); it.Next() {
				if string(it.Item().Key()) == string(exclude) {
					continue
				}
				if m, ok := decodeItem(it.Item()); ok {
					out = append(out, m)
				}
				if len(out) >= limit {
					break
				}
			}
		}
		return nil
	})
	return out
}

func decodeItem(item *badger.Item) (BufferedMessage, bool) {
	var m BufferedMessage
	err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &m)
	})
	return m, err == nil
}
