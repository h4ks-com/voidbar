package model

// IRC channel-membership modes surfaced as synthetic hoisted Discord
// roles, so channel status prefixes (~&@%+) become visible sections and
// name colors in the member sidebar. The mapping is fixed:
//
//	q (~) founder, a (&) admin, o (@) operator, h (%) half-op, v (+) voice
const (
	IrcModeFounder = "q"
	IrcModeAdmin   = "a"
	IrcModeOp      = "o"
	IrcModeHalfOp  = "h"
	IrcModeVoice   = "v"
)

// ircRoleDefs is ordered by descending privilege; Position doubles as the
// role's wire position (higher wins for name color).
var ircRoleDefs = []struct {
	Mode     string
	Name     string
	Color    int
	Position int
}{
	{IrcModeFounder, "Founder", 0xFAA61A, 5},
	{IrcModeAdmin, "Admin", 0xEB459E, 4},
	{IrcModeOp, "Operator", 0x1ABC9C, 3},
	{IrcModeHalfOp, "Half-op", 0x57F287, 2},
	{IrcModeVoice, "Voice", 0x5865F2, 1},
}

// IrcRoleModes returns the mapped modes in descending privilege order.
func IrcRoleModes() []string {
	out := make([]string, 0, len(ircRoleDefs))
	for _, d := range ircRoleDefs {
		out = append(out, d.Mode)
	}
	return out
}

// IrcModeRank returns the privilege rank of a mode (0 for plain members).
func IrcModeRank(mode string) int {
	for _, d := range ircRoleDefs {
		if d.Mode == mode {
			return d.Position
		}
	}
	return 0
}

// IrcRoleID maps an IRC membership mode to a stable role snowflake. The
// client parses GUILD_MEMBER_LIST_UPDATE group ids with Long.parseLong,
// so the id must be a plain positive decimal; same construction as
// IrcAuthorID. Deterministic, so GUILD_CREATE roles and lazy-list groups
// stay in sync across restarts.
func IrcRoleID(mode string) string {
	return hashSnowflake("irc-role:" + mode)
}

// IrcRolePayloads returns the wire role objects in descending privilege
// order. hoist:true is what makes the client render them as sidebar
// sections; colors land on member names via the highest colored role.
func IrcRolePayloads() []any {
	out := make([]any, 0, len(ircRoleDefs))
	for _, d := range ircRoleDefs {
		out = append(out, map[string]any{
			"id":            IrcRoleID(d.Mode),
			"name":          d.Name,
			"color":         d.Color,
			"hoist":         true,
			"icon":          nil,
			"unicode_emoji": nil,
			"position":      d.Position,
			"permissions":   "0",
			"managed":       false,
			"mentionable":   false,
			"flags":         0,
		})
	}
	return out
}

// EveryoneRolePayload builds the @everyone role every guild carries; its
// id must equal the guild id and the client computes channel permissions
// through it. reactCapable gates ADD_REACTIONS: on upstreams without
// msgids (no MSGREFTYPES in ISUPPORT) reactions cannot bridge, so the
// client's picker is hidden rather than offering taps that go nowhere.
func EveryoneRolePayload(guildID string, reactCapable bool) map[string]any {
	perms := "104324740520977" // exactly what the bridge honours; no ADD_REACTIONS
	if reactCapable {
		perms = "104324740521041" // + ADD_REACTIONS (picker UI)
	}
	return map[string]any{
		"id":            guildID,
		"name":          "@everyone",
		"color":         0,
		"hoist":         false,
		"icon":          nil,
		"unicode_emoji": nil,
		"position":      0,
		"permissions":   perms, // CHANGE_NICKNAME is what un-disables the Edit Server Profile nickname field; never MANAGE_MESSAGES (no IRC mapping), never USE_EXTERNAL_EMOJIS (no custom emojis exist)
		"managed":       false,
		"mentionable":   false,
		"flags":         0,
	}
}
