package ircmanage

import (
	"strings"
	"time"
	"unicode"

	"github.com/h4ks-com/voidbar/internal/discord/model"
)

// Mention bridging. IRC convention is a bare nick ("doesnm: look"),
// Discord's is a snowflake marker; each side gets what it understands:
//
//	outgoing  <@id> / <@!id> -> bare nick,   <#id> -> #name
//	incoming  bare nick      -> <@id>,       #name -> <#id>
//
// The nick universe for a target is its live occupants (NAMES state)
// plus every bouncer member of the network under their live nick; the
// channel universe is the member's auto-join channels. Unknown ids and
// nicks pass through untouched, so nothing is ever lost in translation.

// ircNickRune reports whether r can appear inside an IRC nick; it draws
// the word boundaries when scanning for bare-nick mentions.
func ircNickRune(r rune) bool {
	switch r {
	case '-', '[', ']', '\\', '`', '_', '^', '{', '|', '}':
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// mentionUser is one mentionable person.
type mentionUser struct {
	nick   string // live IRC nick
	id     string // Discord user id
	bounce bool   // bouncer member (client already knows their user)
	mode   string // highest channel mode (for role colors), "" = plain
}

// mentionChannel is one mentionable channel.
type mentionChannel struct {
	ircName string // "#test"
	id      string // registry snowflake
	name    string // "test" (Discord channel names carry no #)
}

// mentionUsers collects the nick universe for a target, longest nick
// first (so "doesnm2" wins over "doesnm"). Bouncer members take
// precedence over same-nick IRC occupants: their real user id is what
// clients must ping.
func (m *Manager) mentionUsers(userID, networkID, ircTarget string) []mentionUser {
	byLower := map[string]mentionUser{}
	if strings.HasPrefix(ircTarget, "#") || strings.HasPrefix(ircTarget, "&") {
		for _, cm := range m.ChannelMembersDetailed(userID, networkID, ircTarget) {
			if cm.Nick == "" {
				continue
			}
			byLower[strings.ToLower(cm.Nick)] = mentionUser{
				nick: cm.Nick,
				id:   model.IrcAuthorID("irc:" + cm.Nick),
				mode: cm.Mode,
			}
		}
	} else if ircTarget != "" {
		// DM: the peer is the only IRC-side user mentionable by nick.
		byLower[strings.ToLower(ircTarget)] = mentionUser{
			nick: ircTarget,
			id:   model.IrcAuthorID("irc:" + ircTarget),
		}
	}
	if members, err := m.store.ListMemberships(networkID); err == nil {
		for _, mem := range members {
			nick := mem.Nick
			if live := m.LiveNick(mem.UserID, networkID); live != "" {
				nick = live
			}
			if nick == "" {
				continue
			}
			byLower[strings.ToLower(nick)] = mentionUser{nick: nick, id: mem.UserID, bounce: true}
		}
	}
	out := make([]mentionUser, 0, len(byLower))
	for _, u := range byLower {
		out = append(out, u)
	}
	// Longest first: overlapping candidates must resolve to the longest
	// nick ("doesnm2" mentioned must not half-match "doesnm" - the
	// boundary check alone does not help when the longer nick is also
	// a candidate).
	sortMentionUsers(out)
	return out
}

func sortMentionUsers(us []mentionUser) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && len(us[j].nick) > len(us[j-1].nick); j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

// mentionChannels collects the member's auto-join channels, longest
// IRC name first.
func (m *Manager) mentionChannels(userID, networkID string) []mentionChannel {
	mem, err := m.store.GetMembership(networkID, userID)
	if err != nil {
		return nil
	}
	chans, err := m.store.ChannelsByIRC(networkID, mem.AutoJoin, m.sf.New)
	if err != nil {
		return nil
	}
	out := make([]mentionChannel, 0, len(chans))
	for _, ch := range chans {
		out = append(out, mentionChannel{
			ircName: ch.IRCName,
			id:      ch.ID,
			name:    ch.Name,
		})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j].ircName) > len(out[j-1].ircName); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// IRCize rewrites Discord mention markers into plain IRC text for the
// wire. User markers become the bare nick (IRC convention - no @);
// channel markers become the #name. Anything unresolvable stays as-is.
func (m *Manager) IRCize(userID, networkID, ircTarget, content string) string {
	if !strings.Contains(content, "<@") && !strings.Contains(content, "<#") {
		return content
	}
	users := m.mentionUsers(userID, networkID, ircTarget)
	chans := m.mentionChannels(userID, networkID)
	var b strings.Builder
	for i := 0; i < len(content); {
		if content[i] == '<' {
			if id, width, ok := cutUserMarker(content[i:]); ok {
				if u, found := resolveMentionUser(users, id); found {
					b.WriteString(u.nick)
					i += width
					continue
				}
			}
			if id, width, ok := cutChannelMarker(content[i:]); ok {
				for _, ch := range chans {
					if ch.id == id {
						b.WriteString(ch.ircName)
						i += width
						continue
					}
				}
			}
		}
		b.WriteByte(content[i])
		i++
	}
	return b.String()
}

// resolveMentionUser finds the user behind a marker id. Bouncer ids are
// their own; IRC peers are matched by their hashed snowflake.
func resolveMentionUser(users []mentionUser, id string) (mentionUser, bool) {
	for _, u := range users {
		if u.id == id {
			return u, true
		}
	}
	return mentionUser{}, false
}

// cutUserMarker matches "<@123>" or "<@!123>" at the start of s and
// returns the id and the marker's byte width.
func cutUserMarker(s string) (string, int, bool) {
	if !strings.HasPrefix(s, "<@") {
		return "", 0, false
	}
	rest := s[2:]
	if strings.HasPrefix(rest, "!") {
		rest = rest[1:]
	}
	end := strings.IndexByte(rest, '>')
	if end <= 0 {
		return "", 0, false
	}
	for _, r := range rest[:end] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
	}
	return rest[:end], len(s) - len(rest) + end + 1, true
}

// cutChannelMarker matches "<#123>" at the start of s and returns the
// id and the marker's byte width.
func cutChannelMarker(s string) (string, int, bool) {
	if !strings.HasPrefix(s, "<#") {
		return "", 0, false
	}
	rest := s[2:]
	end := strings.IndexByte(rest, '>')
	if end <= 0 {
		return "", 0, false
	}
	for _, r := range rest[:end] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
	}
	return rest[:end], len(s) - len(rest) + end + 1, true
}

// Discordize scans IRC text for bare nicks (an optional leading @ is
// swallowed - some people write @nick) and #channel references, rewrites
// them into Discord markers, and returns the mentioned users/channels so
// callers can build the payload arrays and upsert unknown peers.
func (m *Manager) Discordize(userID, networkID, ircTarget, content string) (string, []mentionUser, []mentionChannel) {
	users := m.mentionUsers(userID, networkID, ircTarget)
	chans := m.mentionChannels(userID, networkID)
	if len(users) == 0 && len(chans) == 0 {
		return content, nil, nil
	}
	runes := []rune(content)
	var b strings.Builder
	var mentionedUsers []mentionUser
	var mentionedChans []mentionChannel
	seenUser := map[string]bool{}
	seenChan := map[string]bool{}
	for i := 0; i < len(runes); {
		// Leading @ before a nick match is dropped: the Discord pill
		// replaces the whole "@nick".
		at := i
		if runes[at] == '@' && at+1 < len(runes) {
			at++
		}
		if u, width := matchNickAt(runes, at, users); width > 0 {
			b.WriteString("<@" + u.id + ">")
			if !seenUser[u.id] {
				seenUser[u.id] = true
				mentionedUsers = append(mentionedUsers, u)
			}
			i = at + width
			continue
		}
		if c, width := matchChannelAt(runes, i, chans); width > 0 {
			b.WriteString("<#" + c.id + ">")
			if !seenChan[c.id] {
				seenChan[c.id] = true
				mentionedChans = append(mentionedChans, c)
			}
			i += width
			continue
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String(), mentionedUsers, mentionedChans
}

// mentionUserPayload builds one mentions[] entry.
func mentionUserPayload(u mentionUser) map[string]any {
	return map[string]any{
		"id":            u.id,
		"username":      u.nick,
		"discriminator": "0",
		"bot":           false,
	}
}

// mentionChannelPayload builds one mention_channels[] entry.
func mentionChannelPayload(c mentionChannel, networkID string) map[string]any {
	return map[string]any{
		"id":       c.id,
		"guild_id": networkID,
		"name":     c.name,
		"type":     0,
	}
}

// upsertMentionedPeers pushes a GUILD_MEMBER_UPDATE for every mentioned
// IRC peer. Clients do NOT ingest users from the mentions array, and a
// peer that joined after GUILD_CREATE and never spoke is unknown to
// them - the pill then renders "@invalid-user". The member update is
// the standard event that upserts both the member row and the user
// object, so the pill resolves. Bouncer members are skipped: they are
// known from READY/GUILD_CREATE, and a roles-less update would strip
// their sidebar color.
func (m *Manager) upsertMentionedPeers(c *conn, users []mentionUser) {
	if len(users) == 0 {
		return
	}
	mem, err := m.store.GetMembership(c.networkID, c.userID)
	if err != nil {
		return
	}
	joinedAt := mem.JoinedAt.Format(time.RFC3339)
	for _, u := range users {
		if u.bounce {
			continue
		}
		m.dispatchPeerMember(c, u.nick, u.mode, joinedAt)
	}
}

// MentionPayloadsFromMarkers builds the mentions/mention_channels
// arrays for content that already carries Discord markers (the client's
// own sends). Pills render from the user store, but HIGHLIGHTING needs
// the mentions array - and the REST echo / MESSAGE_CREATE fanout of own
// sends must carry it just like inbound relays do.
func (m *Manager) MentionPayloadsFromMarkers(userID, networkID, ircTarget, content string) ([]any, []any) {
	users := m.mentionUsers(userID, networkID, ircTarget)
	chans := m.mentionChannels(userID, networkID)
	var mentioned []any
	var mentionChans []any
	seen := map[string]bool{}
	for i := 0; i < len(content); {
		if content[i] != '<' {
			i++
			continue
		}
		if id, width, ok := cutUserMarker(content[i:]); ok {
			i += width
			if seen[id] {
				continue
			}
			seen[id] = true
			if u, found := resolveMentionUser(users, id); found {
				mentioned = append(mentioned, mentionUserPayload(u))
			}
			continue
		}
		if id, width, ok := cutChannelMarker(content[i:]); ok {
			i += width
			if seen["#"+id] {
				continue
			}
			seen["#"+id] = true
			for _, ch := range chans {
				if ch.id == id {
					mentionChans = append(mentionChans, mentionChannelPayload(ch, networkID))
				}
			}
		}
	}
	return mentioned, mentionChans
}

// sweepPeerMembers announces every foreign occupant of the member's
// channels after an upstream (re)connect. Peers that were already in
// the channels produce no JOIN events for us - without the sweep, a
// client that has been running since before the peer arrived (or across
// a bouncer restart) never learns their user object, and mention pills
// render @invalid-user until the peer rejoins.
func (m *Manager) sweepPeerMembers(c *conn, autoJoin []string) {
	go func() {
		// NAMES state settles asynchronously after our JOINs; the sweep
		// is advisory, so a fixed grace beat is enough. Late arrivals are
		// covered by the JOIN upserts anyway.
		time.Sleep(3 * time.Second)
		bouncers := map[string]bool{}
		if members, err := m.store.ListMemberships(c.networkID); err == nil {
			for _, mem := range members {
				nick := mem.Nick
				if live := m.LiveNick(mem.UserID, c.networkID); live != "" {
					nick = live
				}
				if nick != "" {
					bouncers[strings.ToLower(nick)] = true
				}
			}
		}
		seen := map[string]bool{}
		for _, chName := range autoJoin {
			for _, cm := range m.ChannelMembersDetailed(c.userID, c.networkID, chName) {
				if cm.Nick == "" || bouncers[strings.ToLower(cm.Nick)] || seen[cm.Nick] {
					continue
				}
				seen[cm.Nick] = true
				m.upsertPeerMember(c, cm.Nick, cm.Mode, "")
			}
		}
	}()
}

func (m *Manager) upsertPeerMember(c *conn, nick, mode, joinedAt string) {
	if joinedAt == "" {
		mem, err := m.store.GetMembership(c.networkID, c.userID)
		if err != nil {
			return
		}
		joinedAt = mem.JoinedAt.Format(time.RFC3339)
	}
	m.dispatchPeerMember(c, nick, mode, joinedAt)
}

// dispatchPeerMember emits the GUILD_MEMBER_UPDATE carrying the peer's
// user object (roles from their channel mode; "" keeps the row plain).
func (m *Manager) dispatchPeerMember(c *conn, nick, mode, joinedAt string) {
	roles := []any{}
	if mode != "" {
		roles = []any{model.IrcRoleID(mode)}
	}
	m.gw.Dispatch(c.userID, "GUILD_MEMBER_UPDATE", map[string]any{
		"guild_id": c.networkID,
		"user": map[string]any{
			"id":            model.IrcAuthorID("irc:" + nick),
			"username":      nick,
			"discriminator": "0",
			"bot":           false,
		},
		"nick":      nil,
		"roles":     roles,
		"joined_at": joinedAt,
	})
}

// matchNickAt returns the candidate nick matching at position i (must
// be at a nick start boundary) plus its rune width.
func matchNickAt(runes []rune, i int, users []mentionUser) (mentionUser, int) {
	if i >= len(runes) || !ircNickRune(runes[i]) {
		return mentionUser{}, 0
	}
	rest := string(runes[i:])
	for _, u := range users {
		if len(u.nick) == 0 || len(u.nick) > len(rest) {
			continue
		}
		if !strings.EqualFold(rest[:len(u.nick)], u.nick) {
			continue
		}
		// Trailing boundary: the next rune must end the nick.
		if len(runes) > i+len(u.nick) && ircNickRune(runes[i+len(u.nick)]) {
			continue
		}
		// Leading boundary for the direct (non-@) case.
		if i > 0 && ircNickRune(runes[i-1]) && runes[i-1] != '@' {
			continue
		}
		return u, len([]rune(u.nick))
	}
	return mentionUser{}, 0
}

// matchChannelAt returns the candidate channel whose "#name" matches at
// position i, plus its rune width.
func matchChannelAt(runes []rune, i int, chans []mentionChannel) (mentionChannel, int) {
	if i >= len(runes) || runes[i] != '#' {
		return mentionChannel{}, 0
	}
	rest := string(runes[i:])
	for _, c := range chans {
		if len(c.ircName) == 0 || len(c.ircName) > len(rest) {
			continue
		}
		if !strings.EqualFold(rest[:len(c.ircName)], c.ircName) {
			continue
		}
		if len(runes) > i+len(c.ircName) && ircNickRune(runes[i+len(c.ircName)]) {
			continue
		}
		return c, len([]rune(c.ircName))
	}
	return mentionChannel{}, 0
}
