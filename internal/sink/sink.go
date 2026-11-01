// Package sink implements audio outputs: the host device, the HTTP stream
// broadcaster, and a null sink for tests.
package sink

import (
	"time"
)

// Kind selects the output configuration.
type Kind string

// Sink kinds.
const (
	KindDevice Kind = "device"
	KindHTTP   Kind = "http"
	KindBoth   Kind = "both"
	KindNull   Kind = "null"
)

// ParseKind validates a sink name.
func ParseKind(s string) (Kind, bool) {
	switch Kind(s) {
	case KindDevice, KindHTTP, KindBoth, KindNull:
		return Kind(s), true
	}
	return "", false
}

// HasDevice reports whether the kind drives the audio device.
func (k Kind) HasDevice() bool { return k == KindDevice || k == KindBoth }

// HasHTTP reports whether the kind serves HTTP streams.
func (k Kind) HasHTTP() bool { return k == KindHTTP || k == KindBoth }

// Format describes the PCM stream: signed 16-bit little-endian interleaved.
type Format struct {
	SampleRate int
	Channels   int
}

// BytesPerFrame is the size of one interleaved frame.
func (f Format) BytesPerFrame() int { return 2 * f.Channels }

// Duration converts a byte count to playback time.
func (f Format) Duration(bytes int) time.Duration {
	return time.Duration(int64(bytes) * int64(time.Second) / int64(f.SampleRate*f.BytesPerFrame()))
}

// Output is a clocked PCM consumer. Write may block to pace the producer.
type Output interface {
	// Write consumes a chunk of PCM. Implementations must not retain p.
	Write(p []byte) error
	// Pause stops audible output, keeping buffered audio.
	Pause()
	// Resume restarts output after Pause.
	Resume()
	// Flush drops buffered audio (track change, stop).
	Flush()
	Close() error
}

// Null discards audio. It does not pace; the engine uses a wall-clock pacer.
type Null struct{}

func (Null) Write([]byte) error { return nil }
func (Null) Pause()             {}
func (Null) Resume()            {}
func (Null) Flush()             {}
func (Null) Close() error       { return nil }

// Pacer throttles a producer to real time (scaled by Speed) when no device
// clock is available. It allows a small lead so downstream buffers stay full.
type Pacer struct {
	rate   int // frames per second
	speed  float64
	lead   time.Duration
	start  time.Time
	frames int64
}

// NewPacer returns a pacer for the given frame rate and speed multiplier.
func NewPacer(rate int, speed float64) *Pacer {
	if speed <= 0 {
		speed = 1
	}
	return &Pacer{rate: rate, speed: speed, lead: 100 * time.Millisecond}
}

// Reset restarts the clock (after pause or idle).
func (p *Pacer) Reset() { p.start = time.Time{}; p.frames = 0 }

// Wait accounts for frames and sleeps until they are due.
func (p *Pacer) Wait(frames int) {
	now := time.Now()
	if p.start.IsZero() {
		p.start = now
	}
	p.frames += int64(frames)
	due := p.start.Add(time.Duration(float64(p.frames) * float64(time.Second) / (float64(p.rate) * p.speed)))
	if d := due.Sub(now) - p.lead; d > 0 {
		time.Sleep(d)
	} else if d < -2*time.Second {
		// Fell far behind (system suspend): resynchronise instead of bursting.
		p.Reset()
	}
}
