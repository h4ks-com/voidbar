package ircmanage

import (
	"strconv"
	"strings"
	"time"

	"github.com/lrstanley/girc"

	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

// chatPrefillLimit is how many messages one prefill asks the upstream for.
const chatPrefillLimit = 50

// chatTimeSelectorLayout is the timestamp= selector format chathistory
// requests carry. Milliseconds are mandatory on some servers (eris parses
// exactly this layout), so the plain RFC3339 form is not accepted there.
const chatTimeSelectorLayout = "2006-01-02T15:04:05.000Z"

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
	if !c.histCap() {
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
			// A pending scroll-fetch waiter (FetchOlder) takes the batch
			// silently - history must not storm attached clients as
			// MESSAGE_CREATE - whether or not it still listens: a waiter
			// whose caller timed out is replaced, never left dangling.
			c.batchMu.Lock()
			wait := c.pageCh
			if wait != nil && acc.target != "" && strings.EqualFold(acc.target, c.pageTarget) {
				c.pageBatch = acc
				c.pageCh = nil
				c.batchMu.Unlock()
				close(wait)
				return
			}
			c.batchMu.Unlock()
			m.flushChatBatch(c, acc, true, "")
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
// buffer append (server-time timestamp) and msgid registration (prefilled
// messages are reactable/deletable immediately). A live batch (join
// prefill) also marks the channel watermark and dispatches MESSAGE_CREATE
// so attached clients with the channel already open see the backlog
// without reopening; a silent batch (scroll backfill) inserts into storage
// only - the scrolling client reads the older page from its own REST
// response, and a MESSAGE_CREATE for old messages would append them at
// the bottom of every open view. Returns how many messages were inserted.
func (m *Manager) flushChatBatch(c *conn, acc *chatBatch, live bool, ceiling string) int {
	if live && acc.target != "" {
		if err := m.store.MarkChatPrefill(c.networkID, acc.target); err != nil {
			m.log.Debug("chathistory watermark persist failed", "err", err, "channel", acc.target)
		}
	}
	if len(acc.frames) == 0 {
		return 0
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
		// Millisecond precision, not seconds: snowflakes are minted from
		// the same value (NewAt), and id order must match chronology - a
		// second-truncated burst would hand out ids by insertion order,
		// inverting a backfill against an earlier prefill of the same
		// second. RFC3339Nano drops a zero fraction, so whole-second
		// times keep their familiar shape.
		ts = ts.Truncate(time.Millisecond)
		ch, err := m.store.EnsureChannel(c.networkID, f.target, m.sf.New)
		if err != nil {
			m.log.Warn("irc channel resolve failed", "err", err, "target", f.target)
			continue
		}
		// Backfilled frames mint strictly below the anchor row's id, so a
		// same-millisecond burst cannot land above its own ceiling in the
		// id-sorted views; prefilled frames have no ceiling.
		var msgID string
		if live || ceiling == "" {
			msgID = m.sf.NewAt(ts)
		} else {
			msgID = m.sf.NewBelow(ts, ceiling)
		}
		if err := m.store.AppendMessage(storage.BufferedMessage{
			ID:         msgID,
			ChannelID:  ch.ID,
			AuthorID:   "irc:" + f.author,
			AuthorName: f.author,
			Content:    f.content,
			Timestamp:  ts.Format(time.RFC3339Nano),
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
		if live {
			m.gw.Dispatch(c.userID, "MESSAGE_CREATE", buildMessagePayload(msgID, ch.ID, f.author, f.content, ts.Format(time.RFC3339Nano)))
		}
		inserted++
	}
	if live {
		m.log.Info("chathistory prefill applied", "user", c.userID, "network", c.networkID, "inserted", inserted, "dup", dup)
	}
	return inserted
}

// FetchOlder backfills one scroll-up step from network history: asks the
// upstream for messages older than the anchor (CHATHISTORY BEFORE) and
// merges the batch into the buffer silently - no watermark, no
// MESSAGE_CREATE. The anchor prefers the floor row's msgid (a row-precise
// selector: bursts within one second are ordered exactly) and falls back
// to a timestamp selector only when the floor carries no msgid. The
// timestamp fallback anchors at the floor's (second-truncated) time, so
// same-second stragglers below an msgid-less floor can be missed - rare
// enough to accept, and the next scroll's floor moves past them.
// ceilingID is the anchor row's own snowflake: inserted ids mint strictly
// below it, keeping id order equal to chronology even for same-millisecond
// bursts. Synchronous with a timeout: the REST page request that triggered
// it re-reads the buffer when this returns. Returns how many new messages
// were inserted.
func (m *Manager) FetchOlder(userID, networkID, ircChannel, anchorMsgID, ceilingID string, anchor time.Time, limit int) int {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	if !ok || c.client == nil || !c.linkUp.Load() {
		return 0
	}
	if !c.histCap() {
		return 0
	}
	if !strings.HasPrefix(ircChannel, "#") && !strings.HasPrefix(ircChannel, "&") {
		return 0
	}
	selector := ""
	if anchorMsgID != "" {
		selector = "msgid=" + anchorMsgID
	} else if !anchor.IsZero() && !anchor.After(time.Now().UTC()) {
		selector = "timestamp=" + anchor.UTC().Format(chatTimeSelectorLayout)
	}
	if selector == "" {
		return 0
	}
	if limit <= 0 {
		return 0
	}
	if limit > 100 {
		limit = 100
	}
	c.pageMu.Lock()
	defer c.pageMu.Unlock()
	c.batchMu.Lock()
	// One fetch at a time; a waiter older than the ask window is stale
	// (its caller timed out) and is replaced, so a late batch lands in a
	// fresh handoff instead of dangling forever.
	if c.pageCh != nil && time.Since(c.pageIssued) < 10*time.Second {
		c.batchMu.Unlock()
		return 0
	}
	ch := make(chan struct{})
	c.pageCh = ch
	c.pageTarget = ircChannel
	c.pageBatch = nil
	c.pageCeiling = ceilingID
	c.pageIssued = time.Now()
	c.batchMu.Unlock()

	c.client.Send(&girc.Event{
		Command: "CHATHISTORY",
		Params:  []string{"BEFORE", ircChannel, selector, strconv.Itoa(limit)},
	})

	select {
	case <-ch:
		c.batchMu.Lock()
		acc := c.pageBatch
		ceiling := c.pageCeiling
		c.pageBatch = nil
		c.pageCeiling = ""
		c.pageTarget = ""
		c.batchMu.Unlock()
		if acc == nil {
			return 0
		}
		n := m.flushChatBatch(c, acc, false, ceiling)
		m.log.Info("chathistory backfill applied", "user", c.userID, "network", c.networkID, "channel", ircChannel, "selector", selector, "inserted", n)
		return n
	case <-time.After(5 * time.Second):
		// Leave the waiter registered: a batch arriving after the timeout
		// is consumed by the handoff (silently dropped) rather than
		// dispatched live at the bottom of open clients.
		return 0
	}
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
