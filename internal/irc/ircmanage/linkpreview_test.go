package ircmanage

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/gateway"
	"github.com/h4ks-com/voidbar/internal/storage"
	"github.com/h4ks-com/voidbar/internal/util"
)

func TestPreviewableLink(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"no links here", ""},
		{"look https://example.com/a?b=c yes", "https://example.com/a?b=c"},
		{"img https://example.com/a.png skip", ""},
		{"img https://example.com/a.png and https://example.com/page", "https://example.com/page"},
		{"bracketed <https://example.com/x>", "https://example.com/x"},
		{"ftp://example.com nope", ""},
		// ACTION/formatted lines wrap links in markdown.
		{"*waves at https://example.com/act*", "https://example.com/act"},
		{"**bold https://example.com/b** tail", "https://example.com/b"},
		{"_https://example.com/u_", "https://example.com/u"},
	}
	for _, c := range cases {
		if got := previewableLink(c.in); got != c.want {
			t.Errorf("previewableLink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePreview(t *testing.T) {
	page := `<!doctype html><html><head>
		<meta property="og:title" content="An Example Article">
		<meta property="og:description" content="A short description of the page.">
		<meta property="og:image" content="/static/og.png">
		<title>Fallback Title</title>
		</head><body>hello</body></html>`
	p := parsePreview([]byte(page), "https://example.org/article")
	if p == nil {
		t.Fatal("no preview parsed")
	}
	if p.title != "An Example Article" {
		t.Errorf("title = %q", p.title)
	}
	if p.description != "A short description of the page." {
		t.Errorf("description = %q", p.description)
	}
	if p.imageURL != "https://example.org/static/og.png" {
		t.Errorf("og:image not resolved absolute: %q", p.imageURL)
	}

	// No og: tags - <title> and meta description carry the preview.
	basic := `<html><head><title>Plain Page</title><meta name="description" content="plain desc"></head></html>`
	p = parsePreview([]byte(basic), "https://example.org/")
	if p == nil || p.title != "Plain Page" || p.description != "plain desc" {
		t.Fatalf("fallback parse: %+v", p)
	}

	// Nothing parseable - no preview, no embed.
	if p := parsePreview([]byte("<html><body>no head data</body></html>"), "https://example.org/x"); p != nil {
		t.Fatalf("expected nil preview, got %+v", p)
	}
}

// TestMessageUpdatePayloadFidelity pins the link-preview MESSAGE_UPDATE
// body: clients replace the message record wholesale, so the update of a
// reply carrying a link must keep the reply shape (message_reference,
// referenced_message, type 19) plus mentions, reactions and the own
// avatar - the bug was "preview lands, reply bar vanishes and the chip
// degrades to @invalid-user".
func TestMessageUpdatePayloadFidelity(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateUser(&storage.User{ID: "u1", Username: "self", Email: "s@x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserAvatar("u1", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNetwork(&storage.Network{ID: "net1", ConnID: "irc://t", Name: "Fake", Host: "h", Port: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMembership(&storage.Membership{UserID: "u1", NetworkID: "net1", Nick: "self", JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ch, err := store.EnsureChannel("net1", "#test", func() string { return "ch1" })
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := store.AppendMessage(storage.BufferedMessage{ID: "snowA", ChannelID: ch.ID, AuthorID: "irc:peer", AuthorName: "peer", Content: "original", Timestamp: ts}); err != nil {
		t.Fatal(err)
	}
	row := storage.BufferedMessage{
		ID: "snowB", ChannelID: ch.ID, AuthorID: "u1", AuthorName: "self",
		Content: "reply with https://example.org/page", Timestamp: ts,
		ReplyTo: "snowA",
		Mentions: []storage.MentionRef{{Nick: "peer", ID: "peerid1"}},
	}
	row.Reactions = map[string][]string{"eyes": {"u1"}}
	if err := store.AppendMessage(row); err != nil {
		t.Fatal(err)
	}
	// The replied-to peer has a mirrored avatar: the bar renders the
	// target's avatar straight from referenced_message.author.
	if err := store.PutPeerAvatar("u1", "net1", "peer", "peerhash9"); err != nil {
		t.Fatal(err)
	}
	stored, ok := store.MessageByID(ch.ID, "snowB")
	if !ok {
		t.Fatal("reply row missing")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(nil, nil, logger, nil, nil)
	manager := New(store, gw, logger, util.NewSnowflake(0, 0))
	// Own avatar rides as the hash (the create-path form); publicURL is
	// armed anyway to prove it does NOT turn into a ready-made URL.
	manager.SetPublicURL("http://vb.example/")

	payload := manager.messageUpdatePayload("u1", &stored)

	if payload["type"] != 19 {
		t.Errorf("type = %v, want 19", payload["type"])
	}
	ref, _ := payload["message_reference"].(map[string]any)
	if ref == nil || ref["message_id"] != "snowA" || ref["guild_id"] != "net1" {
		t.Errorf("message_reference = %v", payload["message_reference"])
	}
	inner, _ := payload["referenced_message"].(map[string]any)
	if inner == nil || inner["id"] != "snowA" {
		t.Errorf("referenced_message = %v", payload["referenced_message"])
	}
	if inner != nil {
		if _, has := inner["message_reference"]; has {
			t.Error("referenced_message must be depth-1 (no reference of its own)")
		}
		iau, _ := inner["author"].(map[string]any)
		if iau == nil || iau["avatar"] != "peerhash9" {
			t.Errorf("referenced author avatar = %v, want the peer's mirrored hash", inner["author"])
		}
	}
	if ms, _ := payload["mentions"].([]any); len(ms) != 1 {
		t.Errorf("mentions = %v, want the peer pill", payload["mentions"])
	}
	rc, _ := payload["reactions"].([]any)
	if len(rc) != 1 {
		t.Fatalf("reactions = %v", payload["reactions"])
	}
	if r, _ := rc[0].(map[string]any); r["me"] != true || r["count"] != 1 {
		t.Errorf("reaction = %v", rc[0])
	}
	au, _ := payload["author"].(map[string]any)
	if au == nil || au["avatar"] != "abc123" {
		t.Errorf("author avatar = %v, want the hash form (abc123)", payload["author"])
	}

	// A peer's plain message degrades to nothing: no reference, no
	// own-avatar patch - just the base shape.
	plain := storage.BufferedMessage{ID: "snowC", ChannelID: ch.ID, AuthorID: "irc:peer", AuthorName: "peer", Content: "plain", Timestamp: ts}
	p2 := manager.messageUpdatePayload("u1", &plain)
	if p2["type"] == 19 {
		t.Error("plain message must not carry a reply type")
	}
	if _, has := p2["message_reference"]; has {
		t.Error("plain message must not carry a reference")
	}
}
