package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/h4ks-com/voidbar/internal/config"
	"github.com/h4ks-com/voidbar/internal/discord/auth"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func newTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	cfg.Server.PublicURL = "http://voidbar.test"
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	user, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := New(svc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	return gw, token, user.ID
}

func startHTTP(t *testing.T, gw *Server) string {
	t.Helper()
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv.URL
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(url, "http") + "/gateway"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("bad frame: %s", data)
	}
	return p
}

func expectClose(t *testing.T, conn *websocket.Conn, codes ...int) {
	t.Helper()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if websocket.IsCloseError(err, codes...) {
			return
		}
		t.Fatalf("expected close %v, got %v", codes, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func identifyFlow(t *testing.T, gw *Server, token string) (*websocket.Conn, string, float64) {
	t.Helper()
	conn := dial(t, startHTTP(t, gw))
	hello := recv(t, conn)
	if hello["op"].(float64) != OpHello {
		t.Fatalf("expected HELLO, got %v", hello)
	}
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{
		"token":      token,
		"properties": map[string]any{"os": "linux"},
	}})
	rdy := recv(t, conn)
	if rdy["op"].(float64) != OpDispatch || rdy["t"] != "READY" {
		t.Fatalf("expected READY, got %v", rdy)
	}
	d := rdy["d"].(map[string]any)
	sid, _ := d["session_id"].(string)
	if sid == "" {
		t.Fatal("empty session_id")
	}
	return conn, sid, rdy["s"].(float64)
}

func TestHelloIdentifyReadyHeartbeat(t *testing.T) {
	gw, token, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))

	hello := recv(t, conn)
	if hello["op"].(float64) != OpHello {
		t.Fatalf("op: %v", hello)
	}
	hd := hello["d"].(map[string]any)
	if hd["heartbeat_interval"].(float64) <= 0 {
		t.Fatalf("heartbeat_interval: %v", hd)
	}

	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": token}})
	rdy := recv(t, conn)
	if rdy["t"] != "READY" || rdy["s"].(float64) != 1 {
		t.Fatalf("ready: %v", rdy["t"])
	}
	d := rdy["d"].(map[string]any)
	if d["v"].(float64) != 9 {
		t.Fatalf("v: %v", d["v"])
	}
	user := d["user"].(map[string]any)
	if user["username"] != "doesnm" || user["id"] == nil {
		t.Fatalf("user: %v", user)
	}
	if guilds := d["guilds"].([]any); len(guilds) != 0 {
		t.Fatalf("guilds: %v", guilds)
	}
	if !strings.HasSuffix(d["resume_url"].(string), "/gateway") {
		t.Fatalf("resume_url: %v", d["resume_url"])
	}
	if _, ok := d["user_settings"].(map[string]any); !ok {
		t.Fatal("missing user_settings")
	}

	send(t, conn, map[string]any{"op": OpHeartbeat, "d": nil})
	ack := recv(t, conn)
	if ack["op"].(float64) != OpHeartbeatACK {
		t.Fatalf("ack: %v", ack)
	}
}

func TestIdentifyBadToken(t *testing.T) {
	gw, _, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": "garbage"}})
	expectClose(t, conn, CloseAuthenticationFailed)
}

func TestIdentifyBotPrefixToken(t *testing.T) {
	gw, token, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": "Bot " + token}})
	rdy := recv(t, conn)
	if rdy["t"] != "READY" {
		t.Fatalf("expected READY, got %v", rdy)
	}
}

func TestDoubleIdentify(t *testing.T) {
	gw, token, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": token}})
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": token}})
	expectClose(t, conn, CloseAlreadyAuthenticated)
}

func TestUnknownOpcode(t *testing.T) {
	gw, _, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": 42, "d": nil})
	expectClose(t, conn, CloseUnknownOpcode)
}

func TestUnauthenticatedCommand(t *testing.T) {
	gw, _, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpPresenceUpdate, "d": map[string]any{}})
	expectClose(t, conn, CloseNotAuthenticated)
}

func TestResumeReplaysOfflineEvents(t *testing.T) {
	gw, token, userID := newTestServer(t)
	conn, sid, seq := identifyFlow(t, gw, token)
	_ = conn.Close()

	waitFor(t, func() bool {
		gw.mu.RLock()
		defer gw.mu.RUnlock()
		sess := gw.sessions[sid]
		return sess != nil && sess.offline()
	})

	gw.Dispatch(userID, "TEST_EVENT", map[string]any{"hello": "world"})
	gw.Dispatch(userID, "TEST_EVENT2", map[string]any{"n": 1})

	conn2 := dial(t, startHTTP(t, gw))
	recv(t, conn2)
	send(t, conn2, map[string]any{"op": OpResume, "d": map[string]any{
		"token":      token,
		"session_id": sid,
		"seq":        seq,
	}})

	ev := recv(t, conn2)
	if ev["t"] != "TEST_EVENT" || ev["s"].(float64) != 2 {
		t.Fatalf("replayed event: %v", ev)
	}
	if ev["d"].(map[string]any)["hello"] != "world" {
		t.Fatalf("event payload: %v", ev)
	}
	ev2 := recv(t, conn2)
	if ev2["t"] != "TEST_EVENT2" || ev2["s"].(float64) != 3 {
		t.Fatalf("replayed event2: %v", ev2)
	}
	res := recv(t, conn2)
	if res["t"] != "RESUMED" || res["s"].(float64) != 4 {
		t.Fatalf("resumed: %v", res)
	}
	if res["d"] != nil {
		t.Fatalf("resumed d should be null: %v", res["d"])
	}
	send(t, conn2, map[string]any{"op": OpHeartbeat, "d": nil})
	if ack := recv(t, conn2); ack["op"].(float64) != OpHeartbeatACK {
		t.Fatalf("ack after resume: %v", ack)
	}
}

func TestResumeUnknownSession(t *testing.T) {
	gw, token, _ := newTestServer(t)
	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpResume, "d": map[string]any{
		"token":      token,
		"session_id": "bogus",
		"seq":        1,
	}})
	inv := recv(t, conn)
	if inv["op"].(float64) != OpInvalidSession || inv["d"] != false {
		t.Fatalf("invalid session: %v", inv)
	}
	send(t, conn, map[string]any{"op": OpIdentify, "d": map[string]any{"token": token}})
	rdy := recv(t, conn)
	if rdy["t"] != "READY" {
		t.Fatalf("re-identify after invalid session: %v", rdy)
	}
}

func TestResumeWrongUser(t *testing.T) {
	gw, token, _ := newTestServer(t)
	_, sid, _ := identifyFlow(t, gw, token)

	_, token2, err := gw.auth.Register("mattf", "mattf@x.io", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}

	conn := dial(t, startHTTP(t, gw))
	recv(t, conn)
	send(t, conn, map[string]any{"op": OpResume, "d": map[string]any{
		"token":      token2,
		"session_id": sid,
		"seq":        1,
	}})
	inv := recv(t, conn)
	if inv["op"].(float64) != OpInvalidSession {
		t.Fatalf("expected INVALID_SESSION, got %v", inv)
	}
}

func TestDispatchToMultipleSessions(t *testing.T) {
	gw, token, userID := newTestServer(t)
	connA, _, _ := identifyFlow(t, gw, token)
	connB, _, _ := identifyFlow(t, gw, token)

	gw.Dispatch(userID, "PING", map[string]any{"x": 1})
	for _, conn := range []*websocket.Conn{connA, connB} {
		ev := recv(t, conn)
		if ev["t"] != "PING" {
			t.Fatalf("dispatch: %v", ev)
		}
	}
}

func TestZlibStreamCompression(t *testing.T) {
	gw, token, _ := newTestServer(t)
	url := startHTTP(t, gw)
	wsURL := "ws" + strings.TrimPrefix(url, "http") + "/gateway?encoding=json&v=9&compress=zlib-stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// zlib-stream is a single continuous stream across frames, so keep one
	// reader and one JSON decoder alive for the whole connection. The reader
	// is created lazily: an empty stream has no zlib header yet.
	stream := &bytes.Buffer{}
	var dec *json.Decoder
	nextJSON := func() map[string]any {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("expected binary frame, got %d", mt)
		}
		stream.Write(data)
		if dec == nil {
			zr, err := zlib.NewReader(stream)
			if err != nil {
				t.Fatalf("zlib reader: %v", err)
			}
			dec = json.NewDecoder(zr)
		}
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return v
	}

	if hello := nextJSON(); hello["op"].(float64) != OpHello {
		t.Fatalf("hello: %v", hello)
	}

	if err := conn.WriteJSON(map[string]any{"op": OpIdentify, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}
	if rdy := nextJSON(); rdy["t"] != "READY" {
		t.Fatalf("ready: %v", rdy["t"])
	}
}

func TestResumeTakesOverOldConnection(t *testing.T) {
	gw, token, _ := newTestServer(t)
	conn1, sid, seq := identifyFlow(t, gw, token)

	conn2 := dial(t, startHTTP(t, gw))
	recv(t, conn2)
	send(t, conn2, map[string]any{"op": OpResume, "d": map[string]any{
		"token":      token,
		"session_id": sid,
		"seq":        seq,
	}})
	res := recv(t, conn2)
	if res["t"] != "RESUMED" {
		t.Fatalf("resumed: %v", res)
	}
	expectClose(t, conn1, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived)
}
