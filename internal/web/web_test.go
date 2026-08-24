package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h4ks-com/voidbar/internal/config"
)

func TestConfigEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = true
	cfg.Client.CdnBase = "https://archive.org/download/voidbar-client/"
	cfg.Client.Build = "january_15_2022"
	cfg.Server.PublicURL = "https://voidbar.example.com"

	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/voidbar/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", res.StatusCode)
	}
	var out clientConfig
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Gateway != "wss://voidbar.example.com/gateway" {
		t.Fatalf("gateway: %q", out.Gateway)
	}
	if out.CdnBase != "https://archive.org/download/voidbar-client" {
		t.Fatalf("cdn_base: %q", out.CdnBase)
	}
	if out.Build != "january_15_2022" {
		t.Fatalf("build: %q", out.Build)
	}
}

func TestConfigEndpointDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Client.Enabled = false

	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/voidbar/config")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", res.StatusCode)
	}
}

func TestStaticFiles(t *testing.T) {
	cfg := config.Default()
	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()

	cases := []struct {
		path, contentType, marker string
	}{
		{"/", "text/html", "voidbar-loading"},
		{"/loader.js", "application/javascript", "GLOBAL_ENV"},
		{"/loading.css", "text/css", "voidbar-loading-bar"},
	}
	for _, tc := range cases {
		res, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, res.StatusCode)
		}
		if got := res.Header.Get("Content-Type"); got != tc.contentType {
			t.Fatalf("%s: content-type %q", tc.path, got)
		}
		defer res.Body.Close()
		buf := make([]byte, 256*1024)
		n, _ := res.Body.Read(buf)
		body := string(buf[:n])
		if !strings.Contains(body, tc.marker) {
			t.Fatalf("%s: marker %q not found", tc.path, tc.marker)
		}
	}
}
