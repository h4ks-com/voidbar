package model

import "time"

// ceilSecond rounds t UP to the next whole second. Message timestamps
// must stay strictly newer than the client's optimistic ghost (client
// clock, millisecond precision): flooring made the confirmed server copy
// sort older, so virtual-scroll caches re-appended the ghost next to the
// reconciled message (duplicate render). Ceiling keeps the confirmed copy
// newer without emitting a fractional part, which the Android client's
// pre-crash wire shape never had.
func ceilSecond(t time.Time) time.Time {
	return t.Truncate(time.Second).Add(time.Second)
}

// NowTimestamp returns the current UTC time ceiled to the next whole
// second, RFC3339 second-precision (no fraction).
func NowTimestamp() string {
	return ceilSecond(time.Now().UTC()).Format(time.RFC3339)
}
