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

// DefaultDeviceBuffer is the default ring buffer duration for local hardware
// playback (1.0 s ≈ 176 KiB at 44.1 kHz stereo 16-bit).
const DefaultDeviceBuffer = 1000 * time.Millisecond

// otoHardwareBuffer is the period/buffer duration requested from oto's
// underlying PulseAudio/PipeWire/ALSA driver.
const otoHardwareBuffer = 100 * time.Millisecond

// Ring is a single-producer/single-consumer byte ring buffer synchronised with
// atomics and lightweight notification channels. The producer blocks when full;
// the consumer waits briefly when empty instead of injecting fake silence.
type Ring struct {
	buf     []byte
	size    uint64
	w       atomic.Uint64 // total bytes written
	r       atomic.Uint64 // total bytes read
	space   chan struct{} // signalled by the consumer after reading
	data    chan struct{} // signalled by the producer after writing
	flushTo atomic.Uint64 // consumer skips to this write offset
}

// NewRing returns a ring of the given capacity.
func NewRing(capacity int) *Ring {
	return &Ring{
		buf:   make([]byte, capacity),
		size:  uint64(capacity),
		space: make(chan struct{}, 1),
		data:  make(chan struct{}, 1),
	}
}

// Buffered returns the number of unread bytes.
func (r *Ring) Buffered() int {
	w, rd := r.w.Load(), r.r.Load()
	if ft := r.flushTo.Load(); ft > rd {
		rd = ft
	}
	if w <= rd {
		return 0
	}
	return int(w - rd)
}

// Write copies p into the ring, blocking while full. It returns early with
// an error if closed is signalled.
func (r *Ring) Write(p []byte, closed <-chan struct{}) error {
	for len(p) > 0 {
		w, rd := r.w.Load(), r.r.Load()
		if ft := r.flushTo.Load(); ft > rd {
			rd = ft
		}
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
		select {
		case r.data <- struct{}{}:
		default:
		}
		p = p[n:]
	}
	return nil
}

// Read copies up to len(p) available bytes out of the ring. Unlike the previous
// implementation, it never zero-pads p[n:] with fake silence; if the ring is
// empty it waits briefly for producer data or returns 0.
func (r *Ring) Read(p []byte, closed <-chan struct{}, frameBytes int) (int, error) {
	if frameBytes <= 0 {
		frameBytes = 4
	}
	for {
		w, rd := r.w.Load(), r.r.Load()
		if ft := r.flushTo.Load(); ft > rd {
			rd = ft
			r.r.Store(rd)
		}
		avail := w - rd
		if avail >= uint64(frameBytes) {
			n := uint64(len(p))
			if n > avail {
				n = avail
			}
			n -= n % uint64(frameBytes)
			if n == 0 {
				return 0, nil
			}
			off := rd % r.size
			first := min(n, r.size-off)
			copy(p[:first], r.buf[off:off+first])
			if first < n {
				copy(p[first:n], r.buf[0:n-first])
			}
			r.r.Store(rd + n)
			select {
			case r.space <- struct{}{}:
			default:
			}
			return int(n), nil
		}

		select {
		case <-r.data:
			continue
		case <-closed:
			return 0, errors.New("closed")
		case <-time.After(10 * time.Millisecond):
			return 0, nil
		}
	}
}

// Flush asks the consumer to drop everything written so far. It must be
// called from the producer goroutine.
func (r *Ring) Flush() {
	r.flushTo.Store(r.w.Load())
	select {
	case r.space <- struct{}{}:
	default:
	}
	select {
	case r.data <- struct{}{}:
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
	if bufferTime <= 0 {
		bufferTime = DefaultDeviceBuffer
	}
	otoMu.Lock()
	defer otoMu.Unlock()
	if otoCtx == nil {
		ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
			SampleRate:      f.SampleRate,
			ChannelCount:    f.Channels,
			Format:          oto.FormatSignedInt16LE,
			BufferSize:      otoHardwareBuffer,
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
	d.paused.Store(true)
	d.player = otoCtx.NewPlayer(deviceReader{d})
	hwBufBytes := int(int64(f.SampleRate*f.BytesPerFrame()) * int64(otoHardwareBuffer) / int64(time.Second))
	hwBufBytes -= hwBufBytes % f.BytesPerFrame()
	d.player.SetBufferSize(hwBufBytes)
	return d, nil
}

type deviceReader struct{ d *Device }

func (r deviceReader) Read(p []byte) (int, error) {
	select {
	case <-r.d.closed:
		return 0, errors.New("closed")
	default:
	}
	if r.d.paused.Load() {
		return 0, nil
	}
	n, err := r.d.ring.Read(p, r.d.closed, r.d.format.BytesPerFrame())
	if err != nil {
		return 0, err
	}
	if n == 0 && !r.d.paused.Load() {
		r.d.underrs.Add(1)
	}
	return n, nil
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

// Underruns returns how many device reads found the ring empty while playing.
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
