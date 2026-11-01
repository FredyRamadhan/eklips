package sink

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ebitengine/oto/v3"

	"github.com/frdn/eklips/internal/apperr"
)

// Ring is a single-producer/single-consumer byte ring buffer synchronised with
// atomics only. The producer blocks (waiting on a signal channel) when full.
type Ring struct {
	buf   []byte
	size  uint64
	w     atomic.Uint64 // total bytes written
	r     atomic.Uint64 // total bytes read
	space chan struct{} // signalled by the consumer after reading
	flushTo atomic.Uint64 // consumer skips to this write offset
}

// NewRing returns a ring of the given capacity.
func NewRing(capacity int) *Ring {
	return &Ring{buf: make([]byte, capacity), size: uint64(capacity), space: make(chan struct{}, 1)}
}

// Buffered returns the number of unread bytes.
func (r *Ring) Buffered() int { return int(r.w.Load() - r.r.Load()) }

// Write copies p into the ring, blocking while full. It returns early with
// an error if closed is signalled.
func (r *Ring) Write(p []byte, closed <-chan struct{}) error {
	for len(p) > 0 {
		w, rd := r.w.Load(), r.r.Load()
		free := r.size - (w - rd)
		if free == 0 {
			select {
			case <-r.space:
			case <-closed:
				return errors.New("sink closed")
			case <-time.After(time.Second):
				// Consumer stalled (device suspended); keep waiting but stay responsive.
			}
			continue
		}
		n := uint64(len(p))
		if n > free {
			n = free
		}
		off := w % r.size
		first := min(n, r.size-off)
		copy(r.buf[off:off+first], p[:first])
		if first < n {
			copy(r.buf[0:n-first], p[first:n])
		}
		r.w.Store(w + n)
		p = p[n:]
	}
	return nil
}

// Read copies up to len(p) bytes out of the ring without blocking. Missing
// bytes are zero-filled (silence), so the device never stalls.
func (r *Ring) Read(p []byte) int {
	w, rd := r.w.Load(), r.r.Load()
	if ft := r.flushTo.Load(); ft > rd {
		rd = ft
		r.r.Store(rd)
	}
	avail := w - rd
	n := uint64(len(p))
	if n > avail {
		n = avail
	}
	off := rd % r.size
	first := min(n, r.size-off)
	copy(p[:first], r.buf[off:off+first])
	if first < n {
		copy(p[first:n], r.buf[0:n-first])
	}
	clear(p[n:])
	r.r.Store(rd + n)
	if n > 0 {
		select {
		case r.space <- struct{}{}:
		default:
		}
	}
	return int(n)
}

// Flush asks the consumer to drop everything written so far. It must be
// called from the producer goroutine.
func (r *Ring) Flush() {
	r.flushTo.Store(r.w.Load())
	select {
	case r.space <- struct{}{}:
	default:
	}
}

// Device plays PCM through the host audio device via oto (PulseAudio/PipeWire
// with ALSA fallback on Linux).
type Device struct {
	ctx     *oto.Context
	player  *oto.Player
	ring    *Ring
	closed  chan struct{}
	once    sync.Once
	paused  atomic.Bool
	format  Format
	underrs atomic.Int64
}

var (
	otoMu  sync.Mutex
	otoCtx *oto.Context
	otoFmt Format
)

// OpenDevice opens the default output device. Only one context may exist per
// process, so it is shared across reopenings.
func OpenDevice(f Format, bufferTime time.Duration) (*Device, error) {
	otoMu.Lock()
	defer otoMu.Unlock()
	if otoCtx == nil {
		ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:      f.SampleRate,
			ChannelCount:    f.Channels,
			Format:          oto.FormatSignedInt16LE,
			BufferSize:      40 * time.Millisecond,
			ApplicationName: "eklips",
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.SinkUnavailable, err, "open audio device")
		}
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			return nil, apperr.New(apperr.SinkUnavailable, "audio device did not become ready")
		}
		if err := ctx.Err(); err != nil {
			return nil, apperr.Wrap(apperr.SinkUnavailable, err, "open audio device")
		}
		otoCtx, otoFmt = ctx, f
	} else if otoFmt != f {
		return nil, apperr.New(apperr.SinkUnavailable, "audio device already open with a different format")
	}
	ringBytes := int(int64(f.SampleRate*f.BytesPerFrame()) * int64(bufferTime) / int64(time.Second))
	ringBytes -= ringBytes % f.BytesPerFrame()
	d := &Device{ctx: otoCtx, ring: NewRing(ringBytes), closed: make(chan struct{}), format: f}
	d.player = otoCtx.NewPlayer(deviceReader{d})
	d.player.SetBufferSize(f.SampleRate * f.BytesPerFrame() / 25) // 40 ms
	d.player.Play()
	return d, nil
}

type deviceReader struct{ d *Device }

func (r deviceReader) Read(p []byte) (int, error) {
	select {
	case <-r.d.closed:
		return 0, errors.New("closed")
	default:
	}
	n := r.d.ring.Read(p)
	if n < len(p) && !r.d.paused.Load() {
		r.d.underrs.Add(1)
	}
	return len(p), nil
}

// Write queues PCM, blocking while the ring is full (this paces the engine).
func (d *Device) Write(p []byte) error {
	if err := d.player.Err(); err != nil {
		return apperr.Wrap(apperr.SinkUnavailable, err, "audio device error")
	}
	return d.ring.Write(p, d.closed)
}

// Pause pauses device output.
func (d *Device) Pause() {
	d.paused.Store(true)
	d.player.Pause()
}

// Resume resumes device output.
func (d *Device) Resume() {
	d.paused.Store(false)
	d.player.Play()
}

// Flush drops queued audio.
func (d *Device) Flush() { d.ring.Flush() }

// Underruns returns how many device reads found the ring short while playing.
func (d *Device) Underruns() int64 { return d.underrs.Load() }

// Close stops the player.
func (d *Device) Close() error {
	d.once.Do(func() {
		close(d.closed)
		d.player.Pause()
		_ = d.player.Close()
	})
	return nil
}

func (d *Device) String() string {
	return fmt.Sprintf("device(%d Hz)", d.format.SampleRate)
}
