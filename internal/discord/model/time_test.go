package model

import (
	"strings"
	"testing"
	"time"
)

func TestCeilSecond(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 8, 28, 12, 0, 5, 123000000, time.UTC), time.Date(2026, 8, 28, 12, 0, 6, 0, time.UTC)},
		{time.Date(2026, 8, 28, 12, 0, 5, 999999999, time.UTC), time.Date(2026, 8, 28, 12, 0, 6, 0, time.UTC)},
		{time.Date(2026, 8, 28, 12, 0, 5, 0, time.UTC), time.Date(2026, 8, 28, 12, 0, 6, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := ceilSecond(c.in); !got.Equal(c.want) {
			t.Fatalf("ceilSecond(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNowTimestamp(t *testing.T) {
	before := time.Now().UTC()
	ts := NowTimestamp()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("NowTimestamp() = %q, not RFC3339: %v", ts, err)
	}
	if strings.Contains(ts, ".") {
		t.Fatalf("NowTimestamp() = %q, has fractional seconds", ts)
	}
	if d := parsed.Sub(before); d <= 0 || d > 2*time.Second {
		t.Fatalf("NowTimestamp() = %q, offset %v outside (0, 2s] from sample %v", ts, d, before)
	}
}
