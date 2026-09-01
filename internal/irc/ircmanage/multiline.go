package ircmanage

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lrstanley/girc"
)

// sendJob is one queued upstream message write (see conn.sendJobs).
type sendJob struct {
	target  string
	content string
	query   bool // DM path: typing TAGMSG + PRIVMSG to a bare nick
}

// enqueueSend hands a message write to the connection's worker. The
// caller has already validated connectivity; FIFO across calls is the
// worker's guarantee, and rate-limiter delays (~1s per girc event) stay
// off the REST path.
func (c *conn) enqueueSend(target, content string, query bool) {
	select {
	case c.sendJobs <- sendJob{target: target, content: content, query: query}:
	default:
		// A full queue means a stuck worker (or a paste storm); the
		// message is dropped rather than blocking the caller.
		return
	}
}

// runSendWorker drains sendJobs until the connection is dropped. One
// worker per conn for the conn's whole life: jobs pick up the CURRENT
// girc client, so a supervisor reconnect mid-queue just continues on
// the fresh socket.
func (m *Manager) runSendWorker(c *conn) {
	for job := range c.sendJobs {
		m.mu.Lock()
		client := c.client
		m.mu.Unlock()
		if client == nil {
			continue
		}
		if typingAllowed(client) {
			sendTypingTag(client, job.target, "done")
		}
		m.sendLines(c, client, job.target, job.content)
	}
}

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

// outFrame is one planned PRIVMSG of an outgoing multiline batch:
// concat-tagged frames glue to their predecessor without a newline
// (how the spec splits lines too long for a single frame).
type outFrame struct {
	text   string
	concat bool
}

// frameBudget is the per-frame byte ceiling for concat splitting: the
// spec's worst-case accounting leaves ~353 bytes for the trailing
// param; batches may run longer per line, but nothing REQUIRES servers
// to accept that, so long logical lines are split conservatively.
const frameBudget = 350

// sendLines relays one message body upstream. Single-line bodies take
// the plain PRIVMSG path; multi-line bodies go out as draft/multiline
// batch(es) when the upstream ACKed the cap (blank inner lines ride
// the batch - the join re-creates them), respecting the advertised
// max-lines/max-bytes budget by splitting into several batches, and
// degrade to one PRIVMSG per non-empty line otherwise (an empty
// PRIVMSG is a wire error, so blanks cannot survive the fallback).
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
	maxBytes, maxLines := c.multilineLimits()
	for _, batch := range planMultiline(lines, maxLines, maxBytes) {
		m.sendMultilineBatch(client, target, batch)
	}
}

// planMultiline packs logical lines into batch frame-lists that respect
// the negotiated limits: at most maxLines frames, at most maxBytes of
// content (frames joined by single \n bytes), and no frame longer than
// frameBudget (long lines are word-split, keeping the trailing space,
// into concat-tagged chunks that re-join without a newline).
func planMultiline(lines []string, maxLines, maxBytes int) [][]outFrame {
	if maxLines < 1 {
		maxLines = 1
	}
	if maxBytes < 1 {
		maxBytes = frameBudget
	}
	// Expand every logical line into frames (1..n per line).
	frames := make([]outFrame, 0, len(lines))
	for _, ln := range lines {
		if len(ln) <= frameBudget {
			frames = append(frames, outFrame{text: ln})
			continue
		}
		rest := ln
		first := true
		for len(rest) > 0 {
			cut := frameBudget
			if cut > len(rest) {
				cut = len(rest)
			}
			chunk := rest[:cut]
			if cut < len(rest) {
				// Split between words, keeping the trailing space with
				// the earlier chunk (spec-recommended so fallback lines
				// stay space-separated).
				if sp := strings.LastIndexByte(chunk, ' '); sp > 0 {
					chunk = chunk[:sp+1]
				}
			}
			frames = append(frames, outFrame{text: chunk, concat: !first})
			first = false
			rest = rest[len(chunk):]
		}
	}
	// Pack frames into batches within the limits.
	var out [][]outFrame
	cur := make([]outFrame, 0, maxLines)
	curBytes := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = make([]outFrame, 0, maxLines)
			curBytes = 0
		}
	}
	for _, f := range frames {
		cost := len(f.text)
		if len(cur) > 0 {
			cost++ // the \n joiner
		}
		if len(cur) >= maxLines || (len(cur) > 0 && curBytes+cost > maxBytes) {
			flush()
			cost = len(f.text)
		}
		cur = append(cur, f)
		curBytes += cost
	}
	flush()
	return out
}

func (m *Manager) sendMultilineBatch(client *girc.Client, target string, frames []outFrame) {
	ref := "vb" + strconv.FormatInt(lineBatchSeq.Add(1), 36)
	client.Send(&girc.Event{Command: "BATCH", Params: []string{"+" + ref, "draft/multiline", target}})
	for _, f := range frames {
		tags := girc.Tags{"batch": ref}
		if f.concat {
			tags["draft/multiline-concat"] = ""
		}
		client.Send(&girc.Event{
			Tags:    tags,
			Command: "PRIVMSG",
			Params:  []string{target, f.text},
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
// Concat-tagged frames glue onto their predecessor without a newline
// (the spec's long-line splitting).
func (m *Manager) lineBatchFrame(c *conn, e girc.Event, ref string) {
	if len(e.Params) == 0 || e.Source == nil {
		return
	}
	at := time.Time{}
	if tag, ok := e.Tags.Get("time"); ok && tag != "" {
		at = parseChatTime(tag)
	}
	msgid, _ := e.Tags.Get("msgid")
	_, concat := e.Tags.Get("draft/multiline-concat")
	c.appendLineFrame(ref, lineFrame{
		line:   e.Last(),
		at:     at,
		msgid:  msgid,
		echo:   e.Echo,
		source: e.Source.Name,
		concat: concat,
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

// accLines flattens the frames into the joined message's lines,
// applying concat gluing (frames tagged draft/multiline-concat glue to
// their predecessor with no separator).
func accLines(acc *lineBatch) []string {
	out := make([]string, 0, len(acc.frames))
	for _, f := range acc.frames {
		if f.concat && len(out) > 0 {
			out[len(out)-1] += f.line
			continue
		}
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
	concat bool // glue to the previous frame without a newline
}
