package util

import (
	"strconv"
	"testing"
	"time"
)

func TestSnowflakeUniqueAndMonotonic(t *testing.T) {
	sf := NewSnowflake(1, 2)
	const n = 10000
	seen := make(map[int64]struct{}, n)
	var prev int64
	for i := 0; i < n; i++ {
		id := sf.Next()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d at %d", id, i)
		}
		seen[id] = struct{}{}
		if id <= prev && i > 0 {
			t.Fatalf("id not monotonic: %d after %d", id, prev)
		}
		prev = id
	}
}

func TestSnowflakeStringAndParse(t *testing.T) {
	sf := NewSnowflake(0, 0)
	s := sf.New()
	id, err := ParseSnowflake(s)
	if err != nil {
		t.Fatal(err)
	}
	ts := SnowflakeTime(id)
	if diff := time.Since(ts); diff < 0 || diff > 5*time.Second {
		t.Fatalf("snowflake time off: %v (%s)", ts, s)
	}
	if got := strconv.FormatInt(id, 10); got != s {
		t.Fatalf("roundtrip mismatch: %s != %s", got, s)
	}
}

func TestParseSnowflakeInvalid(t *testing.T) {
	if _, err := ParseSnowflake("abc"); err == nil {
		t.Fatal("expected error for non-numeric")
	}
	if _, err := ParseSnowflake("-1"); err == nil {
		t.Fatal("expected error for negative")
	}
}
