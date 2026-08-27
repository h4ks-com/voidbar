package ircmanage

import (
	"bufio"
	"context"
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

// logSink is a slog.Handler that records formatted lines for assertions.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	s.mu.Lock()
	s.lines = append(s.lines, b.String())
	s.mu.Unlock()
	return nil
}

func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

func (s *logSink) has(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// fakeIRCServer pretends to be an IRC server where the configured nick is
// already taken: registration with NICK doesnm gets 433, girc retries with
// doesnm_ and the server accepts that (001 carries the actual nick, which
// is how girc's GetNick tracking learns it).
func fakeIRCServer(t *testing.T, ln net.Listener) chan net.Conn {
	t.Helper()
	conns := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(conns)
			return
		}
		conns <- conn
		go func() {
			defer func() { _ = conn.Close() }()
			r := bufio.NewReader(conn)
			welcomed := false
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				switch {
				case strings.HasPrefix(line, "CAP LS"):
					_, _ = conn.Write([]byte("CAP * LS :\r\n"))
				case strings.HasPrefix(line, "NICK doesnm_"):
					if !welcomed {
						_, _ = conn.Write([]byte(":fake 001 doesnm_ :Welcome to fake\r\n"))
						welcomed = true
					}
				case strings.HasPrefix(line, "NICK doesnm"):
					_, _ = conn.Write([]byte(":fake 433 * doesnm :Nickname is already in use\r\n"))
				case strings.HasPrefix(line, "PING"):
					_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
				}
			}
		}()
	}()
	return conns
}

func waitFor(t *testing.T, sink *logSink, substr string) {
	t.Helper()
	waitForCount(t, sink, substr, 1)
}

func waitForCount(t *testing.T, sink *logSink, substr string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count(substr) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d log %q; got %d:\n%s", want, substr, sink.count(substr), strings.Join(sink.lines, "\n"))
}

// TestNickCollisionDoesNotEatForeignMessages reproduces the reported bug:
// the member's configured nick (doesnm) collides and the server renames the
// bouncer to doesnm_. PRIVMSG from the OTHER user holding the original
// nick must still be relayed; only our own (doesnm_) messages are skipped
// as echo. The old check compared against Config.Nick (the configured
// nick) and silently ate the foreign user's messages instead.
func TestNickCollisionDoesNotEatForeignMessages(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fakeConns := fakeIRCServer(t, ln)
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
		Nick: "doesnm", Username: "doesnm", Realname: "doesnm",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")

	// The collision rename (doesnm -> doesnm_) must be persisted into the
	// membership so the Discord side displays the nick we actually hold.
	mem, err := store.GetMembership("net1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if mem.Nick != "doesnm_" {
		t.Fatalf("membership nick after collision = %q, want doesnm_", mem.Nick)
	}

	fake, ok := <-fakeConns
	if !ok {
		t.Fatal("fake server saw no connection")
	}

	// Foreign user holds the original nick: must be RELAYED.
	if _, err := fake.Write([]byte(":doesnm!someone@host PRIVMSG #test :collide probe\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, sink, "from=doesnm target=#test")

	// Our own (renamed) nick: girc flags this as an echo and never delivers
	// it to command handlers - it must NOT produce a second relay.
	if _, err := fake.Write([]byte(":doesnm_!someone@host PRIVMSG #test :own echo\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)

	// Exactly one relay happened - the echo was not double-counted.
	if n := sink.count("irc message relayed"); n != 1 {
		t.Fatalf("relay count = %d, want 1:\n%s", n, strings.Join(sink.lines, "\n"))
	}
	if sink.has("from=doesnm_") {
		t.Fatalf("own echo leaked into relay:\n%s", strings.Join(sink.lines, "\n"))
	}
}

// servingConn is a minimal registration flow for the reconnect test: no
// collision, 001 on first NICK, PING/PONG.
func serveRegistration(conn net.Conn) {
	go func() {
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		welcomed := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				_, _ = conn.Write([]byte("CAP * LS :\r\n"))
			case strings.HasPrefix(line, "NICK"):
				if !welcomed {
					nick := strings.TrimPrefix(line, "NICK ")
					_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome to fake\r\n"))
					welcomed = true
				}
			case strings.HasPrefix(line, "PING"):
				_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
			}
		}
	}()
}

// serveJoinable extends serveRegistration with JOIN semantics: #secret is
// invite-only (473), everything else echoes the join back.
func serveJoinable(conn net.Conn) {
	go func() {
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		welcomed := false
		nick := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				_, _ = conn.Write([]byte("CAP * LS :\r\n"))
			case strings.HasPrefix(line, "NICK"):
				if !welcomed {
					nick = strings.TrimPrefix(line, "NICK ")
					_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome to fake\r\n"))
					welcomed = true
				}
			case strings.HasPrefix(line, "PING"):
				_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
			case strings.HasPrefix(line, "JOIN"):
				parts := strings.SplitN(strings.TrimPrefix(line, "JOIN "), " ", 2)
				channel := parts[0]
				if strings.EqualFold(channel, "#secret") {
					_, _ = conn.Write([]byte(":fake 473 " + nick + " #secret :Cannot join channel (+i)\r\n"))
				} else {
					_, _ = conn.Write([]byte(":" + nick + "!u@h JOIN " + channel + "\r\n"))
				}
			}
		}
	}()
}

// awayOf reads a nick's away state on a (user, network) conn; test-only
// view into conn.away.
func (m *Manager) awayOf(userID, networkID, nick string) bool {
	m.mu.Lock()
	c, ok := m.conns[key(userID, networkID)]
	m.mu.Unlock()
	if !ok {
		return false
	}
	return c.isAway(nick)
}

// serveJoinableWHO extends serveJoinable with a WHO responder: busybee
// is away (G), everyone else here (H). For the away-seed tests - the
// connect-time WHO sweep needs server-side answers on the same reader
// loop (a second reader would race serveJoinable for lines).
func serveJoinableWHO(conn net.Conn) {
	go func() {
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		welcomed := false
		nick := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				_, _ = conn.Write([]byte("CAP * LS :\r\n"))
			case strings.HasPrefix(line, "NICK"):
				if !welcomed {
					nick = strings.TrimPrefix(line, "NICK ")
					_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome to fake\r\n"))
					welcomed = true
				}
			case strings.HasPrefix(line, "PING"):
				_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
			case strings.HasPrefix(line, "JOIN"):
				parts := strings.SplitN(strings.TrimPrefix(line, "JOIN "), " ", 2)
				channel := parts[0]
				_, _ = conn.Write([]byte(":" + nick + "!u@h JOIN " + channel + "\r\n"))
				_, _ = conn.Write([]byte(":fake 353 " + nick + " = " + channel + " :" + nick + " sleepy busybee\r\n"))
				_, _ = conn.Write([]byte(":fake 366 " + nick + " " + channel + " :End of NAMES list\r\n"))
			case strings.HasPrefix(line, "WHO "):
				channel := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
				for _, entry := range []struct{ n, flag string }{
					{"sleepy", "H"}, {"busybee", "G"}, {nick, "H"},
				} {
					_, _ = conn.Write([]byte(":fake 352 " + nick + " " + channel + " u h s " + entry.n + " " + entry.flag + " :0 z\r\n"))
				}
				_, _ = conn.Write([]byte(":fake 315 " + nick + " " + channel + " :End of WHO list\r\n"))
			}
		}
	}()
}

// TestChannelMembersFromNAMES: the fake server sends a NAMES reply for
// #test after the join echo; ChannelMembers must surface the other
// occupants (own nick excluded) sorted.
func TestChannelMembersFromNAMES(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinable(conn)
			conns <- conn
		}
	}()
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
		Nick: "listguy", Username: "l", Realname: "l",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}
	// NAMES for #test: two other users + ourselves (the member list shows
	// the requester too, like Discord's own list).
	if _, err := fake.Write([]byte(":fake 353 listguy = #test :listguy alice bob\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = manager.ChannelMembers("u1", "net1", "#test")
		if len(got) == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 3 || got[0] != "alice" || got[1] != "bob" || got[2] != "listguy" {
		t.Fatalf("members: %v", got)
	}
}

// TestChannelMembersModesFromNAMES: prefixed NAMES entries must surface
// their highest channel-membership mode (girc maps prefixes through the
// server's advertised PREFIX; the fake advertises none, so the standard
// @=op +=voice apply).
func TestChannelMembersModesFromNAMES(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinable(conn)
			conns <- conn
		}
	}()
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
		Nick: "modguy", Username: "m", Realname: "m",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}
	if _, err := fake.Write([]byte(":fake 353 modguy = #test :modguy @opnick +voicenick plainnick\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got []ChannelMember
	for time.Now().Before(deadline) {
		got = manager.ChannelMembersDetailed("u1", "net1", "#test")
		if len(got) == 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	modes := map[string]string{}
	for _, cm := range got {
		modes[cm.Nick] = cm.Mode
	}
	if modes["opnick"] != "o" || modes["voicenick"] != "v" || modes["plainnick"] != "" || modes["modguy"] != "" {
		t.Fatalf("modes: %v", modes)
	}
}

// TestOccupancyNotifier: upstream JOIN/PART/QUIT must fire the occupancy
// callback with the channel (empty for QUIT - it hits every shared
// channel at once) so the member sidebar can resync.
func TestOccupancyNotifier(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinable(conn)
			conns <- conn
		}
	}()
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
		Nick: "watchguy", Username: "w", Realname: "w",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	events := make(chan string, 8)
	manager.SetOccupancyNotifier(func(userID, networkID, ircChannel string) {
		if userID == "u1" && networkID == "net1" {
			events <- ircChannel
		}
	})

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}

	next := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("occupancy: got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("occupancy: no event for %q", want)
		}
	}
	// The fake echoes our autojoin back: consume that notification first.
	next("#test")
	if _, err := fake.Write([]byte(":newguy JOIN :#test\r\n")); err != nil {
		t.Fatal(err)
	}
	next("#test")
	if _, err := fake.Write([]byte(":oldguy PART #test :bye\r\n")); err != nil {
		t.Fatal(err)
	}
	next("#test")
	if _, err := fake.Write([]byte(":oldguy QUIT :gone\r\n")); err != nil {
		t.Fatal(err)
	}
	next("")
}

// TestAwaySeedFromWHO: the connect-time WHO sweep (352's H/G flag) must
// seed per-nick away state, and end-of-WHO (315) must fire the occupancy
// callback for the channel so subscribed lists resync.
func TestAwaySeedFromWHO(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinableWHO(conn)
			conns <- conn
		}
	}()
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
		Nick: "whoguy", Username: "w", Realname: "w",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	if _, ok := <-conns; !ok {
		t.Fatal("fake server saw no connection")
	}

	// Away state lands via the connect-time WHO sweep (352 H/G + 315).
	deadline := time.Now().Add(5 * time.Second)
	var got []ChannelMember
	for time.Now().Before(deadline) {
		got = manager.ChannelMembersDetailed("u1", "net1", "#test")
		if len(got) == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 3 {
		t.Fatalf("members: %v", got)
	}
	away := map[string]bool{}
	for _, cm := range got {
		away[cm.Nick] = cm.Away
	}
	if away["sleepy"] || !away["busybee"] || away["whoguy"] {
		t.Fatalf("away states: %v", away)
	}
}

// TestAwayNotifyPush: with the away-notify cap negotiated, AWAY/BACK
// events must flip the away state live and fire the occupancy callback
// (empty channel - the affected set isn't knowable).
func TestAwayNotifyPush(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinableAway(conn)
			conns <- conn
		}
	}()
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
		Nick: "pushguy", Username: "p", Realname: "p",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	occupancy := make(chan string, 8)
	manager.SetOccupancyNotifier(func(userID, networkID, ircChannel string) {
		if userID == "u1" && networkID == "net1" {
			occupancy <- ircChannel
		}
	})

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}

	// Wait for the occupant to exist, then flip away and back.
	deadline := time.Now().Add(5 * time.Second)
	saw := false
	for time.Now().Before(deadline) && !saw {
		if len(manager.ChannelMembers("u1", "net1", "#test")) == 2 {
			saw = true
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !saw {
		t.Fatal("occupant never arrived")
	}

	// Drain the connect-time notifications (JOIN echo, WHO-sweep 315)
	// before asserting on the AWAY pushes.
	for {
		select {
		case <-occupancy:
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}

	drain := func() string {
		select {
		case got := <-occupancy:
			return got
		case <-time.After(5 * time.Second):
			return ""
		}
	}
	if _, err := fake.Write([]byte(":sleepy AWAY :brb food\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := drain(); got != "" {
		t.Fatalf("away push occupancy: %q", got)
	}
	if !manager.awayOf("u1", "net1", "sleepy") {
		t.Fatal("sleepy should be away")
	}
	if _, err := fake.Write([]byte(":sleepy AWAY\r\n")); err != nil {
		t.Fatal(err)
	}
	if got := drain(); got != "" {
		t.Fatalf("back push occupancy: %q", got)
	}
	if manager.awayOf("u1", "net1", "sleepy") {
		t.Fatal("sleepy should be back")
	}
}

// serveJoinableAway: like serveJoinable but negotiates away-notify (CAP
// LS advertises it, CAP REQ gets ACKed) and seeds one occupant.
func serveJoinableAway(conn net.Conn) {
	go func() {
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		welcomed := false
		nick := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				_, _ = conn.Write([]byte("CAP * LS :away-notify\r\n"))
			case strings.HasPrefix(line, "CAP REQ"):
				req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
				_, _ = conn.Write([]byte("CAP * ACK :" + req + "\r\n"))
			case strings.HasPrefix(line, "NICK"):
				if !welcomed {
					nick = strings.TrimPrefix(line, "NICK ")
					_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome to fake\r\n"))
					welcomed = true
				}
			case strings.HasPrefix(line, "PING"):
				_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
			case strings.HasPrefix(line, "JOIN"):
				parts := strings.SplitN(strings.TrimPrefix(line, "JOIN "), " ", 2)
				channel := parts[0]
				_, _ = conn.Write([]byte(":" + nick + "!u@h JOIN " + channel + "\r\n"))
				_, _ = conn.Write([]byte(":fake 353 " + nick + " = " + channel + " :" + nick + " sleepy\r\n"))
				_, _ = conn.Write([]byte(":fake 366 " + nick + " " + channel + " :End of NAMES list\r\n"))
			case strings.HasPrefix(line, "WHO "):
				channel := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
				_, _ = conn.Write([]byte(":fake 352 " + nick + " " + channel + " u h s sleepy H :0 z\r\n"))
				_, _ = conn.Write([]byte(":fake 315 " + nick + " " + channel + " :End of WHO list\r\n"))
			}
		}
	}()
}
// the upstream refuses (473 invite-only) must leave no trace in the
// auto-join list, and Clyde must DM the user the reason. A join the server
// accepts echoes back and triggers no rollback.
func TestJoinRejectedRollsBackAndClydeExplains(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveJoinable(conn)
			conns <- conn
		}
	}()
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
		Nick: "joiner", Username: "j", Realname: "j",
		AutoJoin: []string{}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	if _, ok := <-conns; !ok {
		t.Fatal("fake server saw no connection")
	}

	// Service-level optimistic create for a channel the server refuses.
	if _, err := store.MembershipAddChannel("net1", "u1", "#secret"); err != nil {
		t.Fatal(err)
	}
	manager.JoinChannel("u1", "net1", "#secret")
	waitFor(t, sink, "irc join rejected")

	// Rollback: auto-join is clean again.
	mem, err := store.GetMembership("net1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mem.AutoJoin) != 0 {
		t.Fatalf("autojoin after rejected join: %v", mem.AutoJoin)
	}

	// Clyde DMed the reason.
	dms, err := store.ListDMChannels("u1")
	if err != nil || len(dms) != 1 {
		t.Fatalf("dm list: %v %v", dms, err)
	}
	if dms[0].Nick != "Clyde" {
		t.Fatalf("dm peer: %+v", dms[0])
	}
	msgs := store.ChannelMessages(dms[0].ID, "", "", 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "#secret") || !strings.Contains(msgs[0].Content, "invite-only") {
		t.Fatalf("clyde message: %+v", msgs)
	}
	if msgs[0].AuthorName != "Clyde" {
		t.Fatalf("clyde author: %+v", msgs[0])
	}

	// An accepted join (echo) triggers no rollback.
	if _, err := store.MembershipAddChannel("net1", "u1", "#open"); err != nil {
		t.Fatal(err)
	}
	manager.JoinChannel("u1", "net1", "#open")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count("irc join rejected") == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := sink.count("irc join rejected"); n != 1 {
		t.Fatalf("accepted join must not roll back; rejected count = %d:\n%s", n, strings.Join(sink.lines, "\n"))
	}
	mem, _ = store.GetMembership("net1", "u1")
	if len(mem.AutoJoin) != 1 || mem.AutoJoin[0] != "#open" {
		t.Fatalf("autojoin after accepted join: %v", mem.AutoJoin)
	}
}

// TestReconnectAfterDrop verifies bouncer semantics on the upstream side:
// when the TCP link dies, the supervisor dials again (fresh client, backoff)
// and the CONNECTED handler re-joins the auto-join channels.
func TestReconnectAfterDrop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveRegistration(conn)
			conns <- conn
		}
	}()
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
		Nick: "reconn", Username: "reconn", Realname: "reconn",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	manager.reconnectBackoff = 150 * time.Millisecond // snappy retries in test
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	first, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no first connection")
	}

	// Kill the link like a network flap: the manager must re-dial.
	_ = first.Close()
	waitForCount(t, sink, "irc connected", 2)
	second, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no second connection")
	}
	_ = second.Close()
}

// TestQueryRelayCreatesDM verifies the inbound-query path: a PRIVMSG whose
// target is our own nick (not #/&) becomes a DM channel + MESSAGE_CREATE,
// lands in the replay buffer and shows up in the user's DM list ordering.
func TestQueryRelayCreatesDM(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			serveRegistration(conn)
			conns <- conn
		}
	}()
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
		Nick: "queryguy", Username: "q", Realname: "q",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	sink := &logSink{}
	logger := slog.New(sink)
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	manager.EnsureConn("u1", "net1")
	waitFor(t, sink, "irc connected")
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}

	// Inbound query: target is our nick, source is the peer.
	if _, err := fake.Write([]byte(":stranger!u@h PRIVMSG queryguy :hi there\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, sink, "irc query relayed")

	dms, err := store.ListDMChannels("u1")
	if err != nil || len(dms) != 1 {
		t.Fatalf("dm list: %v %v", dms, err)
	}
	if dms[0].Nick != "stranger" || dms[0].NetworkID != "net1" || dms[0].OwnerID != "u1" {
		t.Fatalf("dm record: %+v", dms[0])
	}
	// Same peer again: still one thread.
	if _, err := fake.Write([]byte(":stranger!u@h PRIVMSG queryguy :again\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, sink, "irc query relayed", 2)
	dms, _ = store.ListDMChannels("u1")
	if len(dms) != 1 {
		t.Fatalf("dm must be deduped by peer nick, got %d", len(dms))
	}
	// Replay buffer holds both messages.
	msgs := store.ChannelMessages(dms[0].ID, "", "", 50)
	if len(msgs) != 2 || msgs[1].Content != "hi there" || msgs[0].Content != "again" {
		t.Fatalf("dm buffer: %+v", msgs)
	}
}
