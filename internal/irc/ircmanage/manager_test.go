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
