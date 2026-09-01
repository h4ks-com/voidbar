package ircmanage

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lrstanley/girc"
)

// draft/multiline batches carry several PRIVMSGs that are ONE message -
// the exact IRC counterpart of the composer's shift+enter. Without the
// cap upstream, newlines cannot survive the wire at all (IRC lines are
// frames), so the split degrades to one PRIVMSG per non-empty line.

// lineBatchSeq generates outgoing batch references (unique per process,
// which is stricter than the per-connection requirement).
var lineBatchSeq atomic.Int64

// lineBatch accumulates the frames of one in-flight draft/multiline
// batch: live traffic to join into a single message, or history frames
// nested inside a chathistory batch (chatRef set).
type lineBatch struct {
	target  string
	chatRef string       // enclosing chathistory batch, when nested
	frames  []lineFrame
}

// isMultilineType reports whether a BATCH type is draft/multiline under
// either of its names (draft prefix optional on some servers).
func isMultilineType(t string) bool {
	return t == "draft/multiline" || t == "multiline"
}

// multilineCapName maps a CAP ACK token (possibly value-carrying) to
// the sticky flag decision.
func isMultilineCap(token string) bool {
	name, _, _ := strings.Cut(token, "=")
	return name == "draft/multiline" || name == "multiline"
}

// sendLines relays one message body upstream. Single-line bodies take
// the plain PRIVMSG path; multi-line bodies go out as a
// draft/multiline batch when the upstream ACKed the cap (blank inner
// lines ride the batch - the join re-creates them), and degrade to one
// PRIVMSG per non-empty line otherwise (an empty PRIVMSG is a wire
// error, so blanks cannot survive the fallback).
func (m *Manager) sendLines(c *conn, client *girc.Client, target, content string) {
	if !strings.Contains(content, "\n") {
		client.Cmd.Message(target, content)
		return
	}
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return
	}
	if !c.multilineCap() {
		for _, ln := range lines {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			client.Cmd.Message(target, ln)
		}
		return
	}
	ref := "vb" + strconv.FormatInt(lineBatchSeq.Add(1), 36)
	client.Send(&girc.Event{Command: "BATCH", Params: []string{"+" + ref, "draft/multiline", target}})
	for _, ln := range lines {
		client.Send(&girc.Event{
			Tags:    girc.Tags{"batch": ref},
			Command: "PRIVMSG",
			Params:  []string{target, ln},
		})
	}
	client.Send(&girc.Event{Command: "BATCH", Params: []string{"-" + ref}})
}

// multilineBatchControl drives the BATCH envelope for incoming
// draft/multiline batches. Nested batches (a multiline message inside a
// chathistory page) bind to the innermost open chathistory batch: on
// close their joined frame lands in that batch instead of the live
// relay. Returns whether the event was consumed.
func (m *Manager) multilineBatchControl(c *conn, e girc.Event) bool {
	if len(e.Params) < 1 {
		return false
	}
	ref := e.Params[0]
	switch {
	case strings.HasPrefix(ref, "+"):
		if len(e.Params) < 2 || !isMultilineType(e.Params[1]) {
			return false
		}
		acc := &lineBatch{target: e.Params[len(e.Params)-1]}
		if chatRef, ok := c.peekChatStack(); ok {
			acc.chatRef = chatRef
		}
		c.openLineBatch(ref[1:], acc)
		return true
	case strings.HasPrefix(ref, "-"):
		acc := c.closeLineBatch(ref[1:])
		if acc == nil {
			return false
		}
		m.flushLineBatch(c, acc)
		return true
	}
	return false
}

// lineBatchFrame accumulates one batched PRIVMSG into its batch.
func (m *Manager) lineBatchFrame(c *conn, e girc.Event, ref string) {
	if len(e.Params) == 0 || e.Source == nil {
		return
	}
	at := time.Time{}
	if tag, ok := e.Tags.Get("time"); ok && tag != "" {
		at = parseChatTime(tag)
	}
	msgid, _ := e.Tags.Get("msgid")
	c.appendLineFrame(ref, lineFrame{
		line:   e.Last(),
		at:     at,
		msgid:  msgid,
		echo:   e.Echo,
		source: e.Source.Name,
	})
}

// flushLineBatch resolves one closed batch: history-nested frames join
// into their chathistory batch, own echoes only bind the msgid, and
// foreign live traffic relays as a single message.
func (m *Manager) flushLineBatch(c *conn, acc *lineBatch) {
	if len(acc.frames) == 0 {
		return
	}
	first := acc.frames[0]
	author := first.source
	at := first.at
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if acc.chatRef != "" {
		c.appendChatFrame(acc.chatRef, chatFrame{
			target:  acc.target,
			author:  author,
			content: strings.Join(accLines(acc), "\n"),
			at:      at,
			msgid:   first.msgid,
		})
		return
	}
	if first.echo {
		// Our own multiline send echoed back (echo-message): the Discord
		// side already has the message; only the msgid binding matters.
		if first.msgid == "" {
			return
		}
		ref := c.popPendingSend(acc.target)
		if ref.Snowflake == "" {
			return
		}
		c.registerMsgid(ref, first.msgid)
		if err := m.store.SetMessageMsgID(c.networkID, ref.ChannelID, ref.Snowflake, first.msgid); err != nil {
			m.log.Debug("msgid persist failed", "err", err, "msg", ref.Snowflake)
		}
		return
	}
	m.dispatchMessage(c, acc.target, author, strings.Join(accLines(acc), "\n"), at.Format(time.RFC3339Nano), first.msgid)
}

// accLines flattens the frames into the joined message's lines.
func accLines(acc *lineBatch) []string {
	out := make([]string, 0, len(acc.frames))
	for _, f := range acc.frames {
		out = append(out, f.line)
	}
	return out
}

// lineFrame is one raw PRIVMSG of a multiline batch, before joining.
type lineFrame struct {
	line   string
	at     time.Time
	msgid  string
	echo   bool
	source string
}
