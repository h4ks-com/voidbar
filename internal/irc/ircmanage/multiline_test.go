package ircmanage

import (
	"bufio"
	"strconv"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// wireTap is the shared fake-server skeleton for the multiline tests:
// full CAP ACK/NAK control, a capture of every client line, and the
// server-side write function for injecting traffic.
type wireTap struct {
	mu    sync.Mutex
	lines []string
	write func(string)
}

func (t *wireTap) record(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
}

func (t *wireTap) count(substr string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, l := range t.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// countExact counts whole-line matches (substring counting would fold
// "@batch=x PRIVMSG" and bare "PRIVMSG" together).
func (t *wireTap) countExact(line string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, l := range t.lines {
		if l == line {
			n++
		}
	}
	return n
}

func (t *wireTap) Write(s string) {
	t.mu.Lock()
	w := t.write
	t.mu.Unlock()
	if w != nil {
		w(s)
	}
}

func waitTap(t *testing.T, tap *wireTap, substr string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if tap.count(substr) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d wire %q; got %d:\n%s", want, substr, tap.count(substr), strings.Join(tap.lines, "\n"))
}

// waitTapExact polls for an exact line (girc's rate limiter delays
// bursts, so wire assertions cannot run synchronously after a send).
func waitTapExact(t *testing.T, tap *wireTap, line string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if tap.countExact(line) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d exact wire %q; got %d:\n%s", want, line, tap.countExact(line), strings.Join(tap.lines, "\n"))
}

// serveMultiline runs the fake server. multilineACK decides the
// draft/multiline REQ outcome; onJoin delivers extra raw lines after the
// JOIN handshake (incoming-path tests).
func serveMultiline(t *testing.T, ln net.Listener, tap *wireTap, multilineACK bool, onJoin func(w func(string), nick, ch string)) {
	t.Helper()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		nick := ""
		w := func(s string) { _, _ = conn.Write([]byte(s)) }
		tap.mu.Lock()
		tap.write = w
		tap.mu.Unlock()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			tap.record(line)
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				w(lsLine() + "\r\n")
			case strings.HasPrefix(line, "CAP REQ"):
				req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
				ack, nak := "", ""
				for _, tok := range strings.Fields(req) {
					tok = strings.TrimPrefix(tok, ":")
					if tok == "draft/multiline" || tok == "multiline" {
						if multilineACK {
							ack += " " + tok
						} else {
							nak += " " + tok
						}
						continue
					}
					ack += " " + tok
				}
				if strings.TrimSpace(ack) != "" {
					w("CAP * ACK :" + strings.TrimSpace(ack) + "\r\n")
				}
				if strings.TrimSpace(nak) != "" {
					w("CAP * NAK :" + strings.TrimSpace(nak) + "\r\n")
				}
			case strings.HasPrefix(line, "NICK") && nick == "":
				nick = strings.TrimPrefix(line, "NICK ")
				w(":fake 001 " + nick + " :Welcome\r\n")
			case strings.HasPrefix(line, "PING"):
				w("PONG" + line[4:] + "\r\n")
			case strings.HasPrefix(line, "JOIN"):
				ch := strings.TrimSpace(strings.TrimPrefix(line, "JOIN "))
				w(":"+nick+"!u@h JOIN "+ch+"\r\n")
				w(":fake 353 "+nick+" = "+ch+" :"+nick+"\r\n")
				w(":fake 366 "+nick+" "+ch+" :End of NAMES\r\n")
				if onJoin != nil {
					onJoin(w, nick, ch)
				}
			case strings.HasPrefix(line, "WHO "):
				ch := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
				w(":fake 315 "+nick+" "+ch+" :End of WHO list\r\n")
			}
		}
	}()
}

// multilineFixture is the common manager/store/log harness.
type multilineFixture struct {
	store   *storage.Storage
	manager *Manager
	sink    *logSink
	tap     *wireTap
}

func newMultilineFixture(t *testing.T, ln net.Listener, multilineACK bool, onJoin func(w func(string), nick, ch string)) *multilineFixture {
	t.Helper()
	tap := &wireTap{}
	serveMultiline(t, ln, tap, multilineACK, onJoin)
	port := ln.Addr().(*net.TCPAddr).Port

	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertNetwork(&storage.Network{
		ID: "net1", ConnID: "irc://127.0.0.1", Name: "Fake",
		Host: "127.0.0.1", Port: port, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{
		UserID: "u1", NetworkID: "net1",
		Nick: "histguy", Username: "h", Realname: "h",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	sf := util.NewSnowflake(0, 0)
	manager := New(store, gw, logger, sf)
	t.Cleanup(func() { manager.Drop("u1", "net1") })
	manager.EnsureConn("u1", "net1")
	return &multilineFixture{store: store, manager: manager, sink: sink, tap: tap}
}

// testChannel resolves (or creates) the #test channel row.
func (fx *multilineFixture) testChannel(t *testing.T) *storage.Channel {
	t.Helper()
	ch, err := fx.store.EnsureChannel("net1", "#test", func() string { return "chtest1" })
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// TestMultilineSendBatch: with the cap ACKed, a multi-line body leaves as
// one draft/multiline batch (blank inner line intact), and the echoed
// batch binds the server msgid to the snowflake.
func TestMultilineSendBatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fx := newMultilineFixture(t, ln, true, nil)
	waitTap(t, fx.tap, "JOIN #test", 1)

	if err := fx.manager.SendChannel("u1", "net1", "#test", "alpha\n\nomega", "sf1", "ch1"); err != nil {
		t.Fatal(err)
	}
	waitTap(t, fx.tap, "BATCH +vb1 draft/multiline #test", 1)
	waitTapExact(t, fx.tap, "@batch=vb1 PRIVMSG #test alpha", 1)
	// The blank inner line rides the batch as an empty trailing param.
	waitTapExact(t, fx.tap, "@batch=vb1 PRIVMSG #test :", 1)
	waitTapExact(t, fx.tap, "@batch=vb1 PRIVMSG #test omega", 1)
	waitTap(t, fx.tap, "BATCH -vb1", 1)
	if got := fx.tap.countExact("PRIVMSG #test alpha"); got != 0 {
		t.Fatalf("bare PRIVMSG leaked outside the batch:\n%s", strings.Join(fx.tap.lines, "\n"))
	}

	// The server-stamped echo of the batch binds its msgid to our
	// snowflake (the batch close flush pops the pending send). The REST
	// layer owns the buffered row; seed it the way handleSendMessage
	// does so the binding has somewhere to land.
	if err := fx.store.AppendMessage(storage.BufferedMessage{
		ID: "sf1", ChannelID: "ch1", AuthorID: "u1", AuthorName: "histguy",
		Content: "alpha\n\nomega", Timestamp: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	fx.tap.Write("BATCH +srv1 draft/multiline #test\r\n")
	fx.tap.Write("@batch=srv1;msgid=m1 :histguy!u@h PRIVMSG #test :alpha\r\n")
	fx.tap.Write("@batch=srv1 :histguy!u@h PRIVMSG #test :omega\r\n")
	fx.tap.Write("BATCH -srv1\r\n")
	deadline := time.Now().Add(10 * time.Second)
	bound := false
	for time.Now().Before(deadline) {
		for _, m := range fx.store.ChannelMessages("ch1", "", "", 50) {
			if m.ID == "sf1" && m.MsgID == "m1" {
				bound = true
			}
		}
		if bound {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !bound {
		t.Fatalf("echoed batch msgid never bound to sf1; wire:\n%s\nlog:\n%s", strings.Join(fx.tap.lines, "\n"), strings.Join(fx.sink.lines, "\n"))
	}
}

// TestMultilineSendFallback: without the cap the body degrades to one
// PRIVMSG per non-empty line.
func TestMultilineSendFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fx := newMultilineFixture(t, ln, false, nil)
	waitTap(t, fx.tap, "JOIN #test", 1)

	if err := fx.manager.SendChannel("u1", "net1", "#test", "alpha\n\nomega", "sf1", "ch1"); err != nil {
		t.Fatal(err)
	}
	waitTapExact(t, fx.tap, "PRIVMSG #test alpha", 1)
	waitTapExact(t, fx.tap, "PRIVMSG #test omega", 1)
	if n := fx.tap.count("PRIVMSG #test "); n != 2 {
		t.Fatalf("blank line must not become a PRIVMSG; got %d:\n%s", n, strings.Join(fx.tap.lines, "\n"))
	}
	if fx.tap.count("BATCH +vb") != 0 {
		t.Fatalf("no batch expected without the cap:\n%s", strings.Join(fx.tap.lines, "\n"))
	}
}

// TestMultilineIncomingLive: a foreign batch joins into ONE relayed
// message with embedded newlines.
func TestMultilineIncomingLive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fx := newMultilineFixture(t, ln, true, func(w func(string), nick, ch string) {
		w("BATCH +p1 draft/multiline " + ch + "\r\n")
		w("@batch=p1 :bob!b@h PRIVMSG " + ch + " :one\r\n")
		w("@batch=p1 :bob!b@h PRIVMSG " + ch + " :two\r\n")
		w("BATCH -p1\r\n")
	})
	waitForCount(t, fx.sink, "irc message relayed", 1)

	ch := fx.testChannel(t)
	msgs := fx.store.ChannelMessages(ch.ID, "", "", 50)
	found := false
	for _, m := range msgs {
		if m.AuthorName == "bob" && m.Content == "one\ntwo" {
			found = true
		}
		if m.AuthorName == "bob" && (m.Content == "one" || m.Content == "two") {
			t.Fatalf("batch frames must not relay separately: %q", m.Content)
		}
	}
	if !found {
		t.Fatalf("joined multiline message missing; buffer: %+v", msgs)
	}
}

// TestPlanMultiline pins the batch packing rules: line limits split into
// several batches, byte budgets are respected, and long lines are
// word-split into concat-tagged chunks that re-join without a newline.
func TestPlanMultiline(t *testing.T) {
	// Line-limit split: 30 lines with max-lines 24 -> 24 + 6.
	batches := planMultiline(mklines(30), 24, 4096)
	if len(batches) != 2 || len(batches[0]) != 24 || len(batches[1]) != 6 {
		t.Fatalf("max-lines split: %d batches %v lens", len(batches), lensOf(batches))
	}
	// Byte budget: with 10 bytes allowed, two 4-byte lines plus their
	// joiner fit (9), the third does not.
	batches = planMultiline([]string{"aaaa", "bbbb", "cccc"}, 24, 10)
	if len(batches) != 2 {
		t.Fatalf("byte split: %d batches", len(batches))
	}
	// Long line: word-split with trailing space kept on the earlier
	// chunk, concat on every chunk after the first.
	long := "word " + strings.Repeat("x", 400) + " tail"
	batches = planMultiline([]string{long}, 24, 65536)
	if len(batches) != 1 {
		t.Fatalf("one logical line must stay one batch: %d", len(batches))
	}
	var plain, concat int
	for _, f := range batches[0] {
		if f.concat {
			concat++
			continue
		}
		plain++
		if len(f.text) > frameBudget {
			t.Fatalf("frame over budget: %d", len(f.text))
		}
	}
	if plain != 1 || concat == 0 {
		t.Fatalf("split shape: plain=%d concat=%d", plain, concat)
	}
	var joined strings.Builder
	for i, f := range batches[0] {
		if i > 0 && !f.concat {
			joined.WriteByte('\n')
		}
		joined.WriteString(f.text)
	}
	if joined.String() != long {
		t.Fatalf("concat re-join mismatch:\n%q\n%q", joined.String(), long)
	}
}

func mklines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line " + strconv.Itoa(i)
	}
	return out
}

func lensOf(batches [][]outFrame) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}

// TestMultilineSendRespectsLimits: the advertised max-lines value splits
// a longer paste into several batches on the wire.
func TestMultilineSendRespectsLimits(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fakeLSLine = "CAP * LS :batch echo-message server-time message-tags draft/chathistory draft/multiline=max-bytes=4096,max-lines=2"
	t.Cleanup(func() { fakeLSLine = "" })
	fx := newMultilineFixture(t, ln, true, nil)
	waitTap(t, fx.tap, "JOIN #test", 1)

	if err := fx.manager.SendChannel("u1", "net1", "#test", "one\ntwo\nthree", "sf1", "ch1"); err != nil {
		t.Fatal(err)
	}
	// Two batches: (one, two) and (three).
	waitTapCount(t, fx.tap, "BATCH +vb", 2)
	waitTapCount(t, fx.tap, "BATCH -vb", 2)
	deadline := time.Now().Add(10 * time.Second)
	refOne, refThree := "", ""
	for time.Now().Before(deadline) && (refOne == "" || refThree == "") {
		refOne, refThree = batchRefOf(fx.tap, "PRIVMSG #test one"), batchRefOf(fx.tap, "PRIVMSG #test three")
		time.Sleep(50 * time.Millisecond)
	}
	if refOne == "" || refThree == "" {
		t.Fatalf("frames missing: one=%q three=%q\n%s", refOne, refThree, strings.Join(fx.tap.lines, "\n"))
	}
	if refOne == refThree {
		t.Fatalf("max-lines=2 must split the third line into a second batch (both %q):\n%s", refOne, strings.Join(fx.tap.lines, "\n"))
	}
	if refTwo := batchRefOf(fx.tap, "PRIVMSG #test two"); refTwo != refOne {
		t.Fatalf("second line must share the first batch: %q vs %q\n%s", refTwo, refOne, strings.Join(fx.tap.lines, "\n"))
	}
}

// batchRefOf extracts the @batch reference of the wire frame containing
// the given substring (empty when absent).
func batchRefOf(tap *wireTap, substr string) string {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	for _, l := range tap.lines {
		if !strings.Contains(l, substr) || !strings.HasPrefix(l, "@batch=") {
			continue
		}
		tag := strings.TrimPrefix(l, "@batch=")
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		if i := strings.IndexByte(tag, ' '); i >= 0 {
			tag = tag[:i]
		}
		return tag
	}
	return ""
}

// TestMultilineIncomingConcat: concat-tagged frames glue into one line.
func TestMultilineIncomingConcat(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fx := newMultilineFixture(t, ln, true, func(w func(string), nick, ch string) {
		w("BATCH +p1 draft/multiline " + ch + "\r\n")
		w("@batch=p1 :bob!b@h PRIVMSG " + ch + " :how is \r\n")
		w("@batch=p1;draft/multiline-concat :bob!b@h PRIVMSG " + ch + " :everyone?\r\n")
		w("@batch=p1 :bob!b@h PRIVMSG " + ch + " :next line\r\n")
		w("BATCH -p1\r\n")
	})
	waitForCount(t, fx.sink, "irc message relayed", 1)

	ch := fx.testChannel(t)
	msgs := fx.store.ChannelMessages(ch.ID, "", "", 50)
	found := false
	for _, m := range msgs {
		if m.AuthorName == "bob" && m.Content == "how is everyone?\nnext line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("concat join missing; buffer: %+v", msgs)
	}
}

// fakeLSLine lets a test override the server's CAP LS line before the
// client connects (limit-value parsing tests). Tests in this package run
// sequentially.
var fakeLSLine string

func lsLine() string {
	if fakeLSLine != "" {
		return fakeLSLine
	}
	return "CAP * LS :batch echo-message server-time message-tags draft/chathistory draft/multiline"
}

func waitTapCount(t *testing.T, tap *wireTap, substr string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if tap.count(substr) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %q; got %d:\n%s", want, substr, tap.count(substr), strings.Join(tap.lines, "\n"))
}

// TestMultilineIncomingHistory: a multiline batch nested in a chathistory
// page joins into one history frame.
func TestMultilineIncomingHistory(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fx := newMultilineFixture(t, ln, true, func(w func(string), nick, ch string) {
		// One chathistory page containing a nested multiline message.
		w("BATCH +ch1 draft/chathistory LATEST " + ch + "\r\n")
		w("@batch=ch1;time=2026-01-01T10:00:00.000Z :alice!a@h PRIVMSG " + ch + " :plain line\r\n")
		w("BATCH +ml1 draft/multiline " + ch + "\r\n")
		w("@batch=ml1;time=2026-01-01T10:01:00.000Z;msgid=mm1 :bob!b@h PRIVMSG " + ch + " :multi one\r\n")
		w("@batch=ml1 :bob!b@h PRIVMSG " + ch + " :multi two\r\n")
		w("BATCH -ml1\r\n")
		w("BATCH -ch1\r\n")
	})
	waitForCount(t, fx.sink, "chathistory prefill applied", 1)

	ch := fx.testChannel(t)
	msgs := fx.store.ChannelMessages(ch.ID, "", "", 50)
	var joined, plain bool
	for _, m := range msgs {
		if m.AuthorName == "bob" && m.Content == "multi one\nmulti two" {
			joined = true
		}
		if m.AuthorName == "bob" && (m.Content == "multi one" || m.Content == "multi two") {
			t.Fatalf("nested frames must not become separate history rows: %q", m.Content)
		}
		if m.AuthorName == "alice" && m.Content == "plain line" {
			plain = true
		}
	}
	if !joined || !plain {
		t.Fatalf("history rows missing (joined=%v plain=%v): %+v", joined, plain, msgs)
	}
}
