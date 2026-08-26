package model

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// IrcAuthorID maps an IRC author id ("irc:<nick>", the storage/relay
// convention) to a stable snowflake. The Android client parses message
// author ids as 64-bit integers: a raw "irc:<nick>" string throws inside
// its JSON deserializer and takes the gateway dispatch handler (and the
// /channels/{id}/messages rendering) down with it. Non-IRC ids pass
// through unchanged. The mapping is hash-based, so the same nick gets the
// same id across restarts and across live vs replayed copies.
func IrcAuthorID(authorID string) string {
	const prefix = "irc:"
	if !strings.HasPrefix(authorID, prefix) {
		return authorID
	}
	return hashSnowflake(authorID)
}

// hashSnowflake maps an arbitrary seed to a stable snowflake-shaped
// decimal string. The Android client parses ids as 64-bit integers, so
// the result must be a plain positive decimal; the value is spread over
// Discord's 2015..2017 era so ids sort like plausible snowflakes and stay
// inside a signed int64 after the <<22 shift; low 22 bits carry the rest
// of the hash.
func hashSnowflake(seed string) string {
	h := fnv.New64a()
	h.Write([]byte(seed))
	v := h.Sum64()
	const epoch = uint64(1420070400000)  // Discord epoch, ms
	ms := epoch + v%(uint64(63072000000)) // + up to ~2 years
	sf := (ms << 22) | (v & 0x3FFFFF)
	return strconv.FormatUint(sf, 10)
}
