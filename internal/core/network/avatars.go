package network

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/h4ks-com/voidbar/internal/discord/model"
	"github.com/h4ks-com/voidbar/internal/storage"
)

// ErrBadAvatarDataURI reports an avatar upload that is not a decodable
// data:image/...;base64 payload.
var ErrBadAvatarDataURI = errors.New("avatar must be a base64 data URI")

// decodeAvatarDataURI splits "data:image/png;base64,...." into its
// content type and bytes.
func decodeAvatarDataURI(uri string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", nil, ErrBadAvatarDataURI
	}
	meta, b64, ok := strings.Cut(rest, ",")
	if !ok || !strings.Contains(meta, "base64") {
		return "", nil, ErrBadAvatarDataURI
	}
	ct, _, _ := strings.Cut(meta, ";")
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, ErrBadAvatarDataURI
	}
	return ct, data, nil
}

// SetPublicURL records the base the CDN routes answer on; avatar URLs
// published upstream via draft/metadata-2 are derived from it. Wired from
// main once config is loaded (the REST side keeps its own copy).
func (s *Service) SetPublicURL(base string) {
	s.publicURL = base
}

// avatarURL renders the public CDN address of a stored avatar hash: the
// client requests exactly this shape (/avatars/{uid}/{hash}.png off the
// discovered CDN base), so upstream peers get a URL that resolves.
func (s *Service) avatarURL(userID, hash string) string {
	if hash == "" || s.publicURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.publicURL, "/") + "/avatars/" + userID + "/" + hash + ".png"
}

// GetAvatar returns the stored avatar metadata and bytes for a hash
// (the REST CDN route serves them from here).
func (s *Service) GetAvatar(hash string) (*storage.Attachment, []byte, error) {
	return s.store.GetAvatar(hash)
}

// SelfAvatar is the account-wide avatar in payload form (own sends).
func (s *Service) SelfAvatar(userID string) any {
	return s.globalAvatarValue(userID)
}

// AuthorAvatar resolves a buffered message author's avatar: peers by
// nick, own rows to the account-wide hash ("" when nothing is known, or
// when this row's network rejected the account-wide avatar).
func (s *Service) AuthorAvatar(userID, channelID, rawAuthorID string) any {
	if !strings.HasPrefix(rawAuthorID, "irc:") {
		if ch, err := s.ChannelByID(channelID); err == nil {
			return s.SelfAvatarFor(rawAuthorID, ch.NetworkID)
		}
		return s.globalAvatarValue(rawAuthorID)
	}
	return s.peerAvatarValue(userID, strings.TrimPrefix(rawAuthorID, "irc:"))
}

// PeerAvatar returns the avatar hash shown for a remote IRC peer.
func (s *Service) PeerAvatar(userID, nick string) string {
	if s.store == nil {
		return ""
	}
	return s.store.PeerAvatar(userID, nick)
}

// peerAvatarValue is PeerAvatar in payload form (nil when unset).
func (s *Service) peerAvatarValue(userID, nick string) any {
	if h := s.PeerAvatar(userID, nick); h != "" {
		return h
	}
	return nil
}

// globalAvatarValue is the account-wide avatar in payload form.
func (s *Service) globalAvatarValue(userID string) any {
	if s.store == nil {
		return nil
	}
	if u, err := s.store.GetUserByID(userID); err == nil && u.Avatar != "" {
		return u.Avatar
	}
	return nil
}

// MemberAvatarFor resolves the avatar a member row should show inside a
// guild: the per-network override when set, else the account-wide avatar
// (Discord's member.avatar-over-user.avatar precedence) - unless THIS
// network rejected the account-wide avatar (AvatarRejected), in which
// case nothing shows here while accepted networks keep it.
func (s *Service) MemberAvatarFor(userID, guildID string) any {
	if s.store == nil {
		return nil
	}
	if mem, err := s.store.GetMembership(guildID, userID); err == nil {
		if mem.Avatar != "" {
			return mem.Avatar
		}
		if mem.AvatarRejected {
			return nil
		}
	}
	return s.globalAvatarValue(userID)
}

// SelfAvatarFor is the account-wide avatar as seen from one network:
// nil when that network rejected it (the IRC peers there see nothing,
// so the Discord view there matches).
func (s *Service) SelfAvatarFor(userID, networkID string) any {
	if s.avatarRejectedFor(userID, networkID) {
		return nil
	}
	return s.globalAvatarValue(userID)
}

// avatarRejectedFor reports whether the network refused the account-wide
// avatar (per-network override rejections revert themselves instead).
func (s *Service) avatarRejectedFor(userID, networkID string) bool {
	if s.store == nil {
		return false
	}
	mem, err := s.store.GetMembership(networkID, userID)
	return err == nil && mem.AvatarRejected
}

// SetGlobalAvatar stores the account-wide avatar (dataURI "" clears it),
// propagates USER_UPDATE to live sessions, and fans the new URL out to
// every upstream speaking draft/metadata-2 - that is the "global" part:
// one change, every network where the bouncer account is logged in.
func (s *Service) SetGlobalAvatar(userID, dataURI string) (*storage.User, error) {
	prev := ""
	if u, err := s.store.GetUserByID(userID); err == nil {
		prev = u.Avatar
	}
	hash := ""
	if dataURI != "" {
		ct, data, err := decodeAvatarDataURI(dataURI)
		if err != nil {
			return nil, err
		}
		hash, err = s.store.PutAvatar(data, ct)
		if err != nil {
			return nil, err
		}
	}
	if err := s.store.SetUserAvatar(userID, hash); err != nil {
		return nil, err
	}
	u, err := s.store.GetUserByID(userID)
	if err != nil {
		return nil, err
	}
	if s.gw != nil {
		// Full MeUser shape (same as READY.user): the client's own-user
		// store re-renders the drawer/profile from this dispatch.
		s.gw.Dispatch(userID, "USER_UPDATE", model.ToUser(u))
		// Own member rows carry the avatar inside guild payloads too.
		if memberships, err := s.store.ListMembershipsForUser(userID); err == nil {
			for _, m := range memberships {
				if payload := s.MemberPayload(userID, m.NetworkID, s.liveNickFor(userID, m.NetworkID, m.Nick)); payload != nil {
					s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", payload)
				}
			}
		}
	}
	// A fresh global avatar resets the per-network rejection marks: the
	// new SET gets its own verdict per network.
	if memberships, err := s.store.ListMembershipsForUser(userID); err == nil {
		for _, m := range memberships {
			if m.AvatarRejected {
				_ = s.store.SetMembershipAvatarRejected(m.NetworkID, userID, false)
			}
		}
	}
	if s.manager != nil {
		s.manager.SetAvatarAll(userID, s.avatarURL(userID, hash), prev)
	}
	return u, nil
}

// MarkAvatarRejected hides the account-wide avatar from THIS network's
// guild view (the upstream refused the SET; accepted networks keep it),
// and refreshes the member row so clients drop it immediately.
func (s *Service) MarkAvatarRejected(userID, networkID string) {
	if err := s.store.SetMembershipAvatarRejected(networkID, userID, true); err != nil {
		s.log.Warn("avatar rejection mark failed", "err", err, "user", userID, "network", networkID)
		return
	}
	if s.gw != nil {
		if mem, err := s.store.GetMembership(networkID, userID); err == nil {
			if payload := s.MemberPayload(userID, networkID, s.liveNickFor(userID, networkID, mem.Nick)); payload != nil {
				s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", payload)
			}
		}
	}
}

// RevertNetworkAvatar restores a previous per-guild avatar override.
func (s *Service) RevertNetworkAvatar(userID, networkID, prevHash string) {
	if err := s.store.SetMembershipAvatar(networkID, userID, prevHash); err != nil {
		s.log.Warn("network avatar revert failed", "err", err, "user", userID, "network", networkID)
		return
	}
	if s.gw != nil {
		if payload := s.MemberPayload(userID, networkID, s.liveNickFor(userID, networkID, "")); payload != nil {
			s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", payload)
		}
	}
}

// liveNickFor prefers the nick actually held on the wire right now.
func (s *Service) liveNickFor(userID, networkID, fallback string) string {
	if s.manager != nil {
		if live := s.manager.LiveNick(userID, networkID); live != "" {
			return live
		}
	}
	return fallback
}

// SetNetworkAvatar sets (dataURI "") the per-guild avatar override and
// bridges it to that network's own metadata avatar only.
func (s *Service) SetNetworkAvatar(userID, guildID, dataURI string) error {
	mem, err := s.store.GetMembership(guildID, userID)
	if err != nil {
		return err
	}
	prev := mem.Avatar
	hash := ""
	if dataURI != "" {
		ct, data, err := decodeAvatarDataURI(dataURI)
		if err != nil {
			return err
		}
		hash, err = s.store.PutAvatar(data, ct)
		if err != nil {
			return err
		}
	}
	if err := s.store.SetMembershipAvatar(mem.NetworkID, userID, hash); err != nil {
		return err
	}
	live := s.liveNickFor(userID, mem.NetworkID, mem.Nick)
	if s.manager != nil {
		s.manager.SetAvatar(userID, mem.NetworkID, s.avatarURL(userID, hash), prev)
	}
	if s.gw != nil {
		if payload := s.MemberPayload(userID, guildID, live); payload != nil {
			s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", payload)
		}
	}
	return nil
}

// RefreshPeerAvatar is the ircmanage metadata callback: a remote peer's
// avatar, bot flag or color changed (or arrived with the join burst).
// The member rows the client holds refresh through GUILD_MEMBER_UPDATE
// scoped to the network the event came in on.
func (s *Service) RefreshPeerAvatar(userID, networkID, nick string) {
	if s.gw == nil || s.store == nil {
		return
	}
	mem, err := s.store.GetMembership(networkID, userID)
	if err != nil {
		return
	}
	mode := ""
	for _, cm := range s.ircOccupants(userID, networkID, mem.AutoJoin) {
		if strings.EqualFold(cm.Nick, nick) {
			mode = cm.Mode
			break
		}
	}
	s.gw.Dispatch(userID, "GUILD_MEMBER_UPDATE", map[string]any{
		"guild_id": networkID,
		"user": map[string]any{
			"id":            model.IrcAuthorID("irc:" + nick),
			"username":      nick,
			"discriminator": "0",
			"bot":           s.peerBotValue(userID, nick),
			"avatar":        s.peerAvatarValue(userID, nick),
		},
		"roles":     s.memberRoleIDs(userID, mode, nick),
		"joined_at": mem.JoinedAt.Format(time.RFC3339),
	})
}
