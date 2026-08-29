package rest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/h4ks-com/voidbar/internal/storage"
)

// maxUploadBytes bounds a single uploaded file (Discord's default tier
// is 10 MiB; a little headroom keeps large pastes working).
const maxUploadBytes = 100 << 20

// randomToken returns a URL-safe secret for upload slots.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sanitizeFilename keeps the stored/served name a single clean path
// segment (client filenames may carry paths or separators).
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// attachmentURL is the public CDN-style address of a stored file. It is
// unauthenticated on purpose: the same URL is what IRC peers receive on
// the wire, and they hold no Discord token.
func (s *Server) attachmentURL(id, filename string) string {
	base := strings.TrimSuffix(s.cfg.Server.PublicURL, "/")
	return base + "/attachments/" + id + "/" + filename
}

// attachmentPayload renders the Discord attachment object for a stored
// file; the client's row id (e.g. "0") is echoed, not the storage id.
func (s *Server) attachmentPayload(rowID string, att *storage.Attachment) map[string]any {
	url := s.attachmentURL(att.ID, att.Filename)
	out := map[string]any{
		"id":           rowID,
		"filename":     att.Filename,
		"size":         att.Size,
		"url":          url,
		"proxy_url":    url,
		"content_type": att.ContentType,
	}
	if att.Width > 0 && att.Height > 0 {
		out["width"] = att.Width
		out["height"] = att.Height
	}
	return out
}

// sniffDims fills width/height for decodable image types.
func sniffDims(att *storage.Attachment, data []byte) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		att.Width, att.Height = cfg.Width, cfg.Height
	}
}

// handleCreateAttachments serves POST /channels/{c}/attachments: mints
// upload slots for the cloud-upload flow (the official client uploads
// bytes via PUT to the returned upload_url, then references
// upload_filename in the message send).
func (s *Server) handleCreateAttachments(w http.ResponseWriter, r *http.Request, u *storage.User) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	var body struct {
		Files []struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
			FileSize int64  `json:"file_size"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(body.Files) == 0 || len(body.Files) > 10 {
		jsonError(w, http.StatusBadRequest, "files must contain 1-10 entries")
		return
	}
	channelID := r.PathValue("channel")
	base := strings.TrimSuffix(s.cfg.Server.PublicURL, "/")
	out := make([]any, 0, len(body.Files))
	for _, f := range body.Files {
		if f.FileSize <= 0 || f.FileSize > maxUploadBytes {
			jsonError(w, http.StatusBadRequest, "file_size out of range")
			return
		}
		filename := sanitizeFilename(f.Filename)
		token := randomToken()
		uploadFilename := randomToken() + "/" + filename
		if err := s.net.CreateUpload(&storage.PendingUpload{
			Token:          token,
			UploadFilename: uploadFilename,
			Filename:       filename,
			UserID:         u.ID,
			ChannelID:      channelID,
			FileSize:       f.FileSize,
			CreatedAt:      time.Now().Unix(),
		}); err != nil {
			jsonError(w, http.StatusInternalServerError, "upload slot failed")
			return
		}
		out = append(out, map[string]any{
			"id":             f.ID,
			"upload_url":     base + "/api/v9/uploads/" + token,
			"upload_filename": uploadFilename,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": out})
}

// handleUpload serves PUT /api/v9/uploads/{token}: the client pushes the
// file bytes. No Authorization header - the token is the credential
// (Discord's upload_url is a presigned GCS URL in exactly the same way).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	upload, err := s.net.GetUpload(r.PathValue("token"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown upload")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "read failed")
		return
	}
	if int64(len(data)) > maxUploadBytes || int64(len(data)) > upload.FileSize {
		jsonError(w, http.StatusBadRequest, "file larger than declared size")
		return
	}
	att := &storage.Attachment{
		ID:          s.net.NewMessageID(),
		Filename:    upload.Filename,
		ContentType: contentTypeFor(upload.Filename, data),
		Size:        len(data),
		UploadedAt:  time.Now().Unix(),
	}
	sniffDims(att, data)
	if err := s.net.BindAttachment(upload, att, data); err != nil {
		jsonError(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// newStoredAttachment persists raw bytes (multipart flow: the files ride
// along with the send request itself).
func (s *Server) newStoredAttachment(filename string, data []byte) *storage.Attachment {
	att := &storage.Attachment{
		ID:          s.net.NewMessageID(),
		Filename:    sanitizeFilename(filename),
		ContentType: contentTypeFor(filename, data),
		Size:        len(data),
		UploadedAt:  time.Now().Unix(),
	}
	sniffDims(att, data)
	if err := s.net.PutAttachment(att, data); err != nil {
		s.log.Warn("attachment store failed", "err", err)
	}
	return att
}

// resolveSendAttachments builds the Discord attachment rows for a send
// request and the URL list the IRC wire copy must carry.
func (s *Server) resolveSendAttachments(reqs []sendAttachment) ([]any, []string, error) {
	var rows []any
	var urls []string
	for _, ra := range reqs {
		if ra.UploadedFilename == "" {
			continue
		}
		att, err := s.net.ResolveUpload(ra.UploadedFilename)
		if err != nil {
			return nil, nil, err
		}
		rowID := ra.ID
		if rowID == "" {
			rowID = strconv.Itoa(len(rows))
		}
		rows = append(rows, s.attachmentPayload(rowID, att))
		urls = append(urls, s.attachmentURL(att.ID, att.Filename))
	}
	return rows, urls, nil
}

// handleGetAttachment serves GET /attachments/{id}/{filename}: the
// public CDN-style download (IRC peers fetch these too, so no auth).
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	if s.net == nil {
		jsonError(w, http.StatusServiceUnavailable, "networks not configured")
		return
	}
	att, err := s.net.GetAttachment(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown attachment")
		return
	}
	data, err := s.net.GetAttachmentData(att.ID)
	if err != nil {
		jsonError(w, http.StatusNotFound, "unknown attachment")
		return
	}
	ct := att.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition", "inline; filename=\""+att.Filename+"\"")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// handleRefreshAttachmentURLs serves POST /attachments/refresh-urls:
// Discord CDN urls expire and clients ask for fresh signed ones. Ours
// never do, so every url is echoed unchanged.
func (s *Server) handleRefreshAttachmentURLs(w http.ResponseWriter, r *http.Request, _ *storage.User) {
	var body struct {
		AttachmentUrls []string `json:"attachment_urls"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid body")
		return
	}
	out := make([]any, 0, len(body.AttachmentUrls))
	for _, u := range body.AttachmentUrls {
		out = append(out, map[string]any{"original": u, "refreshed": u})
	}
	writeJSON(w, http.StatusOK, map[string]any{"refreshed_urls": out})
}

// contentTypeFor prefers the extension, falls back to magic sniffing.
func contentTypeFor(filename string, data []byte) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}
