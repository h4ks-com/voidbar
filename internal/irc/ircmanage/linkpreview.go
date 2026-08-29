package ircmanage

import (
	"bytes"
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

// linkPreview is the unfurled essence of a page, cached per URL so a
// reposted link never refetches (Discord's media proxy caches 30 min;
// so do we).
type linkPreview struct {
	title       string
	description string
	imageURL    string // original og:image
	mirrorURL   string // bouncer-local mirror of the image, when armed
	at          time.Time
}

const (
	previewCacheTTL   = 30 * time.Minute
	maxPreviewPage    = 1 << 20 // pages bigger than this lose their tail
	previewReadBudget = 4 * time.Second
	titleMax          = 500
	snippetMax        = 500
)

// previewCache is process-local: previews are advisory decoration, they
// do not need to survive restarts. Negative results are cached too, so
// dead links are not retried per repost.
var (
	previewMu    sync.Mutex
	previewCache = map[string]*linkPreview{}

	contentRe = regexp.MustCompile(`(?i)content\s*=\s*["']([^"']*)["']`)
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaRe    = map[string]*regexp.Regexp{}
	metaOnce  sync.Once
)

func metaTagRe(key string) *regexp.Regexp {
	metaOnce.Do(func() {
		for _, k := range []string{"og:title", "og:description", "description", "og:image", "og:site_name"} {
			metaRe[k] = regexp.MustCompile(`(?i)<meta[^>]+(?:property|name)\s*=\s*["']` + regexp.QuoteMeta(k) + `["'][^>]*>`)
		}
	})
	return metaRe[key]
}

// previewableLink returns the first http(s) URL of the message that is
// not a direct image (those already arrive as mirrored attachments).
func previewableLink(content string) string {
	for _, tok := range strings.Fields(content) {
		// Angle-bracketed URLs (some clients paste them that way) need
		// the delimiters stripped before the scheme check.
		tok = strings.Trim(tok, "<>")
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		u := strings.Trim(tok, "()[]\"',")
		pathPart := u
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			pathPart = u[:i]
		}
		lower := strings.ToLower(pathPart)
		dot := strings.LastIndex(lower, ".")
		if dot >= 0 && imageExtensions[lower[dot:]] {
			continue
		}
		if len(u) > 2048 {
			continue
		}
		return u
	}
	return ""
}

// fetchPage downloads (a bounded prefix of) a page for preview parsing.
func fetchPage(u string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "voidbar-link-preview/1.0")
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(ct, "html") && !strings.Contains(ct, "xml") && !strings.Contains(ct, "text/plain") {
		return nil
	}
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, io.LimitReader(resp.Body, maxPreviewPage+1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(previewReadBudget):
		// Slow dribble: <head> lives up front, take what came.
		cancel()
		<-done
	}
	return buf.Bytes()
}

// previewFor fetches (and caches) the preview of a URL. With the public
// origin armed the og:image is mirrored into local storage, so embed
// thumbnails load from the bouncer only.
func (m *Manager) previewFor(u string) *linkPreview {
	previewMu.Lock()
	if p, ok := previewCache[u]; ok && time.Since(p.at) < previewCacheTTL {
		previewMu.Unlock()
		return p
	}
	previewMu.Unlock()

	p := parsePreview(fetchPage(u), u)
	if p == nil {
		p = &linkPreview{}
	}
	p.at = time.Now()
	if p.imageURL != "" && m.publicURL != "" {
		if data, contentType, w, h := fetchImage(p.imageURL); data != nil {
			att := &storage.Attachment{
				ID:          m.sf.New(),
				Filename:    mirrorFilename(p.imageURL, contentType),
				ContentType: contentType,
				Size:        len(data),
				Width:       w,
				Height:      h,
				UploadedAt:  time.Now().Unix(),
			}
			if err := m.store.PutAttachment(att, data); err != nil {
				m.log.Warn("preview mirror store failed", "err", err, "url", p.imageURL)
			} else {
				p.mirrorURL = m.publicURL + "/attachments/" + att.ID + "/" + att.Filename
			}
		}
	}

	previewMu.Lock()
	previewCache[u] = p
	previewMu.Unlock()
	return p
}

// parsePreview pulls title/description/og:image out of an HTML prefix.
// Regex scraping is deliberately dumb: <head> meta is a flat format and
// this keeps the dependency tree empty.
func parsePreview(body []byte, pageURL string) *linkPreview {
	if len(body) == 0 {
		return nil
	}
	text := string(body)
	head := text
	if i := strings.Index(strings.ToLower(text), "</head>"); i > 0 {
		head = text[:i]
	}
	meta := func(key string) string {
		m := metaTagRe(key).FindString(head)
		if m == "" {
			return ""
		}
		cm := contentRe.FindStringSubmatch(m)
		if len(cm) > 1 {
			return strings.TrimSpace(html.UnescapeString(cm[1]))
		}
		return ""
	}
	title := meta("og:title")
	if title == "" {
		if sm := titleRe.FindStringSubmatch(head); len(sm) > 1 {
			title = strings.TrimSpace(html.UnescapeString(sm[1]))
		}
	}
	description := meta("og:description")
	if description == "" {
		description = meta("description")
	}
	image := meta("og:image")
	if image != "" {
		if base, err := url.Parse(pageURL); err == nil {
			if ref, err := url.Parse(image); err == nil {
				image = base.ResolveReference(ref).String()
			}
		}
	}
	if title == "" && description == "" {
		return nil
	}
	if len(title) > titleMax {
		title = title[:titleMax]
	}
	if len(description) > snippetMax {
		description = description[:snippetMax]
	}
	return &linkPreview{title: title, description: description, imageURL: image}
}

// previewEmbed renders the Discord embed for a fetched preview. Text
// embeds render on the official client against third-party instances
// (only embed images are broken there - the container and text show);
// the thumbnail rides our mirror, so image-capable clients (Flicker)
// show it without touching the origin.
func previewEmbed(p *linkPreview, pageURL string) map[string]any {
	out := map[string]any{
		"type": "link",
		"url":  pageURL,
	}
	if p.title != "" {
		out["title"] = p.title
	}
	if p.description != "" {
		out["description"] = p.description
	}
	if host := hostOf(pageURL); host != "" {
		out["provider"] = map[string]any{"name": host}
	}
	if p.mirrorURL != "" {
		out["thumbnail"] = map[string]any{"url": p.mirrorURL, "proxy_url": p.mirrorURL}
	}
	return out
}

// hostOf is the embed provider label.
func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return ""
}

// SendLinkPreview is the REST-side entry: own sends unfurl too (Discord
// embeds your own pasted links the same way).
func (m *Manager) SendLinkPreview(userID, channelID, msgID, content string) {
	m.sendLinkPreview(userID, channelID, msgID, content)
}

// buildMessagePayloadFromRow renders the MESSAGE_UPDATE body for either
// author kind: peers hash their "irc:<nick>" id, own sends carry the
// real user id and keep their nonce.
func buildMessagePayloadFromRow(row *storage.BufferedMessage) map[string]any {
	authorID := row.AuthorID
	authorName := row.AuthorName
	if strings.HasPrefix(authorID, "irc:") {
		authorName = strings.TrimPrefix(authorID, "irc:")
		authorID = model.IrcAuthorID(row.AuthorID)
	}
	payload := map[string]any{
		"id":               row.ID,
		"channel_id":       row.ChannelID,
		"content":          row.Content,
		"timestamp":        row.Timestamp,
		"edited_timestamp": nil,
		"tts":              false,
		"mention_everyone": false,
		"mentions":         []any{},
		"mention_roles":    []any{},
		"mention_channels": []any{},
		"attachments":      []any{},
		"embeds":           []any{},
		"reactions":        []any{},
		"nonce":            row.Nonce,
		"pinned":           false,
		"type":             0,
		"flags":            0,
		"author": map[string]any{
			"id":            authorID,
			"username":      authorName,
			"discriminator": "0",
			"bot":           false,
		},
	}
	return payload
}

// sendLinkPreview unfurls the first previewable link of a freshly
// relayed message and delivers it as a Discord-side edit: the message
// itself is already on its way (obbies architecture - preview follows
// later, or never). The buffered row gains the embeds, so history and
// reconnects render the preview too.
func (m *Manager) sendLinkPreview(userID, channelID, msgID, content string) {
	link := previewableLink(content)
	if link == "" {
		return
	}
	go func() {
		p := m.previewFor(link)
		if p.title == "" && p.description == "" {
			return
		}
		embeds := []any{previewEmbed(p, link)}
		row, err := m.store.SetMessageEmbeds(channelID, msgID, embeds)
		if err != nil {
			m.log.Warn("preview persist failed", "err", err, "channel", channelID, "msg", msgID)
			return
		}
		payload := buildMessagePayloadFromRow(row)
		payload["embeds"] = embeds
		if len(row.Attachments) > 0 {
			payload["attachments"] = row.Attachments
		}
		m.gw.Dispatch(userID, "MESSAGE_UPDATE", payload)
	}()
}
