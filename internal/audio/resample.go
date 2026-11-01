package audio

import (
	"errors"
	"io"
)

// Resampler converts a stereo float32 Source to another sample rate using
// linear interpolation. It keeps state across calls so output is continuous.
type Resampler struct {
	src     Source
	step    float64 // source frames per output frame
	pos     float64 // fractional position relative to buf frame 0
	buf     []float32
	frames  int // valid frames in buf
	srcDone bool
}

// NewResampler wraps src to produce outRate output.
func NewResampler(src Source, outRate int) *Resampler {
	return &Resampler{
		src:  src,
		step: float64(src.SampleRate()) / float64(outRate),
		buf:  make([]float32, 0, 4096*Channels),
	}
}

func (r *Resampler) fill() error {
	if r.srcDone {
		return io.EOF
	}
	// Keep the last frame (needed for interpolation) and drop consumed ones.
	drop := int(r.pos)
	if drop > r.frames-1 {
		drop = r.frames - 1
	}
	if drop > 0 {
		copy(r.buf[:cap(r.buf)], r.buf[drop*Channels:r.frames*Channels])
		r.frames -= drop
		r.pos -= float64(drop)
	}
	space := cap(r.buf)/Channels - r.frames
	if space <= 0 {
		return nil
	}
	b := r.buf[:cap(r.buf)]
	n, err := r.src.Read(b[r.frames*Channels : (r.frames+space)*Channels])
	r.frames += n
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.srcDone = true
			if n > 0 {
				return nil
			}
			return io.EOF
		}
		return err
	}
	return nil
}

// Read produces interleaved stereo frames at the output rate.
func (r *Resampler) Read(dst []float32) (int, error) {
	want := len(dst) / Channels
	out := 0
	for out < want {
		i := int(r.pos)
		if i+1 >= r.frames {
			if err := r.fill(); err != nil {
				if errors.Is(err, io.EOF) {
					// Flush the final frame without a successor.
					if i < r.frames && r.frames > 0 && i >= 0 && out < want && r.pos-float64(i) == 0 {
						dst[out*2] = r.buf[i*2]
						dst[out*2+1] = r.buf[i*2+1]
						out++
						r.pos += r.step
					}
					if out == 0 {
						return 0, io.EOF
					}
					return out, nil
				}
				return out, err
			}
			continue
		}
		frac := float32(r.pos - float64(i))
		a := r.buf[i*2 : i*2+4]
		dst[out*2] = a[0] + (a[2]-a[0])*frac
		dst[out*2+1] = a[1] + (a[3]-a[1])*frac
		out++
		r.pos += r.step
	}
	return out, nil
}
