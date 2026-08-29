package storage

import (
	"encoding/json"
	"errors"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// Attachment is a stored file: metadata is indexed under attKey, the
// bytes under attDataKey. Urls are derived from the id, so the mapping
// is stable across restarts (the replay buffer references it).
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	UploadedAt  int64  `json:"uploaded_at"`
}

// PendingUpload is a minted upload slot: the client PUTs the file bytes
// to a URL carrying the secret token, then references the upload by
// upload_filename when sending the message.
type PendingUpload struct {
	Token          string `json:"token"`
	UploadFilename string `json:"upload_filename"`
	Filename       string `json:"filename"`
	UserID         string `json:"user_id"`
	ChannelID      string `json:"channel_id"`
	FileSize       int64  `json:"file_size"`
	CreatedAt      int64  `json:"created_at"`
}

// AttachmentRef is the upload_filename -> attachment id binding created
// when the bytes land, so the message send resolves its attachments.
type AttachmentRef struct {
	AttachmentID string `json:"attachment_id"`
}

const uploadTTL = 24 * time.Hour

func uploadKey(token string) []byte {
	return []byte("upload:" + token)
}

func uploadRefKey(uploadFilename string) []byte {
	return []byte("uploadref:" + uploadFilename)
}

func attKey(id string) []byte {
	return []byte("att:" + id)
}

func attDataKey(id string) []byte {
	return []byte("attdata:" + id)
}

// ErrAttachmentNotFound reads better than leaking badger's KeyNotFound
// through the service layer.
var ErrAttachmentNotFound = errors.New("attachment not found")

// CreateUpload records a pending upload slot.
func (s *Storage) CreateUpload(up *PendingUpload) error {
	return s.db.Update(func(txn *badger.Txn) error {
		raw, err := json.Marshal(up)
		if err != nil {
			return err
		}
		e := badger.NewEntry(uploadKey(up.Token), raw).WithTTL(uploadTTL)
		return txn.SetEntry(e)
	})
}

// GetUpload fetches a pending upload slot by its secret token.
func (s *Storage) GetUpload(token string) (*PendingUpload, error) {
	var out *PendingUpload
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(uploadKey(token))
		if err != nil {
			return err
		}
		return item.Value(func(raw []byte) error {
			out = &PendingUpload{}
			return json.Unmarshal(raw, out)
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutAttachment stores attachment metadata and bytes.
func (s *Storage) PutAttachment(att *Attachment, data []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		meta, err := json.Marshal(att)
		if err != nil {
			return err
		}
		if err := txn.Set(attKey(att.ID), meta); err != nil {
			return err
		}
		return txn.Set(attDataKey(att.ID), data)
	})
}

// BindUpload ties an upload_filename to a stored attachment id (TTL'd:
// unused bindings expire with their upload slots).
func (s *Storage) BindUpload(uploadFilename, attachmentID string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		ref, err := json.Marshal(&AttachmentRef{AttachmentID: attachmentID})
		if err != nil {
			return err
		}
		e := badger.NewEntry(uploadRefKey(uploadFilename), ref).WithTTL(uploadTTL)
		return txn.SetEntry(e)
	})
}

// BindAttachment stores the uploaded bytes and binds the upload slot's
// upload_filename to the minted attachment id.
func (s *Storage) BindAttachment(upload *PendingUpload, att *Attachment, data []byte) error {
	if err := s.PutAttachment(att, data); err != nil {
		return err
	}
	return s.BindUpload(upload.UploadFilename, att.ID)
}

// ResolveUpload maps an upload_filename (the value the send request
// carries) to the stored attachment.
func (s *Storage) ResolveUpload(uploadFilename string) (*Attachment, error) {
	var att *Attachment
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(uploadRefKey(uploadFilename))
		if err != nil {
			return err
		}
		return item.Value(func(raw []byte) error {
			ref := &AttachmentRef{}
			if err := json.Unmarshal(raw, ref); err != nil {
				return err
			}
			attItem, err := txn.Get(attKey(ref.AttachmentID))
			if err != nil {
				return err
			}
			return attItem.Value(func(raw []byte) error {
				att = &Attachment{}
				return json.Unmarshal(raw, att)
			})
		})
	})
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, ErrAttachmentNotFound
		}
		return nil, err
	}
	return att, nil
}

// GetAttachment returns the metadata of a stored attachment.
func (s *Storage) GetAttachment(id string) (*Attachment, error) {
	var att *Attachment
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(attKey(id))
		if err != nil {
			return err
		}
		return item.Value(func(raw []byte) error {
			att = &Attachment{}
			return json.Unmarshal(raw, att)
		})
	})
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, ErrAttachmentNotFound
		}
		return nil, err
	}
	return att, nil
}

// GetAttachmentData returns the bytes of a stored attachment.
func (s *Storage) GetAttachmentData(id string) ([]byte, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(attDataKey(id))
		if err != nil {
			return err
		}
		return item.Value(func(raw []byte) error {
			out = append([]byte(nil), raw...)
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, ErrAttachmentNotFound
		}
		return nil, err
	}
	return out, nil
}
