package rest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/core/network"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/irc/ircmanage"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func httpGet(url, token string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// TestCreateGuildAndGuildDetail exercises the connection-string-as-invite
// flow end to end on the REST surface. The IRC manager is wired but no real
// connection is attempted (no server configured for the host; EnsureConn
// spawns a goroutine that fails to connect and is harmless in tests).
func TestCreateGuildAndGuildDetail(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	// Create a network from a connection string (Discord's "Create server"
	// form posts {name: ...}).
	rec, out := do(t, h, "POST", "/api/v9/guilds", token, map[string]string{
		"name": "ircs://irc.libera.chat:6697/#go,#rust?name=Libera",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create guild: %d %v", rec.Code, out)
	}
	guildID, _ := out["id"].(string)
	if guildID == "" {
		t.Fatalf("guild id missing: %v", out)
	}

	// Duplicate join is idempotent: same connection string -> same guild.
	rec, out = do(t, h, "POST", "/api/v9/guilds", token, map[string]string{
		"name": "ircs://irc.libera.chat:6697/#other",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("rejoin: %d %v", rec.Code, out)
	}
	if out["id"] != guildID {
		t.Fatalf("rejoin returned different guild: %v vs %v", out["id"], guildID)
	}

	// Bad string -> 400.
	rec, out = do(t, h, "POST", "/api/v9/guilds", token, map[string]string{"name": "ftp://host"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad string: %d %v", rec.Code, out)
	}

	// Guild detail shows auto-join channels as Discord channels. The
	// re-join above merged #other into the same membership.
	rec, out = do(t, h, "GET", "/api/v9/guilds/"+guildID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("guild detail: %d %v", rec.Code, out)
	}
	channels, _ := out["channels"].([]any)
	if len(channels) != 3 {
		t.Fatalf("channels: %v", channels)
	}
	if name := channels[0].(map[string]any)["name"]; name != "go" {
		t.Fatalf("channel[0] name: %v", name)
	}
	if name := channels[2].(map[string]any)["name"]; name != "other" {
		t.Fatalf("merged channel name: %v", name)
	}
}

// TestGatewayGuildCreateFlow verifies that after a network is joined, the
// gateway READY carries unavailable stubs and GUILD_CREATE fills the rail.
func TestGatewayGuildCreateFlow(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	gw.SetGuildProviders(
		func(u string) ([]any, error) { return netSvc.ReadyGuildPayloads(u), nil },
		netSvc.GuildCreateForUser,
	)

	// Join a network first (no real IRC connection happens - the host has no
	// listener; EnsureConn just spawns a goroutine that fails to connect).
	if _, err := netSvc.Join(user.ID, "ircs://irc.libera.chat:6697/#go,#rust?name=Libera"); err != nil {
		t.Fatal(err)
	}

	h := New(svc, cfg, logger, gw, netSvc, manager)
	srv := httptest.NewServer(h)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway/?v=9&encoding=json"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	recvJSON := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	_ = recvJSON() // HELLO
	if err := conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}

	rdy := recvJSON()
	if rdy["t"] != "READY" {
		t.Fatalf("expected READY, got %v", rdy)
	}
	readyGuilds := rdy["d"].(map[string]any)["guilds"].([]any)
	if len(readyGuilds) != 1 {
		t.Fatalf("expected 1 ready guild, got %v", readyGuilds)
	}
	rg := readyGuilds[0].(map[string]any)
	if u := rg["unavailable"]; u != false {
		t.Fatalf("ready guild should be available: %v", rg)
	}
	// Wire format: channels is a flat array (Gateway Guild Object spec);
	// the client hydrates it into its internal versioned structure itself.
	if _, ok := rg["channels"].([]any); !ok {
		t.Fatalf("ready guild channels must be a flat array: %v", rg["channels"])
	}
	if _, ok := rg["guild_hashes"].(map[string]any); !ok {
		t.Fatalf("ready guild must carry guild_hashes: %v", rg)
	}

	create := recvJSON()
	if create["t"] != "GUILD_CREATE" {
		t.Fatalf("expected GUILD_CREATE after READY, got %v", create)
	}
	d := create["d"].(map[string]any)
	if d["name"] != "Libera" {
		t.Fatalf("guild name: %v", d["name"])
	}
	chans := d["channels"].([]any)
	if len(chans) != 2 {
		t.Fatalf("channels: %v", chans)
	}
	members := d["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members: %v", members)
	}
	if d["owner_id"] != model.ClydeID {
		t.Fatalf("owner must be Clyde, got %v", d["owner_id"])
	}
}
func TestJoinInvite(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	rec, out := do(t, h, "POST", "/api/v9/invites/ircs%3A%2F%2Firc.libera.chat%3A6697%2F%23go", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("join invite: %d %v", rec.Code, out)
	}
	guild, _ := out["guild"].(map[string]any)
	if guild == nil || guild["id"] == "" {
		t.Fatalf("guild: %v", out)
	}
}

// TestCreateDM: POST /users/@me/channels with a fellow member's user id
// opens a 1:1 channel (recipient resolved through the network's
// membership list); unknown ids are rejected; the channel then shows up
// in GET /users/@me/channels.
func TestCreateDM(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	buddy, _, err := svc.Register("ircbuddy", "ircbuddy@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	h := New(svc, cfg, logger, gw, netSvc, manager)

	const conn = "ircs://irc.libera.chat:6697/#go?name=Libera"
	if _, err := netSvc.Join(user.ID, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := netSvc.Join(buddy.ID, conn); err != nil {
		t.Fatal(err)
	}

	rec, out := do(t, h, "POST", "/api/v9/users/@me/channels", token, map[string]any{
		"recipient_id": buddy.ID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create dm: %d %v", rec.Code, out)
	}
	dm := out
	if dm["type"] != float64(1) {
		t.Fatalf("dm type: %v", dm["type"])
	}
	recipients, _ := dm["recipients"].([]any)
	if len(recipients) != 1 {
		t.Fatalf("recipients: %v", dm["recipients"])
	}
	peer, _ := recipients[0].(map[string]any)
	if peer["id"] != buddy.ID || peer["username"] != "ircbuddy" {
		t.Fatalf("peer: %v", peer)
	}

	// Same recipient returns the same channel (idempotent open).
	rec, out2 := do(t, h, "POST", "/api/v9/users/@me/channels", token, map[string]any{
		"recipient_id": buddy.ID,
	})
	if rec.Code != http.StatusOK || out2["id"] != dm["id"] {
		t.Fatalf("idempotent dm: %d %v vs %v", rec.Code, out2["id"], dm["id"])
	}

	rec, _ = do(t, h, "POST", "/api/v9/users/@me/channels", token, map[string]any{
		"recipient_id": "123456789012345678",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown recipient: %d", rec.Code)
	}

	rec, raw3 := doAny(t, h, "GET", "/api/v9/users/@me/channels", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list dms: %d", rec.Code)
	}
	list, _ := raw3.([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != dm["id"] {
		t.Fatalf("dm list: %v", raw3)
	}
}

// TestTypingBothWays: draft/typing TAGMSGs flow both directions -
// inbound TAGMSG (+typing=active) becomes a gateway TYPING_START, and
// POST /channels/{id}/typing relays out as @+typing=active TAGMSG (a
// message send follows up with "done"). The fake advertises ONLY
// message-tags (eris-style): draft/typing is a client-only tag and
// relay works without the capability being advertised.
func TestTypingBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	var mu sync.Mutex
	received := []string{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				nick := ""
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					mu.Lock()
					received = append(received, line)
					mu.Unlock()
					switch {
					case strings.HasPrefix(line, "CAP LS"):
						_, _ = conn.Write([]byte("CAP * LS :message-tags\r\n"))
					case strings.HasPrefix(line, "CAP REQ"):
						req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
						_, _ = conn.Write([]byte("CAP * ACK :" + req + "\r\n"))
					case strings.HasPrefix(line, "NICK") && nick == "":
						nick = strings.TrimPrefix(line, "NICK ")
						_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome\r\n"))
					case strings.HasPrefix(line, "PING"):
						_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
					case strings.HasPrefix(line, "JOIN"):
						ch := strings.TrimSpace(strings.TrimPrefix(line, "JOIN "))
						_, _ = conn.Write([]byte(":" + nick + "!u@h JOIN " + ch + "\r\n"))
						_, _ = conn.Write([]byte(":fake 353 " + nick + " = " + ch + " :" + nick + " sleepy\r\n"))
						_, _ = conn.Write([]byte(":fake 366 " + nick + " " + ch + " :End of NAMES\r\n"))
					case strings.HasPrefix(line, "WHO "):
						ch := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
						_, _ = conn.Write([]byte(":fake 315 " + nick + " " + ch + " :End of WHO list\r\n"))
					}
				}
			}(conn)
			conns <- conn
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	h := New(svc, cfg, logger, gw, netSvc, manager)
	srv := httptest.NewServer(h)
	defer srv.Close()

	net, err := netSvc.Join(user.ID, "irc://127.0.0.1:"+fmt.Sprint(port)+"/#test?name=Fake")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Drop(user.ID, net.ID) })

	// Resolve the Discord channel id for #test via the guild detail.
	deadline := time.Now().Add(5 * time.Second)
	var guildID, channelID string
	for time.Now().Before(deadline) && channelID == "" {
		guilds := []map[string]any{}
		if resp, err := httpGet(srv.URL+"/api/v9/users/@me/guilds", token); err == nil {
			_ = json.Unmarshal(resp, &guilds)
		}
		if len(guilds) > 0 {
			guildID, _ = guilds[0]["id"].(string)
		}
		if guildID != "" {
			if resp, err := httpGet(srv.URL+"/api/v9/guilds/"+guildID, token); err == nil {
				var detail map[string]any
				_ = json.Unmarshal(resp, &detail)
				if chans, ok := detail["channels"].([]any); ok {
					for _, c := range chans {
						cm := c.(map[string]any)
						if cm["name"] == "test" {
							channelID, _ = cm["id"].(string)
						}
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if channelID == "" {
		t.Fatal("channel never registered")
	}

	// Open a gateway session.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway/?v=9&encoding=json"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	recv := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		_ = json.Unmarshal(data, &p)
		return p
	}
	_ = recv() // HELLO
	if err := conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}
	for {
		p := recv()
		if p["t"] == "READY" {
			break
		}
	}

	// Inbound: sleepy starts typing in #test -> TYPING_START on the wire.
	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}
	waitForIRC := func(t *testing.T, prefix string) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for time.Now().Before(dl) {
			mu.Lock()
			for _, l := range received {
				if strings.HasPrefix(l, prefix) {
					mu.Unlock()
					return
				}
			}
			dump := append([]string{}, received...)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			_ = dump
		}
		mu.Lock()
		dump := append([]string{}, received...)
		mu.Unlock()
		t.Fatalf("fake never saw %q; got %q", prefix, dump)
	}
	waitForIRC(t, "WHO #test")
	if _, err := fake.Write([]byte("@+typing=active :sleepy!u@h TAGMSG #test\r\n")); err != nil {
		t.Fatal(err)
	}
	var typing map[string]any
	for {
		p := recv()
		if p["t"] == "TYPING_START" {
			typing, _ = p["d"].(map[string]any)
			break
		}
	}
	if typing["channel_id"] != channelID || typing["user_id"] != model.IrcAuthorID("irc:sleepy") || typing["type"] != float64(1) {
		t.Fatalf("typing payload: %v", typing)
	}
	if g, _ := typing["guild_id"].(string); g != guildID {
		t.Fatalf("typing guild: %v", typing["guild_id"])
	}

	// Outgoing: POST /typing relays @+typing=active TAGMSG #test.
	req, _ := http.NewRequest("POST", srv.URL+"/api/v9/channels/"+channelID+"/typing", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("typing POST: %d", resp.StatusCode)
	}
	waitForIRC(t, "@+typing=active TAGMSG #test")

	// A message send wraps up with the "done" tag before PRIVMSG.
	req, _ = http.NewRequest("POST", srv.URL+"/api/v9/channels/"+channelID+"/messages", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	waitForIRC(t, "@+typing=done TAGMSG #test")
	waitForIRC(t, "PRIVMSG #test")
}

// TestReactBothWays: draft/react TAGMSGs flow both directions through the
// msgid <-> snowflake registry. Inbound +reply/+draft/react becomes
// MESSAGE_REACTION_ADD; a REST reaction PUT on a message whose msgid we
// learned via echo-message relays out as +reply/+draft/react TAGMSG, and
// buffered history renders the accumulated reactions.
func TestReactBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	var mu sync.Mutex
	received := []string{}
	nextMsgid := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				nick := ""
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					mu.Lock()
					received = append(received, line)
					mu.Unlock()
					switch {
					case strings.HasPrefix(line, "CAP LS"):
						_, _ = conn.Write([]byte("CAP * LS :message-tags echo-message\r\n"))
					case strings.HasPrefix(line, "CAP REQ"):
						req := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "CAP REQ")), ":")
						_, _ = conn.Write([]byte("CAP * ACK :" + req + "\r\n"))
					case strings.HasPrefix(line, "NICK") && nick == "":
						nick = strings.TrimPrefix(line, "NICK ")
						_, _ = conn.Write([]byte(":fake 001 " + nick + " :Welcome\r\n"))
					case strings.HasPrefix(line, "PING"):
						_, _ = conn.Write([]byte("PONG" + line[4:] + "\r\n"))
					case strings.HasPrefix(line, "JOIN"):
						ch := strings.TrimSpace(strings.TrimPrefix(line, "JOIN "))
						_, _ = conn.Write([]byte(":" + nick + "!u@h JOIN " + ch + "\r\n"))
						_, _ = conn.Write([]byte(":fake 353 " + nick + " = " + ch + " :" + nick + " sleepy\r\n"))
						_, _ = conn.Write([]byte(":fake 366 " + nick + " " + ch + " :End of NAMES\r\n"))
					case strings.HasPrefix(line, "WHO "):
						ch := strings.TrimSpace(strings.TrimPrefix(line, "WHO "))
						_, _ = conn.Write([]byte(":fake 315 " + nick + " " + ch + " :End of WHO list\r\n"))
					case strings.HasPrefix(line, "PRIVMSG"):
						// echo-message + msgid: our own message comes back
						// stamped, which is how the registry binds it.
						mu.Lock()
						nextMsgid++
						id := fmt.Sprintf("M%d", nextMsgid)
						mu.Unlock()
						_, _ = conn.Write([]byte("@msgid=" + id + " :" + nick + "!u@h " + line + "\r\n"))
					}
				}
			}(conn)
			conns <- conn
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger, nil, nil)
	manager := ircmanage.New(store, gw, logger, util.NewSnowflake(0, 0))
	netSvc := network.NewService(store, gw, util.NewSnowflake(0, 0), manager, nil)
	h := New(svc, cfg, logger, gw, netSvc, manager)
	srv := httptest.NewServer(h)
	defer srv.Close()

	net, err := netSvc.Join(user.ID, "irc://127.0.0.1:"+fmt.Sprint(port)+"/#test?name=Fake")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Drop(user.ID, net.ID) })

	deadline := time.Now().Add(5 * time.Second)
	var channelID string
	for time.Now().Before(deadline) && channelID == "" {
		guilds := []map[string]any{}
		if resp, err := httpGet(srv.URL+"/api/v9/users/@me/guilds", token); err == nil {
			_ = json.Unmarshal(resp, &guilds)
		}
		if len(guilds) > 0 {
			if resp, err := httpGet(srv.URL+"/api/v9/guilds/"+guilds[0]["id"].(string), token); err == nil {
				var detail map[string]any
				_ = json.Unmarshal(resp, &detail)
				if chans, ok := detail["channels"].([]any); ok {
					for _, c := range chans {
						if cm := c.(map[string]any); cm["name"] == "test" {
							channelID, _ = cm["id"].(string)
						}
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if channelID == "" {
		t.Fatal("channel never registered")
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway/?v=9&encoding=json"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	recv := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		_ = json.Unmarshal(data, &p)
		return p
	}
	_ = recv() // HELLO
	if err := conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}
	for {
		p := recv()
		if p["t"] == "READY" {
			break
		}
	}

	fake, ok := <-conns
	if !ok {
		t.Fatal("fake server saw no connection")
	}
	waitForIRCContains := func(t *testing.T, needle string) {
		t.Helper()
		dl := time.Now().Add(5 * time.Second)
		for time.Now().Before(dl) {
		mu.Lock()
		for _, l := range received {
			if strings.Contains(l, needle) {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	dump := append([]string{}, received...)
	mu.Unlock()
	t.Fatalf("fake never saw %q; got %q", needle, dump)
}
	waitForIRCContains(t, "WHO #test")

	// A foreign message with a msgid arrives; sleepy then reacts to it.
	if _, err := fake.Write([]byte("@msgid=F1 :sleepy!u@h PRIVMSG #test :react to me\r\n")); err != nil {
		t.Fatal(err)
	}
	var foreignID string
	for {
		p := recv()
		if p["t"] == "MESSAGE_CREATE" {
			if d, _ := p["d"].(map[string]any); d["author"].(map[string]any)["username"] == "sleepy" {
				foreignID, _ = d["id"].(string)
				break
			}
		}
	}
	if _, err := fake.Write([]byte("@+reply=F1;+draft/react=\U0001F44D :sleepy!u@h TAGMSG #test\r\n")); err != nil {
		t.Fatal(err)
	}
	for {
		p := recv()
		if p["t"] == "MESSAGE_REACTION_ADD" {
			d, _ := p["d"].(map[string]any)
			if d["message_id"] != foreignID || d["user_id"] != model.IrcAuthorID("irc:sleepy") {
				t.Fatalf("reaction add payload: %v", d)
			}
			if name := d["emoji"].(map[string]any)["name"]; name != "\U0001F44D" {
				t.Fatalf("reaction emoji: %v", name)
			}
			break
		}
	}

	// Own message: its echo (msgid F2) binds the REST snowflake, so a
	// reaction PUT can reference it upstream.
	reqBody := `{"content":"mine"}`
	req, _ := http.NewRequest("POST", srv.URL+"/api/v9/channels/"+channelID+"/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	_ = resp.Body.Close()
	ownID, _ := sent["id"].(string)
	waitForIRCContains(t, "PRIVMSG #test") // girc omits the ":" for one-word trailing params
	time.Sleep(300 * time.Millisecond) // let the echo bind msgid -> snowflake

	emojiPath := "%E2%98%BA" // ☺
	put, _ := http.NewRequest("PUT", srv.URL+"/api/v9/channels/"+channelID+"/messages/"+ownID+"/reactions/"+emojiPath+"/@me", nil)
	put.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reaction PUT: %d", resp.StatusCode)
	}
	// girc renders tags in map order; assert both on one line.
	waitForIRCContains(t, "+draft/react="+"\u263A")
	mu.Lock()
	reactLine := ""
	for _, l := range received {
		if strings.Contains(l, "TAGMSG #test") && strings.Contains(l, "+draft/react="+"\u263A") && strings.Contains(l, "+reply=M1") {
			reactLine = l
		}
	}
	mu.Unlock()
	if reactLine == "" {
		t.Fatal("no react TAGMSG with both +reply=M1 and +draft/react")
	}

	// Buffered history renders the accumulated state (me flag included).
	hist, err := httpGet(srv.URL+"/api/v9/channels/"+channelID+"/messages?limit=50", token)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []map[string]any{}
	_ = json.Unmarshal(hist, &msgs)
	var ownReactions []any
	for _, m := range msgs {
		if m["id"] == ownID {
			ownReactions, _ = m["reactions"].([]any)
		}
	}
	if len(ownReactions) != 1 {
		t.Fatalf("own message reactions: %v", ownReactions)
	}
	rc := ownReactions[0].(map[string]any)
	if rc["count"] != float64(1) || rc["me"] != true || rc["emoji"].(map[string]any)["name"] != "\u263A" {
		t.Fatalf("own reaction aggregate: %v", rc)
	}

	// An upstream unreact removes the pill.
	if _, err := fake.Write([]byte("@+reply=F1;+draft/unreact=\U0001F44D :sleepy!u@h TAGMSG #test\r\n")); err != nil {
		t.Fatal(err)
	}
	for {
		p := recv()
		if p["t"] == "MESSAGE_REACTION_REMOVE" {
			d, _ := p["d"].(map[string]any)
			if d["message_id"] != foreignID {
				t.Fatalf("reaction remove payload: %v", d)
			}
			break
		}
	}
}
