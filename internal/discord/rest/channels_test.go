package rest

import (
	"net/http"
	"testing"
)

// TestChannelEndpoints exercises the channel-view surface the client hits
// when a guild is opened. Channel ids contain ':' and '#' (networkID + IRC
// channel), so this also pins URL handling for such ids.
func TestChannelEndpoints(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)
	channel := "1541425263529689088:#go"

	// History: empty array = start of channel.
	rec, out := doAny(t, h, "GET", "/api/v9/channels/"+channel+"/messages?limit=50", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages: %d", rec.Code)
	}
	if arr, ok := out.([]any); !ok || len(arr) != 0 {
		t.Fatalf("messages: %v", out)
	}

	// Pins.
	rec, out = doAny(t, h, "GET", "/api/v9/channels/"+channel+"/pins", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pins: %d", rec.Code)
	}
	if arr, ok := out.([]any); !ok || len(arr) != 0 {
		t.Fatalf("pins: %v", out)
	}

	// Read-state ack.
	rec, _ = do(t, h, "POST", "/api/v9/channels/"+channel+"/messages/123/ack", token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ack: %d", rec.Code)
	}
}
