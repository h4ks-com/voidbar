package ircmanage

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// serveIrcCaps negotiates the peer-facts caps (extended-join,
// account-notify, chghost, invite-notify, standard-replies) and seeds
// one occupant. JOIN echoes carry the extended-join shape.
func serveIrcCaps(conn net.Conn) {
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
				_, _ = conn.Write([]byte("CAP * LS :extended-join account-notify chghost invite-notify standard-replies away-notify\r\n"))
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
				_, _ = conn.Write([]byte(":" + nick + "!me@self.example JOIN " + channel + " * :the bouncer\r\n"))
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

// TestPeerFactsCaps: extended-join seeds account+host, account-notify and
// chghost update them live, and PeerInfoByAuthor resolves the hashed
// author id back to the facts.
func TestPeerFactsCaps(t *testing.T) {
	ln, mgr, conns := startCapsManager(t)
	port := ln.Addr().(*net.TCPAddr).Port
	seedCapsMember(t, mgr, port)
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}

	// extended-join: JOIN <channel> <account> <realname>.
	if _, err := fake.Write([]byte(":wallet!w@wallet.example JOIN #test walletacc :The Wallet\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		for _, cm := range mgr.ChannelMembersDetailed("u1", "net1", "#test") {
			if cm.Nick == "wallet" {
				return cm.Account == "walletacc" && cm.Host == "w@wallet.example"
			}
		}
		return false
	}, "extended-join facts to land")

	// account-notify: logout normalizes "*" to "".
	if _, err := fake.Write([]byte(":wallet ACCOUNT *\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		for _, cm := range mgr.ChannelMembersDetailed("u1", "net1", "#test") {
			if cm.Nick == "wallet" {
				return cm.Account == ""
			}
		}
		return false
	}, "account-notify logout to clear the account")

	// chghost: the cloak change rewrites the host.
	if _, err := fake.Write([]byte(":wallet CHGHOST w2 cloak.example\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		for _, cm := range mgr.ChannelMembersDetailed("u1", "net1", "#test") {
			if cm.Nick == "wallet" {
				return cm.Host == "w2@cloak.example"
			}
		}
		return false
	}, "chghost to rewrite the host")

	// WHO 352 seeds facts for pre-existing occupants.
	waitForCond(t, func() bool {
		for _, cm := range mgr.ChannelMembersDetailed("u1", "net1", "#test") {
			if cm.Nick == "sleepy" {
				return cm.Host == "u@h"
			}
		}
		return false
	}, "WHO to seed sleepy's host")

	nick, _, host, ok := mgr.PeerInfoByAuthor("u1", model.IrcAuthorID("irc:wallet"))
	if !ok || nick != "wallet" || host != "w2@cloak.example" {
		t.Fatalf("PeerInfoByAuthor: %q %q %v", nick, host, ok)
	}
}

// TestInviteAndStandardReplyRelay: invite-notify broadcasts land in the
// channel buffer, direct invites to us land as a DM, and a FAIL naming a
// joined channel surfaces there as a server message.
func TestInviteAndStandardReplyRelay(t *testing.T) {
	ln, mgr, conns := startCapsManager(t)
	port := ln.Addr().(*net.TCPAddr).Port
	seedCapsMember(t, mgr, port)
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}
	ourNick := mgr.LiveNick("u1", "net1")

	// invite-notify broadcast: we're in #test, so it lands there.
	if _, err := fake.Write([]byte(":op!o@h INVITE newbie #test\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		return bufferedHas(t, mgr, "#test", "op", "invited newbie to the channel")
	}, "invite broadcast to land in the channel")

	// Direct invite to us for a channel we're NOT in: DM, no ghost row.
	if _, err := fake.Write([]byte(":op!o@h INVITE " + ourNick + " #elsewhere\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		dms, err := mgr.store.ListDMChannels("u1")
		if err != nil {
			return false
		}
		for _, dm := range dms {
			for _, m := range mgr.store.ChannelMessages(dm.ID, "", "", 50) {
				if dm.Nick == "op" && m.AuthorName == "op" && strings.Contains(m.Content, "invited you to #elsewhere") {
					return true
				}
			}
		}
		return false
	}, "direct invite to land as a DM")
	if _, err := mgr.store.GetChannelByIRC("net1", "#elsewhere"); err == nil {
		t.Fatal("direct invite must not create a ghost channel row")
	}

	// standard-replies: FAIL with a channel context surfaces there.
	if _, err := fake.Write([]byte(":fake FAIL MSG RATE_LIMITED #test :slow down\r\n")); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, func() bool {
		return bufferedHas(t, mgr, "#test", "server", "FAIL RATE_LIMITED: slow down")
	}, "FAIL to surface in the channel")
}

// bufferedHas reports whether the channel's buffer already holds a
// message from author containing content.
func bufferedHas(t *testing.T, mgr *Manager, ircName, author, content string) bool {
	t.Helper()
	ch, err := mgr.store.GetChannelByIRC("net1", ircName)
	if err != nil {
		return false
	}
	for _, m := range mgr.store.ChannelMessages(ch.ID, "", "", 50) {
		if m.AuthorName == author && strings.Contains(m.Content, content) {
			return true
		}
	}
	return false
}

// startCapsManager boots the listener + manager pair used by the caps
// tests; the caller pulls the accepted connection off the channel.
func startCapsManager(t *testing.T) (net.Listener, *Manager, chan net.Conn) {
	t.Helper()
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
			serveIrcCaps(conn)
			conns <- conn
		}
	}()

	mgr := newCapsManager(t, ln.Addr().(*net.TCPAddr).Port)
	t.Cleanup(func() { mgr.Drop("u1", "net1") })
	return ln, mgr, conns
}

// seedCapsMember provisions the network + membership and connects.
func seedCapsMember(t *testing.T, mgr *Manager, port int) {
	t.Helper()
	if err := mgr.store.UpsertNetwork(&storage.Network{
		ID: "net1", ConnID: "irc://127.0.0.1", Name: "Fake",
		Host: "127.0.0.1", Port: port, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.store.UpsertMembership(&storage.Membership{
		UserID: "u1", NetworkID: "net1",
		Nick: "capsguy", Username: "c", Realname: "c",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mgr.EnsureConn("u1", "net1")
	waitForCond(t, func() bool { return len(mgr.ChannelMembers("u1", "net1", "#test")) >= 2 }, "roster to settle")
}

func newCapsManager(t *testing.T, port int) *Manager {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := &logSink{}
	gw := gateway.New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	mgr := New(store, gw, slog.New(sink), util.NewSnowflake(0, 0))
	mgr.reconnectBackoff = 150 * time.Millisecond
	return mgr
}

// waitForCond polls cond until it holds or the deadline trips.
func waitForCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}
