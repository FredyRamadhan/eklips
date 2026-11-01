// Package events is the in-process publish/subscribe bus behind the SSE
// endpoint. Events carry monotonically increasing IDs and the most recent
// events are kept in a ring so reconnecting clients can resume.
package events

import (
	"encoding/json"
	"sync"
)

// Type names an event kind.
type Type string

// Event types.
const (
	Snapshot Type = "snapshot"
	State    Type = "state"
	Track    Type = "track"
	Queue    Type = "queue"
	Volume   Type = "volume"
	Mute     Type = "mute"
	Shuffle  Type = "shuffle"
	Loop     Type = "loop"
	Error    Type = "error"
	Device   Type = "device"
	Shutdown Type = "shutdown"
)

// Event is one published message. Data is pre-encoded JSON so it is
// marshalled once regardless of subscriber count.
type Event struct {
	ID   uint64
	Type Type
	Data json.RawMessage
}

// Bus fans events out to subscribers.
type Bus struct {
	mu      sync.Mutex
	nextID  uint64
	ring    []Event
	ringPos int
	ringLen int
	subs    map[*Sub]struct{}
	bufSize int
}

// Sub is a subscription. C is closed when the subscriber is evicted or
// unsubscribed.
type Sub struct {
	C       chan Event
	bus     *Bus
	once    sync.Once
	Evicted bool
}

// NewBus creates a bus keeping ringSize past events, with bufSize-deep
// subscriber queues.
func NewBus(ringSize, bufSize int) *Bus {
	return &Bus{ring: make([]Event, ringSize), subs: map[*Sub]struct{}{}, bufSize: bufSize}
}

// Publish encodes data and delivers it to all subscribers. Subscribers that
// cannot keep up are evicted (they reconnect with Last-Event-ID).
func (b *Bus) Publish(t Type, data any) Event {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{}`)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	ev := Event{ID: b.nextID, Type: t, Data: raw}
	if len(b.ring) > 0 {
		b.ring[b.ringPos] = ev
		b.ringPos = (b.ringPos + 1) % len(b.ring)
		if b.ringLen < len(b.ring) {
			b.ringLen++
		}
	}
	for s := range b.subs {
		select {
		case s.C <- ev:
		default:
			s.Evicted = true
			delete(b.subs, s)
			s.close()
		}
	}
	return ev
}

// LastID returns the ID of the most recent event.
func (b *Bus) LastID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID
}

// Subscribe registers a subscriber. If lastID > 0 and all events after it
// are still in the ring, they are returned for replay and ok is true;
// otherwise ok is false and the caller should send a fresh snapshot.
func (b *Bus) Subscribe(lastID uint64) (s *Sub, replay []Event, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s = &Sub{C: make(chan Event, b.bufSize), bus: b}
	b.subs[s] = struct{}{}
	if lastID == 0 || lastID > b.nextID {
		return s, nil, false
	}
	if lastID == b.nextID {
		return s, nil, true
	}
	oldest := b.nextID - uint64(b.ringLen) + 1
	if lastID+1 < oldest {
		return s, nil, false
	}
	start := (b.ringPos - b.ringLen + len(b.ring)) % len(b.ring)
	for i := 0; i < b.ringLen; i++ {
		ev := b.ring[(start+i)%len(b.ring)]
		if ev.ID > lastID {
			replay = append(replay, ev)
		}
	}
	return s, replay, true
}

// Unsubscribe removes the subscriber.
func (b *Bus) Unsubscribe(s *Sub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
	s.close()
}

// Count returns the number of subscribers.
func (b *Bus) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (s *Sub) close() { s.once.Do(func() { close(s.C) }) }
