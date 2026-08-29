package ircmanage

import (
	"testing"
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
