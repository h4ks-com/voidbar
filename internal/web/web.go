// Package web serves Voidbar's own client loader. It deliberately contains no
// Discord assets: the loader downloads a frozen client build from a configured
// CDN mirror at runtime and patches it in the browser.
package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/h4ks-com/voidbar/internal/config"
)

//go:embed static/index.html static/loader.js static/loading.css
var files embed.FS

func Handler(cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /voidbar/config", handleConfig(cfg))
	mux.HandleFunc("GET /loader.js", serveStatic("static/loader.js", "application/javascript"))
	mux.HandleFunc("GET /loading.css", serveStatic("static/loading.css", "text/css"))
	mux.HandleFunc("GET /", serveStatic("static/index.html", "text/html"))
	return mux
}

type clientConfig struct {
	InstanceName string `json:"instance_name"`
	Gateway      string `json:"gateway"`
	CdnBase      string `json:"cdn_base"`
	Build        string `json:"build"`
	Html         string `json:"html"`
}

func handleConfig(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !cfg.Client.Enabled {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"client loader is disabled on this instance"}`))
			return
		}
		html := cfg.Client.Html
		if html == "" {
			html = "app.html"
		}
		writeJSON(w, http.StatusOK, clientConfig{
			InstanceName: "Voidbar",
			Gateway:      cfg.GatewayWSURL(),
			CdnBase:      strings.TrimSuffix(cfg.Client.CdnBase, "/"),
			Build:        cfg.Client.Build,
			Html:         html,
		})
	}
}

func serveStatic(path, contentType string) http.HandlerFunc {
	data, err := files.ReadFile(path)
	if err != nil {
		panic("web: embedded file missing: " + path)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
