package ircmanage

import (
	"bufio"
	"bytes"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

// tinyPNG encodes a 2x2 red PNG for the avatar flows.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for i := range img.Pix {
		img.Pix[i] = []byte{255, 0, 0, 255}[i%4]
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestMetadataAvatarFlow covers the draft/metadata-2 avatar bridging:
// SUB after registration, an inbound peer METADATA notification fetched
// and mirrored locally (notifier + store), and the outbound SET for our
// own avatar.
func TestMetadataAvatarFlow(t *testing.T) {
	pngBytes := tinyPNG(t)
	avSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(avSrv.Close)

	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wantHash, err := store.PutAvatar(pngBytes, "image/png")
	if err != nil {
		t.Fatal(err)
	}

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
				w("CAP * LS :batch echo-message server-time draft/metadata-2\r\n")
			case strings.HasPrefix(line, "CAP REQ"):
				req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
				w("CAP * ACK :" + req + "\r\n")
			case strings.HasPrefix(line, "CAP END"):
				w(":fake 001 " + nick + " :Welcome\r\n")
			case strings.HasPrefix(line, "NICK") && nick == "":
				nick = strings.TrimPrefix(line, "NICK ")
			case strings.HasPrefix(line, "PING"):
				w("PONG" + line[4:] + "\r\n")
			case strings.HasPrefix(line, "METADATA * SUB"):
				// Spec: the subscription sticks; the peer's current value
				// arrives as a METADATA notification.
				w(":fake 770 " + nick + " avatar\r\n")
				w(":bob!u@h METADATA bob avatar * :" + avSrv.URL + "/avatar.png\r\n")
			}
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	if err := store.UpsertNetwork(&storage.Network{
		ID: "net1", ConnID: "irc://127.0.0.1", Name: "Fake",
		Host: "127.0.0.1", Port: port,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{
		UserID: "u1", NetworkID: "net1",
		Nick: "metatester", Username: "m", Realname: "m",
		JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	t.Cleanup(func() { manager.Drop("u1", "net1") })

	avatarNick := make(chan string, 4)
	manager.SetPeerAvatarNotifier(func(userID, networkID, nick string) {
		select {
		case avatarNick <- nick:
		default:
		}
	})

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
		t.Fatalf("timed out waiting for %q; sent:\n%s", substr, strings.Join(sent, "\n"))
		return ""
	}

	// Registration completes, the avatar subscription goes out.
	manager.EnsureConn("u1", "net1")
	if l := waitLine("METADATA * SUB avatar"); !strings.Contains(l, "avatar") {
		t.Fatalf("sub line: %q", l)
	}

	// The peer notification is fetched and mirrored.
	select {
	case nick := <-avatarNick:
		if nick != "bob" {
			t.Fatalf("notified nick: %q", nick)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("peer avatar notifier never fired")
	}
	if got := store.PeerAvatar("u1", "bob"); got != wantHash {
		t.Fatalf("peer avatar hash = %q, want %q", got, wantHash)
	}

	// Our own avatar SET reaches the wire.
	manager.SetAvatar("u1", "net1", "http://example.com/me.png")
	if l := waitLine("METADATA * SET avatar"); !strings.Contains(l, "http://example.com/me.png") {
		t.Fatalf("set line: %q", l)
	}
}
