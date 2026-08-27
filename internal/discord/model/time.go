package model

import "time"

// TimestampLayout is Discord-style ISO timestamps with millisecond
// precision. Second-precision truncation made client-side optimistic
// ghosts (client clock, milliseconds) sort "newer" than the confirmed
// server copy, so virtual-scroll caches re-appended the ghost next to
// the reconciled message (duplicate render).
const TimestampLayout = "2006-01-02T15:04:05.000Z07:00"

// NowTimestamp returns the current UTC time in TimestampLayout.
func NowTimestamp() string {
	return time.Now().UTC().Format(TimestampLayout)
}
