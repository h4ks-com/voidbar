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
