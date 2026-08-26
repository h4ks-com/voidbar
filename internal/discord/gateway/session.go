package gateway

import (
	"encoding/json"
	"sync"
)

const replayLimit = 1024

type eventRecord struct {
	seq   int64
	frame []byte
}

type Session struct {
	ID     string
	UserID string

	mu     sync.Mutex
	seq    int64
	events []eventRecord
	send   chan []byte

	// memberLists tracks which lazy member lists this session asked for
	// via op 14 (key "<guild>\x00<channel>", empty channel = the
	// guild-wide everyone list). Live occupancy pushes are only sent for
	// lists the session actually holds, mirroring real Discord's
	// subscription semantics.
	memberLists map[string]bool
}

// watchMemberList records an op 14 subscription.
func (s *Session) watchMemberList(guildID, channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memberLists == nil {
		s.memberLists = make(map[string]bool)
	}
	s.memberLists[guildID+"\x00"+channelID] = true
}

// memberListSet returns a copy of the session's op 14 subscriptions.
func (s *Session) memberListSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.memberLists))
	for k := range s.memberLists {
		out[k] = true
	}
	return out
}

func (s *Session) attach(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.send != nil && s.send != ch {
		close(s.send)
	}
	s.send = ch
}

func (s *Session) detach(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.send == ch {
		s.send = nil
	}
}

func (s *Session) offline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send == nil
}

func (s *Session) dispatch(t string, d any, record bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dd json.RawMessage
	if d != nil {
		b, err := json.Marshal(d)
		if err != nil {
			return 0, err
		}
		dd = b
	}
	s.seq++
	seq := s.seq
	frame, err := json.Marshal(Payload{Op: OpDispatch, S: &seq, T: t, D: dd})
	if err != nil {
		s.seq--
		return 0, err
	}
	if record {
		s.events = append(s.events, eventRecord{seq: seq, frame: frame})
		if len(s.events) > replayLimit {
			s.events = s.events[len(s.events)-replayLimit:]
		}
	}
	s.sendFrame(frame)
	return seq, nil
}

func (s *Session) sendFrame(frame []byte) {
	if s.send == nil {
		return
	}
	select {
	case s.send <- frame:
	default:
		close(s.send)
		s.send = nil
	}
}

func (s *Session) replay(since int64, ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.seq > since {
			select {
			case ch <- ev.frame:
			default:
			}
		}
	}
}
