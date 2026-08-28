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

func TestSnowflakeNewAtEncodesGivenTime(t *testing.T) {
	sf := NewSnowflake(0, 0)
	when := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	id, err := ParseSnowflake(sf.NewAt(when))
	if err != nil {
		t.Fatal(err)
	}
	if got := SnowflakeTime(id); !got.Equal(when) {
		t.Fatalf("time-anchored snowflake carries %v, want %v", got, when)
	}
	// Negative input clamps to the epoch, not a panic.
	if _, err := ParseSnowflake(sf.NewAt(time.Unix(0, 0).UTC())); err != nil {
		t.Fatal(err)
	}
}

func TestSnowflakeNewAtOrderAndDistinctness(t *testing.T) {
	sf := NewSnowflake(0, 0)
	past := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, 5)
	for i := 0; i < 3; i++ {
		id, err := ParseSnowflake(sf.NewAt(past)) // same millisecond
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	later, err := ParseSnowflake(sf.NewAt(past.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, later)
	// A wall-clock id minted now must sort after all past-anchored ones.
	now, err := ParseSnowflake(sf.New())
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, now)

	seen := make(map[int64]struct{}, len(ids))
	for i, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d at %d", id, i)
		}
		seen[id] = struct{}{}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids out of order at %d: %d after %d", i, id, ids[i-1])
		}
	}
}
