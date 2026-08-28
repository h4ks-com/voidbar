package ircmanage

import (
	"strings"
	"time"

	"github.com/lrstanley/girc"

	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

// chatPrefillLimit is how many messages one prefill asks the upstream for.
const chatPrefillLimit = 50

// chatFrame is one message accumulated from a chathistory batch.
type chatFrame struct {
	target  string
	author  string
	content string
	at      time.Time
	msgid   string
}

// chatBatch accumulates the frames of one in-flight draft/chathistory batch.
// LATEST answers arrive newest-first; frames are re-sorted at flush time.
type chatBatch struct {
	target string // channel the request asked about (batch start params)
	frames []chatFrame
}

// isChatSubcommand reports whether s is a CHATHISTORY subcommand name.
func isChatSubcommand(s string) bool {
	switch s {
	case "LATEST", "BEFORE", "AFTER", "BETWEEN", "AROUND", "TARGETS":
		return true
	}
	return false
}

// maybeChatPrefill fires the one-shot chathistory prefill for a channel we
// just joined: with the draft/chathistory cap negotiated and the persisted
// watermark absent, ask the upstream for the most recent messages so a
// freshly joined channel opens with history instead of an empty screen.
// The watermark is only set when a batch completes: if the request dies with
// the connection, the next JOIN re-asks and the msgid dedup absorbs overlap.
func (m *Manager) maybeChatPrefill(c *conn, ircChannel string) {
	if !strings.HasPrefix(ircChannel, "#") && !strings.HasPrefix(ircChannel, "&") {
		return // channel history only; DM prefill is a separate ask
	}
	if !c.client.HasCapability("draft/chathistory") {
		return
	}
	if m.store.ChatPrefillDone(c.networkID, ircChannel) {
		return
	}
	c.client.Send(&girc.Event{
		Command: "CHATHISTORY",
		Params:  []string{"LATEST", ircChannel, "*", "50"},
	})
	m.log.Info("chathistory prefill requested", "user", c.userID, "network", c.networkID, "channel", ircChannel)
}

// chatBatchControl drives the BATCH open/close envelope. Only
// draft/chathistory batches are tracked; any other batch type (none today)
// passes through untracked. Close frames carry a single param (the ref),
// open frames carry the type and the echoed request after it.
func (m *Manager) chatBatchControl(c *conn, e girc.Event) {
	if len(e.Params) < 1 {
		return
	}
	ref := e.Params[0]
	switch {
	case strings.HasPrefix(ref, "+"):
		if len(e.Params) < 2 {
			return
		}
		batchType := e.Params[1]
		if batchType != "draft/chathistory" && batchType != "chathistory" {
			return
		}
		acc := &chatBatch{}
		// Two start shapes exist in the wild: eris sends
		// BATCH +ref <type> <target>, spec-echoing servers send
		// BATCH +ref <type> <subcmd> <target> ... (e.g. soju). Resolve
		// the target from whichever position carries it; an unrecognized
		// shape (TARGETS has no single target) stays unmarked.
		if len(e.Params) > 3 && isChatSubcommand(e.Params[2]) {
			acc.target = e.Params[3]
		} else if len(e.Params) > 2 {
			acc.target = e.Params[2]
		}
		c.openChatBatch(ref[1:], acc)
	case strings.HasPrefix(ref, "-"):
		if acc := c.closeChatBatch(ref[1:]); acc != nil {
			m.flushChatBatch(c, acc)
		}
	}
}

// chatBatchFrame accumulates one batched PRIVMSG/NOTICE into its batch.
func (m *Manager) chatBatchFrame(c *conn, e girc.Event, ref string) {
	if len(e.Params) == 0 || e.Source == nil {
		return
	}
	at := time.Now().UTC()
	if tag, ok := e.Tags.Get("time"); ok && tag != "" {
		if t := parseChatTime(tag); !t.IsZero() {
			at = t
		}
	}
	msgid, _ := e.Tags.Get("msgid")
	c.appendChatFrame(ref, chatFrame{
		target:  e.Params[0],
		author:  e.Source.Name,
		content: e.Last(),
		at:      at,
		msgid:   msgid,
	})
}

// inChatBatch reports whether the event is a frame of a chathistory batch
// currently being accumulated: those frames are history, not live traffic,
// and must not run the live relay paths.
func (m *Manager) inChatBatch(c *conn, e girc.Event) bool {
	ref, ok := e.Tags.Get("batch")
	if !ok || ref == "" {
		return false
	}
	return c.chatBatchActive(ref)
}

// flushChatBatch applies a completed batch: msgid dedup against everything
// the bouncer ever buffered, then per message a time-anchored snowflake,
// buffer append (server-time timestamp), msgid registration (prefilled
// messages are reactable/deletable immediately) and a MESSAGE_CREATE
// dispatch so attached clients with the channel already open see the
// backlog without reopening.
func (m *Manager) flushChatBatch(c *conn, acc *chatBatch) {
	if acc.target != "" {
		if err := m.store.MarkChatPrefill(c.networkID, acc.target); err != nil {
			m.log.Debug("chathistory watermark persist failed", "err", err, "channel", acc.target)
		}
	}
	if len(acc.frames) == 0 {
		return
	}
	// Batches arrive newest-first; dispatch chronologically so clients that
	// append MESSAGE_CREATEs sequentially render them in order.
	frames := append([]chatFrame(nil), acc.frames...)
	for i := 0; i < len(frames); i++ {
		for j := i + 1; j < len(frames); j++ {
			if frames[j].at.Before(frames[i].at) {
				frames[i], frames[j] = frames[j], frames[i]
			}
		}
	}
	inserted, dup := 0, 0
	for _, f := range frames {
		if !strings.HasPrefix(f.target, "#") && !strings.HasPrefix(f.target, "&") {
			continue
		}
		if f.msgid != "" {
			if _, _, ok := m.store.LookupMessageByMsgID(c.networkID, f.msgid); ok {
				dup++
				continue
			}
		}
		ts := f.at
		if ts.IsZero() || ts.After(time.Now()) {
			ts = time.Now().UTC()
		}
		ts = ts.Truncate(time.Second)
		ch, err := m.store.EnsureChannel(c.networkID, f.target, m.sf.New)
		if err != nil {
			m.log.Warn("irc channel resolve failed", "err", err, "target", f.target)
			continue
		}
		msgID := m.sf.NewAt(ts)
		if err := m.store.AppendMessage(storage.BufferedMessage{
			ID:         msgID,
			ChannelID:  ch.ID,
			AuthorID:   "irc:" + f.author,
			AuthorName: f.author,
			Content:    f.content,
			Timestamp:  ts.Format(time.RFC3339),
			Type:       0,
			MsgID:      f.msgid,
		}); err != nil {
			m.log.Warn("buffer append failed", "err", err, "channel", ch.ID, "msg_id", msgID)
			continue // no buffer row -> the msgid index below would dangle
		}
		if f.msgid != "" {
			c.registerMsgid(msgRef{Snowflake: msgID, ChannelID: ch.ID, GuildID: c.networkID}, f.msgid)
			if err := m.store.SetMessageMsgID(c.networkID, ch.ID, msgID, f.msgid); err != nil {
				m.log.Debug("msgid persist failed", "err", err, "msg", msgID)
			}
		}
		m.gw.Dispatch(c.userID, "MESSAGE_CREATE", buildMessagePayload(msgID, ch.ID, f.author, f.content, ts.Format(time.RFC3339)))
		inserted++
	}
	m.log.Info("chathistory prefill applied", "user", c.userID, "network", c.networkID, "inserted", inserted, "dup", dup)
}

// parseChatTime parses an IRCv3 server-time tag value. Returns the zero
// time on malformed input (the caller falls back to receive time).
func parseChatTime(tag string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, tag)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// buildMessagePayload renders the MESSAGE_CREATE wire shape shared by the
// live relay and the chathistory prefill.
func buildMessagePayload(msgID, channelID, author, content, ts string) map[string]any {
	return map[string]any{
		"id":               msgID,
		"channel_id":       channelID,
		"content":          content,
		"timestamp":        ts,
		"edited_timestamp": nil,
		"tts":              false,
		"mention_everyone": false,
		"mentions":         []any{},
		"mention_roles":    []any{},
		"mention_channels": []any{},
		"attachments":      []any{},
		"embeds":           []any{},
		"reactions":        []any{},
		"nonce":            nil,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            model.IrcAuthorID("irc:" + author),
			"username":      author,
			"discriminator": "0",
			"bot":           false,
		},
	}
}
