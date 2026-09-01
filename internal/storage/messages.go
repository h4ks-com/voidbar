package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// MsgBufferCap is the replay ring size kept per channel.
const MsgBufferCap = 500

// BufferedMessage is the persisted form of a relayed message. It stores the
// fields the Discord payload shape needs; everything else is rebuilt at
// read time so the wire format can evolve without migrations.
// MentionRef is one mentioned user of a buffered message.
type MentionRef struct {
	Nick string // live IRC nick at send time
	ID   string // Discord user id the marker used
}

type BufferedMessage struct {
	ID         string `json:"id"`
	ChannelID  string `json:"channel_id"`
	AuthorID   string `json:"author_id"` // user id for own sends, "irc:<nick>" for relays
	AuthorName string `json:"author_name"`
	Content    string `json:"content"`
	Nonce      any    `json:"nonce,omitempty"`
	Timestamp  string `json:"timestamp"` // RFC3339
	Type       int    `json:"type"`
	// MsgID is the upstream IRC msgid of this message (msgid-expriment
	// networks), persisted so reactions can anchor to messages that
	// predate a bouncer restart.
	MsgID string `json:"irc_msgid,omitempty"`
	// Reactions is emoji -> reacting user ids, persisted on every change so
	// pills survive bouncer restarts (the live-wire msgid registry cannot).
	Reactions map[string][]string `json:"reactions,omitempty"`
	// Attachments/Embeds are the ready-made Discord payload fragments
	// (upload rows, synthesized image-link embeds) so history renders
	// them identically to live dispatches.
	Attachments []any `json:"attachments,omitempty"`
	Embeds      []any `json:"embeds,omitempty"`
	// Mentions holds the markerized message's mentioned users (nick for
	// fact lookups, the exact Discord id the pill used), so history
	// fetches can rebuild the mentions array and upsert peer users the
	// client may not have seen this session.
	Mentions []MentionRef `json:"mention_refs,omitempty"`
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

func msgidKey(networkID, msgid string) []byte {
	return []byte("ircmsgid:" + networkID + ":" + msgid)
}

func chatPrefillKey(networkID, ircChannel string) []byte {
	return []byte("chatprefill:" + networkID + ":" + ircChannel)
}

// ChatPrefillDone reports whether the chathistory prefill already completed
// for this channel. Reconnects re-JOIN every channel; without the mark each
// reconnect would re-request and re-deliver the same history batch.
func (s *Storage) ChatPrefillDone(networkID, ircChannel string) bool {
	ok := false
	_ = s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(chatPrefillKey(networkID, ircChannel))
		if err == nil {
			ok = true
		}
		return nil
	})
	return ok
}

// MarkChatPrefill records that prefill completed; the value is the
// completion time, kept for debugging.
func (s *Storage) MarkChatPrefill(networkID, ircChannel string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(chatPrefillKey(networkID, ircChannel),
			[]byte(time.Now().UTC().Format(time.RFC3339)))
	})
}

// SetMessageMsgID binds a buffered message to its upstream IRC msgid and
// indexes it for reverse lookup. Idempotent.
func (s *Storage) SetMessageMsgID(networkID, channelID, id, msgid string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			return err
		}
		var m BufferedMessage
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
			return err
		}
		if m.MsgID != msgid {
			m.MsgID = msgid
			val, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if err := txn.Set(msgKey(channelID, id), val); err != nil {
				return err
			}
		}
		return txn.Set(msgidKey(networkID, msgid), []byte(channelID+"\x00"+id))
	})
}

// SetMessageEmbeds attaches link-preview embeds to a buffered message
// and returns the updated row, so the caller can rebuild the full
// MESSAGE_UPDATE payload (content/author/attachments included).
func (s *Storage) SetMessageEmbeds(channelID, id string, embeds []any) (*BufferedMessage, error) {
	var out *BufferedMessage
	err := s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			return err
		}
		m := &BufferedMessage{}
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, m) }); err != nil {
			return err
		}
		m.Embeds = embeds
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := txn.Set(msgKey(channelID, id), val); err != nil {
			return err
		}
		out = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LookupMessageByMsgID resolves an IRC msgid to (channel, message id).
func (s *Storage) LookupMessageByMsgID(networkID, msgid string) (channelID, messageID string, ok bool) {
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(msgidKey(networkID, msgid))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			if i := bytes.IndexByte(val, 0); i > 0 {
				channelID, messageID, ok = string(val[:i]), string(val[i+1:]), true
			}
			return nil
		})
	})
	return channelID, messageID, ok
}

// MessageMsgID returns the upstream msgid bound to a buffered message, if any.
func (s *Storage) MessageMsgID(channelID, id string) string {
	var msgid string
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			var m BufferedMessage
			if json.Unmarshal(val, &m) == nil {
				msgid = m.MsgID
			}
			return nil
		})
	})
	return msgid
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

// MessageByID returns a buffered message by id.
func (s *Storage) MessageByID(channelID, id string) (BufferedMessage, bool) {
	var m BufferedMessage
	ok := false
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			return nil
		}
		return item.Value(func(val []byte) error {
			if json.Unmarshal(val, &m) == nil {
				ok = true
			}
			return nil
		})
	})
	return m, ok
}

// DeleteMessage removes a buffered message together with its msgid index
// entry, so deleted messages stay gone from replay and can no longer be
// reaction/redaction targets. Unknown ids are a success no-op: callers
// treat a missing message as already deleted.
func (s *Storage) DeleteMessage(networkID, channelID, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			if err == badger.ErrKeyNotFound {
				return nil
			}
			return err
		}
		var m BufferedMessage
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
			return err
		}
		if m.MsgID != "" {
			if err := txn.Delete(msgidKey(networkID, m.MsgID)); err != nil {
				return err
			}
		}
		return txn.Delete(msgKey(channelID, id))
	})
}

// UpdateMessageReactions replaces the reaction set stored on a message
// (emoji -> user ids). Read-modify-write of the whole blob: reaction
// traffic is tiny next to message writes, and no schema migration is
// needed. A trimmed or unknown message is a no-op error for the caller
// to log.
func (s *Storage) UpdateMessageReactions(channelID, id string, byEmoji map[string][]string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(msgKey(channelID, id))
		if err != nil {
			return err
		}
		var m BufferedMessage
		if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &m) }); err != nil {
			return err
		}
		if len(byEmoji) == 0 {
			m.Reactions = nil
		} else {
			m.Reactions = byEmoji
		}
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return txn.Set(msgKey(channelID, id), val)
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
