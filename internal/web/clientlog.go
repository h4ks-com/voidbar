package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// clientLogEntry is one deduplicated console record from the browser.
type clientLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
	Count   int    `json:"count"`
	Stack   string `json:"stack,omitempty"`
	Href    string `json:"href,omitempty"`
}

// handleClientLog receives client-side console errors/warnings streamed by
// the loader, so debugging never requires copying from DevTools: everything
// lands in the same instance log as unknown_path and friends.
func handleClientLog(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Entries []clientLogEntry `json:"entries"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for i, e := range body.Entries {
			if i >= 100 {
				break
			}
			attrs := []any{
				"msg2", truncateStr(e.Message, 2000),
				"count", e.Count,
				"href", e.Href,
			}
			if e.Stack != "" {
				attrs = append(attrs, "stack", truncateStr(e.Stack, 2000))
			}
			if e.Level == "error" {
				log.Error("client_error", attrs...)
			} else {
				log.Warn("client_warn", attrs...)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
