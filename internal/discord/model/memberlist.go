package model

// MemberListID returns the lazy member-list id for one channel. The
// client keys member lists by the channel's member_list_id wire field
// verbatim, and a GUILD_MEMBER_LIST_UPDATE SYNC must carry the exact
// same id or the rows never populate. The id is per-channel: all
// channels sharing one id means one cached list serves every channel,
// and the last-synced channel's occupants bleed into the others.
// The empty channelID is the guild-wide "everyone" list.
func MemberListID(guildID, channelID string) string {
	if channelID == "" {
		return guildID + ":everyone:0,99"
	}
	return guildID + ":" + channelID + ":0,99"
}
