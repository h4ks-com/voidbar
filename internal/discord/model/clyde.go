package model

// ClydeID is the service-bot identity every network reports as its owner.
// The client gates owner-only actions (notably "Delete server" in guild
// settings) on owner_id == me.id; a bouncer user only ever *joins*
// networks, so the owner is a fictional service bot — Clyde, Discord's
// own. He also rides along in the members payload (the client's member
// list itself isn't rendered until presence ships).
const ClydeID = "901392366394585088"

// DMPeer renders the wire user for a DM channel peer given the IRC nick.
// The service nick "Clyde" is special-cased to the real Clyde identity so
// system notices render as the bot himself.
func DMPeer(nick string) map[string]any {
	return DMPeerID(nick, IrcAuthorID("irc:"+nick))
}

// DMPeerID is DMPeer with an explicit peer id, for peers whose payload id
// is not the IRC-author hash (fellow bouncer members show their real user
// id in guild payloads, so their DM peer must match or the client treats
// it as a different user).
func DMPeerID(nick, id string) map[string]any {
	if nick == "Clyde" {
		return map[string]any{
			"id":            ClydeID,
			"username":      "Clyde",
			"discriminator": "0",
			"bot":           true,
		}
	}
	return map[string]any{
		"id":            id,
		"username":      nick,
		"discriminator": "0",
		"bot":           false,
	}
}

// ClydeMember is the guild-member wire shape for the owner bot.
func ClydeMember(joinedAt string) map[string]any {
	return map[string]any{
		"user": map[string]any{
			"id":            ClydeID,
			"username":      "Clyde",
			"discriminator": "0",
			"bot":           true,
		},
		"roles":     []any{},
		"joined_at": joinedAt,
	}
}
