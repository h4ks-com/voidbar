package gateway

import (
	"encoding/json"
	"sync"
	"time"
)

const (
	replayLimit = 256
	// replayBytes caps the retained replay log per session: a couple of
	// READY-sized frames alone can run to megabytes, and sessions linger
	// for their resume TTL, so frames alone are not a safe bound.
	replayBytes = 4 << 20
)

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
	// eventsBytes tracks the retained replay log size for replayBytes.
	eventsBytes int
	send        chan writeRequest
	// detachedAt is set when the socket goes away; a session that stays
	// offline past the resume TTL is reaped wholesale (see
	// Server.reapStaleLocked) - otherwise every reconnect with a fresh
	// IDENTIFY parked another 256-frame session forever.
	detachedAt time.Time

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

// attach binds the session to a connection's write queue, reporting
// whether it displaced a live queue (the old socket loses its writer
// and dies without a close frame).
func (s *Session) attach(ch chan writeRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tookOver := s.send != nil && s.send != ch
	if tookOver {
		close(s.send)
	}
	s.send = ch
	s.detachedAt = time.Time{}
	return tookOver
}

func (s *Session) detach(ch chan writeRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.send == ch {
		s.send = nil
		s.detachedAt = time.Now()
	}
}

func (s *Session) offline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send == nil
}

// stale reports whether the session has been offline past ttl and can be
// dropped entirely - a later RESUME will just get an INVALID_SESSION and
// re-IDENTIFY, which is the documented cold path.
func (s *Session) stale(ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.send == nil && !s.detachedAt.IsZero() && time.Since(s.detachedAt) > ttl
}

func (s *Session) dispatch(t string, d any, record bool) (int64, error) {
	var dd json.RawMessage
	if d != nil {
		b, err := json.Marshal(d)
		if err != nil {
			return 0, err
		}
		dd = b
	}
	return s.dispatchRaw(t, dd, record)
}

// dispatchRaw is dispatch with a pre-marshalled payload. The fan-out path
// marshals the event once and hands the same bytes to every session -
// per-session json.Marshal showed up as steady CPU once orphaned sessions
// accumulated.
func (s *Session) dispatchRaw(t string, dd json.RawMessage, record bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	seq := s.seq
	frame, err := json.Marshal(Payload{Op: OpDispatch, S: &seq, T: t, D: dd})
	if err != nil {
		s.seq--
		return 0, err
	}
	if record {
		s.events = append(s.events, eventRecord{seq: seq, frame: frame})
		s.eventsBytes += len(frame)
		for s.eventsBytes > replayBytes && len(s.events) > 1 {
			s.eventsBytes -= len(s.events[0].frame)
			s.events = s.events[1:]
		}
		if len(s.events) > replayLimit {
			s.events = s.events[len(s.events)-replayLimit:]
		}
	}
	s.sendFrameLocked(frame)
	return seq, nil
}

func (s *Session) sendFrameLocked(frame []byte) {
	if s.send == nil {
		return
	}
	select {
	case s.send <- writeRequest{frame: frame}:
	default:
		close(s.send)
		s.send = nil
		s.detachedAt = time.Now()
	}
}

// queueFlush enqueues a write barrier: it resolves once the connection's
// writer has handed every earlier frame to the socket. REST handlers use
// it to order a gateway event ahead of their HTTP response - the client
// rebuilds UI state from its store, which only the event updates.
func (s *Session) queueFlush(done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.send == nil {
		close(done)
		return
	}
	select {
	case s.send <- writeRequest{done: done}:
	default:
		// The queue is full; earlier frames (the ones we care about)
		// are already waiting, so the barrier is trivially satisfied.
		close(done)
	}
}

func (s *Session) replay(since int64, ch chan writeRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.seq > since {
			select {
			case ch <- writeRequest{frame: ev.frame}:
			default:
			}
		}
	}
}
