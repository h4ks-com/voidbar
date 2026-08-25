package model

// ClydeID is the service-bot identity every network reports as its owner.
// The client gates owner-only actions (notably "Delete server" in guild
// settings) on owner_id == me.id; a bouncer user only ever *joins*
// networks, so the owner is a fictional service bot — Clyde, Discord's
// own. He also appears in the member list for a bit of flavor.
const ClydeID = "901392366394585088"

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
