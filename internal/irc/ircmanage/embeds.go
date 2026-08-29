package ircmanage

import (
	"bytes"
	"context"
	"image"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// imageExtensions: URL paths ending in these are treated as images, the
// same way Discord's server-side link unfurler works.
var imageExtensions = map[string]bool{
	".png": true, ".apng": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".webp": true, ".bmp": true,
}

// fetchClient bounds the unfurler: downloads with a hard deadline, so a
// slow remote never stalls a relay for long.
var fetchClient = &http.Client{Timeout: 8 * time.Second}

// maxMirrorBytes caps a mirrored link image.
const maxMirrorBytes = 10 << 20

// fetchImage downloads a linked image whole - or as much of it as the
// remote manages to send within the read budget: some image hosts
// (robohash among them) dribble the body without ever closing it
// cleanly, which used to burn the full client timeout and fail the
// unfurl. Partial reads are fine here: image headers live up front.
func fetchImage(u string) (data []byte, contentType string, w, h int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, 0
	}
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, "", 0, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", 0, 0
	}
	buf := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(buf, io.LimitReader(resp.Body, maxMirrorBytes+1))
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(readBudget):
		// Stall: take what arrived and abort the rest via the context.
		cancel()
		<-done
	}
	data = buf.Bytes()
	if len(data) == 0 || len(data) > maxMirrorBytes {
		return nil, "", 0, 0
	}
	contentType = resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", 0, 0
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h = cfg.Width, cfg.Height
	}
	return data, contentType, w, h
}

// readBudget bounds a single image download before the partial-read
// fallback kicks in.
const readBudget = 4 * time.Second

// imageAttachments mirrors direct image URLs of an IRC message into
// local storage and returns Discord attachment rows for them. Clients
// render attachment images through their upload pipeline (which works
// on third-party instances), while embed images empirically do not -
// the same official build shows blank embeds against Oldcord Staging
// too. Without a public origin (or when the remote is unreachable) no
// rows are produced: the URL stays a plain clickable link in the text.
func (m *Manager) imageAttachments(content string) []any {
	if m.publicURL == "" {
		return nil
	}
	var rows []any
	seen := map[string]bool{}
	for _, tok := range strings.Fields(content) {
		if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
			continue
		}
		u := strings.Trim(tok, "<>()[]\"',")
		pathPart := u
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			pathPart = u[:i]
		}
		lower := strings.ToLower(pathPart)
		dot := strings.LastIndex(lower, ".")
		if dot < 0 || !imageExtensions[lower[dot:]] {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		data, contentType, w, h := fetchImage(u)
		if data == nil {
			continue
		}
		att := &storage.Attachment{
			ID:          m.sf.New(),
			Filename:    mirrorFilename(pathPart, contentType),
			ContentType: contentType,
			Size:        len(data),
			Width:       w,
			Height:      h,
			UploadedAt:  time.Now().Unix(),
		}
		if err := m.store.PutAttachment(att, data); err != nil {
			m.log.Warn("link mirror store failed", "err", err, "url", u)
			continue
		}
		rows = append(rows, map[string]any{
			"id":           att.ID,
			"filename":     att.Filename,
			"size":         att.Size,
			"url":          m.publicURL + "/attachments/" + att.ID + "/" + att.Filename,
			"proxy_url":    m.publicURL + "/attachments/" + att.ID + "/" + att.Filename,
			"content_type": att.ContentType,
			"width":        att.Width,
			"height":       att.Height,
		})
		if len(rows) >= 4 {
			break
		}
	}
	return rows
}

// mirrorFilename names a mirrored image after its source path (or the
// content type when the path carries no usable name).
func mirrorFilename(u, contentType string) string {
	base := path.Base(u)
	if base == "" || base == "." || base == "/" || strings.ContainsAny(base, "\\") {
		base = ""
	}
	if base == "" {
		ext := ".png"
		if i := strings.Index(contentType, "/"); i > 0 {
			e := "." + contentType[i+1:]
			if e != "/" && len(e) > 1 {
				ext = e
			}
		}
		base = "image" + ext
	}
	return base
}
