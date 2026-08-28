package util

import (
	"errors"
	"strconv"
	"sync"
	"time"
)

const discordEpoch int64 = 1420070400000

type Snowflake struct {
	mu      sync.Mutex
	worker  int64
	process int64
	lastMs  int64
	seq     int64
	// aux is a separate per-millisecond sequence space for time-anchored
	// ids (NewAt), so minting ids in the past never disturbs the
	// wall-clock monotonicity Next relies on.
	aux map[int64]int64
}

func NewSnowflake(worker, process int64) *Snowflake {
	return &Snowflake{worker: worker & 0x1F, process: process & 0x1F}
}

func (s *Snowflake) Next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := time.Now().UnixMilli() - discordEpoch
	if ms <= s.lastMs {
		ms = s.lastMs
		s.seq = (s.seq + 1) & 0xFFF
		if s.seq == 0 {
			ms++
		}
	} else {
		s.seq = 0
	}
	s.lastMs = ms
	return (ms << 22) | (s.worker << 17) | (s.process << 12) | s.seq
}

func (s *Snowflake) New() string {
	return strconv.FormatInt(s.Next(), 10)
}

// NewAt mints a snowflake whose embedded timestamp comes from t instead of
// the wall clock. Chathistory prefill needs this: fetched messages carry
// server-time in the past, and both storage ordering and client rendering
// sort by id, so a prefetched message's id must encode its own (past) time,
// not the receive time. Time-anchored ids share the worker/process bits but
// a separate per-ms sequence space starting at 1, so they cannot collide
// with Next's ids (whose per-ms sequence starts at 0). The one theoretical
// overlap is a NewAt clamped to "now" landing on a millisecond Next is also
// bursting through - 50-message prefills make that unreachable in practice.
func (s *Snowflake) NewAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aux == nil {
		s.aux = make(map[int64]int64)
	}
	seq := s.aux[ms] + 1
	for seq > 0xFFF { // >4096 ids in one ms: spill into the next millisecond
		delete(s.aux, ms)
		ms++
		seq = s.aux[ms] + 1
	}
	s.aux[ms] = seq
	return strconv.FormatInt((ms<<22)|(s.worker<<17)|(s.process<<12)|seq, 10)
}

func ParseSnowflake(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("snowflake must be non-negative")
	}
	return n, nil
}

func SnowflakeTime(id int64) time.Time {
	return time.UnixMilli((id >> 22) + discordEpoch).UTC()
}
