package storage

import (
	"fmt"
	"testing"
)

func openTest(t *testing.T) *Storage {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func appendSeq(t *testing.T, s *Storage, channel string, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		if err := s.AppendMessage(BufferedMessage{
			ID:         fmt.Sprintf("90000000000000%05d", i),
			ChannelID:  channel,
			AuthorID:   "irc:someone",
			AuthorName: "someone",
			Content:    fmt.Sprintf("message %d", i),
			Timestamp:  "2026-08-25T00:00:00Z",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func ids(msgs []BufferedMessage) []int64 {
	const base = int64(9000000000000000000)
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		var n int64
		fmt.Sscanf(m.ID, "%d", &n)
		out = append(out, n-base)
	}
	return out
}

func eq(t *testing.T, got, want []int64, ctx string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got %v want %v", ctx, got, want)
	}
}

func TestBufferDefaultNewestFirst(t *testing.T) {
	s := openTest(t)
	appendSeq(t, s, "c1", 1, 10)
	got := ids(s.ChannelMessages("c1", "", "", 50))
	eq(t, got, []int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, "default order")
}

func TestBufferChannelsAreIsolated(t *testing.T) {
	s := openTest(t)
	appendSeq(t, s, "c1", 1, 3)
	appendSeq(t, s, "c2", 4, 6)
	eq(t, ids(s.ChannelMessages("c1", "", "", 50)), []int64{3, 2, 1}, "c1")
	eq(t, ids(s.ChannelMessages("c2", "", "", 50)), []int64{6, 5, 4}, "c2")
}

func TestBufferBeforePagination(t *testing.T) {
	s := openTest(t)
	appendSeq(t, s, "c1", 1, 10)
	// Oldest-first pages scrolled from the newest end, Discord-style.
	eq(t, ids(s.ChannelMessages("c1", "9000000000000000006", "", 50)), []int64{5, 4, 3, 2, 1}, "before=6")
	// limit clamps the page.
	eq(t, ids(s.ChannelMessages("c1", "9000000000000000006", "", 2)), []int64{5, 4}, "before=6 limit=2")
	// before older than everything -> empty.
	eq(t, ids(s.ChannelMessages("c1", "9000000000000000000", "", 50)), []int64{}, "before=0")
}

func TestBufferAfterPagination(t *testing.T) {
	s := openTest(t)
	appendSeq(t, s, "c1", 1, 10)
	// Ascending, exclusive of the anchor.
	eq(t, ids(s.ChannelMessages("c1", "", "9000000000000000003", 50)), []int64{4, 5, 6, 7, 8, 9, 10}, "after=3")
	eq(t, ids(s.ChannelMessages("c1", "", "9000000000000000003", 2)), []int64{4, 5}, "after=3 limit=2")
}

func TestBufferTrimsToCap(t *testing.T) {
	s := openTest(t)
	appendSeq(t, s, "c1", 1, MsgBufferCap+50)
	got := s.ChannelMessages("c1", "", "", MsgBufferCap+100)
	if len(got) != MsgBufferCap {
		t.Fatalf("buffer size: got %d want %d", len(got), MsgBufferCap)
	}
	eq(t, []int64{int64(len(got)), 1}, []int64{MsgBufferCap, 1}, "sanity")
	// The oldest kept message is #51: ids 1..50 were trimmed.
	eq(t, ids([]BufferedMessage{got[len(got)-1]}), []int64{51}, "oldest kept")
}

func TestBufferSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendSeq(t, s, "c1", 1, 5)
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	eq(t, ids(s2.ChannelMessages("c1", "", "", 50)), []int64{5, 4, 3, 2, 1}, "after reopen")
}
