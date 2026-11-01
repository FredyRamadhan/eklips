package sink

import (
	"encoding/binary"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frdn/eklips/internal/apperr"
)

// Chunk is a pooled, reference-counted PCM buffer shared by all listeners.
type Chunk struct {
	Data []byte
	refs atomic.Int32
	b    *Broadcaster
}

// Release drops one reference; the last one returns the chunk to the pool.
func (c *Chunk) Release() {
	if c.refs.Add(-1) == 0 {
		c.b.pool.Put(c)
	}
}

// Listener is one HTTP stream client.
type Listener struct {
	ch      chan *Chunk
	done    chan struct{}
	once    sync.Once
	evicted atomic.Bool
}

func (l *Listener) stop(evicted bool) {
	l.once.Do(func() {
		l.evicted.Store(evicted)
		close(l.done)
	})
}

// Broadcaster fans one PCM stream out to many listeners. Publishing never
// blocks: a listener whose queue is full is evicted.
type Broadcaster struct {
	format    Format
	queueLen  int
	max       int
	listeners atomic.Pointer[[]*Listener] // copy-on-write
	mu        sync.Mutex                  // serialises add/remove only
	pool      sync.Pool
	joined    chan struct{}
	evictions atomic.Int64
	published atomic.Int64
}

// NewBroadcaster creates a broadcaster. queueTime bounds per-listener
// buffering; maxListeners caps concurrent clients.
func NewBroadcaster(f Format, chunkBytes int, queueTime time.Duration, maxListeners int) *Broadcaster {
	chunkDur := f.Duration(chunkBytes)
	q := int(queueTime / chunkDur)
	if q < 4 {
		q = 4
	}
	b := &Broadcaster{format: f, queueLen: q, max: maxListeners, joined: make(chan struct{}, 1)}
	b.pool.New = func() any { return &Chunk{Data: make([]byte, 0, chunkBytes), b: b} }
	empty := []*Listener{}
	b.listeners.Store(&empty)
	return b
}

// Joined is signalled when a listener connects (wakes an idle engine).
func (b *Broadcaster) Joined() <-chan struct{} { return b.joined }

// Count returns the number of connected listeners.
func (b *Broadcaster) Count() int { return len(*b.listeners.Load()) }

// Evictions returns how many slow listeners were disconnected.
func (b *Broadcaster) Evictions() int64 { return b.evictions.Load() }

// Publish copies p into a pooled chunk and offers it to every listener.
func (b *Broadcaster) Publish(p []byte) {
	ls := *b.listeners.Load()
	if len(ls) == 0 {
		return
	}
	c := b.pool.Get().(*Chunk)
	c.Data = append(c.Data[:0], p...)
	c.refs.Store(int32(len(ls)))
	b.published.Add(1)
	for _, l := range ls {
		select {
		case l.ch <- c:
		default:
			// Slow consumer: evict rather than stall the decoder.
			c.Release()
			if !l.evicted.Load() {
				b.evictions.Add(1)
			}
			l.stop(true)
			go b.remove(l)
		}
	}
}

func (b *Broadcaster) add() (*Listener, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := *b.listeners.Load()
	if b.max > 0 && len(old) >= b.max {
		return nil, apperr.New(apperr.TooManyListeners, "maximum of %d stream listeners reached", b.max)
	}
	l := &Listener{ch: make(chan *Chunk, b.queueLen), done: make(chan struct{})}
	next := make([]*Listener, len(old), len(old)+1)
	copy(next, old)
	next = append(next, l)
	b.listeners.Store(&next)
	select {
	case b.joined <- struct{}{}:
	default:
	}
	return l, nil
}

func (b *Broadcaster) remove(l *Listener) {
	b.mu.Lock()
	old := *b.listeners.Load()
	next := make([]*Listener, 0, len(old))
	for _, x := range old {
		if x != l {
			next = append(next, x)
		}
	}
	b.listeners.Store(&next)
	b.mu.Unlock()
}

// drain releases chunks left in a removed listener's queue.
func drain(l *Listener) {
	for {
		select {
		case c := <-l.ch:
			c.Release()
		default:
			return
		}
	}
}

// Subscribe registers a raw listener (used by tests and benchmarks). The
// returned cancel function must be called when done.
func (b *Broadcaster) Subscribe() (<-chan *Chunk, <-chan struct{}, func(), error) {
	l, err := b.add()
	if err != nil {
		return nil, nil, nil, err
	}
	return l.ch, l.done, func() {
		l.stop(false)
		b.remove(l)
		drain(l)
	}, nil
}

// WAVHeader returns a streaming RIFF/WAVE header with maximal sizes.
func WAVHeader(f Format) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 0xFFFFFFFF)
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(f.SampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(f.SampleRate*f.BytesPerFrame()))
	binary.LittleEndian.PutUint16(h[32:], uint16(f.BytesPerFrame()))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], 0xFFFFFFFF)
	return h
}

// ErrEvicted is returned by ServeHTTP when the listener fell behind.
var ErrEvicted = errors.New("listener evicted: too slow")

// ServeHTTP streams audio to one client until it disconnects, is evicted, or
// shutdown is closed.
func (b *Broadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request, shutdown <-chan struct{}) error {
	l, err := b.add()
	if err != nil {
		return err
	}
	defer func() {
		l.stop(false)
		b.remove(l)
		drain(l)
	}()
	rc := http.NewResponseController(w)
	// The stream endpoint has no write deadline overall, but each write gets
	// a generous deadline so dead TCP peers are detected.
	h := w.Header()
	h.Set("Content-Type", "audio/wav")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Accept-Ranges", "none")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	if _, err := w.Write(WAVHeader(b.format)); err != nil {
		return nil
	}
	_ = rc.Flush()
	ctx := r.Context()
	for {
		select {
		case c := <-l.ch:
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := w.Write(c.Data)
			c.Release()
			if err != nil {
				return nil
			}
			// Coalesce: flush only when the queue is empty.
			if len(l.ch) == 0 {
				if err := rc.Flush(); err != nil {
					return nil
				}
			}
		case <-l.done:
			if l.evicted.Load() {
				return ErrEvicted
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-shutdown:
			return nil
		}
	}
}
