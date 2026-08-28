package rest

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

// TestSendMessageMultipartPayloadJSON pins the web-client send shape: the
// message object packed into a payload_json form field (Flicker's send
// path doubles as the upload path) must decode - a 400 "invalid body"
// here is the send-ghost bug: the client gets no echo, its optimistic
// row never reconciles. Past the decode, an unknown channel id answers
// 404, which is this test's expected terminal state.
func TestSendMessageMultipartPayloadJSON(t *testing.T) {
	h := newServer(t, "open")
	token := registerAndLogin(t, h)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormField("payload_json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(`{"content":"multipart hello","nonce":"flicker-1","tts":false}`)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v9/channels/123/messages", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("multipart payload_json rejected at decode: %d %s", rec.Code, rec.Body.String())
	}
}
