package audio

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/frdn/eklips/internal/apperr"
)

const (
	wavFormatPCM        = 1
	wavFormatFloat      = 3
	wavFormatExtensible = 0xFFFE
)

type wavSource struct {
	f          *os.File
	r          *bufio.Reader
	rate       int
	channels   int
	bits       int
	float      bool
	frameBytes int
	remaining  int64 // bytes left in data chunk
	total      int64
	raw        []byte
}

func newWAV(f *os.File) (Source, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, apperr.New(apperr.DecodeError, "not a WAV file: %v", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, apperr.New(apperr.DecodeError, "not a RIFF/WAVE file")
	}
	w := &wavSource{f: f}
	gotFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return nil, apperr.New(apperr.DecodeError, "WAV data chunk not found")
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			if size < 16 || size > 1<<16 {
				return nil, apperr.New(apperr.DecodeError, "invalid WAV fmt chunk")
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(f, b); err != nil {
				return nil, apperr.New(apperr.DecodeError, "truncated WAV fmt chunk")
			}
			tag := binary.LittleEndian.Uint16(b[0:2])
			w.channels = int(binary.LittleEndian.Uint16(b[2:4]))
			w.rate = int(binary.LittleEndian.Uint32(b[4:8]))
			w.bits = int(binary.LittleEndian.Uint16(b[14:16]))
			if tag == wavFormatExtensible && size >= 40 {
				tag = binary.LittleEndian.Uint16(b[24:26])
			}
			switch tag {
			case wavFormatPCM:
				if w.bits != 8 && w.bits != 16 && w.bits != 24 && w.bits != 32 {
					return nil, apperr.New(apperr.UnsupportedFormat, "unsupported WAV bit depth %d", w.bits)
				}
			case wavFormatFloat:
				if w.bits != 32 && w.bits != 64 {
					return nil, apperr.New(apperr.UnsupportedFormat, "unsupported WAV float depth %d", w.bits)
				}
				w.float = true
			default:
				return nil, apperr.New(apperr.UnsupportedFormat, "unsupported WAV encoding 0x%x", tag)
			}
			if w.channels < 1 || w.channels > 32 || w.rate < 1000 || w.rate > 768000 {
				return nil, apperr.New(apperr.DecodeError, "invalid WAV format (%d ch, %d Hz)", w.channels, w.rate)
			}
			if size%2 == 1 {
				f.Seek(1, io.SeekCurrent)
			}
			gotFmt = true
		case "data":
			if !gotFmt {
				return nil, apperr.New(apperr.DecodeError, "WAV data before fmt chunk")
			}
			w.frameBytes = w.channels * w.bits / 8
			// Streaming WAVs may declare 0 or 0xFFFFFFFF; clamp to file size.
			if st, err := f.Stat(); err == nil {
				pos, _ := f.Seek(0, io.SeekCurrent)
				if avail := st.Size() - pos; size == 0 || size == 0xFFFFFFFF || size > avail {
					size = avail
				}
			}
			size -= size % int64(w.frameBytes)
			w.remaining = size
			w.total = size / int64(w.frameBytes)
			w.r = bufio.NewReaderSize(f, 64<<10)
			return w, nil
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				return nil, apperr.New(apperr.DecodeError, "truncated WAV chunk %q", id)
			}
		}
	}
}

func (w *wavSource) SampleRate() int    { return w.rate }
func (w *wavSource) TotalFrames() int64 { return w.total }
func (w *wavSource) Close() error       { return w.f.Close() }

func (w *wavSource) Read(dst []float32) (int, error) {
	if w.remaining <= 0 {
		return 0, io.EOF
	}
	frames := len(dst) / Channels
	if max := w.remaining / int64(w.frameBytes); int64(frames) > max {
		frames = int(max)
	}
	need := frames * w.frameBytes
	if cap(w.raw) < need {
		w.raw = make([]byte, need)
	}
	raw := w.raw[:need]
	n, err := io.ReadFull(w.r, raw)
	frames = n / w.frameBytes
	w.remaining -= int64(n)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read WAV: %w", err)
	}
	if err != nil {
		w.remaining = 0
	}
	bps := w.bits / 8
	for i := 0; i < frames; i++ {
		base := i * w.frameBytes
		l := w.sample(raw[base:])
		r := l
		if w.channels > 1 {
			r = w.sample(raw[base+bps:])
		}
		dst[i*2] = l
		dst[i*2+1] = r
	}
	if frames == 0 {
		return 0, io.EOF
	}
	return frames, nil
}

func (w *wavSource) sample(b []byte) float32 {
	if w.float {
		if w.bits == 32 {
			return math.Float32frombits(binary.LittleEndian.Uint32(b))
		}
		return float32(math.Float64frombits(binary.LittleEndian.Uint64(b)))
	}
	switch w.bits {
	case 8:
		return (float32(b[0]) - 128) / 128
	case 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case 24:
		v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
		return float32(v) / 8388608
	default:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	}
}
