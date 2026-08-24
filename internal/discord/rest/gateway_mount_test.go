package rest

import (
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
	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func TestGatewayThroughRESTRouter(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Default()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.New(store, util.NewSnowflake(0, 0), "open")
	_, token, err := svc.Register("doesnm", "doesnm@0ut0f.space", "hunter2hunter2", "")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(svc, cfg, logger)
	h := New(svc, cfg, logger, gw)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Match how real clients connect: GATEWAY_ENDPOINT + "/?v=9&encoding=json"
	// (zlib-stream compression is covered by the gateway package tests).
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway/?v=9&encoding=json"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Op int `json:"op"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Op != 10 {
		t.Fatalf("hello: %s", data)
	}

	if err := conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var rdy struct {
		Op int    `json:"op"`
		T  string `json:"t"`
	}
	if err := json.Unmarshal(data, &rdy); err != nil || rdy.Op != 0 || rdy.T != "READY" {
		t.Fatalf("ready: %s", data)
	}
}
