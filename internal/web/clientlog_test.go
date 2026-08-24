package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
)

func TestClientLogEndpoint(t *testing.T) {
	cfg := config.Default()
	srv := httptest.NewServer(Handler(cfg, testLogger()))
	defer srv.Close()

	body := `{"entries":[
		{"level":"error","message":"TypeError: Cannot read properties of undefined (reading 'forEach')","count":7,"href":"http://127.0.0.1:18084/channels/@me"},
		{"level":"warn","message":"[voidbar] selector undefined: function(){...}","count":1}
	]}`
	res, err := http.Post(srv.URL+"/voidbar/client-log", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status: %d", res.StatusCode)
	}

	// Bad JSON -> 400.
	res, err = http.Post(srv.URL+"/voidbar/client-log", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json status: %d", res.StatusCode)
	}
}
