package audio

import (
	"bufio"
	"errors"
	"io"
	"os"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/jfreymuth/oggvorbis"
	"github.com/mewkiz/flac"
)

// ---- Ogg/Vorbis ----

type vorbisSource struct {
	f    *os.File
	r    *oggvorbis.Reader
	ch   int
	buf  []float32
	tot  int64
	rate int
}

func newVorbis(f *os.File) (Source, error) {
	r, err := oggvorbis.NewReader(f)
	if err != nil {
		return nil, apperr.Wrap(apperr.DecodeError, err, "invalid Ogg/Vorbis")
	}
	ch := r.Channels()
	if ch < 1 {
		return nil, apperr.New(apperr.DecodeError, "invalid channel count")
	}
	return &vorbisSource{f: f, r: r, ch: ch, tot: r.Length(), rate: r.SampleRate()}, nil
}

func (v *vorbisSource) SampleRate() int { return v.rate }
func (v *vorbisSource) TotalFrames() int64 {
	if v.tot <= 0 {
		return -1
	}
	return v.tot
}
func (v *vorbisSource) Close() error { return v.f.Close() }

func (v *vorbisSource) Read(dst []float32) (int, error) {
	frames := len(dst) / Channels
	if v.ch == 2 {
		n, err := v.r.Read(dst[:frames*2])
		return n / 2, eofOr(err, n)
	}
	need := frames * v.ch
	if cap(v.buf) < need {
		v.buf = make([]float32, need)
	}
	n, err := v.r.Read(v.buf[:need])
	got := n / v.ch
	for i := 0; i < got; i++ {
		l := v.buf[i*v.ch]
		r := l
		if v.ch > 1 {
			r = v.buf[i*v.ch+1]
		}
		dst[i*2], dst[i*2+1] = l, r
	}
	return got, eofOr(err, got)
}

func eofOr(err error, n int) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		if n > 0 {
			return nil
		}
		return io.EOF
	}
	return err
}

// ---- FLAC ----

type flacSource struct {
	f      *os.File
	s      *flac.Stream
	ch     int
	scale  float32
	rate   int
	total  int64
	l, r   []int32
	off    int
	frames int
}

func newFLAC(f *os.File) (Source, error) {
	s, err := flac.New(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return nil, apperr.Wrap(apperr.DecodeError, err, "invalid FLAC")
	}
	if s.Info == nil || s.Info.NChannels == 0 || s.Info.BitsPerSample == 0 {
		return nil, apperr.New(apperr.DecodeError, "invalid FLAC stream info")
	}
	tot := int64(s.Info.NSamples)
	if tot == 0 {
		tot = -1
	}
	return &flacSource{
		f:     f,
		s:     s,
		ch:    int(s.Info.NChannels),
		scale: 1 / float32(int64(1)<<(s.Info.BitsPerSample-1)),
		rate:  int(s.Info.SampleRate),
		total: tot,
	}, nil
}

func (s *flacSource) SampleRate() int    { return s.rate }
func (s *flacSource) TotalFrames() int64 { return s.total }
func (s *flacSource) Close() error       { return s.f.Close() }

func (s *flacSource) Read(dst []float32) (int, error) {
	want := len(dst) / Channels
	out := 0
	for out < want {
		if s.off >= s.frames {
			fr, err := s.s.ParseNext()
			if err != nil {
				if errors.Is(err, io.EOF) {
					if out > 0 {
						return out, nil
					}
					return 0, io.EOF
				}
				if out > 0 {
					return out, nil
				}
				return 0, err
			}
			if len(fr.Subframes) == 0 {
				continue
			}
			s.l = fr.Subframes[0].Samples
			if s.ch > 1 && len(fr.Subframes) > 1 {
				s.r = fr.Subframes[1].Samples
			} else {
				s.r = s.l
			}
			s.frames = len(s.l)
			s.off = 0
		}
		n := min(want-out, s.frames-s.off)
		for i := 0; i < n; i++ {
			dst[(out+i)*2] = float32(s.l[s.off+i]) * s.scale
			dst[(out+i)*2+1] = float32(s.r[s.off+i]) * s.scale
		}
		out += n
		s.off += n
	}
	return out, nil
}
