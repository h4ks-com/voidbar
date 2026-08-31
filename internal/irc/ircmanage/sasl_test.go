package ircmanage

import (
	"bufio"
	"encoding/base64"
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

// TestSASLPlainConnect verifies the SASL PLAIN flow: a network with SASL
// credentials authenticates via AUTHENTICATE during CAP negotiation (the
// payload is the base64 of authzid\0authcid\0password), and the session
// proceeds to normal auto-joins afterwards.
func TestSASLPlainConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var sent []string
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		nick := ""
		w := func(s string) { _, _ = conn.Write([]byte(s)) }
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			mu.Lock()
			sent = append(sent, line)
			mu.Unlock()
			switch {
			case strings.HasPrefix(line, "CAP LS"):
				w("CAP * LS :sasl away-notify\r\n")
			case strings.HasPrefix(line, "CAP REQ"):
				req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
				w("CAP * ACK :" + req + "\r\n")
			case line == "AUTHENTICATE PLAIN":
				w("AUTHENTICATE +\r\n")
			case strings.HasPrefix(line, "AUTHENTICATE "):
				w(":fake 900 " + nick + " " + nick + "!u@h " + nick + " :You are now logged in\r\n")
				w(":fake 903 " + nick + " :SASL authentication successful\r\n")
			case strings.HasPrefix(line, "CAP END"):
				w(":fake 001 " + nick + " :Welcome\r\n")
			case strings.HasPrefix(line, "NICK") && nick == "":
				nick = strings.TrimPrefix(line, "NICK ")
			case strings.HasPrefix(line, "PING"):
				w("PONG" + line[4:] + "\r\n")
			case strings.HasPrefix(line, "JOIN"):
				ch := strings.TrimSpace(strings.TrimPrefix(line, "JOIN "))
				w(":" + nick + "!u@h JOIN " + ch + "\r\n")
				w(":fake 353 " + nick + " = " + ch + " :" + nick + "\r\n")
				w(":fake 366 " + nick + " " + ch + " :End of NAMES\r\n")
			case strings.HasPrefix(line, "WHO "):
				ch := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
				w(":fake 315 " + nick + " " + ch + " :End of WHO list\r\n")
			}
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
		Host: "127.0.0.1", Port: port,
		SASLUser: "vbtest", SASLPass: "hunter2",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{
		UserID: "u1", NetworkID: "net1",
		Nick: "sasluser", Username: "s", Realname: "s",
		AutoJoin: []string{"#test"}, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	waitLine := func(substr string) string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			for _, l := range sent {
				if strings.Contains(l, substr) {
					mu.Unlock()
					return l
				}
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("timed out waiting for wire line %q; sent:\n%s", substr, strings.Join(sent, "\n"))
		return ""
	}

	manager.EnsureConn("u1", "net1")

	// The handshake rode CAP negotiation and the payload matches the
	// network's credentials (skip the method-selection "PLAIN" line).
	authLine := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && authLine == "" {
		mu.Lock()
		for _, l := range sent {
			if strings.HasPrefix(l, "AUTHENTICATE ") && l != "AUTHENTICATE PLAIN" && l != "AUTHENTICATE +" {
				authLine = l
				break
			}
		}
		mu.Unlock()
		if authLine == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if authLine == "" {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no AUTHENTICATE payload on the wire; sent:\n%s", strings.Join(sent, "\n"))
	}
	b64 := strings.TrimPrefix(authLine, "AUTHENTICATE ")
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("payload not base64: %q", authLine)
	}
	parts := strings.Split(string(blob), "\x00")
	if len(parts) != 3 || parts[0] != "vbtest" || parts[1] != "vbtest" || parts[2] != "hunter2" {
		t.Fatalf("sasl payload = %q (parts %v)", blob, parts)
	}
	// And the session went on to auto-join.
	waitLine("JOIN #test")
}
