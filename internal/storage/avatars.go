package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// MaxAvatarBytes caps stored avatar images (uploads and mirrored IRC
// metadata avatars alike): Discord-side this is cosmetic profile art.
const MaxAvatarBytes = 4 << 20

// allowedAvatarTypes are the content types the client can render and the
// metadata avatar key semantically allows (an image URL). SVG stays out:
// it is a script vector served from our own origin.
var allowedAvatarTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
}

// AvatarAllowed reports whether a content type may be stored as an avatar.
func AvatarAllowed(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	return allowedAvatarTypes[ct]
}

// ErrAvatarTooLarge / ErrAvatarType are the avatar-side guard errors.
var (
	ErrAvatarTooLarge = errors.New("avatar exceeds size limit")
	ErrAvatarType     = errors.New("unsupported avatar content type")
)

// avatarContentType sniffs the likely image type from magic bytes for the
// common formats; empty when unknown.
func avatarContentType(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "image/avif"
	}
	return ""
}

// PutAvatar stores avatar bytes under their content hash and returns the
// hash to use as the avatar reference in payloads (the CDN route serves
// /avatars/{uid}/{hash}.png from the attachment store). Re-uploads of the
// same image collapse onto the same hash, which keeps the client cache
// happy; a changed picture is a new hash.
func (s *Storage) PutAvatar(data []byte, contentType string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty avatar")
	}
	if len(data) > MaxAvatarBytes {
		return "", ErrAvatarTooLarge
	}
	if contentType == "" || !AvatarAllowed(contentType) {
		contentType = avatarContentType(data)
	}
	if contentType == "" || !AvatarAllowed(contentType) {
		return "", ErrAvatarType
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:16])
	att := &Attachment{
		ID:          hash,
		Filename:    hash + ".png",
		ContentType: contentType,
		Size:        len(data),
		UploadedAt:  time.Now().UTC().Unix(),
	}
	if err := s.PutAttachment(att, data); err != nil {
		return "", err
	}
	return hash, nil
}

// GetAvatar returns the stored avatar metadata and bytes for a hash.
func (s *Storage) GetAvatar(hash string) (*Attachment, []byte, error) {
	att, err := s.GetAttachment(hash)
	if err != nil {
		return nil, nil, err
	}
	data, err := s.GetAttachmentData(hash)
	if err != nil {
		return nil, nil, err
	}
	return att, data, nil
}

func peerAvatarKey(userID, nick string) []byte {
	return []byte("peeravatar/" + userID + "/" + strings.ToLower(nick))
}

// PutPeerAvatar remembers the avatar hash shown for a remote IRC user.
// Keyed by nick (the author-id seed is "irc:"+nick, so nick-scoped is
// exactly as unique as the ids the client already sees).
func (s *Storage) PutPeerAvatar(userID, nick, hash string) error {
	if nick == "" {
		return nil
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(peerAvatarKey(userID, nick), []byte(hash))
	})
}

// PeerAvatar returns the stored avatar hash for a remote peer ("" unset).
func (s *Storage) PeerAvatar(userID, nick string) string {
	var hash string
	_ = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(peerAvatarKey(userID, nick))
		if err != nil {
			return nil
		}
		return item.Value(func(v []byte) error {
			hash = string(v)
			return nil
		})
	})
	return hash
}

// SetUserAvatar sets (or clears, on "") the account-wide avatar hash.
func (s *Storage) SetUserAvatar(userID, hash string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(userKey(userID))
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			var u User
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			u.Avatar = hash
			enc, err := json.Marshal(&u)
			if err != nil {
				return err
			}
			return txn.Set(userKey(userID), enc)
		})
	})
}

// SetMembershipAvatar sets (or clears) the per-network avatar override.
func (s *Storage) SetMembershipAvatar(networkID, userID, hash string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(memberKey(networkID, userID))
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			var mem Membership
			if err := json.Unmarshal(v, &mem); err != nil {
				return err
			}
			mem.Avatar = hash
			enc, err := json.Marshal(&mem)
			if err != nil {
				return err
			}
			return txn.Set(memberKey(networkID, userID), enc)
		})
	})
}
